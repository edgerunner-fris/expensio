# ADR-0014：自动续费、模拟支付、租户删除、下游熔断与日志聚合

- 状态：已接受（M18）
- 日期：2026-10-28

## 背景

M17 结束时，billing 只有"创建订阅 + 首期账单 + 支付回调"的闭环：周期不会
自动前进，`mockpay` 只在文档里（手工构造 HMAC 请求），租户无法注销，网关在
下游挂掉时只会持续把请求送过去，日志只进 Docker json-file、无法按
trace 检索。M18 补齐这五块。

## 决策

### 1. 自动续费：锚点日 + 只进不退的周期

**问题**：`current_period_end` 之后怎么办？按月加一天会在短月漂移：
1 月 31 日起租，按"下次月底"走会变成 2 月 28 日 → 3 月 28 日，越走越早。

**方案**（migration `billing/000003_renewal`）：

- `subscriptions.anchor_day smallint`，取值 1–31，由首期
  `current_period_start` 的日号决定，BEFORE INSERT 触发器写入，之后不可改；
- 下一期起点 = "当前起点 + N 个月"那个月的 `min(anchor_day, 月末)`；
  下一期终点 = 起点 + N 个月的同一规则 - 1 天；
  于是 1/31 → 2/28–3/30 → 3/31–4/29，锚点在长月自动回到 31；
- `subscriptions_guard_update` 只放行**连续前进**的周期更新：
  `new_start = old_end + 1`、两端都严格变大、状态保持 `active`；
  向后退、留空档、只改一端、借改周期改状态，全部 `23514`；
- 账单金额取订阅当前金额，`invoices_period_key` 保证同一周期只开一张票；
  事务里先锁订阅行再复查，余额/状态读到的都是最新值。

**跨租户扫描**：续费任务要在"没有租户上下文"的情况下找出到期订阅，而
`subscriptions` 是 `ENABLE + FORCE RLS`——沿用 M17 的教训，definer 函数
绕不过 FORCE。方案是维护一张**无 RLS 的目录表** `app.renewal_schedule`
（subscription_id 主键、tenant_id、current_period_end、status），由
`SECURITY DEFINER` 触发器在订阅 insert/update 后 `ON CONFLICT DO UPDATE`；
到期查询走 `app.due_subscriptions(as_of, max_rows)`（同样 definer，`STABLE`，
`REVOKE ALL` + 只 `GRANT EXECUTE` 给 `billing_app`）。任务拿到
`(subscription_id, tenant_id)` 后，再按租户用 `WithinTenantID` 进入正常 RLS 上下文处理。

为什么不是"给 billing_app 加 BYPASSRLS"或"定时全表扫描 definer 函数"：
前者把绕过的能力永久放在应用角色上；后者每次都要扫全表且与 FORCE 规则
正面冲突。目录表只有四列、由触发器单向同步，漂移风险被限制在"触发器没跑"
（同一事务，失败即回滚）。

**不续费的情况**（都计入 `expensio.billing.renewal.results` 的 outcome）：

| outcome | 条件 |
| --- | --- |
| `not_active` | 订阅已取消 / 暂停 / `past_due` |
| `up_to_date` | 周期尚未到期（目录表有轻微滞后时的兜底） |
| `unpaid` | 当前周期仍有未支付账单 |

"有未付账单就不续费"是有意的：否则每次任务运行都会再开一张票，用户欠费
期间账单无限堆叠。代价是欠费用户需要补缴后由下一轮任务追平；追平是
**每轮每个订阅只前进一期**，落后 N 期需要 N 轮——这是为了保住"周期连续"
的约束，宁可慢也不跳期。

任务 `RenewalJob` 启动立刻跑一次，之后按 `billing.renewal_interval`（默认
1h）tick；多副本安全依赖行锁 + `invoices_period_key`，先到的实例处理，
后到的读到已前进的周期返回 `up_to_date`。

### 2. `mockpay`：可重复的第三方支付模拟器

