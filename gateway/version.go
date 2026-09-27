package gateway

import (
	"fmt"
	"sort"
	"sync"
)

// VersionStatus 描述一个接口版本的生命周期状态。
type VersionStatus int

const (
	StatusActive     VersionStatus = iota // 在线
	StatusDeprecated                      // 已废弃：仍服务，但响应带警告
	StatusSunset                          // 已下线：拒绝服务
)

func (s VersionStatus) String() string {
	switch s {
	case StatusActive:
		return "active"
	case StatusDeprecated:
		return "deprecated"
	case StatusSunset:
		return "sunset"
	default:
		return "unknown"
	}
}

// Handler 是某个版本的具体实现。
type Handler func(req *Request) (*Response, error)

// VersionedHandler 绑定版本号、状态与实现。
type VersionedHandler struct {
	Version string
	Status  VersionStatus
	Handler Handler
}

// VersionRouter 按 endpoint+version 路由到对应实现。
// 版本不存在、已下线、已废弃三种情况被明确区分：
//   - 不存在：ErrCodeVersionNotFound，并附上可用版本列表
//   - 已下线：ErrCodeVersionSunset
//   - 已废弃：正常返回，调用方可在响应上看到 Deprecated 标记
type VersionRouter struct {
	mu     sync.RWMutex
	routes map[string]map[string]VersionedHandler // endpoint -> version -> handler
}

func NewVersionRouter() *VersionRouter {
	return &VersionRouter{routes: make(map[string]map[string]VersionedHandler)}
}

// Register 注册某 endpoint 的某版本实现；重复注册同版本会覆盖。
func (r *VersionRouter) Register(endpoint string, h VersionedHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.routes[endpoint]
	if !ok {
		m = make(map[string]VersionedHandler)
		r.routes[endpoint] = m
	}
	m[h.Version] = h
}

// SetStatus 调整某版本的生命周期状态（如升级后将旧版本标记为废弃/下线），无需重启。
func (r *VersionRouter) SetStatus(endpoint, version string, status VersionStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.routes[endpoint]
	if !ok {
		return &Error{Code: ErrCodeVersionNotFound, Message: fmt.Sprintf("endpoint %q 未注册", endpoint)}
	}
	h, ok := m[version]
	if !ok {
		return &Error{Code: ErrCodeVersionNotFound, Message: fmt.Sprintf("endpoint %q 没有版本 %q", endpoint, version)}
	}
	h.Status = status
	m[version] = h
	return nil
}

// Resolve 解析请求应命中的版本实现；版本不存在与已下线返回不同错误。
func (r *VersionRouter) Resolve(endpoint, version string) (VersionedHandler, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.routes[endpoint]
	if !ok {
		return VersionedHandler{}, &Error{
			Code:    ErrCodeVersionNotFound,
			Message: fmt.Sprintf("endpoint %q 不存在", endpoint),
		}
	}
	h, ok := m[version]
	if !ok {
		return VersionedHandler{}, &Error{
			Code:    ErrCodeVersionNotFound,
			Message: fmt.Sprintf("endpoint %q 没有版本 %q，可用版本: %v", endpoint, version, r.versionsLocked(endpoint)),
		}
	}
	if h.Status == StatusSunset {
		return VersionedHandler{}, &Error{
			Code:    ErrCodeVersionSunset,
			Message: fmt.Sprintf("endpoint %q 的版本 %q 已下线，请迁移到: %v", endpoint, version, r.activeVersionsLocked(endpoint)),
		}
	}
	return h, nil
}

// versionsLocked 返回 endpoint 下全部已注册版本（排序），调用方需持有读锁。
func (r *VersionRouter) versionsLocked(endpoint string) []string {
	vs := make([]string, 0, len(r.routes[endpoint]))
	for v := range r.routes[endpoint] {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	return vs
}

// activeVersionsLocked 返回仍可服务（active/deprecated）的版本列表。
func (r *VersionRouter) activeVersionsLocked(endpoint string) []string {
	vs := make([]string, 0)
	for v, h := range r.routes[endpoint] {
		if h.Status != StatusSunset {
			vs = append(vs, v)
		}
	}
	sort.Strings(vs)
	return vs
}
