package redislock

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

var (
	// go:embed./lock.lua
	luaLock string
	// go:embed ./unlock.lua
	luaUnlock string
	// go:embed./refresh.lua
	luaRefresh string

	ErrFailedToPreemptLock = errors.New("rloock: 获取锁失败")
	ErrLockNotHold         = errors.New("rlock: 未持有锁")
)

type Client struct {
	client redis.Cmdable      // 连接对象
	g      singleflight.Group // 用于并发控制的单例模式
	valuer func() string      // 生成锁value的函数
}

func newClient(c redis.Cmdable) *Client {
	return &Client{
		client: c,
		valuer: func() string {
			return uuid.NewString()
		},
	}
}

// lock
func (c *Client) Lock(ctx context.Context, key string, expiration time.Duration, retry RetryStrategy, timeout time.Duration) (*Lock, error) {
	val := c.valuer()

	// 重试策略
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		lctx, cancle := context.WithTimeout(ctx, timeout)
		// 执行lua脚本
		res, err := c.client.Eval(lctx, luaLock, []string{key}, val, expiration.Seconds()).Result()
		cancle()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			// 非超时错误，说明遇到一些难以挽回的场景，没太大必要重试
			return nil, err
		}
		if res == "OK" {
			// 获取锁成功
			return newLock(c.client, key, val, expiration), nil
		}
		// 获取锁失败，需要重试
		interval, ok := retry.Next()
		// 没有重试机会了
		if !ok {
			if err != nil {
				err = fmt.Errorf("最后一次重试错误 %w", err)
			} else {
				err = fmt.Errorf("锁被人持有: %w", ErrFailedToPreemptLock)
			}
			return nil, fmt.Errorf("rlock: 重试机会耗尽，%w", err)
		}
		if timer == nil {
			timer = time.NewTimer(interval)
		} else {
			timer.Reset(interval)
		}

		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

	}

}

func (c *Client) Trylock(ctx context.Context, key string, expiration time.Duration) (*Lock, error) {
	val := c.valuer()
	ok, err := c.client.SetNX(ctx, key, val, expiration).Result()
	if err != nil {
		// 网络问题或者超时了
		return nil, err
	}
	if !ok {
		return nil, ErrFailedToPreemptLock
	}
	// 获取锁成功
	return newLock(c.client, key, val, expiration), nil
}

type Lock struct {
	client           redis.Cmdable // 连接对象
	key              string        // 锁的key
	value            string        // 锁的value
	expiration       time.Duration // 锁的过期时间
	unlock           chan struct{} // 用于通知锁的释放
	signalUnlockOnce sync.Once     // 用于保证信号量只发送一次
}

// newLock 创建一个新的锁对象
func newLock(c redis.Cmdable, key string, val string, expiration time.Duration) *Lock {
	return &Lock{
		client:           c,
		key:              key,
		value:            val,
		expiration:       expiration,
		unlock:           make(chan struct{}, 1),
		signalUnlockOnce: sync.Once{},
	}
}

// Unlock 函数用于释放分布式锁
func (l *Lock) Unlock(ctx context.Context) error {
	// 执行 Lua 脚本解锁，并获取返回结果
	res, err := l.client.Eval(ctx, luaUnlock, []string{l.key}, l.value).Int64()
	// 延迟执行，确保在函数返回前执行
	defer func() {
		// 避免重复解锁导致的 panic
		l.signalUnlockOnce.Do(func() {
			// 发送解锁信号
			l.unlock <- struct{}{}
			// 关闭解锁通道
			close(l.unlock)
		})
	}()
	// 如果 key 不存在，返回错误
	if err == redis.Nil {
		return ErrLockNotHold
	}
	// 如果执行过程中发生错误，返回错误
	if err != nil {
		return err
	}
	// 如果返回结果不等于 1，说明锁被其他持有者持有
	if res != 1 {
		return ErrLockNotHold
	}
	// 解锁成功，返回 nil
	return nil
}

// AutoRefresh 函数会在指定的时间间隔内尝试刷新锁的过期时间（续约），直到锁被释放或者发生错误
func (l *Lock) AutoRefresh(interval time.Duration, timeout time.Duration) error {
	// 创建一个定时器，用于定时刷新锁
	ticker := time.NewTicker(interval)
	// 用于处理刷新超时的 channel
	ch := make(chan struct{}, 1)
	// 延迟关闭定时器和 channel
	defer func() {
		ticker.Stop()
		close(ch)
	}()
	// 无限循环，直到锁被释放或者发生错误
	for {
		select {
		// 定时器触发，尝试刷新锁
		case <-ticker.C:
			// 创建一个带超时的上下文
			ctx, cannel := context.WithTimeout(context.Background(), timeout)
			// 尝试刷新锁
			err := l.Refresh(ctx)
			// 取消上下文
			cannel()
			// 如果刷新超时，继续尝试
			if err == context.DeadlineExceeded {
				// 因为有两个可能的地方要写入数据，而 ch
				// 容量只有一个，所以如果写不进去就说明前一次调用超时了，并且还没被处理，
				// 与此同时计时器也触发了
				select {
				// 将超时事件写入 channel
				case ch <- struct{}{}:
				// 如果 channel 已满，不做任何处理
				default:
				}
				continue
			}
		// 从 channel 中读取超时事件，尝试刷新锁
		case <-ch:
			// 创建一个带超时的上下文
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			// 尝试刷新锁
			err := l.Refresh(ctx)
			// 取消上下文
			cancel()
			// 如果超时，可以继续尝试
			if err == context.DeadlineExceeded {
				// 将超时事件写入 channel
				select {
				case ch <- struct{}{}:
				// 如果 channel 已满，不做任何处理
				default:
				}
				continue
			}
			// 如果刷新失败，返回错误
			if err != nil {
				return err
			}
		// 如果锁被释放，退出循环
		case <-l.unlock:
			return nil
		}
	}
}

// Refresh 函数用于刷新 Redis 锁的过期时间
func (l *Lock) Refresh(ctx context.Context) error {
	// 使用 Lua 脚本刷新锁的过期时间
	res, err := l.client.Eval(ctx, luaRefresh,
		[]string{l.key}, l.value, l.expiration.Seconds()).Int64()
	// 如果执行脚本时发生错误，返回错误
	if err != nil {
		return err
	}
	// 如果返回结果不是 1，表示锁没有被持有，返回错误
	if res != 1 {
		return ErrLockNotHold
	}
	// 刷新成功，返回 nil
	return nil
}
