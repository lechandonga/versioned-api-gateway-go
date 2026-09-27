package gateway

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 组装一个带日志的完整网关，日志直接进测试输出。
func newTestGateway(t *testing.T, writeCalls *int32) *Gateway {
	t.Helper()
	reg := NewVersionRegistry()
	reg.Register("orders.create", "v1", StatusDeprecated, func(req *Request) (*Response, error) {
		return &Response{Status: 200, Body: []byte("v1:" + string(req.Body))}, nil
	})
	reg.Register("orders.create", "v2", StatusActive, func(req *Request) (*Response, error) {
		if writeCalls != nil {
			atomic.AddInt32(writeCalls, 1)
		}
		return &Response{Status: 200, Body: []byte("v2:" + string(req.Body))}, nil
	})
	reg.Register("orders.create", "v0", StatusSunset, func(req *Request) (*Response, error) {
		return &Response{Status: 200, Body: []byte("v0")}, nil
	})
	lim := NewRateLimiter(map[string]LimitConfig{
		"orders.create": {RatePerSec: 1000, Burst: 1000},
	})
	gw := NewGateway(reg, lim, NewIdempotencyStore(time.Hour))
	gw.Logger = func(format string, args ...any) {
		t.Helper()
		t.Logf("gateway: "+format, args...)
	}
	return gw
}

// 端到端：客户端看到的失败必须全部是可分类的 *ClientError。
func TestClientErrorClassification(t *testing.T) {
	gw := newTestGateway(t, nil)
	client := NewClient(gw, "caller-X")

	cases := []struct {
		name     string
		version  string
		wantKind ErrorKind
	}{
		{"sunset version", "v0", KindSunsetVersion},
		{"unknown version", "v9", KindUnknownVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.Call("orders.create", tc.version, "", []byte(`{}`))
			t.Logf("ctx: caller=caller-X version=%s -> classified err=%v", tc.version, err)
			if !IsKind(err, tc.wantKind) {
				t.Fatalf("want kind %s, got %v", tc.wantKind, err)
			}
			var ce *ClientError
			if !errors.As(err, &ce) {
				t.Fatalf("error must be *ClientError, got %T", err)
			}
		})
	}
}

// 底层业务实现的原始错误必须被归类为 INTERNAL，不能原样透出。
func TestClientWrapsRawHandlerError(t *testing.T) {
	rawErr := fmt.Errorf("sql: connection reset by peer")
	reg := NewVersionRegistry()
	reg.Register("db.write", "v1", StatusActive, func(req *Request) (*Response, error) {
		return nil, rawErr
	})
	gw := NewGateway(reg, NewRateLimiter(nil), NewIdempotencyStore(time.Hour))
	client := NewClient(gw, "caller-Y")

	_, err := client.Call("db.write", "v1", "", []byte(`{}`))
	t.Logf("ctx: raw handler error %q -> client sees: %v", rawErr, err)
	if !IsKind(err, KindInternal) {
		t.Fatalf("raw error must be classified INTERNAL, got %v", err)
	}
	if err == rawErr {
		t.Fatal("raw underlying error leaked to client")
	}
	if !errors.Is(err, rawErr) {
		t.Fatal("cause should still be inspectable via errors.Is")
	}
}

// 端到端：限流拒绝携带可区分类别与重试提示。
func TestGatewayRateLimitEndToEnd(t *testing.T) {
	gw := newTestGateway(t, nil)
	gw.Limiter.UpdateConfig(map[string]LimitConfig{
		"orders.create": {RatePerSec: 1, Burst: 1},
	})
	client := NewClient(gw, "caller-Z")

	if _, err := client.Call("orders.create", "v2", "", []byte(`{}`)); err != nil {
		t.Fatalf("first call should pass: %v", err)
	}
	_, err := client.Call("orders.create", "v2", "", []byte(`{}`))
	t.Logf("ctx: caller=caller-Z burst=1 second call -> %v", err)
	if !IsKind(err, KindRateLimited) {
		t.Fatalf("want RATE_LIMITED, got %v", err)
	}
	var ce *ClientError
	errors.As(err, &ce)
	if ce.RetryAfter <= 0 {
		t.Fatalf("rate-limited error must carry retry-after, got %v", ce.RetryAfter)
	}
}

// 端到端：并发下同一幂等键的重复写只生效一次，重放结果一致。
func TestGatewayIdempotentWriteUnderConcurrency(t *testing.T) {
	var writeCalls int32
	gw := newTestGateway(t, &writeCalls)
	client := NewClient(gw, "caller-W")
	body := []byte(`{"item":"book"}`)

	const n = 16
	var wg sync.WaitGroup
	bodies := make([]string, n)
	replayed := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := client.Call("orders.create", "v2", "order-key-1", body)
			if err != nil {
				errs[i] = err
				return
			}
			bodies[i] = string(resp.Body)
			replayed[i] = resp.Replayed
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("call %d failed: %v", i, errs[i])
		}
		if bodies[i] != "v2:"+string(body) {
			t.Fatalf("call %d got inconsistent body %q", i, bodies[i])
		}
	}
	t.Logf("ctx: concurrent=%d same idem-key -> underlying writes=%d", n, writeCalls)
	if writeCalls != 1 {
		t.Fatalf("write executed %d times, want exactly 1", writeCalls)
	}
}

// 端到端：废弃版本仍被服务但带升级提示。
func TestGatewayDeprecatedVersionHint(t *testing.T) {
	gw := newTestGateway(t, nil)
	client := NewClient(gw, "caller-D")
	resp, err := client.Call("orders.create", "v1", "", []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("deprecated version should still be served: %v", err)
	}
	t.Logf("ctx: version=v1 -> deprecated=%v hint=%q", resp.Deprecated, resp.HeaderHint)
	if !resp.Deprecated || resp.HeaderHint == "" {
		t.Fatalf("deprecated response must carry upgrade hint, got %+v", resp)
	}
	if string(resp.Body) != `v1:{"a":1}` {
		t.Fatalf("v1 caller got wrong version body: %q", resp.Body)
	}
}

// 端到端：同键不同内容在网关层被拒绝为幂等冲突。
func TestGatewayIdempotencyConflictEndToEnd(t *testing.T) {
	gw := newTestGateway(t, nil)
	client := NewClient(gw, "caller-C")
	if _, err := client.Call("orders.create", "v2", "k-1", []byte(`{"qty":1}`)); err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	_, err := client.Call("orders.create", "v2", "k-1", []byte(`{"qty":2}`))
	t.Logf("ctx: same key different payload -> %v", err)
	if !IsKind(err, KindIdempotencyConflict) {
		t.Fatalf("want IDEMPOTENCY_CONFLICT, got %v", err)
	}
}
