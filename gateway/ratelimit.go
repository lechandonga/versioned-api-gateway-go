package gateway

import (
	"sync"
	"time"
)

// RateLimitConfig 是单维度（调用方+接口）的限流配置。
type RateLimitConfig struct {
	Rate  float64 // 每秒补充的令牌数
	Burst int     // 桶容量（允许的突发量）
}

// bucket 是单个限流维度的令牌桶状态。
type bucket struct {
	tokens float64
	last   time.Time
}

// RateLimiter 按 caller+endpoint 维度做令牌桶限流。
//
// 并发安全：所有桶操作都在同一把互斥锁内完成，突发与并发下配额不会被突破；
// 窗口切换：令牌按经过时间精确补充，不依赖固定窗口，不存在窗口边界双倍放行问题；
// 热更新：SetConfig 立即生效，已有桶的存量令牌会被截断到新 Burst 以内。
type RateLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*bucket
	configs    map[string]RateLimitConfig // "caller|endpoint" 或 "caller"
	defaultCfg RateLimitConfig
	now        func() time.Time // 可注入时钟，便于测试
}

func NewRateLimiter(defaultCfg RateLimitConfig) *RateLimiter {
	return &RateLimiter{
		buckets:    make(map[string]*bucket),
		configs:    make(map[string]RateLimitConfig),
		defaultCfg: defaultCfg,
		now:        time.Now,
	}
}

// SetClock 注入时钟（仅测试用）。
func (l *RateLimiter) SetClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

func limitKey(caller, endpoint string) string { return caller + "|" + endpoint }

// configFor 查找生效配置：caller+endpoint 精确配置 > caller 级配置 > 默认配置。
func (l *RateLimiter) configFor(caller, endpoint string) RateLimitConfig {
	if cfg, ok := l.configs[limitKey(caller, endpoint)]; ok {
		return cfg
	}
	if cfg, ok := l.configs[caller]; ok {
		return cfg
	}
	return l.defaultCfg
}

// Allow 判断请求是否放行；被限流时返回建议的重试等待秒数（补充一个令牌所需时间）。
func (l *RateLimiter) Allow(caller, endpoint string) (allowed bool, retryAfter float64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cfg := l.configFor(caller, endpoint)
	key := limitKey(caller, endpoint)
	now := l.now()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(cfg.Burst), last: now}
		l.buckets[key] = b
	}
	// 按经过时间补充令牌，封顶到 Burst。
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * cfg.Rate
	if max := float64(cfg.Burst); b.tokens > max {
		b.tokens = max
	}
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// 计算等到 1 个令牌需要的秒数，作为重试提示。
	wait := (1 - b.tokens) / cfg.Rate
	return false, wait
}

// SetConfig 热更新限流配置：endpoint 非空时设置 caller+endpoint 维度，
// endpoint 为空时设置 caller 级默认。已有桶的存量令牌立即按新 Burst 截断。
func (l *RateLimiter) SetConfig(caller, endpoint string, cfg RateLimitConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := caller
	if endpoint != "" {
		key = limitKey(caller, endpoint)
	}
	l.configs[key] = cfg
	// 所有已存在的桶按各自生效配置重新截断存量令牌，
	// 避免旧配额在新配置下被超额使用。
	for k, b := range l.buckets {
		c, e := splitLimitKey(k)
		if max := float64(l.configFor(c, e).Burst); b.tokens > max {
			b.tokens = max
		}
	}
}

// splitLimitKey 把桶键还原为 caller 与 endpoint。
func splitLimitKey(key string) (caller, endpoint string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '|' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}
