# Expensio 架构

Expensio 是一个多租户 B2B 费用管理平台：员工提交费用单，管理员审批，
预算在提交时预占、审批通过时转为实际支出，报销完成后进入报表。

本文只写当前真实存在的结构与边界；规划中的部分会明确标注里程碑。
关键取舍见 `docs/adr/`。

## 服务

| 服务 | 端口 | 数据库 | 职责 |
| --- | --- | --- | --- |
| gateway | 8080 | 无 | 统一入口、JWT 校验、路由、限流、请求 ID |
| identity | 8081 | `identity_db` | 租户、用户、成员、角色、成员邀请、登录与刷新令牌、审计 |
| expense | 8082 | `expense_db` | 费用单、审批状态机、预算预占与扣减 |
| billing | 8083 | `billing_db` | 账单、订阅、支付回调 |
| report | 8084 | `report_db` | 事件驱动的报表读模型 |
| notification | 8085 | `notification_db` | 站内通知与邮件外发 |
| web | 8086（宿主 5173） | - | 管理台静态资源、`/api` 反代到网关 |

本地编排见 `docker-compose.yml`（core / obs / mail / app / load 五个 profile），
`make up` 启动依赖，`make migrate` 对六个库执行迁移；
`make app-up` 启动完整容器栈（含管理台），`make load` 跑 k6。
生产部署见 `docs/DEPLOYMENT.md`。

## 请求路径

