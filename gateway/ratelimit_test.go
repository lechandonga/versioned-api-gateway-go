package gateway

import (
	"sync"
	"testing"
	"time"
)

// TestRateLimitConcurrentBurst 并发突发下配额不能被突破：
// Burst=10，100 个并发请求，放行数必须恰好等于 10。
func TestRateLimitConcurrentBurst(t *testing.T) {
	l := NewRateLimiter(RateLimitConfig{Rate: 1, Burst: 10})
	l.SetClock(func() time.Time { return time.Unix(0, 0) }) // 冻结时钟，不补充令牌

	const total = 100
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _ := l.Allow("caller-a", "/orders")
			if ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 10 {
		t.Fatalf("并发突发下放行 %d 个，期望恰好 10（配额被突破或误杀）", allowed)
	}
	t.Logf("ctx: caller=caller-a endpoint=/orders burst=10 并发=%d -> 放行=%d 拒绝=%d", total, allowed, total-allowed)
}

// TestRateLimitRefillAcrossWindow 窗口推进时令牌按速率精确补充，
// 不存在固定窗口边界的双倍放行。
func TestRateLimitRefillAcrossWindow(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewRateLimiter(RateLimitConfig{Rate: 5, Burst: 5}) // 每秒 5 个
	l.SetClock(func() time.Time { return now })

	// 打空桶
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("c", "/e"); !ok {
			t.Fatalf("第 %d 个请求应放行", i)
		}
	}
	if ok, wait := l.Allow("c", "/e"); ok {
		t.Fatal("桶空后应被限流")
	} else {
		t.Logf("ctx: 桶空 -> 拒绝 retryAfter=%.3fs（判定依据: 令牌不足）", wait)
	}

	// 窗口边界：0.9 秒后再打 5 个，只补充了 4.5 个令牌，第 5 个必须被拒绝。
	now = now.Add(900 * time.Millisecond)
	allowed := 0
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("c", "/e"); ok {
			allowed++
		}
	}
	if allowed != 4 {
		t.Fatalf("窗口边界放行 %d 个，期望 4（令牌桶按时间精确补充，不允许边界突刺）", allowed)
	}
	t.Logf("ctx: 窗口推进 0.9s -> 补充 4.5 令牌，放行=%d", allowed)
}

// TestRateLimitPerCallerAndEndpoint 限流维度是 caller+endpoint，互不影响。
func TestRateLimitPerCallerAndEndpoint(t *testing.T) {
	l := NewRateLimiter(RateLimitConfig{Rate: 1, Burst: 2})
	l.SetClock(func() time.Time { return time.Unix(0, 0) })

	l.Allow("a", "/e1")
	l.Allow("a", "/e1")
	if ok, _ := l.Allow("a", "/e1"); ok {
		t.Fatal("caller=a endpoint=/e1 应被限流")
	}
	if ok, _ := l.Allow("a", "/e2"); !ok {
		t.Fatal("同 caller 不同 endpoint 应有独立配额")
	}
	if ok, _ := l.Allow("b", "/e1"); !ok {
		t.Fatal("不同 caller 应有独立配额")
	}
	t.Log("ctx: caller+endpoint 维度隔离验证通过")
}

// TestRateLimitHotReload 配置热更新无需重启即生效。
func TestRateLimitHotReload(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewRateLimiter(RateLimitConfig{Rate: 1, Burst: 2})
	l.SetClock(func() time.Time { return now })

	l.Allow("c", "/e")
	l.Allow("c", "/e")
	if ok, _ := l.Allow("c", "/e"); ok {
		t.Fatal("默认配额已耗尽，应被限流")
	}

	// 热更新：调大 caller+endpoint 的配额，新封顶立即生效（无需重启）。
	// 注意：调大 Burst 不凭空发令牌，而是提高补充封顶。
	l.SetConfig("c", "/e", RateLimitConfig{Rate: 5, Burst: 5})
	now = now.Add(time.Second) // 补充 5 个令牌；若仍按旧 Burst=2 封顶只会补到 2
	allowed := 0
	for i := 0; i < 6; i++ {
		if ok, _ := l.Allow("c", "/e"); ok {
			allowed++
		}
	}
	if allowed != 5 {
		t.Fatalf("热更新后 1s 应放行 5 个（新 Burst 生效），实际 %d", allowed)
	}
	t.Logf("ctx: SetConfig 后 burst=5 -> 1s 补充后放行 %d 个（新封顶已生效，无需重启）", allowed)

	// 热更新：调小配额，存量令牌被截断，立即生效。
	l.SetConfig("c", "/e", RateLimitConfig{Rate: 1, Burst: 1})
	if ok, _ := l.Allow("c", "/e"); ok {
		t.Fatal("调小配额后存量令牌应被截断到 1，之前的请求已用掉，应被限流")
	}
	t.Log("ctx: SetConfig 调小 burst=1 -> 存量令牌截断，限流立即生效")
}

// TestRateLimitRetryAfterHint 被限流时给出可用的重试等待提示。
func TestRateLimitRetryAfterHint(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewRateLimiter(RateLimitConfig{Rate: 2, Burst: 1}) // 每 0.5s 一个令牌
	l.SetClock(func() time.Time { return now })

	l.Allow("c", "/e")
	ok, wait := l.Allow("c", "/e")
	if ok {
		t.Fatal("应被限流")
	}
	if wait <= 0 || wait > 0.5 {
		t.Fatalf("retryAfter=%.3f，期望在 (0, 0.5] 区间", wait)
	}
	// 按提示等待后应放行。
	now = now.Add(time.Duration(wait*1000) * time.Millisecond)
	if ok, _ := l.Allow("c", "/e"); !ok {
		t.Fatalf("按 retryAfter=%.3fs 等待后应放行", wait)
	}
	t.Logf("ctx: 限流提示 retryAfter=%.3fs，等待后重试成功", wait)
}
