package gateway

import (
	"errors"
	"fmt"
)

// ClientError 是客户端侧的统一错误：底层异常一律被归类包装，
// 调用方只需按 Kind 分支处理，永远不会直接拿到网关内部的原始错误值。
type ClientError struct {
	Kind       ErrorKind
	Message    string
	RetryAfter float64
	cause      error
}

func (e *ClientError) Error() string {
	return fmt.Sprintf("gateway client error [%s]: %s", e.Kind, e.Message)
}

// Unwrap 暴露被包装的原因，供 errors.Is/As 诊断，但类型本身已是分类后的。
func (e *ClientError) Unwrap() error { return e.cause }

// Client 是对外调用网关的客户端封装。
type Client struct {
	gw       *Gateway
	callerID string
}

// NewClient 创建绑定某个调用方标识的客户端。
func NewClient(gw *Gateway, callerID string) *Client {
	return &Client{gw: gw, callerID: callerID}
}

// Call 发起一次调用并把一切失败归类为 *ClientError。
func (c *Client) Call(api, version, idempotencyKey string, body []byte) (*Response, error) {
	resp, err := c.gw.Handle(&Request{
		API:            api,
		Version:        version,
		CallerID:       c.callerID,
		IdempotencyKey: idempotencyKey,
		Body:           body,
	})
	if err == nil {
		return resp, nil
	}
	var ge *Error
	if errors.As(err, &ge) {
		return nil, &ClientError{
			Kind:       ge.Kind,
			Message:    ge.Message,
			RetryAfter: ge.RetryAfter,
			cause:      ge,
		}
	}
	// 非网关错误（业务实现或底层库抛出）：统一归类为 INTERNAL，不原样透出。
	return nil, &ClientError{
		Kind:    KindInternal,
		Message: "internal gateway failure",
		cause:   err,
	}
}

// IsKind 判断一次客户端调用失败是否属于某个类别。
func IsKind(err error, kind ErrorKind) bool {
	var ce *ClientError
	if errors.As(err, &ce) {
		return ce.Kind == kind
	}
	return false
}
