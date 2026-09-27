package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lechandonga/versioned-api-gateway-go/gateway"
)

// newTestClient 构建一个全链路（client -> gateway）的测试环境。
func newTestClient() (*Client, *gateway.RateLimiter) {
	r := gateway.NewVersionRouter()
	active := gateway.Handler(func(req *gateway.Request) (*gateway.Response, error) {
		return &gateway.Response{StatusCode: 200, Body: []byte("ok-v2")}, nil
	})
	r.Register("/orders", gateway.VersionedHandler{Version: "v1", Status: gateway.StatusSunset, Handler: active})
	r.Register("/orders", gateway.VersionedHandler{Version: "v2", Status: gateway.StatusActive, Handler: active})

	limiter := gateway.NewRateLimiter(gateway.RateLimitConfig{Rate: 1000, Burst: 1000})
	gw := gateway.NewGateway(r, limiter, gateway.NewIdempotencyStore())
	return New(&LocalTransport{GW: gw}), limiter
}

// TestClientErrorClassification 客户端能把各类失败归到可区分的 ErrorKind。
func TestClientErrorClassification(t *testing.T) {
	c, limiter := newTestClient()
	ctx := context.Background()

	// 限流用例使用独立 caller（burst=1，第二发触发），不影响其他用例。
	limiter.SetConfig("throttled", "/orders", gateway.RateLimitConfig{Rate: 0.001, Burst: 1})

	cases := []struct {
		name string
		req  *gateway.Request
		want ErrorKind
	}{
		{"版本不存在", &gateway.Request{CallerID: "c", Endpoint: "/orders", Version: "v9"}, KindVersionNotFound},
		{"版本已下线", &gateway.Request{CallerID: "c", Endpoint: "/orders", Version: "v1"}, KindVersionSunset},
		{"参数不合法", &gateway.Request{CallerID: "", Endpoint: "/orders", Version: "v2"}, KindInvalidRequest},
		{"被限流", &gateway.Request{CallerID: "throttled", Endpoint: "/orders", Version: "v2"}, KindRateLimited}, // 第二发触发
	}
	// 先消耗掉 throttled 的 burst=1 那一发。
	if _, err := c.Call(ctx, &gateway.Request{CallerID: "throttled", Endpoint: "/orders", Version: "v2"}); err != nil {
		t.Fatalf("首发应成功: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Call(ctx, tc.req)
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("错误类型 = %T，期望 *client.Error（不能透出底层原始错误）", err)
			}
			if ce.Kind != tc.want {
				t.Fatalf("Kind = %s，期望 %s", ce.Kind, tc.want)
			}
			if tc.want == KindRateLimited && ce.RetryAfter <= 0 {
				t.Fatalf("限流错误应携带 RetryAfter 提示，得到 %v", ce.RetryAfter)
			}
			t.Logf("ctx: caller=%s version=%s -> kind=%s retryAfter=%.1fs", tc.req.CallerID, tc.req.Version, ce.Kind, ce.RetryAfter)
		})
	}
}

// TestClientIdempotencyKinds 幂等冲突与失败也能被客户端区分。
func TestClientIdempotencyKinds(t *testing.T) {
	c, _ := newTestClient()
	ctx := context.Background()

	// 先成功一次，再用同键不同内容触发冲突。
	okReq := &gateway.Request{CallerID: "c", Endpoint: "/orders", Version: "v2", IdempotencyKey: "k1", Body: []byte("a")}
	if _, err := c.Call(ctx, okReq); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	_, err := c.Call(ctx, &gateway.Request{CallerID: "c", Endpoint: "/orders", Version: "v2", IdempotencyKey: "k1", Body: []byte("b")})
	var ce *Error
	if !errors.As(err, &ce) || ce.Kind != KindIdempotencyConflict {
		t.Fatalf("同键不同内容应归类为 IdempotencyConflict，得到: %v", err)
	}
	t.Logf("ctx: key=k1 内容变更 -> kind=%s", ce.Kind)

	// 同键同内容重放：成功且结果一致。
	first, _ := c.Call(ctx, okReq)
	replay, err := c.Call(ctx, okReq)
	if err != nil || string(replay.Body) != string(first.Body) {
		t.Fatalf("重放应成功且一致: replay=%v err=%v", replay, err)
	}
	t.Logf("ctx: key=k1 同内容重放 -> 成功 body=%q", replay.Body)
}

// TestClientDoesNotLeakRawErrors 非标准底层错误被归为 Unknown，不原样透出。
func TestClientDoesNotLeakRawErrors(t *testing.T) {
	raw := errors.New("connection reset by peer 10.0.0.1:5432")
	c := New(transportFunc(func(ctx context.Context, req *gateway.Request) (*gateway.Response, error) {
		return nil, raw
	}))
	_, err := c.Call(context.Background(), &gateway.Request{CallerID: "c", Endpoint: "/e", Version: "v1"})
	var ce *Error
	if !errors.As(err, &ce) || ce.Kind != KindUnknown {
		t.Fatalf("应归类为 Unknown，得到: %v", err)
	}
	if err == raw || ce.Message == raw.Error() {
		t.Fatalf("底层原始错误被透出: %v", err)
	}
	t.Logf("ctx: 底层错误 %q -> 归类 kind=%s msg=%q（未透出原始细节）", raw, ce.Kind, ce.Message)
}

// TestClientRateLimitRetry 客户端按 RetryAfter 提示重试后成功。
func TestClientRateLimitRetry(t *testing.T) {
	c, limiter := newTestClient()
	limiter.SetConfig("c", "/orders", gateway.RateLimitConfig{Rate: 50, Burst: 1})
	ctx := context.Background()

	req := &gateway.Request{CallerID: "c", Endpoint: "/orders", Version: "v2"}
	if _, err := c.Call(ctx, req); err != nil {
		t.Fatalf("首发应成功: %v", err)
	}
	_, err := c.Call(ctx, req)
	var ce *Error
	if !errors.As(err, &ce) || ce.Kind != KindRateLimited {
		t.Fatalf("第二发应被限流，得到: %v", err)
	}
	time.Sleep(time.Duration(ce.RetryAfter*1000)*time.Millisecond + 10*time.Millisecond)
	if _, err := c.Call(ctx, req); err != nil {
		t.Fatalf("按 RetryAfter=%.3fs 等待后重试应成功: %v", ce.RetryAfter, err)
	}
	t.Logf("ctx: 限流后按 retryAfter=%.3fs 重试 -> 成功", ce.RetryAfter)
}

type transportFunc func(ctx context.Context, req *gateway.Request) (*gateway.Response, error)

func (f transportFunc) Do(ctx context.Context, req *gateway.Request) (*gateway.Response, error) {
	return f(ctx, req)
}
