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

	ErrFailedToPreemptLock = errors.New("rloock: failed to preempt lock")
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

func (l *Lock) Unlock(ctx context.Context) error {
	res, err := l.client.Eval(ctx, luaUnlock, []string{l.key}, l.value).Int64()
	defer func() {
		// 避免重复解锁 panic
		l.signalUnlockOnce.Do(func() {
			l.unlock <- struct{}{}
			close(l.unlock)
		})
	}()
	// key 不存在
	if err == redis.Nil {
		return ErrLockNotHold
	}
	// 解锁失败
	if err != nil {
		return err
	}
	// res 不等于 1 说明锁被人持有
	if res != 1 {
		return ErrLockNotHold
	}
	return nil
}
