package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
)

// Request 是进入网关的统一请求。
type Request struct {
	CallerID       string
	Endpoint       string
	Version        string
	IdempotencyKey string // 写接口必填；空表示只读请求，不做幂等保护
	Body           []byte
}

// Response 是网关统一响应。
type Response struct {
	StatusCode int
	Body       []byte
	Version    string // 实际命中版本
	Deprecated bool   // 命中的是废弃版本
}

// Gateway 组合版本路由、限流与幂等保护。
type Gateway struct {
	Router  *VersionRouter
	Limiter *RateLimiter
	Idem    *IdempotencyStore
	logger  *log.Logger
}

func NewGateway(r *VersionRouter, l *RateLimiter, idem *IdempotencyStore) *Gateway {
	return &Gateway{Router: r, Limiter: l, Idem: idem}
}

// SetLogger 注入决策日志输出（测试里可接到 t.Log）。
func (g *Gateway) SetLogger(l *log.Logger) { g.logger = l }

func (g *Gateway) logf(format string, args ...any) {
	if g.logger != nil {
		g.logger.Printf(format, args...)
	}
}

// Handle 处理一个请求：参数校验 -> 限流 -> 版本路由 -> （写接口）幂等执行。
// 每一步的判定依据都会写入日志，错误都是可区分的 *Error。
func (g *Gateway) Handle(ctx context.Context, req *Request) (*Response, error) {
	if req.CallerID == "" || req.Endpoint == "" || req.Version == "" {
		return nil, &Error{Code: ErrCodeInvalidRequest, Message: "CallerID、Endpoint、Version 均不能为空"}
	}

	// 1. 限流
	allowed, retryAfter := g.Limiter.Allow(req.CallerID, req.Endpoint)
	if !allowed {
		g.logf("[ratelimit] caller=%s endpoint=%s 判定=拒绝 retryAfter=%.3fs", req.CallerID, req.Endpoint, retryAfter)
		return nil, &Error{
			Code:       ErrCodeRateLimited,
			Message:    "请求被限流，请稍后重试",
			RetryAfter: retryAfter,
		}
	}
	g.logf("[ratelimit] caller=%s endpoint=%s 判定=放行", req.CallerID, req.Endpoint)

	// 2. 版本路由
	vh, err := g.Router.Resolve(req.Endpoint, req.Version)
	if err != nil {
		g.logf("[version] endpoint=%s version=%s 判定=拒绝 原因=%v", req.Endpoint, req.Version, err)
		return nil, err
	}
	g.logf("[version] endpoint=%s version=%s 判定=命中 status=%s", req.Endpoint, req.Version, vh.Status)

	// 3. 执行（写接口包一层幂等保护）
	exec := func() (*Response, error) {
		resp, err := vh.Handler(req)
		if err != nil {
			return nil, err
		}
		resp.Version = vh.Version
		resp.Deprecated = vh.Status == StatusDeprecated
		return resp, nil
	}

	if req.IdempotencyKey == "" {
		return exec()
	}
	hash := sha256.Sum256(req.Body)
	payloadHash := hex.EncodeToString(hash[:])
	g.logf("[idempotency] endpoint=%s key=%s payloadHash=%s", req.Endpoint, req.IdempotencyKey, payloadHash[:12])
	return g.Idem.Execute(ctx, req.Endpoint, req.IdempotencyKey, payloadHash, exec)
}
