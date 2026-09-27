package gateway

import (
	"sync"
	"testing"
	"time"
)

// 并发突发下，放行数绝不能超过桶容量。
func TestRateLimiter_ConcurrentBurstNeverExceedsQuota(t *testing.T) {
	lim := NewRateLimiter(map[string]LimitConfig{
		"orders.create": {RatePerSec: 10, Burst: 20},
	})

	const goroutines = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := lim.Allow("caller-A", "orders.create")
			if d.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	t.Logf("ctx: caller=caller-A api=orders.create burst=20 concurrent=%d -> allowed=%d", goroutines, allowed)
	if allowed != 20 {
		t.Fatalf("quota breached: allowed=%d, want exactly 20 (burst)", allowed)
	}
}

// 限流维度是 (调用方, 接口)：不同调用方互不影响。
func TestRateLimiter_PerCallerIsolation(t *testing.T) {
	lim := NewRateLimiter(map[string]LimitConfig{
		"orders.create": {RatePerSec: 1, Burst: 2},
	})
	for _, caller := range []string{"A", "B"} {
		for i := 0; i < 2; i++ {
			if d := lim.Allow(caller, "orders.create"); !d.Allowed {
				t.Fatalf("caller %s request %d should pass", caller, i)
			}
		}
		if d := lim.Allow(caller, "orders.create"); d.Allowed {
			t.Fatalf("caller %s 3rd request should be limited", caller)
		}
	}
	t.Logf("ctx: burst=2, callers A/B each got 2 tokens independently")
}

// 窗口切换（时间前进）按经过时间精确补令牌，不允许边界重置放大配额。
func TestRateLimiter_WindowRefill(t *testing.T) {
	now := time.Now()
	lim := NewRateLimiter(map[string]LimitConfig{
		"api": {RatePerSec: 10, Burst: 5},
	})
	lim.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		if d := lim.Allow("c", "api"); !d.Allowed {
			t.Fatalf("request %d should pass", i)
		}
	}
	if d := lim.Allow("c", "api"); d.Allowed {
		t.Fatal("6th request should be limited")
	}
	// 前进 0.2s -> 恰好补 2 个令牌，第 3 个必须被拒。
	now = now.Add(200 * time.Millisecond)
	if d := lim.Allow("c", "api"); !d.Allowed {
		t.Fatal("after 0.2s refill, request 1 should pass")
	}
	if d := lim.Allow("c", "api"); !d.Allowed {
		t.Fatal("after 0.2s refill, request 2 should pass")
	}
	d := lim.Allow("c", "api")
	if d.Allowed {
		t.Fatal("after 0.2s refill, request 3 must be rejected")
	}
	t.Logf("ctx: rate=10/s burst=5, advanced 0.2s -> refilled 2 tokens, retry-after=%.3fs", d.RetryAfter)
	if d.RetryAfter <= 0 {
		t.Fatalf("rejected request must carry retry-after hint, got %v", d.RetryAfter)
	}
}

// 配置热更新立即生效，且已有桶的令牌被钳制到新容量内。
func TestRateLimiter_HotReloadConfig(t *testing.T) {
	now := time.Now()
	lim := NewRateLimiter(map[string]LimitConfig{
		"api": {RatePerSec: 100, Burst: 50},
	})
	lim.now = func() time.Time { return now }

	// 消耗掉 50 个令牌中的 10 个。
	for i := 0; i < 10; i++ {
		lim.Allow("c", "api")
	}
	// 热更新为更严格的配置：容量 5。
	lim.UpdateConfig(map[string]LimitConfig{"api": {RatePerSec: 1, Burst: 5}})
	// 旧桶里有 40 个令牌，但新容量为 5：最多还能放行 5 个。
	allowed := 0
	for i := 0; i < 20; i++ {
		if lim.Allow("c", "api").Allowed {
			allowed++
		}
	}
	t.Logf("ctx: burst 50->5 hot reload, remaining tokens clamped, allowed=%d", allowed)
	if allowed > 5 {
		t.Fatalf("hot reload must clamp tokens to new capacity: allowed=%d", allowed)
	}
}
