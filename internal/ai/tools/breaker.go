package tools

import (
	"context"
	"sync"
	"time"
)

// 熔断：单工具连续失败到阈值即"打开"，冷却期内直接拒绝（不再打数据源）；
// 冷却结束后放一个试探请求（半开）。**打开/关闭都要计数**，否则"工具挂了"
// 会伪装成"用户没问这类问题"（§10.1 路径④同理）。
type breaker struct {
	mu        sync.Mutex
	fails     map[string]int
	openUntil map[string]time.Time
	threshold int
	cooldown  time.Duration
	now       func() time.Time
	cnt       Counter
}

func newBreaker(threshold int, cooldown time.Duration, cnt Counter) *breaker {
	if threshold <= 0 {
		threshold = 3
	}
	return &breaker{
		fails:     map[string]int{},
		openUntil: map[string]time.Time{},
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
		cnt:       cnt,
	}
}

// allow 是否放行（冷却期内拒绝；冷却到期放行并计数半开）
func (b *breaker) allow(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	until, ok := b.openUntil[name]
	if !ok {
		return true
	}
	if b.now().Before(until) {
		b.inc("tool_circuit_rejected_total")
		return false
	}
	delete(b.openUntil, name) // 半开：放一个请求进去试探
	b.inc("tool_circuit_halfopen_total")
	return true
}

// success 成功即清零（半开成功 → 恢复）
func (b *breaker) success(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails[name] = 0
	delete(b.openUntil, name)
}

// fail 记一次失败，返回是否刚打开熔断
func (b *breaker) fail(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails[name]++
	if b.fails[name] >= b.threshold {
		b.openUntil[name] = b.now().Add(b.cooldown)
		b.inc("tool_circuit_open_total")
		b.fails[name] = 0 // 打开后清零，冷却结束重新计数
		return true
	}
	return false
}

func (b *breaker) inc(key string) {
	if b.cnt != nil {
		b.cnt.Inc(key)
	}
}

// withTimeout 统一超时（§8.5.4：1.5s）。超时归入 unavailable 并计数，
// 不与"查不到"混淆 —— 用户看到的话术相同（转人工），但口径必须分开。
func withTimeout(parent context.Context, d time.Duration, fn func(context.Context) Result) (Result, bool) {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()

	done := make(chan Result, 1)
	go func() { done <- fn(ctx) }()

	select {
	case r := <-done:
		return r, false
	case <-ctx.Done():
		return Result{Kind: KindUnavailable, Reason: "tool_timeout"}, true
	}
}
