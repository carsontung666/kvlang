package dispatch

import "time"

// SetTimeoutForTest 临时调小委托超时，返回还原函数。
// 不这么做就没法覆盖超时路径 —— 每个用例等 30 秒等于没人会写这类测试。
func SetTimeoutForTest(d time.Duration) func() {
	old := defaultTimeout
	defaultTimeout = d
	return func() { defaultTimeout = old }
}
