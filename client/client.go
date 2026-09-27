// Package client 提供网关的调用方客户端，把网关错误归类为可区分的类型。
package client

import (
	"context"
	"errors"
	"fmt"

	"github.com/lechandonga/versioned-api-gateway-go/gateway"
)

// ErrorKind 是客户端可区分的错误类别。
type ErrorKind int

const (
	KindUnknown             ErrorKind = iota
	KindVersionNotFound               // 版本不存在
	KindVersionSunset                 // 版本已下线
	KindRateLimited                   // 被限流（可依据 RetryAfter 重试）
	KindIdempotencyConflict           // 幂等键绑定了不同请求内容
	KindIdempotencyFailed             // 幂等键的首次请求失败
	KindInvalidRequest                // 请求参数不合法
)

func (k ErrorKind) String() string {
	switch k {
	case KindVersionNotFound:
		return "VersionNotFound"
	case KindVersionSunset:
		return "VersionSunset"
	case KindRateLimited:
		return "RateLimited"
	case KindIdempotencyConflict:
		return "IdempotencyConflict"
	case KindIdempotencyFailed:
		return "IdempotencyFailed"
	case KindInvalidRequest:
		return "InvalidRequest"
	default:
		return "Unknown"
	}
}

// Error 是客户端侧的分类错误，调用方可按 Kind 分别处理。
type Error struct {
	Kind       ErrorKind
	Message    string
	RetryAfter float64 // 秒，仅 KindRateLimited 有意义
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// Transport 抽象到网关的传输（本地进程内实现，便于测试，不依赖外部服务）。
type Transport interface {
	Do(ctx context.Context, req *gateway.Request) (*gateway.Response, error)
}

// LocalTransport 是进程内 Transport，直接调用 Gateway。
type LocalTransport struct {
	GW *gateway.Gateway
}

func (t *LocalTransport) Do(ctx context.Context, req *gateway.Request) (*gateway.Response, error) {
	return t.GW.Handle(ctx, req)
}

// Client 是网关调用方客户端。
type Client struct {
	t Transport
}

func New(t Transport) *Client { return &Client{t: t} }

// Call 发起调用并把底层错误归类为 *client.Error，不向调用方透出网关内部原始错误。
func (c *Client) Call(ctx context.Context, req *gateway.Request) (*gateway.Response, error) {
	resp, err := c.t.Do(ctx, req)
	if err != nil {
		return nil, classify(err)
	}
	return resp, nil
}

// classify 把网关错误映射为客户端可区分的 ErrorKind。
func classify(err error) error {
	var ge *gateway.Error
	if !errors.As(err, &ge) {
		// 非网关标准错误：归为 Unknown，不原样透出底层细节。
		return &Error{Kind: KindUnknown, Message: "网关内部错误"}
	}
	out := &Error{Message: ge.Message, RetryAfter: ge.RetryAfter}
	switch ge.Code {
	case gateway.ErrCodeVersionNotFound:
		out.Kind = KindVersionNotFound
	case gateway.ErrCodeVersionSunset:
		out.Kind = KindVersionSunset
	case gateway.ErrCodeRateLimited:
		out.Kind = KindRateLimited
	case gateway.ErrCodeIdempotencyConflict:
		out.Kind = KindIdempotencyConflict
	case gateway.ErrCodeIdempotencyFailed:
		out.Kind = KindIdempotencyFailed
	case gateway.ErrCodeInvalidRequest:
		out.Kind = KindInvalidRequest
	default:
		out.Kind = KindUnknown
	}
	return out
}