```text
浏览器
   │
   ▼
  web :8086
   ├─ 提供 Vite 构建产物，未知前端路由回退 index.html
   ├─ 静态资源与 /api 同源，浏览器不需要 CORS
   └─ /api/* 反向代理
        │
        ▼
  gateway :8080
   ├─ 生成/透传 X-Request-ID
   ├─ 校验 access token（EdDSA / Ed25519，issuer/audience 固定）
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
- 没有用户 JWT 的服务间调用（notification → identity 取收件人）使用独立的
  HMAC 短时令牌：`{tenant_id, exp}` 放在签名载荷里，有效期 2 分钟，
  网关不转发 `/api/v1/internal/*`。详见 ADR-0006。
- 租户表启用 `FORCE ROW LEVEL SECURITY`，策略基于事务级
  `app.tenant_id`；运行时角色 `{service}_app` 为 `NOBYPASSRLS`，
  迁移角色 `{service}_migrator` 仅用于 DDL。详见 ADR-0001。
- 预算归 expense 服务所有，预占与费用写入在同一个本地事务内完成。
  详见 ADR-0003。

## 身份与权限

- 密码使用 argon2id（PHC 格式）；不存在账号与密码错误返回同一状态码与文案，
  并执行等价的哈希校验，避免账号枚举。
- access token：JWT **EdDSA（Ed25519）**，claims 含 `tid`（租户）、`role`、`sid`，
  issuer/audience 固定为 `expensio-api`。只有 identity 持有私钥并签发；
  其余五个服务只挂公钥验签，identity 通过 `/.well-known/jwks.json` 公布
  JWKS（`kid` = RFC 7638 指纹）。验签端用 `jwt.WithValidMethods` 锁定算法，
  拒绝 `alg=none` 与 RS/HS 混淆。只接受 EdDSA：M12 删除了迁移期的
  HS256 兼容路径与共享密钥配置（`EXPENSIO_JWT_SECRET` /
  `EXPENSIO_JWT_ALLOW_HS256`），不存在对称回退。历史与迁移步骤见 ADR-0008。
- refresh token：32 字节随机值，仅存 SHA-256 摘要；刷新时条件轮换，
  旧令牌再次出现视为重放，整族撤销。
- 角色：`owner > admin > member > viewer`。
  viewer 只读；member 可以管理自己的费用；admin/owner 可以审批与管理预算；
  只有 owner 能修改 owner。
- 密钥轮换：`EXPENSIO_JWT_PUBLIC_KEYS` 支持逗号分隔的多个公钥，可先加新公钥
  再切私钥，最后移除旧公钥。私钥只从环境变量 / K8s Secret 读取，不落库、不入仓。
- 历史取舍：HS256 需要所有服务共享密钥，服务被攻陷即可签发任意租户令牌。
  M6–M8 使用了该方案，M9 完成 EdDSA/JWKS 迁移；理由与迁移步骤见 ADR-0008。
- 成员邀请：owner/admin 可创建邀请，只有 owner 能邀请 owner。令牌是
  32 字节随机值，数据库只存 SHA-256 摘要；同一租户同一邮箱最多一条
  待接受邀请，再次邀请会撤销旧链接，默认 7 天过期。接受页与接受接口公开
  （网关 IP 限流），但**任何情况下都必须验证密码**：新邮箱设置密码，
  已有账号输入原密码。邀请邮件经 notification 的内部接口进入发件箱，
  入队失败不回滚邀请，管理员仍可复制一次性链接。详见 ADR-0010。

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

## 通知与邮件（M5）

notification 消费 `expensio.expense.events.v1` 与
`expensio.billing.events.v1` 两个主题（消费组 `notification-expense-v1` /
`notification-billing-v1`），把事件渲染成每个用户自己的站内通知，并在同一
事务内写 `app.email_outbox`。通知是派生数据：可以全部丢弃后由事件重建，
已读状态不回写、不产生事件，也不参与任何业务判定。

- **幂等**：`processed_events` 主键 `(tenant_id, consumer, event_id)`，
  通知再加 `(tenant_id, user_id, event_id, kind)` 唯一约束；重复投递不会双写。
- **收件人**：消费事件时没有用户 JWT，notification 用签名服务令牌调用
  identity 的 `/api/v1/internal/tenant-contacts`；租户只存在于签名载荷里，
  不接受 query/path/body，网关也不转发该前缀。
- **邮件**：消费事务只写发件箱，不碰网络；后台 worker 通过
  `SECURITY DEFINER` 的 `app.claim_emails()`（`FOR UPDATE SKIP LOCKED`）
  跨租户领取一批，运行时账号不需要 `BYPASSRLS`。失败按 1/2/4/8/16 分钟
  退避，最多 5 次，之后保留 `failed` 供人工排查。
- **隔离**：`notifications` 与 `processed_events` 是 `ENABLE + FORCE` RLS；
  `email_outbox` 只 `ENABLE`，让表所有者的领取函数能跨租户，同时运行时
  账号仍被租户策略约束。

详见 ADR-0006。

## 容器化与部署（M6）

六个 Go 服务共用一个多阶段 `Dockerfile`，用 `SERVICE` 构建参数选择入口；
运行层只有静态二进制与 CA 证书，非 root UID 10001，可直接启用
`readOnlyRootFilesystem`。管理台用 `Dockerfile.web` 三段构建（Node → Go →
alpine），由 `internal/web` 提供静态文件与 `/api` 反代，详见 ADR-0007。
迁移镜像用 `Dockerfile.migrate`（golang-migrate CLI + `migrations/`）打包，
供 Helm 迁移 Job 使用；Compose 直接把 `migrations/` 挂进上游 `migrate` 镜像，
省去本地构建一次。

- **Compose**：`app` profile 编排六个服务、管理台、迁移 Job 与建主题 Job；
  应用容器只读根文件系统、丢弃全部 capability、UID 10001，
  只有 gateway 和 web 发布到宿主，且绑定 `127.0.0.1`。
- **Helm**：`deploy/helm/expensio` 只部署无状态服务，PostgreSQL / Redis /
  Kafka 与 Secret 由平台提供；迁移和主题创建是 `pre-install,pre-upgrade`
  Hook。Pod 使用非 root、只读根文件系统、默认拒绝入站。
- **CI**：GitHub Actions 执行 gofmt/vet/build、强制真实依赖的集成测试、
  前端构建、Helm lint/template，并在 main 上推送服务镜像。

## 可观测性与运维

- 日志：zap JSON，敏感字段（token、密码、卡号）在 logger 层脱敏。
- 健康检查：`/healthz` 不碰下游；`/readyz` 并行检查并共享预算。
- 优雅关闭：摘流量 → 排空 → Shutdown → 强制关闭；WSL2 下已用 SIGTERM 实测。
- 遥测：六个服务与管理台统一用 OTel SDK，经 OTLP/gRPC 发往 Collector。
  Collector 把 trace 转 Tempo，把指标通过 Prometheus exporter 暴露给
  Prometheus；Grafana 同时查询两者。HTTP 用 W3C `traceparent`，Kafka 用
  记录头传播；outbox 边界会断开 trace（上下文未落库），详见 ADR-0009。
- 指标：HTTP 请求数/延迟/并发、Kafka 生产与消费、outbox 领取与投递、
  邮件发送、网关限流决策、pgx 连接池；路由使用低基数模板，健康检查不计数。
  HTTP 与 Kafka 延迟直方图都显式设置了亚秒级边界。
- 告警：Prometheus 8 条规则（服务缺失、5xx、P95、Kafka、outbox、邮件、
  限流器 fail-open、连接池），本地规则与看板随 compose 一起提供。
- 压测：`load/k6/api.js` 对已启动的整栈跑真实业务循环，阈值与实测结果
  记录在 `docs/PROGRESS.md`。
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
- notification 另有独立集成用例：真实 Kafka 重复投递只落一行、契约错误进 DLQ、
  identity 暂不可用时退避重试且不提交位点、worker 跨租户领取与 5 次上限。
  端到端联调再覆盖 gateway → expense → Kafka → notification → Mailpit 全链路。

## 里程碑

| 里程碑 | 内容 | 状态 |
| --- | --- | --- |
| M0 | 骨架、配置、日志、健康检查、编排、迁移基线 | 完成 |
| M1 | identity、gateway、RLS、集成测试 | 完成 |
| M2 | expense、审批状态机、预算预占 | 完成 |
| M3 | Kafka、outbox、报表读模型 | 完成 |
| M4 | billing、订阅与支付回调幂等 | 完成 |
| M5 | 前端、通知 | 完成 |
| M6 | Dockerfile、K8s、Helm、GitHub Actions | 已完成（本地整栈验收通过；CI 已实测全绿，集群 apply 待验证） |
| M7 | 指标、trace、压测与告警 | 已完成（本地整栈验收通过；生产采样、集群采集待验证） |
| M8 | 前端包体优化、浏览器级 E2E | 已完成（风险 34/36 关闭） |
| M9 | 令牌签名迁移到 EdDSA/JWKS | 已完成（风险 17 关闭；HS256 路径已在 M12 删除） |
| M10 | 成员邀请与一次性接受链接 | 已完成（风险 35 关闭） |
| M11 | SMTP STARTTLS、证书校验与认证实测 | 已完成（风险 33 协议层关闭；供应商接入待做） |
| M12 | 删除 HS256 兼容路径与共享密钥 | 已完成（只接受 EdDSA，配置面已收敛） |

## ADR

- [ADR-0001：共享 schema + RLS 多租户隔离](adr/0001-multi-tenancy-and-rls.md)
- [ADR-0002：持久化与依赖装配方式](adr/0002-persistence-and-wiring.md)
- [ADR-0003：预算归属与预占生命周期](adr/0003-budget-ownership-and-reservation.md)
- [ADR-0004：事务内 outbox 与报表读模型](adr/0004-event-driven-read-model.md)
- [ADR-0005：订阅计费、出账与支付回调](adr/0005-billing-subscriptions-and-payment-callbacks.md)
- [ADR-0006：站内通知、邮件发件箱与服务间取数](adr/0006-notifications-and-email-outbox.md)
- [ADR-0007：用 Go 服务提供管理台静态资源与网关反代](adr/0007-web-serving.md)
- [ADR-0008：令牌签名方案（M6 选 HS256，M9 迁移到 EdDSA）](adr/0008-token-signing-in-m6.md)
- [ADR-0009：可观测性链路与压测验收](adr/0009-observability-and-load-testing.md)
- [ADR-0010：成员邀请与一次性接受链接](adr/0010-member-invitations.md)
