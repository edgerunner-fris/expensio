# 面试讲解手册

这份文档回答三个问题：这个项目是什么、为什么这样做、被追问时怎么答。
所有数字都来自仓库当前状态（`git ls-files`、测试输出、`docs/PROGRESS.md`），
没有估算值；未验证的部分明确写"未验证"，不包装成已完成。

## 一句话版本

Expensio 是一个多租户 B2B 费用管理平台：六个 Go 微服务 + Vue 3 管理台，
PostgreSQL 行级安全隔离租户，Kafka 事件驱动报表与通知，自带集成测试、
浏览器 E2E、Helm 部署与真实 k3d 集群验收。规模约 3.1 万行 Go、0.56 万行
前端 TypeScript/Vue，69 个测试文件、34 个迁移文件、14 份 ADR。

## 两分钟版本

业务主线是"提交 → 审批 → 报销"：员工建费用单并提交，预算在**提交时预占**，
审批通过转为实际支出，驳回或撤销时释放；管理员按金额分级审批；报销完成后
事实进入报表读模型。

技术上刻意做了几个不讨巧但必要的选择：

- **服务独占数据库 + FORCE RLS**。租户 ID 只从验签后的 JWT 取，运行账号
  `NOBYPASSRLS`。即使业务代码忘了写 `WHERE tenant_id = ?`，数据库也会返回
  空结果。集成测试里专门有"直连另一租户查不到数据"的回归。
- **事务内 outbox**。业务事务只写本地 outbox 表，提交后由投递器发 Kafka。
  不在事务内直接发消息——回滚后消息撤不回来。消费端手动提交位点、按
  `(tenant_id, consumer, event_id)` 幂等、永久错误进 DLQ。
- **金额只用 `int64` 最小货币单位**，数据库 `bigint`。预算检查与扣减是一条
  条件 UPDATE，不做"先读余额再写"。
- **令牌从 HS256 迁移到 EdDSA**。M6 用共享密钥跑通，M9 换成 Ed25519：
  identity 持私钥签、其余服务只挂公钥验签，共享密钥被彻底删除，
  见 ADR-0008。这是一次真实的重构，不是纸面设计。

交付层面，每个里程碑都有可复现的命令输出记录在 `docs/PROGRESS.md`；
`api/openapi.yaml` 与真实路由之间有双向契约测试，网关漏配前缀也会被测试抓住。

## 架构与请求路径

```text
浏览器 → web(:8086, 静态 + /api 反代) → gateway(:8080)
                                            │ EdDSA 验签、限流、请求 ID
        ┌──────────────┬──────────────┬─────┴────────┬──────────────┐
        ▼              ▼              ▼              ▼              ▼
   identity        expense         billing        report      notification
   identity_db     expense_db      billing_db     report_db   notification_db
                                     │              ▲              ▲
                                     └──── Kafka ───┴── outbox ─────┘
```

- 网关只做入口治理（验签、限流、路由），不做业务判断；每个业务服务**再次**
  验签，不假设网关一定验过。
- 服务间没有用户 JWT 的调用（notification 取收件人）使用独立的 HMAC 短时
  令牌，载荷里带 `tenant_id`，网关不转发 `/api/v1/internal/*`。
- 报表只读自己的 `app.expense_facts`，不跨库 JOIN、不回查 expense API。

## 关键取舍（面试官最爱追问的部分）

### 1. 为什么共享 schema + RLS，而不是每租户一个 schema/库？

每租户一库在几百个租户时迁移、连接池、备份都会爆炸；共享 schema 的风险是
漏写租户条件。RLS 把这条约束下沉到数据库，并用 `FORCE` 让表所有者也不能
绕过。代价是每个事务都要 `SET LOCAL app.tenant_id`，连接池复用时要确保
事务级设置不泄漏——这一点由 `internal/platform/db` 统一封装，业务代码
拿不到裸连接。

### 2. 为什么预算放在 expense 服务？

预算预占和费用单写入必须在同一个事务里。拆成独立服务就只能上分布式事务
或接受最终一致，而"预算超支"是业务不能容忍的。边界按事务一致性划，
不按名词划。对应 ADR-0003。

### 3. 为什么不用 sqlc / wire？

