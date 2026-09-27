package gateway

import (
	"context"
	"fmt"
	"sync"
)

// record 是单个幂等键的执行记录。
type record struct {
	payloadHash string
	done        chan struct{} // 首次执行完成后关闭
	resp        *Response
	err         error
}

// IdempotencyStore 为写接口提供幂等保护：
//   - 同一幂等键 + 相同请求内容：只执行一次，重复请求拿到与首次一致的结果（重放）；
//   - 同一幂等键 + 不同请求内容：明确拒绝（ErrCodeIdempotencyConflict）；
//   - 首次请求仍在处理中：重复请求阻塞等待首次完成，再返回一致结果，不会并发重复写入。
type IdempotencyStore struct {
	mu      sync.Mutex
	records map[string]*record // "endpoint|key" -> record
}

func NewIdempotencyStore() *IdempotencyStore {
	return &IdempotencyStore{records: make(map[string]*record)}
}

func idemKey(endpoint, key string) string { return endpoint + "|" + key }

// Execute 在幂等保护下执行 fn。fn 对同一 (endpoint, key) 至多被执行一次。
func (s *IdempotencyStore) Execute(ctx context.Context, endpoint, key, payloadHash string, fn func() (*Response, error)) (*Response, error) {
	k := idemKey(endpoint, key)

	s.mu.Lock()
	if rec, ok := s.records[k]; ok {
		s.mu.Unlock()
		if rec.payloadHash != payloadHash {
			return nil, &Error{
				Code:    ErrCodeIdempotencyConflict,
				Message: fmt.Sprintf("幂等键 %q 已绑定不同的请求内容，拒绝重放", key),
			}
		}
		// 内容一致：等待首次执行完成（可能已完成，select 会立即返回）。
		select {
		case <-rec.done:
		case <-ctx.Done():
			return nil, &Error{
				Code:    ErrCodeIdempotencyFailed,
				Message: fmt.Sprintf("等待幂等键 %q 的首次请求完成时被取消: %v", key, ctx.Err()),
			}
		}
		return rec.resp, rec.err
	}
	// 首次见到该键：占位后再执行，保证并发下同键只有一个执行者。
	rec := &record{payloadHash: payloadHash, done: make(chan struct{})}
	s.records[k] = rec
	s.mu.Unlock()

	resp, err := fn()
	if err != nil {
		// 首次失败也记录，重复提交拿到与首次一致的失败结果，不会重复写入。
		rec.err = &Error{
			Code:    ErrCodeIdempotencyFailed,
			Message: fmt.Sprintf("幂等键 %q 的首次请求处理失败: %v", key, err),
		}
	} else {
		rec.resp = resp
	}
	close(rec.done)
	// 首次调用与后续重放返回完全一致的结果。
	return rec.resp, rec.err
}
