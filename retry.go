package redislock

import "time"

type RetryStrategy interface {
	// 获取下一次重试的时间间隔和是否继续重试, 第一个参数为重试次数，第二个参数为是否继续重试
	Next() (time.Duration, bool)
}

// 固定时间间隔重试
type FixIntervalRetry struct {
	// 重试间隔
	Interval time.Duration
	// 最大次数
	Max int
	cnt int
}

// 线性重试
func (f *FixIntervalRetry) Next() (time.Duration, bool) {
	f.cnt++
	return f.Interval, f.cnt <= f.Max
}
