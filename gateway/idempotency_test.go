package gateway

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 同一标识重复提交：只执行一次，后续拿到与首次一致的结果且标记为重放。
func TestIdempotency_ReplayReturnsSameResult(t *testing.T) {
	store := NewIdempotencyStore(time.Hour)
	var calls int32
	fn := func() (*Response, error) {
		atomic.AddInt32(&calls, 1)
		return &Response{Status: 200, Body: []byte("created-order-1")}, nil
	}
	body := []byte(`{"item":"book","qty":1}`)

	first, err := store.Execute("orders.create", "key-1", body, fn)
	if err != nil {
		t.Fatalf("first execute failed: %v", err)
	}
	second, err := store.Execute("orders.create", "key-1", body, fn)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	t.Logf("ctx: key=key-1 same-payload replay -> calls=%d first.Replayed=%v second.Replayed=%v",
		calls, first.Replayed, second.Replayed)
	if calls != 1 {
		t.Fatalf("handler executed %d times, want 1", calls)
	}
	if first.Replayed {
		t.Fatal("first response must not be marked replayed")
	}
	if !second.Replayed {
		t.Fatal("second response must be marked replayed")
	}
	if string(second.Body) != string(first.Body) {
		t.Fatalf("replay body %q != first %q", second.Body, first.Body)
	}
}

// 同一标识配不同内容：必须明确拒绝为冲突。
func TestIdempotency_ConflictingPayloadRejected(t *testing.T) {
	store := NewIdempotencyStore(time.Hour)
	fn := func() (*Response, error) {
		return &Response{Status: 200, Body: []byte("ok")}, nil
	}
	if _, err := store.Execute("orders.create", "key-2", []byte(`{"qty":1}`), fn); err != nil {
		t.Fatalf("first execute failed: %v", err)
	}
	_, err := store.Execute("orders.create", "key-2", []byte(`{"qty":2}`), fn)
	t.Logf("ctx: key=key-2 different-payload -> err=%v", err)
	if KindOf(err) != KindIdempotencyConflict {
		t.Fatalf("want IDEMPOTENCY_CONFLICT, got %v", err)
	}
}

// 并发重复提交在途请求：只执行一次，所有等待者拿到一致结果。
func TestIdempotency_ConcurrentInProgressSingleExecution(t *testing.T) {
	store := NewIdempotencyStore(time.Hour)
	var calls int32
	release := make(chan struct{})
	fn := func() (*Response, error) {
		atomic.AddInt32(&calls, 1)
		<-release // 模拟处理耗时，让重复提交发生在在途期间
		return &Response{Status: 200, Body: []byte("slow-result")}, nil
	}
	body := []byte(`{"item":"pen"}`)

	const n = 32
	var wg sync.WaitGroup
	results := make([]*Response, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = store.Execute("orders.create", "key-3", body, fn)
		}(i)
	}
	// 确保所有 goroutine 都已进入等待后再放行。
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	replays := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d got error: %v", i, errs[i])
		}
		if string(results[i].Body) != "slow-result" {
			t.Fatalf("goroutine %d got inconsistent body %q", i, results[i].Body)
		}
		if results[i].Replayed {
			replays++
		}
	}
	t.Logf("ctx: key=key-3 concurrent=%d -> handler calls=%d, replayed responses=%d", n, calls, replays)
	if calls != 1 {
		t.Fatalf("handler executed %d times under concurrency, want exactly 1", calls)
	}
}

// 在途期间同标识不同内容：也必须拒绝为冲突，不能等待后错乱。
func TestIdempotency_ConflictWhileInProgress(t *testing.T) {
	store := NewIdempotencyStore(time.Hour)
	release := make(chan struct{})
	fn := func() (*Response, error) {
		<-release
		return &Response{Status: 200, Body: []byte("ok")}, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.Execute("orders.create", "key-4", []byte(`{"v":1}`), fn)
	}()
	time.Sleep(20 * time.Millisecond) // 确保第一个请求已在途

	_, err := store.Execute("orders.create", "key-4", []byte(`{"v":2}`), fn)
	t.Logf("ctx: key=key-4 conflict-during-inflight -> err=%v", err)
	if KindOf(err) != KindIdempotencyConflict {
		t.Fatalf("want IDEMPOTENCY_CONFLICT, got %v", err)
	}
	close(release)
	<-done
}

// 不同接口的标识空间互相隔离。
func TestIdempotency_ScopeIsolation(t *testing.T) {
	store := NewIdempotencyStore(time.Hour)
	var calls int32
	fn := func() (*Response, error) {
		atomic.AddInt32(&calls, 1)
		return &Response{Status: 200, Body: []byte("ok")}, nil
	}
	body := []byte(`{"x":1}`)
	store.Execute("orders.create", "key-5", body, fn)
	store.Execute("orders.cancel", "key-5", body, fn)
	t.Logf("ctx: same key across two apis -> calls=%d", calls)
	if calls != 2 {
		t.Fatalf("same key in different scopes must execute independently, calls=%d", calls)
	}
}
