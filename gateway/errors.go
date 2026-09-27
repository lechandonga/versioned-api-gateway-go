package gateway

import "fmt"

// ErrorCode 是网关对外暴露的可区分错误类别。
type ErrorCode string

const (
	ErrCodeVersionNotFound     ErrorCode = "VERSION_NOT_FOUND"    // 版本完全不存在
	ErrCodeVersionSunset       ErrorCode = "VERSION_SUNSET"       // 版本已下线
	ErrCodeVersionDeprecated   ErrorCode = "VERSION_DEPRECATED"   // 版本已废弃（仅提示，仍可服务）
	ErrCodeRateLimited         ErrorCode = "RATE_LIMITED"         // 触发限流
	ErrCodeIdempotencyConflict ErrorCode = "IDEMPOTENCY_CONFLICT" // 同一幂等键携带不同请求内容
	ErrCodeIdempotencyFailed   ErrorCode = "IDEMPOTENCY_FAILED"   // 首次请求处理失败
	ErrCodeInvalidRequest      ErrorCode = "INVALID_REQUEST"      // 请求本身不合法
)

// Error 是网关统一错误类型，携带可区分的 Code 与重试提示。
type Error struct {
	Code       ErrorCode
	Message    string
	RetryAfter float64 // 秒，仅限流等场景有意义
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}
