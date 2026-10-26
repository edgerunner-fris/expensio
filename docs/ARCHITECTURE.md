# Expensio 架构

Expensio 是一个多租户 B2B 费用管理平台：员工提交费用单，管理员审批，
预算在提交时预占、审批通过时转为实际支出，报销完成后进入报表。

本文只写当前真实存在的结构与边界；规划中的部分会明确标注里程碑。
关键取舍见 `docs/adr/`。

## 服务

| 服务 | 端口 | 数据库 | 职责 |
| --- | --- | --- | --- |
| gateway | 8080 | 无 | 统一入口、JWT 校验、路由、限流、请求 ID |
| identity | 8081 | `identity_db` | 租户、用户、成员、角色、登录与刷新令牌、审计 |
| expense | 8082 | `expense_db` | 费用单、审批状态机、预算预占与扣减 |
| billing | 8083 | `billing_db` | 账单、订阅、支付回调（M4） |
| report | 8084 | `report_db` | 事件驱动的报表读模型（M3） |
| notification | 8085 | `notification_db` | 邮件/站内通知（M5） |
| frontend | 5173 | - | Vue 3 + TS 管理台（M5） |

本地编排见 `docker-compose.yml`（core / obs / mail 三个 profile），
`make up` 启动依赖，`make migrate` 对五个库执行迁移。

## 请求路径

```text
浏览器 / API 客户端
        │
        ▼
  gateway :8080
   ├─ 生成/透传 X-Request-ID
   ├─ 校验 access token（HS256，issuer/audience 固定）
   ├─ 按租户或 IP 做 Redis 令牌桶限流
   ├─ 剥离伪造的 X-Tenant-Id / X-User-Id / X-User-Role
   └─ 按最长前缀转发到下游服务
        │
        ▼
  identity / expense / billing / report / notification
   ├─ 再次校验 token（不假设网关一定校验过）
   ├─ 从 claims 建立 tenantctx，绝不从 body/query/path 取租户
   └─ service → repository → PostgreSQL（事务内设置 RLS 上下文）
```

网关只做入口治理，不做业务判断；业务服务可以独立启动并直接对内提供服务。

## 数据归属与隔离

- 每个服务独占一个数据库，**禁止跨服务直连数据库、禁止跨库 JOIN**。
- 跨服务协作只能通过 API（当前）或事件（M3 起）。
- 租户表启用 `FORCE ROW LEVEL SECURITY`，策略基于事务级
  `app.tenant_id`；运行时角色 `{service}_app` 为 `NOBYPASSRLS`，
  迁移角色 `{service}_migrator` 仅用于 DDL。详见 ADR-0001。
- 预算归 expense 服务所有，预占与费用写入在同一个本地事务内完成。
  详见 ADR-0003。

## 身份与权限

- 密码使用 argon2id（PHC 格式）；不存在账号与密码错误返回同一状态码与文案，
  并执行等价的哈希校验，避免账号枚举。
- access token：JWT HS256，claims 含 `tid`（租户）、`role`、`sid`，
  issuer/audience 固定为 `expensio-api`。
- refresh token：32 字节随机值，仅存 SHA-256 摘要；刷新时条件轮换，
  旧令牌再次出现视为重放，整族撤销。
- 角色：`owner > admin > member > viewer`。
  viewer 只读；member 可以管理自己的费用；admin/owner 可以审批与管理预算；
  只有 owner 能修改 owner。
- 已知取舍：HS256 需要所有服务共享密钥，服务被攻陷即可签发任意租户令牌。
  M6 上 K8s 前评估改为非对称签名（ADR 待补）。

## 金额

所有金额是 `int64` 最小货币单位（如分）+ 三位大写 ISO-4217 币种，
数据库为 `bigint`。禁止 `float32/float64`，禁止在应用层先读后算预算，
预算检查与扣减由一条条件 UPDATE 完成。详见 ADR-0003。

## 事件与异步（M3）

M3 采用事务内 outbox（ADR-0004），当前已完成事件契约、outbox 存储、投递器与报表消费者：

