package gateway

// Request 是经过网关统一抽象后的请求。
type Request struct {
	API            string // 接口名，如 "orders.create"
	Version        string // 调用方声明的版本，如 "v1"
	CallerID       string // 调用方标识，用于限流维度
	IdempotencyKey string // 写接口幂等标识，可为空（非写接口）
	Body           []byte // 请求内容
}

// Response 是网关统一抽象后的响应。
type Response struct {
	Status     int
	Body       []byte
	Deprecated bool   // 命中的版本已废弃时置位，提示调用方升级
	Replayed   bool   // 命中幂等重放时置位
	HeaderHint string // 给调用方的可读提示（如升级建议、限流说明）
}
