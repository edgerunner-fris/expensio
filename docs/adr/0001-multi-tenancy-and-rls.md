# ADR-0001：共享 schema + PostgreSQL RLS 的多租户隔离

- 状态：已接受
- 日期：2026-10-26
- 相关：所有服务的数据库访问

## 背景

Expensio 是 B2B 多租户系统。每条业务数据都必须属于且只属于一个租户；
一次漏加 `WHERE tenant_id = ?` 就可能把 A 公司的报销单暴露给 B 公司。
仅在应用层"记得过滤"不能满足这个要求，因为漏写不会编译失败，也不会在
单元测试里必然暴露。

## 决策

1. **共享 schema，行级租户字段**：业务表都有 `tenant_id uuid NOT NULL`，
   不使用"每租户一个 schema / 一个库"。
2. **每个服务一个独立数据库**，库内一对角色：
   - `{service}_migrator`：库与表属主，只用于迁移；
   - `{service}_app`：运行时账号，显式 `NOBYPASSRLS`。
   应用绝不能用属主账号连库。
3. **RLS 强制生效**：租户表同时 `ENABLE` 与 `FORCE ROW LEVEL SECURITY`，
   策略为
   `tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid`，
   `WITH CHECK` 与 `USING` 相同。没有租户上下文时比较结果为 NULL，
   默认行为是"查不到任何行"，而不是"全量可见"。
4. **租户 ID 只来自 JWT**：HTTP 层禁止从 body/query/path 读取租户；
   `db.WithinTenant` 从 `tenantctx` 取租户并在事务内执行
   `set_config('app.tenant_id', $1, true)`。事务结束自动失效。
5. **登录/刷新等"租户建立之前"的查询**使用 `db.WithTx`；必须按租户 ID
   读取受 RLS 保护的表时使用 `db.WithinTenantID`，且该 ID 只能来自数据库中
   已可信的记录（如刷新令牌行）。
6. **测试必须用 `{service}_app` 连库**，否则 RLS 不生效，测出来的隔离是假的。

## 影响

- 所有仓储方法都接收调用方传入的 `pgx.Tx`，事务边界由 service 决定，
  避免仓储自己开事务绕过租户上下文。
- 跨租户写入会被 `WITH CHECK` 拒绝（SQLSTATE 42501），已有集成测试锁定。
- 数据库连接串、角色口令只从环境变量 / K8s Secret 读取。
- 报表类跨租户聚合必须走单独的、显式授权的路径，不能复用业务角色。

## 被否决的方案

1. **只靠应用层 WHERE 过滤**：漏写无编译期提示，风险不可接受。
2. **每租户一个 schema**：迁移数量随租户线性增长，连接池与运维复杂。
3. **每租户一个数据库**：成本与迁移编排成本过高，本地与 kind 环境不现实。
