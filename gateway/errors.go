// Package gateway 提供带版本协商、限流与幂等保护的 API 网关能力。
package gateway

import "fmt"

// ErrorKind 描述网关对外暴露的可区分错误类别。
type ErrorKind int

const (
	// KindUnknownVersion 请求的版本号从未注册过。
	KindUnknownVersion ErrorKind = iota
	// KindSunsetVersion 版本已下线，不再提供任何服务。
	KindSunsetVersion
	// KindDeprecatedVersion 版本仍可用但已废弃（提示性，不阻断）。
	KindDeprecatedVersion
	// KindRateLimited 请求被限流。
	KindRateLimited
	// KindIdempotencyConflict 同一幂等标识携带了不同的请求内容。
	KindIdempotencyConflict
	// KindIdempotencyInProgress 同一幂等标识的请求仍在处理中。
	KindIdempotencyInProgress
	// KindInternal 网关内部错误。
	KindInternal
)

// String 返回错误类别的稳定字符串表示，用于日志与客户端分类。
func (k ErrorKind) String() string {
	switch k {
	case KindUnknownVersion:
		return "UNKNOWN_VERSION"
	case KindSunsetVersion:
		return "SUNSET_VERSION"
	case KindDeprecatedVersion:
		return "DEPRECATED_VERSION"
	case KindRateLimited:
		return "RATE_LIMITED"
	case KindIdempotencyConflict:
		return "IDEMPOTENCY_CONFLICT"
	case KindIdempotencyInProgress:
		return "IDEMPOTENCY_IN_PROGRESS"
	case KindInternal:
		return "INTERNAL"
	default:
		return fmt.Sprintf("ErrorKind(%d)", int(k))
	}
}

// Error 是网关的统一错误类型，携带类别、可读信息与可选的重试提示。
type Error struct {
	Kind       ErrorKind
	Message    string
	RetryAfter float64 // 秒；仅限流等场景有意义，0 表示无提示
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Kind, e.Message) }

// KindOf 从任意 error 中提取网关错误类别；非网关错误归为 KindInternal。
func KindOf(err error) ErrorKind {
	if ge, ok := err.(*Error); ok {
		return ge.Kind
	}
	return KindInternal
}
