package gateway

import (
	"sync"
	"sync/atomic"
	"time"
)

// LimitConfig 是单个接口的限流配置：速率 + 突发容量。
type LimitConfig struct {
	RatePerSec float64 // 每秒补充的令牌数
	Burst      int     // 桶容量，即可容忍的最大突发
}

// bucket 是单个 (调用方, 接口) 维度的令牌桶。
type bucket struct {
	tokens float64
	last   time.Time
}

// RateLimiter 按 (调用方, 接口) 维度做令牌桶限流。
// 配置通过原子指针热更新，无需重启；已有桶在新配置下继续工作，
// 令牌数会被钳制到新容量以内，保证配置切换瞬间也不会突破新配额。
type RateLimiter struct {
	cfg     atomic.Value // 存 map[string]LimitConfig，key 为接口名
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time // 可注入时钟便于测试
}

// NewRateLimiter 创建限流器并应用初始配置。
func NewRateLimiter(cfg map[string]LimitConfig) *RateLimiter {
	l := &RateLimiter{
		buckets: make(map[string]*bucket),
		now:     time.Now,
	}
	l.UpdateConfig(cfg)
	return l
}

// UpdateConfig 热更新限流配置，立即生效。
func (l *RateLimiter) UpdateConfig(cfg map[string]LimitConfig) {
	cp := make(map[string]LimitConfig, len(cfg))
	for k, v := range cfg {
		cp[k] = v
	}
	l.cfg.Store(cp)
}

// Decision 是一次限流判定的结果，包含可解释的原因与重试提示。
type Decision struct {
	Allowed    bool
	Reason     string  // 判定依据，用于日志与错误信息
	RetryAfter float64 // 被限流时建议的等待秒数
}

// Allow 判定 (caller, api) 这次请求是否放行。
// 突发由 Burst 吸收；窗口（ refill ）切换通过按经过时间精确补令牌实现，
// 不存在窗口边界重置导致的配额放大。
func (l *RateLimiter) Allow(caller, api string) Decision {
	cfg := l.cfg.Load().(map[string]LimitConfig)
	c, ok := cfg[api]
	if !ok {
		return Decision{Allowed: true, Reason: "no limit configured for api " + api}
	}
	key := caller + "|" + api
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(c.Burst), last: now}
		l.buckets[key] = b
	}
	// 按经过时间补令牌，钳制到当前容量。
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * c.RatePerSec
	if b.tokens > float64(c.Burst) {
		b.tokens = float64(c.Burst)
	}
	if b.tokens < 1 {
		// 计算攒够一个令牌还需多久，作为重试提示。
		wait := (1 - b.tokens) / c.RatePerSec
		return Decision{
			Allowed:    false,
			Reason:     "token bucket empty: caller=" + caller + " api=" + api,
			RetryAfter: wait,
		}
	}
	b.tokens--
	return Decision{Allowed: true, Reason: "token consumed"}
}
