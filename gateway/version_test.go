package gateway

import "testing"

func newMatrixRegistry() *VersionRegistry {
	reg := NewVersionRegistry()
	reg.Register("orders.create", "v1", StatusDeprecated, func(req *Request) (*Response, error) {
		return &Response{Status: 200, Body: []byte("v1-result")}, nil
	})
	reg.Register("orders.create", "v2", StatusActive, func(req *Request) (*Response, error) {
		return &Response{Status: 200, Body: []byte("v2-result")}, nil
	})
	reg.Register("orders.create", "v0", StatusSunset, func(req *Request) (*Response, error) {
		return &Response{Status: 200, Body: []byte("v0-result")}, nil
	})
	return reg
}

// 版本兼容矩阵：在线 / 废弃 / 下线 / 不存在 四种情形必须可区分。
func TestVersionNegotiationMatrix(t *testing.T) {
	reg := newMatrixRegistry()
	cases := []struct {
		name        string
		version     string
		wantErr     bool
		wantKind    ErrorKind
		wantBody    string
		wantDeprHit bool
	}{
		{"active version routed to v2 impl", "v2", false, 0, "v2-result", false},
		{"deprecated version still served with hint", "v1", false, 0, "v1-result", true},
		{"sunset version rejected distinctly", "v0", true, KindSunsetVersion, "", false},
		{"never-existed version rejected distinctly", "v9", true, KindUnknownVersion, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := reg.Resolve("orders.create", tc.version)
			t.Logf("ctx: api=orders.create version=%s -> err=%v", tc.version, err)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error kind %s, got nil", tc.wantKind)
				}
				if got := KindOf(err); got != tc.wantKind {
					t.Fatalf("want kind %s, got %s", tc.wantKind, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Deprecated != tc.wantDeprHit {
				t.Fatalf("deprecated flag = %v, want %v", res.Deprecated, tc.wantDeprHit)
			}
			resp, herr := res.Handler(&Request{API: "orders.create", Version: tc.version})
			if herr != nil {
				t.Fatalf("handler error: %v", herr)
			}
			if string(resp.Body) != tc.wantBody {
				t.Fatalf("body = %q, want %q", resp.Body, tc.wantBody)
			}
		})
	}
}

// 下线与不存在绝不能混为同一个错误类别。
func TestSunsetDistinctFromUnknown(t *testing.T) {
	reg := newMatrixRegistry()
	_, errSunset := reg.Resolve("orders.create", "v0")
	_, errUnknown := reg.Resolve("orders.create", "v9")
	if KindOf(errSunset) == KindOf(errUnknown) {
		t.Fatalf("sunset and unknown must differ, both = %s", KindOf(errSunset))
	}
	t.Logf("ctx: sunset=%s unknown=%s (distinct)", KindOf(errSunset), KindOf(errUnknown))
}

// 版本升级后旧调用方仍按自己声明的版本路由，不会静默拿到新版本结果。
func TestNoSilentCrossVersionRouting(t *testing.T) {
	reg := newMatrixRegistry()
	res, err := reg.Resolve("orders.create", "v1")
	if err != nil {
		t.Fatalf("v1 should still resolve: %v", err)
	}
	resp, _ := res.Handler(&Request{API: "orders.create", Version: "v1"})
	if string(resp.Body) != "v1-result" {
		t.Fatalf("v1 caller silently got wrong version result: %q", resp.Body)
	}
	t.Logf("ctx: caller pinned to v1 received v1-result despite v2 being active")
}

// 未注册的接口名也要归为“版本不存在”类，而不是内部错误。
func TestUnknownAPI(t *testing.T) {
	reg := newMatrixRegistry()
	_, err := reg.Resolve("payments.refund", "v1")
	if KindOf(err) != KindUnknownVersion {
		t.Fatalf("unregistered api should be KindUnknownVersion, got %s", KindOf(err))
	}
	t.Logf("ctx: api=payments.refund -> %s", KindOf(err))
}