1. 业务事务内写 `app.outbox_events`（`internal/platform/outbox`）；
2. 提交后由后台投递器同步发送到 Kafka（`expense` 进程内，Kafka 故障不影响 HTTP）；
3. report 消费组用 `DisableAutoCommit` 手动提交：业务事务提交后才提交位点，
   瞬时错误原地退避重试，永久错误原字节转 DLQ 后再提交位点；
4. 消费端按 `(tenant_id, consumer, event_id)` 幂等，事实行只接受
   `version >= 当前版本` 的更新，乱序旧事件不会回滚状态。

**禁止在数据库事务内直接发 Kafka 消息**，否则回滚后消息无法撤回。
事件信封与费用快照定义在 `internal/platform/events`；主题为
`expensio.expense.events.v1`，分区键固定为 `tenant_id`。

## 报表查询（M3）

report 暴露 `GET /api/v1/reports/expenses/summary`，只读自己的
`app.expense_facts`（不调用 expense API、不跨库 JOIN）。聚合在查询时完成，
按 `(分组维度, 币种)` 输出，币种之间永不求和。owner/admin 默认
`scope=all`，member/viewer 只能用 `mine`，越权返回 403 而不是静默降级。
所有查询经 FORCE RLS + JWT 派生的租户上下文，读不到邻租户数据。

## 可观测性与运维

- 日志：zap JSON，敏感字段（token、密码、卡号）在 logger 层脱敏。
- 健康检查：`/healthz` 不碰下游；`/readyz` 并行检查并共享预算。
- 优雅关闭：摘流量 → 排空 → Shutdown → 强制关闭；WSL2 下已用 SIGTERM 实测。
- OTel / Prometheus / Grafana / Loki / Tempo 在 compose 中已编排，
  指标与 trace 的完整接入在 M7。
- 配置只从环境变量 / K8s Secret 读取；仓库内不提交 `.env` 或真实密钥。

## 持久化与装配

当前使用手写 pgx 查询与 `cmd/*/main.go` 显式装配；**没有使用 sqlc 或 wire**。
这是与初始技术栈清单的已知偏离，原因与后续计划见 ADR-0002。

## 测试

- 单元测试不依赖 Docker。
- 集成测试用 testcontainers 启动真实 PostgreSQL 16 / Redis 7 / Kafka 3.9，
  复用仓库里的初始化脚本与迁移文件，应用代码用 `{service}_app` 连库。
  Kafka 用与 compose 相同的 `apache/kafka` 镜像（官方模块只支持 confluent 镜像），
  宿主端口随机分配并写入 advertised listener；测试显式建主题，不依赖 auto-create。
- 本地没有 Docker 时允许跳过；CI 设置 `EXPENSIO_REQUIRE_DOCKER=1`，
  此时 Docker 不可用即失败，跳过不算通过。
- 关键回归：RLS 隔离、令牌轮换与重放、RBAC 矩阵、状态机、预算并发与回滚。
- 跨服务链路在 `internal/e2e` 单独回归：expense 真实写 outbox → 投递器发到
  真实 Kafka → report 消费者落读模型 → 报表 HTTP API 返回聚合，全程无假实现。

## 里程碑

| 里程碑 | 内容 | 状态 |
| --- | --- | --- |
| M0 | 骨架、配置、日志、健康检查、编排、迁移基线 | 完成 |
| M1 | identity、gateway、RLS、集成测试 | 完成 |
| M2 | expense、审批状态机、预算预占 | 完成 |
| M3 | Kafka、outbox、报表读模型 | 完成 |
| M4 | billing、订阅与支付回调幂等 | 未开始 |
| M5 | 前端、通知 | 未开始 |
| M6 | Dockerfile、K8s、Helm、GitHub Actions | 未开始 |
| M7 | 指标、trace、压测与告警 | 未开始 |
| M8 | 文档、演示、验收 | 未开始 |

## ADR

- [ADR-0001：共享 schema + RLS 多租户隔离](adr/0001-multi-tenancy-and-rls.md)
- [ADR-0002：持久化与依赖装配方式](adr/0002-persistence-and-wiring.md)
- [ADR-0003：预算归属与预占生命周期](adr/0003-budget-ownership-and-reservation.md)
- [ADR-0004：事务内 outbox 与报表读模型](adr/0004-event-driven-read-model.md)
