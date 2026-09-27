# versioned-api-gateway-go

带**版本协商、限流和幂等保护**的 API 网关能力（纯本地实现，不依赖外部服务或真实凭据）。
不同版本的调用方在并发和重试场景下都能拿到一致、可解释的结果。

## 结构

```
gateway/   网关核心：版本路由、令牌桶限流、幂等保护、统一错误类型
client/    调用方客户端：把网关错误归类为可区分的 ErrorKind，不透出底层原始异常
```

## 版本策略（gateway/version.go）

- 每个 endpoint 可同时注册多个版本，按调用方声明的 `Version` 精确路由，**不会**在版本升级后把旧调用方静默路由到新版本。
- 版本有三种生命周期状态，错误可明确区分：

| 状态 | 行为 | 错误码 |
|---|---|---|
| `active` | 正常服务 | — |
| `deprecated` | 仍服务，响应带 `Deprecated=true` 标记 | — |
| `sunset`（已下线） | 拒绝服务 | `VERSION_SUNSET`（附可迁移版本列表） |
| 未注册（不存在） | 拒绝服务 | `VERSION_NOT_FOUND`（附可用版本列表） |

- `SetStatus` 可在不重启的情况下把旧版本标记为废弃/下线。

## 限流（gateway/ratelimit.go）

- **算法**：令牌桶。按经过时间精确补充令牌（`tokens += elapsed * rate`，封顶 `burst`），不依赖固定窗口，因此**窗口切换边界不会出现双倍放行**。
- **维度**：`caller + endpoint`；配置查找优先级：`caller+endpoint` 精确配置 > `caller` 级配置 > 默认配置。
- **并发安全**：所有桶操作在同一把互斥锁内完成，突发与并发下配额不会被突破（测试：burst=10、100 并发，放行恰好 10）。
- **热更新**：`SetConfig` 立即生效，无需重启；调小配额时存量令牌按新 `burst` 截断，调大配额只提高补充封顶（不凭空发令牌）。
- **重试提示**：被限流时返回 `RATE_LIMITED` 错误并携带 `RetryAfter`（补一个令牌所需秒数），按提示等待后重试即可成功。

## 幂等语义（gateway/idempotency.go）

写接口携带 `IdempotencyKey` 即受保护，粒度为 `endpoint + key`：

| 场景 | 行为 |
|---|---|
| 同键 + 相同内容（Body 的 SHA-256 一致） | 只执行一次，重复请求**重放**首次结果 |
| 同键 + 不同内容 | 明确拒绝 `IDEMPOTENCY_CONFLICT`，不发生写入 |
| 首次请求仍在处理中 | 重复请求阻塞等待首次完成（可被 ctx 取消），返回一致结果，不并发重复写入 |
| 首次失败 | 记录失败，重放返回一致的 `IDEMPOTENCY_FAILED`，不重复执行 |

底层 handler 对同一幂等键**至多执行一次**（并发测试：处理中并发重复提交 20 次，底层写入恰好 1 次）。

## 错误分类

网关侧统一为 `*gateway.Error`（`Code` 可区分）；客户端侧 `client.Call` 将其映射为 `*client.Error` 的 `Kind`：

| gateway.ErrorCode | client.ErrorKind |
|---|---|
| `VERSION_NOT_FOUND` | `KindVersionNotFound` |
| `VERSION_SUNSET` | `KindVersionSunset` |
| `RATE_LIMITED` | `KindRateLimited`（带 `RetryAfter`） |
| `IDEMPOTENCY_CONFLICT` | `KindIdempotencyConflict` |
| `IDEMPOTENCY_FAILED` | `KindIdempotencyFailed` |
| `INVALID_REQUEST` | `KindInvalidRequest` |
| 其他非标准底层错误 | `KindUnknown`（不透出底层细节） |

## 本地复现验证

```bash
go build ./...
go vet ./...
go test -v ./...                     # 单测日志含请求上下文与判定依据
CGO_ENABLED=1 go test -race ./...    # race 检测（并发限流、并发幂等）
```

覆盖场景：

- `gateway/version_test.go`：版本兼容矩阵（在线/废弃/下线/不存在/接口不存在）、下线与不存在错误可区分、升级后旧调用方不被静默错路由。
- `gateway/ratelimit_test.go`：并发突发配额不突破、窗口推进精确补充（无边界突刺）、caller+endpoint 维度隔离、配置热更新、RetryAfter 提示可用。
- `gateway/idempotency_test.go`：重放一致性、内容冲突拒绝、处理中并发重复提交、失败重放一致、不同键隔离。
- `client/client_test.go`：各类失败归类为可区分 `ErrorKind`、底层原始错误不透出、按 RetryAfter 重试成功。
