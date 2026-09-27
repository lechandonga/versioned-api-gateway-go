package gateway

import "sync"

// VersionStatus 表示一个接口版本的生命周期状态。
type VersionStatus int

const (
	// StatusActive 正常在线。
	StatusActive VersionStatus = iota
	// StatusDeprecated 仍可用但已废弃，响应中应提示调用方升级。
	StatusDeprecated
	// StatusSunset 已下线，请求必须被拒绝且与“版本不存在”区分。
	StatusSunset
)

// String 返回状态的稳定字符串表示。
func (s VersionStatus) String() string {
	switch s {
	case StatusActive:
		return "ACTIVE"
	case StatusDeprecated:
		return "DEPRECATED"
	case StatusSunset:
		return "SUNSET"
	default:
		return "UNKNOWN"
	}
}

// Handler 是某个版本对应的业务实现。
type Handler func(req *Request) (*Response, error)

// versionEntry 记录单个版本的注册信息。
type versionEntry struct {
	status  VersionStatus
	handler Handler
}

// VersionRegistry 维护“接口名 -> 版本 -> 实现”的路由表，并发安全。
type VersionRegistry struct {
	mu      sync.RWMutex
	entries map[string]map[string]*versionEntry
}

// NewVersionRegistry 创建空的版本注册表。
func NewVersionRegistry() *VersionRegistry {
	return &VersionRegistry{entries: make(map[string]map[string]*versionEntry)}
}

// Register 注册一个版本的实现与状态；重复注册会覆盖（用于版本状态热更新）。
func (r *VersionRegistry) Register(api, version string, status VersionStatus, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.entries[api]
	if !ok {
		m = make(map[string]*versionEntry)
		r.entries[api] = m
	}
	m[version] = &versionEntry{status: status, handler: h}
}

// Resolution 是一次版本协商的结果。
type Resolution struct {
	Handler    Handler
	Status     VersionStatus
	Deprecated bool // 为 true 时调用方应收到升级提示，但请求仍被执行
}

// Resolve 按调用方声明的版本查找实现。
// 版本不存在返回 KindUnknownVersion，已下线返回 KindSunsetVersion，二者严格区分。
func (r *VersionRegistry) Resolve(api, version string) (*Resolution, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.entries[api]
	if !ok {
		return nil, &Error{Kind: KindUnknownVersion, Message: "api not registered: " + api}
	}
	e, ok := m[version]
	if !ok {
		return nil, &Error{Kind: KindUnknownVersion, Message: "version not registered: " + api + "@" + version}
	}
	if e.status == StatusSunset {
		return nil, &Error{Kind: KindSunsetVersion, Message: "version sunset: " + api + "@" + version}
	}
	return &Resolution{Handler: e.handler, Status: e.status, Deprecated: e.status == StatusDeprecated}, nil
}