`cmd/mockpay` 是一个独立 CLI：`--url` 指向
`POST /api/v1/webhooks/payments`，`--event` 选 `succeeded` / `failed`，
签名密钥只从 `EXPENSIO_BILLING_WEBHOOK_SECRET` 读取（少于 32 字节直接退出），
provider 从 `EXPENSIO_BILLING_WEBHOOK_PROVIDER` 读取。请求体与 billing 的
`DisallowUnknownFields` 结构逐字段对齐，签名是
`hex(HMAC-SHA256(secret, "<timestamp>.<raw body>"))`，与真实 PSP 的
"对时间戳 + 原始报文签名"一致（而不是对解析后的 JSON 重编码——字段顺序或
空白不同就会验签失败）。时间戳超出 5 分钟窗口会被 billing 拒绝，CLI 因此
每次重新生成时间戳。

它不做重试：非 2xx 直接退出码 1，方便脚本用 `set -e` 捕获。这样"支付成功 →
billing 更新状态 → 下一轮续费"可以完全脚本化，不依赖外网 PSP。

### 3. 租户删除：软删除 + 立即断链 + 已知的 15 分钟尾巴

`DELETE /api/v1/tenants/current`（仅 owner）要求请求体回写当前 `slug`：
删除是不可逆操作，单纯的身份校验不足以防止误点/脚本误触发。

同一事务内：

1. `app.tenants.deleted_at` 置位；
2. 该租户全部 `memberships` 软删除（成员列表、切换租户立即不可见）；
3. 该租户全部刷新令牌 `revoked_at` 置位（无法续期）；
4. 未接受的邀请全部置 `revoked_at`（邮件里的链接立即失效）；
5. 审计 `tenant.deleted`（操作者、slug、撤销的会话数）。

**没有做物理删除**：账务与审计数据必须保留，且跨服务删除需要分布式事务
或 saga。软删除后各服务的查询都以 `deleted_at IS NULL` / 租户存在性为前提，
实际效果是"不可访问"而非"数据消失"；真正的清理（按保留策略导出后物理删除）
是运维流程，不在本期。

**已知尾巴**：已签发的 access token 在剩余 TTL（≤15 分钟）内仍能通过网关
验签并调用只读接口——下游服务只验 JWT 签名，不查租户状态。这与改密后的
行为一致，修复需要引入令牌黑名单或每请求查库，两者都与"无状态验签"的
设计冲突。文档与前端提示里都写明了；刷新链已全部撤销，最多 15 分钟后
彻底失去访问。

删除后重复调用返回 404（租户已不可见），而不是幂等的 200：调用者需要知道
"这次什么都没删除"。

### 4. 熔断：按下游独立、只对基础设施类失败计数

网关每个下游一个 `gobreaker`（`CircuitBreaker[*http.Response]`）：
连续 5 次失败打开，30 秒后进入 half-open 放 1 个探测请求，成功即闭合。

**失败判定**：网络错误 + 下游返回 502/503/504。

- 500 不计入：多数 500 由个别坏请求触发，用一次坏输入熔断整个下游会让
  故障面扩大；
- `context.Canceled` 不计入：客户端走了，不是下游故障；
- 响应仍原样转发：gobreaker 只按 error 计数，5xx 是"需要计数但必须返回"
  的响应，因此闭包把响应暂存、向 breaker 返回哨兵错误，外层再交还原响应。

**打开期间**：直接返回 503 `common.upstream_circuit_open` + `Retry-After`
（等于 open 超时），不再触达下游；日志与 `expensio.gateway.circuit.rejections`
指标可区分"主动拒绝"和"下游答错"。状态切换写
`expensio.gateway.circuit.transitions{from,to}`。

**传输链顺序**：熔断器 → otelhttp → 重试 → 连接。熔断在最外层，一次逻辑
请求（含重试）只记一次成败；otelhttp 仍在重试之外，保留每次实际下发的
client span。

