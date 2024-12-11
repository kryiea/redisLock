# RedisLock

## 目录

- [功能](#功能)
- [安装](#安装)
- [使用方法](#使用方法)
  - [创建客户端](#创建客户端)
  - [获取锁](#获取锁)
  - [释放锁](#释放锁)
  - [TryLock](#trylock)
- [API](#api)
  - [Client](#client)
  - [Lock](#lock)
- [错误处理](#错误处理)
- [重试策略](#重试策略)
- [配置](#配置)
- [Lua 脚本](#lua-脚本)
- [贡献](#贡献)
- [许可证](#许可证)

## 功能

- **原子操作**：使用 Lua 脚本确保锁操作的原子性。
- **重试机制**：实现可自定义的重试策略以获取锁。
- **Singleflight 组**：防止同一进程内重复的锁获取尝试。
- **上下文支持**：利用 Go 的 `context.Context` 管理超时和取消操作。
- **基于 UUID 的值**：通过 UUID 确保锁的唯一性。


## 使用方法

以下示例展示了如何在 Go 项目中使用 RedisLock。

### 创建客户端

首先，初始化一个 Redis 客户端并创建 RedisLock 客户端。

```go
package main

import (
    "github.com/redis/go-redis/v9"
    "github.com/kryiea/redislock" 
    "context"
    "log"
)

func main() {
    // 初始化 Redis 客户端
    rdb := redis.NewClient(&redis.Options{
        Addr: "localhost:6379",
        DB:   0, // 使用默认数据库
    })

    // 创建 RedisLock 客户端
    client := redislock.NewClient(rdb)

    // 使用 client...
}
```

### 获取锁

使用 `Lock` 方法获取具有重试能力的分布式锁。

```go
ctx := context.Background()
lockKey := "resource_lock"
expiration := 10 * time.Second
retryStrategy := redislock.NewExponentialBackoff(100*time.Millisecond, 2*time.Second)
timeout := 5 * time.Second

lock, err := client.Lock(ctx, lockKey, expiration, retryStrategy, timeout)
if err != nil {
    log.Fatalf("获取锁失败: %v", err)
}
defer func() {
    if err := lock.Unlock(ctx); err != nil {
        log.Fatalf("释放锁失败: %v", err)
    }
}()

// 在持有锁的情况下执行操作
```

### 释放锁

使用 `Lock` 实例的 `Unlock` 方法释放锁。

```go
err = lock.Unlock(ctx)
if err != nil {
    log.Fatalf("释放锁失败: %v", err)
}
```

### TryLock

使用 `Trylock` 方法进行非阻塞的锁获取尝试。

```go
lock, err := client.Trylock(ctx, lockKey, expiration)
if err != nil {
    if errors.Is(err, redislock.ErrFailedToPreemptLock) {
        log.Println("锁已被其他进程持有")
    } else {
        log.Fatalf("获取锁时发生错误: %v", err)
    }
} else {
    defer lock.Unlock(ctx)
    // 在持有锁的情况下执行操作
}
```

## API

### Client

`Client` 结构体管理与 Redis 的交互以实现锁机制。

#### 方法

- **Lock**

  ```go
  func (c *Client) Lock(ctx context.Context, key string, expiration time.Duration, retry RetryStrategy, timeout time.Duration) (*Lock, error)
  ```

  尝试获取指定 key、过期时间、重试策略和超时的分布式锁。

- **Trylock**

  ```go
  func (c *Client) Trylock(ctx context.Context, key string, expiration time.Duration) (*Lock, error)
  ```

  尝试获取锁，不进行重试。如果锁不可用，立即返回。

### Lock

`Lock` 结构体表示一个分布式锁。

#### 方法

- **Unlock**

  ```go
  func (l *Lock) Unlock(ctx context.Context) error
  ```

  释放已获取的锁。确保只有锁的持有者可以释放锁。

## 错误处理

RedisLock 定义了多个错误变量以处理常见的失败场景：

- **ErrFailedToPreemptLock**

  在锁获取尝试耗尽但仍未成功时返回。

- **ErrLockNotHold**

  尝试释放一个未持有或已释放的锁时返回。

## 重试策略

RedisLock 支持在尝试获取锁时自定义重试策略。您可以通过实现 `RetryStrategy` 接口来自定义策略，或使用提供的策略如指数退避。

```go
type RetryStrategy interface {
    Next() (time.Duration, bool)
}
```

使用指数退避的示例：

```go
retryStrategy := redislock.NewExponentialBackoff(100*time.Millisecond, 2*time.Second)
```

## 配置

RedisLock 提供了灵活的配置选项以适应不同的应用需求，包括锁的过期时间、重试策略和超时设置。

### 参数

- **key**: 锁的唯一标识符。
- **expiration**: 锁的自动过期时间。
- **retryStrategy**: 定义在获取锁失败时的重试行为策略。
- **timeout**: 在放弃获取锁之前的最大尝试时间。

## Lua 脚本

RedisLock 使用 Lua 脚本确保锁操作的原子性：

- **lock.lua**: 原子性地管理锁的获取。
- **unlock.lua**: 确保只有锁的持有者可以释放锁。
- **refresh.lua**: 处理锁过期时间的刷新（如有需要）。

这些脚本通过 Go 的 `embed` 包嵌入到代码中，确保高效执行和一致性。

