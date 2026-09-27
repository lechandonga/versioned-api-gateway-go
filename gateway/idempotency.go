package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// idemEntry 记录一个幂等标识的处理状态。
type idemEntry struct {
	fingerprint string // 请求内容指纹，用于检测“同标识不同内容”
	done        chan struct{}
	resp        *Response
	err         error
	completed   bool
	completedAt time.Time
}

// IdempotencyStore 保证同一幂等标识的请求只生效一次。
// 并发重复提交：内容一致的在途请求会被等待并拿到与首次一致的结果；
// 内容不一致的一律拒绝为 KindIdempotencyConflict。
type IdempotencyStore struct {
	mu      sync.Mutex
	entries map[string]*idemEntry
	ttl     time.Duration
	now     func() time.Time
}

// NewIdempotencyStore 创建幂等存储；ttl 为结果保留时长（<=0 表示不过期）。
func NewIdempotencyStore(ttl time.Duration) *IdempotencyStore {
	return &IdempotencyStore{
		entries: make(map[string]*idemEntry),
		ttl:     ttl,
		now:     time.Now,
	}
}

// Fingerprint 计算请求内容的稳定指纹。
func Fingerprint(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Execute 在幂等保护下执行 fn。scope 用于隔离不同接口的标识空间（如 api 名）。
// 返回的 Response.Replayed 为 true 表示这是重放结果，fn 并未再次执行。
func (s *IdempotencyStore) Execute(scope, key string, body []byte, fn func() (*Response, error)) (*Response, error) {
	fp := Fingerprint(body)
	full := scope + "|" + key

	s.mu.Lock()
	if e, ok := s.entries[full]; ok && !s.expired(e) {
		if e.fingerprint != fp {
			s.mu.Unlock()
			return nil, &Error{
				Kind:    KindIdempotencyConflict,
				Message: "idempotency key reused with different payload: " + key,
			}
		}
		if !e.completed {
			// 内容一致且首次请求仍在处理：等待其完成，返回一致结果。
			done := e.done
			s.mu.Unlock()
			<-done
			s.mu.Lock()
			resp, err := e.resp, e.err
			s.mu.Unlock()
			return markReplayed(resp), err
		}
		resp, err := e.resp, e.err
		s.mu.Unlock()
		return markReplayed(resp), err
	}
	// 首次执行：占位为 in-progress，释放锁后再跑业务，避免阻塞其他键。
	e := &idemEntry{fingerprint: fp, done: make(chan struct{})}
	s.entries[full] = e
	s.mu.Unlock()

	resp, err := fn()

	s.mu.Lock()
	e.resp, e.err = resp, err
	e.completed = true
	e.completedAt = s.now()
	close(e.done)
	s.mu.Unlock()
	return resp, err
}

// expired 惰性过期：过期的完成结果视为不存在，允许标识被重新使用。
func (s *IdempotencyStore) expired(e *idemEntry) bool {
	if !e.completed || s.ttl <= 0 {
		return false
	}
	return s.now().Sub(e.completedAt) > s.ttl
}

// markReplayed 返回响应的副本并置重放标记，避免调用方篡改缓存内容。
func markReplayed(resp *Response) *Response {
	if resp == nil {
		return nil
	}
	cp := *resp
	cp.Replayed = true
	return &cp
}
