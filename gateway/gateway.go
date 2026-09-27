package gateway

import "fmt"

// Gateway 把版本协商、限流、幂等保护串成一条处理链。
type Gateway struct {
	Registry *VersionRegistry
	Limiter  *RateLimiter
	Idem     *IdempotencyStore
	// Logger 接收每次请求的判定依据；为 nil 时丢弃。
	Logger func(format string, args ...any)
}

// NewGateway 组装一个完整网关。
func NewGateway(reg *VersionRegistry, lim *RateLimiter, idem *IdempotencyStore) *Gateway {
	return &Gateway{Registry: reg, Limiter: lim, Idem: idem}
}

func (g *Gateway) logf(format string, args ...any) {
	if g.Logger != nil {
		g.Logger(format, args...)
	}
}

// Handle 处理一个请求，依次经过：版本协商 -> 限流 -> 幂等 -> 版本实现。
// 所有失败都以 *Error 返回，类别可区分。
func (g *Gateway) Handle(req *Request) (*Response, error) {
	ctx := fmt.Sprintf("api=%s version=%s caller=%s idem=%q",
		req.API, req.Version, req.CallerID, req.IdempotencyKey)

	res, err := g.Registry.Resolve(req.API, req.Version)
	if err != nil {
		g.logf("version-negotiation reject %s reason=%v", ctx, err)
		return nil, err
	}
	g.logf("version-negotiation ok %s status=%s", ctx, res.Status)

	if d := g.Limiter.Allow(req.CallerID, req.API); !d.Allowed {
		g.logf("rate-limit reject %s reason=%s retry-after=%.3fs", ctx, d.Reason, d.RetryAfter)
		return nil, &Error{
			Kind:       KindRateLimited,
			Message:    d.Reason,
			RetryAfter: d.RetryAfter,
		}
	}
	g.logf("rate-limit ok %s", ctx)

	exec := func() (*Response, error) {
		resp, herr := res.Handler(req)
		if herr != nil {
			return nil, herr
		}
		if resp != nil && res.Deprecated {
			resp.Deprecated = true
			resp.HeaderHint = fmt.Sprintf("version %s of %s is deprecated; please upgrade",
				req.Version, req.API)
		}
		return resp, nil
	}

	if req.IdempotencyKey == "" {
		return exec()
	}
	resp, err := g.Idem.Execute(req.API, req.IdempotencyKey, req.Body, exec)
	if err != nil {
		g.logf("idempotency reject %s reason=%v", ctx, err)
		return nil, err
	}
	if resp != nil && resp.Replayed {
		g.logf("idempotency replay %s", ctx)
	}
	return resp, err
}