手写 pgx 查询 + `cmd/*/main.go` 显式装配，是为了让 RLS 上下文、事务边界和
依赖顺序在代码里直接可见。sqlc 会把 SQL 藏进生成代码，wire 会让启动路径
不直观。这是与初始技术栈清单的**已知偏离**，理由写在 ADR-0002，不是遗漏。

### 4. outbox 投递器为什么同步发送？

当前规模下同步发送足够：HTTP 请求不等待投递，投递器独立循环；失败重试，
积压有指标和告警。改成异步批量会增加延迟与顺序问题，收益要到更高吞吐才
显现。取舍记录在 ADR-0004。

### 5. 为什么报表用事件读模型，而不是直接查 expense 库？

服务独占数据库是硬约束，跨库 JOIN 被禁止。读模型让报表查询与写入解耦，
也让乱序事件有明确处理规则（只接受 `version >= 当前版本`）。代价是最终
一致：刚提交的单据可能延迟几百毫秒出现在报表里，这一点在文档里明说。

### 6. 令牌为什么最终选 EdDSA？

HS256 要求所有服务共享同一密钥，任何一个服务被攻陷就能签发任意租户的
令牌。EdDSA 让只有 identity 持私钥，其余服务只验签；公钥支持在线轮换
（先加新公钥、再切私钥、最后移除旧公钥），已在真实 Compose 栈完成四阶段
演练。M12 删除了 HS256 兼容路径，配置面不存在对称回退。

## 我踩过并修掉的真问题

面试时讲"修过什么"比讲"设计了什么"更可信。以下都是仓库里能查到的：

1. **网关漏配前缀导致 404**：`/api/v1/approval-policies` 在 expense 注册、
   OpenAPI 也有文档，但网关没有转发规则，走网关直接 404，直连服务却正常。
   修完后加了双向契约测试：OpenAPI 里的每个路径必须有网关规则，
   每条网关规则也必须对应真实端点，防止再次漂移。
2. **HS256 的共享密钥**：M6 用 HS256 跑通，但所有服务共享同一密钥，
   任何一个服务被攻陷就能签发任意租户令牌。M9 迁移到 EdDSA/JWKS，
   M12 删除 HS256 兼容路径与 `EXPENSIO_JWT_SECRET` 配置，不留对称回退。
   迁移分四阶段执行，步骤记录在 ADR-0008。
3. **outbox 事务边界**：不在事务内直接发 Kafka（回滚后消息撤不回来），
   事务内只写 outbox，提交后由投递器发送。见 ADR-0004。
4. **邀请链接的密码校验**：公开接受页如果只凭链接就入组，等于把账号交给
   转发邮件的人。现在新邮箱必须设置密码、已有账号必须输入原密码；
   令牌只存 SHA-256 摘要，默认 7 天过期，重复邀请撤销旧链接。
5. **测试不能假装通过**：CI 设置 `EXPENSIO_REQUIRE_DOCKER=1`，Docker 不可用
   时集成测试失败而不是 `t.Skip`；里程碑关闭前跑 `-race -count=5`。

## 高频追问 Q&A

**Q：怎么证明租户隔离真的有效？**
A：三层证据。(1) 迁移里 `ENABLE + FORCE ROW LEVEL SECURITY`，运行账号
`NOBYPASSRLS`；(2) 集成测试用两个租户的真实数据，断言互相查不到；
(3) 业务代码只从 JWT claims 建立 `tenantctx`，网关还会剥离伪造的
`X-Tenant-Id`。三条任意一条失守，另外两条仍在。

**Q：重复消息会不会写两条报表数据？**
A：不会。`processed_events` 主键 `(tenant_id, consumer, event_id)`，
通知再加 `(tenant_id, user_id, event_id, kind)` 唯一约束；集成测试用真实
Kafka 重复投递验证只落一行。

**Q：支付回调怎么防重放和伪造？**
A：HMAC 签名 + 时间戳窗口 + `event_id` 幂等；未知字段直接拒绝，避免
"多传一个字段改变语义"。回调没有 JWT，租户通过 invoice 映射解析，
不接受请求体里的 `tenant_id`。见 ADR-0005。

