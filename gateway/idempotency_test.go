package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newIdemGateway 构建一个带计数写接口的网关，writes 记录真实执行次数。
func newIdemGateway(writes *int32, block chan struct{}) *Gateway {
	r := NewVersionRouter()
	r.Register("/orders", VersionedHandler{Version: "v1", Status: StatusActive, Handler: func(req *Request) (*Response, error) {
		if block != nil {
			<-block // 模拟处理中，便于构造并发重复提交
		}
		n := atomic.AddInt32(writes, 1)
		return &Response{StatusCode: 201, Body: []byte{byte('0' + n)}}, nil
	}})
	return NewGateway(r, NewRateLimiter(RateLimitConfig{Rate: 1000, Burst: 1000}), NewIdempotencyStore())
}

// TestIdempotencyReplay 同一幂等键+相同内容重复提交：只写入一次，重放结果与首次一致。
func TestIdempotencyReplay(t *testing.T) {
	var writes int32
	gw := newIdemGateway(&writes, nil)
	ctx := context.Background()

	req := &Request{CallerID: "c", Endpoint: "/orders", Version: "v1", IdempotencyKey: "k1", Body: []byte("order-1")}
	first, err := gw.Handle(ctx, req)
	if err != nil {
		t.Fatalf("首次请求失败: %v", err)
	}
	for i := 0; i < 3; i++ {
		replay, err := gw.Handle(ctx, req)
		if err != nil {
			t.Fatalf("第 %d 次重放失败: %v", i, err)
		}
		if string(replay.Body) != string(first.Body) {
			t.Fatalf("重放结果 %q 与首次 %q 不一致", replay.Body, first.Body)
		}
	}
	if got := atomic.LoadInt32(&writes); got != 1 {
		t.Fatalf("底层写入执行了 %d 次，期望恰好 1 次", got)
	}
	t.Logf("ctx: key=k1 重放 3 次 -> 结果一致 body=%q，底层写入仅 1 次", first.Body)
}

// TestIdempotencyConflict 同一幂等键+不同内容：必须明确拒绝。
func TestIdempotencyConflict(t *testing.T) {
	var writes int32
	gw := newIdemGateway(&writes, nil)
	ctx := context.Background()

	_, err := gw.Handle(ctx, &Request{CallerID: "c", Endpoint: "/orders", Version: "v1", IdempotencyKey: "k1", Body: []byte("order-1")})
	if err != nil {
		t.Fatalf("首次请求失败: %v", err)
	}
	_, err = gw.Handle(ctx, &Request{CallerID: "c", Endpoint: "/orders", Version: "v1", IdempotencyKey: "k1", Body: []byte("order-2")})
	ge, ok := err.(*Error)
	if !ok || ge.Code != ErrCodeIdempotencyConflict {
		t.Fatalf("同键不同内容应返回 IDEMPOTENCY_CONFLICT，得到: %v", err)
	}
	if got := atomic.LoadInt32(&writes); got != 1 {
		t.Fatalf("冲突请求触发了额外写入，writes=%d", got)
	}
	t.Logf("ctx: key=k1 内容变更 -> 拒绝 code=%s（未发生重复写入）", ge.Code)
}

// TestIdempotencyConcurrentDuplicate 首次请求处理中时被并发重复提交：
// 重复请求等待首次完成，拿到一致结果，底层只写入一次。
func TestIdempotencyConcurrentDuplicate(t *testing.T) {
	var writes int32
	block := make(chan struct{})
	gw := newIdemGateway(&writes, block)
	ctx := context.Background()

	req := &Request{CallerID: "c", Endpoint: "/orders", Version: "v1", IdempotencyKey: "k1", Body: []byte("order-1")}

	firstDone := make(chan *Response, 1)
	go func() {
		resp, err := gw.Handle(ctx, req)
		if err != nil {
			t.Errorf("首次请求失败: %v", err)
		}
		firstDone <- resp
	}()

	// 等首次请求进入处理中，再并发发起 20 个重复提交。
	time.Sleep(50 * time.Millisecond)
	const dup = 20
	results := make([]*Response, dup)
	var wg sync.WaitGroup
	for i := 0; i < dup; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := gw.Handle(ctx, req)
			if err != nil {
				t.Errorf("并发重复提交失败: %v", err)
				return
			}
			results[i] = resp
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(block) // 放行首次请求
	wg.Wait()
	first := <-firstDone

	for i, r := range results {
		if r == nil || string(r.Body) != string(first.Body) {
			t.Fatalf("第 %d 个并发重复请求结果与首次不一致: %v vs %v", i, r, first)
		}
	}
	if got := atomic.LoadInt32(&writes); got != 1 {
		t.Fatalf("并发重复提交导致底层写入 %d 次，期望恰好 1 次", got)
	}
	t.Logf("ctx: key=k1 处理中并发重复提交 %d 次 -> 全部等待并拿到一致结果，底层写入 1 次", dup)
}

// TestIdempotencyFailureReplay 首次失败后，重复提交拿到一致的失败，不重复写入。
func TestIdempotencyFailureReplay(t *testing.T) {
	var calls int32
	r := NewVersionRouter()
	r.Register("/orders", VersionedHandler{Version: "v1", Status: StatusActive, Handler: func(req *Request) (*Response, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("db down")
	}})
	gw := NewGateway(r, NewRateLimiter(RateLimitConfig{Rate: 1000, Burst: 1000}), NewIdempotencyStore())
	ctx := context.Background()

	req := &Request{CallerID: "c", Endpoint: "/orders", Version: "v1", IdempotencyKey: "k1", Body: []byte("x")}
	_, err1 := gw.Handle(ctx, req)
	_, err2 := gw.Handle(ctx, req)

	ge1, ok1 := err1.(*Error)
	ge2, ok2 := err2.(*Error)
	if !ok1 || !ok2 || ge1.Code != ErrCodeIdempotencyFailed || ge2.Code != ErrCodeIdempotencyFailed {
		t.Fatalf("首次与重放都应返回 IDEMPOTENCY_FAILED，得到: %v / %v", err1, err2)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("失败后重放触发了重复执行，calls=%d", got)
	}
	t.Logf("ctx: key=k1 首次失败 -> 重放返回一致失败 code=%s，底层未重复执行", ge2.Code)
}

// TestIdempotencyDifferentKeysIndependent 不同幂等键互不影响。
func TestIdempotencyDifferentKeysIndependent(t *testing.T) {
	var writes int32
	gw := newIdemGateway(&writes, nil)
	ctx := context.Background()

	for _, key := range []string{"k1", "k2", "k3"} {
		if _, err := gw.Handle(ctx, &Request{CallerID: "c", Endpoint: "/orders", Version: "v1", IdempotencyKey: key, Body: []byte("same-body")}); err != nil {
			t.Fatalf("key=%s 失败: %v", key, err)
		}
	}
	if got := atomic.LoadInt32(&writes); got != 3 {
		t.Fatalf("不同键应各自生效，writes=%d，期望 3", got)
	}
	t.Log("ctx: 3 个不同幂等键 -> 各自写入 1 次，互不影响")
}
