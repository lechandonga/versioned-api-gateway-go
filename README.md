# versioned-api-gateway-go

一套带**版本协商、限流、幂等保护**的 API 网关能力（纯标准库，无外部依赖）。
不同版本的调用方在并发与重试场景下都能拿到一致、可解释、可分类的结果。

## 结构

```
gateway/
  types.go        请求/响应抽象
  errors.go       统一错误类别 ErrorKind 与 *Error
  version.go      版本注册表与协商（VersionRegistry）
  ratelimit.go    令牌桶限流器（RateLimiter，配置可热更新）
  idempotency.go  幂等存储（IdempotencyStore）
  gateway.go      处理链：版本协商 -> 限流 -> 幂等 -> 版本实现
  client.go       客户端封装，失败统一归类为 *ClientError
```

## 版本策略

每个接口（`api`）可注册多个版本，每个版本有生命周期状态：

| 状态 | 行为 |
|---|---|
| `ACTIVE` | 正常路由到该版本实现 |
| `DEPRECATED` | 仍路由到该版本实现，但响应置 `Deprecated=true` 并附 `HeaderHint` 升级提示 |
| `SUNSET` | 拒绝，返回 `SUNSET_VERSION` 错误 |
| 未注册 | 拒绝，返回 `UNKNOWN_VERSION` 错误 |

要点：

- **下线与不存在严格区分**：`SUNSET_VERSION` ≠ `UNKNOWN_VERSION`，调用方可据此决定是“升级版本”还是“修正配置”。
- **不做静默跨版本路由**：调用方声明 `v1` 就只会拿到 `v1` 的结果，即使 `v2` 已上线（见 `TestNoSilentCrossVersionRouting`）。
- `Register` 可重复调用以热更新版本状态（如把 `v1` 从 ACTIVE 置为 SUNSET），无需重启。

## 限流

算法：**令牌桶**，维度为 `(调用方, 接口)`。

```go
lim := gateway.NewRateLimiter(map[string]gateway.LimitConfig{
    "orders.create": {RatePerSec: 10, Burst: 20}, // 每秒补 10 个令牌，最多积压 20
})
```

- **突发**：由 `Burst` 容量吸收，容量内瞬时放行。
- **并发安全**：所有桶共用一把互斥锁，200 并发突发下放行数精确等于容量（race 检测覆盖）。
- **窗口切换**：不按固定窗口重置，而是按经过时间精确补令牌（`elapsed * rate`），不存在窗口边界配额放大。
- **热更新**：`UpdateConfig` 原子替换配置，立即生效；已有桶的令牌会被钳制到新容量内，配置收紧瞬间也不会突破新配额。
- **被限流的响应**：错误类别 `RATE_LIMITED`，并携带 `RetryAfter`（攒够一个令牌所需秒数）作为重试提示。
- 未配置限流的接口默认放行。

## 幂等语义

写接口在请求中携带 `IdempotencyKey`，标识空间按接口隔离（`api|key`）：

| 场景 | 行为 |
|---|---|
| 首次提交 | 执行业务逻辑，缓存结果 |
| 同标识 + 同内容（已完成） | 不重复执行，返回与首次一致的结果，置 `Replayed=true` |
| 同标识 + 同内容（仍在处理中） | **等待**首次完成，返回一致结果（不重入、不重复写） |
| 同标识 + 不同内容（无论是否在途） | 拒绝，`IDEMPOTENCY_CONFLICT` |
| 不同接口的相同标识 | 互不影响 |

内容一致性用请求体的 SHA-256 指纹判定。结果缓存有 TTL（`NewIdempotencyStore(ttl)`），过期后标识可被重新使用；TTL<=0 表示不过期。缓存响应在返回前复制，调用方无法篡改缓存内容。

## 错误分类

所有失败对外暴露为可区分的类别（`ErrorKind`）：

| Kind | 含义 | 附带信息 |
|---|---|---|
| `UNKNOWN_VERSION` | 接口或版本从未注册 | — |
| `SUNSET_VERSION` | 版本已下线 | — |
| `RATE_LIMITED` | 被限流 | `RetryAfter` 秒 |
| `IDEMPOTENCY_CONFLICT` | 幂等标识配了不同内容 | — |
| `INTERNAL` | 业务实现或底层库错误 | 原始错误仅通过 `Unwrap` 供诊断 |

客户端（`Client.Call`）把一切失败包装为 `*ClientError`：**底层库的原始异常不会原样透出**，
统一归为 `INTERNAL`；用 `gateway.IsKind(err, kind)` 做分类分支。

## 本地复现验证

全部测试在本地运行，不依赖外部服务或真实凭据；测试日志打印请求上下文与判定依据。

```bash
# race 检测需要 cgo
CGO_ENABLED=1 go test -race -v ./gateway/
```

覆盖场景：

- 并发限流：`TestRateLimiter_ConcurrentBurstNeverExceedsQuota`（200 并发不超配额）、
  `TestRateLimiter_PerCallerIsolation`、`TestRateLimiter_WindowRefill`、`TestRateLimiter_HotReloadConfig`
- 版本兼容矩阵：`TestVersionNegotiationMatrix`（ACTIVE/DEPRECATED/SUNSET/未注册）、
  `TestSunsetDistinctFromUnknown`、`TestNoSilentCrossVersionRouting`
- 幂等重放与冲突：`TestIdempotency_ReplayReturnsSameResult`、
  `TestIdempotency_ConflictingPayloadRejected`、
  `TestIdempotency_ConcurrentInProgressSingleExecution`（32 并发只写一次）、
  `TestIdempotency_ConflictWhileInProgress`
- 端到端与客户端分类：`TestClientErrorClassification`、`TestClientWrapsRawHandlerError`、
  `TestGatewayRateLimitEndToEnd`、`TestGatewayIdempotentWriteUnderConcurrency` 等
