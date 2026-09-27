package gateway

import (
	"context"
	"testing"
)

// newTestGateway 构建一个带两个版本（v1 废弃、v2 在线）的测试网关。
func newTestGateway(t *testing.T) *Gateway {
	t.Helper()
	r := NewVersionRouter()
	r.Register("/orders", VersionedHandler{Version: "v1", Status: StatusDeprecated, Handler: echoHandler("v1")})
	r.Register("/orders", VersionedHandler{Version: "v2", Status: StatusActive, Handler: echoHandler("v2")})
	r.Register("/legacy", VersionedHandler{Version: "v1", Status: StatusSunset, Handler: echoHandler("v1")})

	l := NewRateLimiter(RateLimitConfig{Rate: 1000, Burst: 1000})
	gw := NewGateway(r, l, NewIdempotencyStore())
	return gw
}

func echoHandler(v string) Handler {
	return func(req *Request) (*Response, error) {
		return &Response{StatusCode: 200, Body: []byte("served-by-" + v)}, nil
	}
}

// TestVersionMatrix 覆盖版本兼容矩阵：在线 / 废弃 / 已下线 / 版本不存在 / 接口不存在。
func TestVersionMatrix(t *testing.T) {
	gw := newTestGateway(t)
	ctx := context.Background()

	cases := []struct {
		name           string
		endpoint       string
		version        string
		wantErrCode    ErrorCode // 空表示期望成功
		wantBody       string
		wantDeprecated bool
	}{
		{"在线版本正常服务", "/orders", "v2", "", "served-by-v2", false},
		{"废弃版本仍服务但带标记", "/orders", "v1", "", "served-by-v1", true},
		{"已下线版本拒绝服务", "/legacy", "v1", ErrCodeVersionSunset, "", false},
		{"版本完全不存在", "/orders", "v9", ErrCodeVersionNotFound, "", false},
		{"接口不存在", "/nope", "v1", ErrCodeVersionNotFound, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := gw.Handle(ctx, &Request{CallerID: "c1", Endpoint: tc.endpoint, Version: tc.version})
			if tc.wantErrCode == "" {
				if err != nil {
					t.Fatalf("期望成功，得到错误: %v", err)
				}
				if string(resp.Body) != tc.wantBody {
					t.Fatalf("响应体 = %q，期望 %q", resp.Body, tc.wantBody)
				}
				if resp.Deprecated != tc.wantDeprecated {
					t.Fatalf("Deprecated = %v，期望 %v", resp.Deprecated, tc.wantDeprecated)
				}
				if resp.Version != tc.version {
					t.Fatalf("命中版本 = %q，期望 %q", resp.Version, tc.version)
				}
				t.Logf("ctx: endpoint=%s version=%s -> 命中版本=%s deprecated=%v", tc.endpoint, tc.version, resp.Version, resp.Deprecated)
				return
			}
			ge, ok := err.(*Error)
			if !ok {
				t.Fatalf("错误类型 = %T，期望 *Error", err)
			}
			if ge.Code != tc.wantErrCode {
				t.Fatalf("错误码 = %s，期望 %s（错误原因必须可区分）", ge.Code, tc.wantErrCode)
			}
			t.Logf("ctx: endpoint=%s version=%s -> 拒绝 code=%s msg=%s", tc.endpoint, tc.version, ge.Code, ge.Message)
		})
	}
}

// TestVersionSunsetAndNotFoundAreDistinct 明确区分"已下线"与"不存在"。
func TestVersionSunsetAndNotFoundAreDistinct(t *testing.T) {
	gw := newTestGateway(t)
	ctx := context.Background()

	_, errSunset := gw.Handle(ctx, &Request{CallerID: "c1", Endpoint: "/legacy", Version: "v1"})
	_, errNotFound := gw.Handle(ctx, &Request{CallerID: "c1", Endpoint: "/orders", Version: "v9"})

	if errSunset.(*Error).Code == errNotFound.(*Error).Code {
		t.Fatalf("已下线与不存在不能混为同一错误: 都是 %s", errSunset.(*Error).Code)
	}
}

// TestVersionUpgradeNoSilentMismatch 版本升级后旧调用方不会被静默路由到新版本。
func TestVersionUpgradeNoSilentMismatch(t *testing.T) {
	r := NewVersionRouter()
	r.Register("/pay", VersionedHandler{Version: "v1", Status: StatusActive, Handler: echoHandler("v1")})
	gw := NewGateway(r, NewRateLimiter(RateLimitConfig{Rate: 1000, Burst: 1000}), NewIdempotencyStore())
	ctx := context.Background()

	// 升级：上线 v2，并把 v1 标记为下线。
	r.Register("/pay", VersionedHandler{Version: "v2", Status: StatusActive, Handler: echoHandler("v2")})
	if err := r.SetStatus("/pay", "v1", StatusSunset); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	resp, err := gw.Handle(ctx, &Request{CallerID: "c1", Endpoint: "/pay", Version: "v2"})
	if err != nil || string(resp.Body) != "served-by-v2" {
		t.Fatalf("v2 应正常服务: resp=%v err=%v", resp, err)
	}

	// 旧调用方仍声明 v1：必须收到明确的已下线错误，而不是静默拿到 v2 的结果。
	_, err = gw.Handle(ctx, &Request{CallerID: "c1", Endpoint: "/pay", Version: "v1"})
	ge, ok := err.(*Error)
	if !ok || ge.Code != ErrCodeVersionSunset {
		t.Fatalf("旧版本应返回 VERSION_SUNSET，得到: %v", err)
	}
	t.Logf("ctx: 旧调用方 version=v1 -> 明确拒绝 code=%s（未静默路由到 v2）", ge.Code)
}