**Q：金额为什么不用 decimal？**
A：Go 标准库没有 decimal，引入第三方库只为一个类型不划算；最小货币单位
的 `int64` 在所有支持币种范围内不会溢出，且数据库 `bigint` 语义直接。
约束是"一个币种内不跨币种求和"，报表按币种分列。

**Q：为什么六个服务而不是模块化单体？**
A：这是简历项目的明确目标：练习服务边界、独立迁移、事件链路与部署。
同时用 RLS、outbox、契约测试把拆分的代价控制住。真实业务早期我会选
模块化单体，这一点可以在面试里直说——知道自己方案的代价，比假装没有代价
更可信。

**Q：可观测性做到什么程度？**
A：六个服务统一 OTel SDK，OTLP/gRPC 到 Collector；trace 进 Tempo、指标进
Prometheus、日志进 Loki，Grafana 里按 `trace_id` 双向跳转已实测。8 条告警
规则覆盖服务缺失、5xx、P95、Kafka、outbox、邮件、限流 fail-open、连接池。
已知断点：outbox 不落 trace 上下文，所以 `HTTP → outbox → 投递器` 不是一条
连续 trace，文档里明确写了，没有藏。

**Q：CI 都跑什么？**
A：gofmt / vet / build、强制真实依赖的集成测试（`-race`）、前端构建、
Helm lint + template + kubeconform、OpenAPI lint；main 分支额外用刚推送的
镜像跑整栈 Playwright E2E，以及 k3d 上的 `helm upgrade --install --wait`
和行为断言（Hook、Ingress、NetworkPolicy、RBAC、事件链路）。

**Q：哪些没做完？**
A：托管云依赖（RDS/Tair/ApsaraMQ/OSS）、Ingress TLS/cert-manager、
云上节点级扩容、多可用区、跨地域容灾未验证；PDB 驱逐、消费者再平衡与连接
排空已在阿里云 ACK 实测。生产 SMTP 供应商的配额与退信处理属于运维配置；
STARTTLS/证书校验已在真实第三方 SMTP 实测，但 DKIM/SPF 未做。
`golang.org/x/crypto` 有一条无修复版本的公告，代码未调用受影响路径，
记录在安全扫描结果里。这些都列在 README 的"已知限制"和 PROGRESS 风险清单中。

## 五分钟演示脚本

```bash
make app-up                      # 整栈：依赖、六服务、管理台、迁移、建主题
go run ./cmd/seed                # 需要 EXPENSIO_SEED_PASSWORD（≥10 字节）
```

`cmd/seed` 只通过公开 HTTP API 造数，不直连数据库：注册租户 → 邀请并提升
审批人 → 建预算 → 建四张费用单并驱动到四种终态 → 建订阅与账单。
重复执行只补齐缺失数据（第二次输出"本次新建 0"），可安全用于演示环境。

演示顺序建议：

1. <http://127.0.0.1:5173/> 用 `owner@demo-seed.test` 登录，看费用单四种状态；
2. 打开一张待审批单，用 `approver@demo-seed.test` 审批，观察预算从预占到支出；
3. 报表页按币种分列，说明数据来自 Kafka 读模型而不是直接查业务库；
4. 账单页用打印出的 `cmd/mockpay` 命令模拟支付，回到页面看订阅续期；
5. Grafana 里按 `trace_id` 在 Loki 与 Tempo 之间跳转；
6. `make e2e` 展示浏览器级回归。

不要把演示口令写进仓库或终端历史；seed 只打印长度，不打印口令。

## 代码地图

| 想看的点 | 位置 |
| --- | --- |
| 租户上下文与 RLS | `internal/platform/db`、`migrations/*/0001_*.sql` |
| 审批状态机 | `internal/expense/model.go`（`CanTransition`）、`workflow_repository.go` |
| 预算条件扣减 | `internal/expense/budget_repository.go` |
| outbox 与投递器 | `internal/platform/outbox` |
| Kafka 消费幂等 | `internal/platform/kafka`、`internal/report/consumer.go` |
| 令牌签发与验签 | `internal/platform/auth/jwt.go` |
| 网关路由表 | `internal/gateway/gateway.go` |
| 契约测试 | `internal/contract` |
| 造数 CLI | `cmd/seed` |
| 部署 | `deploy/helm/expensio`、`docs/DEPLOYMENT.md` |
| 验收记录 | `docs/PROGRESS.md` |