被否决的**全站共用熔断器**：报表慢查询会连带把登录熔断，故障隔离失效。
被否决的**按失败比例**：低流量下游（notification）几次失败就可能超过比例
阈值，而"连续 5 次"在小样本下更稳定。

### 5. 日志聚合：Loki + Alloy，trace_id 直连 Tempo

- 本地：`deploy/local/loki/config.yaml`（单进程、filesystem、7 天保留）+
  `deploy/local/alloy/config.alloy`（Docker 发现，按 compose 服务名打标签）；
- 生产：`deploy/cluster/logging/` 下的 `loki-values.yaml`（SingleBinary +
  S3 兼容对象存储、30 天）与 `alloy-values.yaml`（DaemonSet，用
  `loki.source.kubernetes` 走 API 读日志，只采集
  `app.kubernetes.io/part-of=expensio` 的 Pod）；
- 访问日志新增 `trace_id` / `span_id`：从 `telemetry.GinMiddleware` 建立的
  span 上下文读取；Grafana 的 Loki 数据源用 derived field 把 32 位
  trace_id 变成 Tempo 链接，Tempo 也配置了 `tracesToLogsV2` 反向跳转。

为什么是 Alloy 而不是 Promtail：Promtail 已在 2025-03 EOL，Alloy 是官方
继任者，且一个 agent 即可承担未来 OTLP 转发。

日志**不参与告警**：告警全部基于指标，采集链路故障时不会静默失效；日志只
用于事后定位。

## 风险

- 续费每轮只追平一期：长期停机后需要多轮才能追到当期，期间用户看到的
  周期是旧的。可接受，因为每一期都有独立账单与 `period_key`，不会漏账。
- 欠费阻塞续费：用户补缴后要等下一轮任务；`unpaid` 会持续出现在指标里，
  便于发现"有人长期欠费"。
- 租户删除后 access token 最长 15 分钟仍可用（见上），且软删除数据仍在库中，
  "删除"在合规意义上不等于"擦除"。
- `renewal_schedule` 是一份可重建的冗余状态；若触发器被禁用（例如手工维护），
  目录会漂移。迁移与集成测试覆盖了触发器路径，运维侧禁止手工改订阅表。
- 熔断阈值是常量（5 次 / 30 秒），未按流量自适应；低流量下游可能因 5 次
  连续失败在数小时内一直半开探测。`circuit.transitions` 指标是调参依据。
- 本地 Alloy 挂载 `docker.sock` 等价于 root 级宿主权限，仅限开发；生产
  values 不挂宿主目录/套接字。
- Loki 单实例在本地不保证高可用；生产 values 也只有单副本（数据在对象
  存储里，Pod 重建不丢日志，但查询会短暂不可用）。
- 日志里的 `tenant_id` / `user_id` 来自已认证请求；未认证路径（登录、回调）
  只有 `request_id` 与 IP。审计日志才是合规追溯来源，Loki 不是。

## 测试

- `internal/billing/renewal_integration_test.go`：真实 PostgreSQL 容器，
  覆盖正常续费、幂等、未付跳过、已取消忽略、短月锚点、`past_due` 跳过，
  以及 guard 对后退/空档/单列修改/状态耦合/改锚点的拒绝；
- `internal/identity/tenant_delete_integration_test.go`：admin 403、slug
  不匹配 400 且无副作用、删除后刷新 401、重新登录 401、切回 403、列表消失、
  数据库断言（成员/邀请/令牌全部失效、审计一条）、重复删除 404；
- `internal/gateway/circuitbreaker_test.go`：5xx 打开、超时后半开恢复、
  500 不熔断、按下游隔离、客户端取消不计入；
- `internal/platform/httpx/middleware/accesslog_test.go`：有 span 时写入
  trace_id/span_id，无 span 时不写空字段；
- `cmd/mockpay/main_test.go`：请求体构造、端点校验、签名格式；
- 本地整栈实测：Loki `/ready` 返回 `ready`，`label/service/values` 含七个
  服务名，按 `{service="identity"}` 查到带 `request_id` 的访问日志。
