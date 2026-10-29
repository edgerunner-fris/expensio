# 进度与验证记录

> 本文件只记录"跑过什么、看到什么"，以及尚未关闭的风险。
> 设计取舍写进 `docs/ARCHITECTURE.md` 与 `docs/adr/`（M2-1 补齐）。

## M0 完成情况

| 项 | 状态 | 说明 |
| --- | --- | --- |
| Go module 与依赖 | 完成 | `github.com/edgerunner-fris/expensio`，Go 1.27 |
| 统一错误模型 | 完成 | `internal/platform/apperr`，code/status/details/cause |
| 配置加载 | 完成 | viper，所有键注册默认值，聚合校验，敏感字段打码 |
| 结构化日志 | 完成 | zap JSON，敏感键脱敏，trace/tenant 字段随 context 注入（M1 起使用） |
| 健康探针 | 完成 | liveness 不碰下游；readiness 并行检查、共享 2s 预算 |
| HTTP 生命周期 | 完成 | 摘流量 → 排空 → Shutdown → 强关，且顺序不可逆 |
| 六个服务入口 | 完成 | `cmd/*/main.go` 只声明身份与端口，生命周期单点实现 |
| 依赖编排 | 完成 | `docker-compose.yml`，core/obs/mail 三 profile，端口只绑 127.0.0.1 |
| 数据库初始化 | 完成 | 5 库 × (migrator/app) 角色，app 角色 NOBYPASSRLS |
| 基线迁移 | 完成 | app schema、默认权限、库级 search_path |
| 冒烟脚本 | 完成 | 编译 → 起 6 个服务 → 探针 + 404 信封 → 退出 |

应用容器化（Dockerfile）按要求留在 M6，因此 `Makefile` 暂时没有 `up-all`——
不留一个注定失败的目标。

## 实际执行的验证

以下均为本机实际运行结果，不是"应该能过"。
其中"实际执行的验证"一节是 Windows 时期的历史记录，保留用于对照；
环境已迁移到 WSL2，最新结果见 `WSL2 环境迁移后的验证`。

```text
$ go build ./...                       # 通过
$ go test ./... -count=1               # 全绿（8 个包有测试）
$ go test ./internal/platform/server -count=40   # 40 次重复，0 失败，0.785s

$ make up
 Container expensio-postgres-1 Healthy
 Container expensio-redis-1 Healthy
 Container expensio-kafka-1 Healthy
 Container expensio-minio-1 Healthy
 Container expensio-prometheus-1 Healthy
 Container expensio-otel-collector-1 Healthy
 Container expensio-grafana-1 Healthy
 Container expensio-mailpit-1 Healthy

$ make migrate
 ==> identity      1/u baseline
 ==> expense       1/u baseline
 ==> billing       1/u baseline
 ==> report        1/u baseline
 ==> notification  1/u baseline
 迁移完成：up

$ make smoke
 OK   gateway :8080
 OK   identity :8081
 OK   expense :8082
 OK   billing :8083
 OK   report :8084
 OK   notification :8085
 冒烟通过：6/6
```

数据库侧另行确认：

```text
rolname              rolsuper  rolcreaterole  rolbypassrls
identity_app            f           f             f
identity_migrator       f           f             f
（其余四对角色相同）

identity_app 连接 identity_db：search_path = app, public，current_schema = app
schema_migrations 只存在于 app schema，五个库版本均为 1，dirty = false
```

## M1 完成情况（已完成）

| 批次 | 内容 | 状态 |
| --- | --- | --- |
| 1/6 | `000002_identity_core`：5 张表、触发器、RLS 策略 | 完成 |
| 2/6 | `internal/platform/db`：只暴露 `WithTx` / `WithinTenant` / `WithinTenantID`，事务级 `set_config` | 完成 |
| 3/6 | `internal/platform/auth`：argon2id、HS256 签发与校验、刷新令牌生成与摘要 | 完成 |
| 4/6 | identity 服务：注册 / 登录 / 刷新 / 登出 / `me` / 成员管理 / 审计日志 | 完成 |
| 5/6 | gateway：JWT 中间件、RBAC、转发、限流 | 完成 |
| 6/6 | 集成测试：RLS 隔离、令牌轮换与重放、RBAC 自动化回归 | 完成 |

### M1 实际执行的验证

服务以 `identity_app`（NOBYPASSRLS）连 `identity_db`，全部请求走真实 HTTP 端口 8081。

```text
$ GET  /readyz
{"status":"ok","service":"identity","version":"dev","checks":{"db":"ok"}}

$ POST /api/v1/auth/register   （acme / owner@acme.test）
201  {"code":"ok","data":{"access_token":"…","refresh_token":"…","profile":{…,"role":"owner"}}}

$ POST /api/v1/auth/login
200  role=owner；密码错误 → 401 auth.unauthorized，文案与"账号不存在"完全一致（耗时同样经过一次 argon2）

$ GET  /api/v1/me
200  {"user_id":"…","tenant_id":"…","tenant_slug":"acme","tenant_name":"Acme Inc","role":"owner"}
401  不带 token / 篡改签名

$ GET  /api/v1/tenants/current/members      owner → 200 total=2
$ GET  /api/v1/audit-logs                   owner → 200，动作序列 register → login → refresh → member.role_updated

刷新轮换与重放：
$ refresh(R0) → 新令牌 R1
$ refresh(R0) 再试一次 → 401（判定为轮换后重用）
$ refresh(R1)          → 401（整族已撤销，攻击者拿到的"新令牌"也失效）
库内：同一 family_id 的两行均 revoked_at 非空，审计多出 auth.refresh_replayed

RBAC（viewer 身份在 acme）：
GET /members → 403 auth.forbidden；GET /audit-logs → 403；GET /me → 200
owner PATCH 该成员 role=admin → 200；再以 admin 身份 GET /members → 200
admin PATCH owner → 403（只有 owner 能改 owner）
owner PATCH 自己 → 409 member.self_change
PATCH 不存在的用户 → 404；role=superuser → 400

租户隔离（HTTP 层，两个真实租户）：
acme token 看到 2 名成员，beta token 看到 1 名成员
acme 审计 11 条，beta 审计 2 条；两边的响应中都不含对方的任何一行
```

```text
$ go build ./... && go vet ./... && gofmt -l .     # 无输出

$ make smoke                                       # identity 已接真实数据库
 OK   gateway :8080
 OK   identity :8081
 OK   expense :8082
 OK   billing :8083
 OK   report :8084
 OK   notification :8085
 冒烟通过：6/6
```

验证结束后清空 `identity_db` 的业务数据（仅保留 schema 与迁移记录），
把固定数据留给后面的 seed 脚本，避免两套 fixture 打架。

### M1-5/6 gateway 实际执行的验证

单元测试用 `httptest.NewServer(engine)` 承载真实 gin 引擎（原因见问题 10），
下游是记录请求头的假服务；限流器用可编程的假实现，单独验证键的维度。

```text
$ go test ./internal/gateway/ -count=1
ok  github.com/edgerunner-fris/expensio/internal/gateway  0.202s

覆盖：公开路由放行 / 受保护路由无令牌 401 且不触达下游 / 有效令牌转发并保留查询串 /
剥掉伪造的 X-Tenant-Id、X-User-Id、X-User-Role / 未知路径 404 信封 /
最长前缀优先 / 429 + Retry-After / 租户键 vs IP 键 / 限流器故障放行 /
下游不可达 502 / 装配错误在启动期拒绝 / 只对"尚未送达"的请求重试
```

联调：identity 监听 8081，gateway 监听 8080，请求全部从网关进入。

```text
$ POST :8080/api/v1/auth/register
201   （响应中 tenant_id=6a5d92fe-…）

$ GET  :8080/api/v1/me                       200  slug 与注册一致
$ GET  :8080/api/v1/me   带伪造 X-Tenant-Id  200  tenant_id 未被替换
$ GET  :8080/api/v1/me   不带令牌            401  auth.unauthorized
$ GET  :8080/api/v1/expenses（expense 未起） 502  common.upstream_unavailable
$ GET  :8080/api/v1/nope                     404  common.not_found

认证接口按 IP 限流（burst 10）：
连续 15 次错误登录 → 前 10 次 400/401，之后 429，Retry-After 为正整数

业务接口按租户限流（临时把 api 限流压到 6/min、burst 1）：
同一租户第 1 次 200，第 2/3 次 429，Retry-After: 10
另一个租户同时请求 → 200（桶按租户隔离，互不牵连）

$ make smoke
冒烟通过：6/6
```

### M1-6/6 自动化集成测试实际执行的验证

测试用 testcontainers 起真实的 PostgreSQL 16 与 Redis 7，复用仓库里的
`deploy/local/postgres/init/01-bootstrap.sh` 与 `migrations/` 做建库和迁移——
测试里的 DDL 与生产是同一份。应用代码一律用 `{service}_app`（NOBYPASSRLS）连库。
本地未设置 `EXPENSIO_REQUIRE_DOCKER` 且 Docker 不可用时整组用例 `t.Skip`，
不影响纯单元测试；CI 固定设置该变量，跳过会变成失败。

```text
$ go test ./internal/identity/ -count=1 -v
--- PASS: TestRLSIsEnforced
    --- PASS: 应用角色既不超级用户也不绕过RLS
    --- PASS: 没有租户上下文时查不到任何成员
    --- PASS: 各租户只看到自己的成员
    --- PASS: 跨租户按ID读取返回空
    --- PASS: 跨租户写入被策略拒绝            （SQLSTATE 42501）
    --- PASS: 审计日志同样按租户隔离
    --- PASS: 事务结束后租户上下文不残留
    --- PASS: 缺少租户上下文时WithinTenant直接报错
    --- PASS: 空租户ID被拒绝
--- PASS: TestRefreshRotationAndReplay
--- PASS: TestConcurrentRefreshOnlyOneWins     （8 并发 → 恰好 1 次成功，整族撤销）
--- PASS: TestLogoutRevokesSession
--- PASS: TestRefreshRejectsUnknownAndEmptyToken
--- PASS: TestTenantMembershipIsRequired
--- PASS: TestRBACMatrix
    --- PASS: me 对所有角色开放
    --- PASS: 管理视图 /api/v1/tenants/current/members
    --- PASS: 管理视图 /api/v1/audit-logs
    --- PASS: 无令牌与坏令牌一律401
    --- PASS: 管理员可以调整非owner成员角色
    --- PASS: 只有owner能修改owner
    --- PASS: 不能修改自己的角色
    --- PASS: 不存在成员返回404
    --- PASS: 非法角色返回400
    --- PASS: 普通成员不能改别人的角色
    --- PASS: 成员列表不跨租户
    --- PASS: 审计记录包含角色变更且不含敏感信息

$ go test ./internal/gateway/ -count=1 -run TestRedisLimiter
--- PASS: TestRedisLimiterAgainstRealRedis
    --- PASS: 令牌耗尽后拒绝并给出等待时间
    --- PASS: 按速率补充令牌
    --- PASS: 不同键互不影响
    --- PASS: 并发请求不会超发              （20 并发、桶容量 5 → 恰好 5 次放行）
    --- PASS: 键会过期，不会永久占内存
    --- PASS: 非法参数不制造故障

$ go test ./internal/platform/httpx/middleware/ -count=1
--- PASS: TestAuthPopulatesTenantContext
--- PASS: TestAuthRejectsBadCredentials     （7 种非法输入）
--- PASS: TestRequireRoleMatrix              （4 种角色 × 管理端点）
--- PASS: TestBearerTokenParsing

$ go test ./... -count=1
ok  gateway 1.900s / identity 6.710s / middleware 0.702s / 其余平台包全部 ok
```

没有 Docker 时会跳过集成用例；CI 必须设置 `EXPENSIO_REQUIRE_DOCKER=1`，
此时 Docker 不可用即失败（见风险 23）。

### WSL2 环境迁移后的验证

环境：Ubuntu 24.04（WSL2）、Go 1.27.0、Docker 29.5.3，
项目路径 `/home/edgerunner/projects/expensio`。2026-10-26 实测：

```text
$ go test ./... -race -count=5 -timeout=15m
ok   github.com/edgerunner-fris/expensio/internal/gateway   4.109s
ok   github.com/edgerunner-fris/expensio/internal/identity  31.461s
ok   apperr / bootstrap / config / health / httpx / middleware
     / logger / server / tenantctx                         全部 ok
```

race detector 反向验证：临时加入一个存在数据竞争的用例，`-race` 稳定报出
`DATA RACE`；删除临时用例后恢复全绿。说明竞态检测确实生效，而不是"没报错"。

SIGTERM 优雅关闭（Windows 上无法验证，风险 16）：

```text
$ /tmp/expensio-billing &
$ curl -sf http://127.0.0.1:8083/healthz          # 200
$ kill -TERM $!
{"msg":"http server draining","service":"billing"}
{"msg":"http server stopped","service":"billing"}
{"msg":"service stopped","service":"billing"}
exit=0
```

Docker 门禁的两条分支（风险 23）：

```text
$ PATH=/tmp/fakedocker:$PATH EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/gateway -run ...
--- FAIL: TestRedisLimiterAgainstRealRedis   集成测试要求 Docker 可用，但 docker info 失败

$ PATH=/tmp/fakedocker:$PATH go test ./internal/gateway -run ...
--- SKIP: TestRedisLimiterAgainstRealRedis   Docker 不可用，跳过依赖容器的集成测试
```

工具链：

```text
$ make help          # 目标列表正常（此前在 Linux 上会因 dash + pipefail 直接失败）
$ make preflight
  OK go        go version go1.27.0 linux/amd64
  OK git       git version 2.43.0
  OK make      GNU Make 4.3
  OK docker    Docker version 29.5.3
  OK compose   5.1.4
```

### M2-1 / M2-2 实际执行的验证（2026-10-26）

迁移 `migrations/expense/000002_expense_core.{up,down}.sql` 在真实 PostgreSQL 16
容器中执行，应用角色为 `expense_app`（NOBYPASSRLS）：

```text
$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/expense -count=1 -v
--- PASS: TestMigrationDownAndUp          （down 删表 → up 重建，RLS 与种子仍在）
--- PASS: TestSchemaApplies               （btree_gist、三表 FORCE RLS、排斥约束、5 条状态边）
--- PASS: TestRLSIsolation                （跨租户 INSERT 42501；无上下文 0 行；跨租户 UPDATE 0 行）
--- PASS: TestStatusTransitionGuard       （非 draft 插入、跳步、提交后改金额、缺审批人、终态出边 → 23514）
--- PASS: TestSoftDeleteRules             （草稿可软删、不可恢复；已提交不可软删）
--- PASS: TestApprovalStepsAreAppendOnly  （UPDATE/DELETE 42501；白名单写操作 42501；动作/理由校验 23514）
--- PASS: TestBudgetPeriodOverlap         （重叠 23P01；相邻期间/不同类别允许；软删后可重建）
--- PASS: TestBudgetGuards                （超占 23514；有占用不可删；软删不可恢复）
--- PASS: TestBudgetFKIsTenantScoped      （跨租户预算引用 23503）
ok  github.com/edgerunner-fris/expensio/internal/expense  5.116s
```

随后跑全量竞态回归：

```text
$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=1 -timeout=15m
ok  internal/expense   8.737s
ok  internal/gateway   4.373s
ok  internal/identity  13.226s
其余平台包全部 ok；gofmt、go vet 均无输出
```

### M2-3 / M2-4 实际执行的验证（2026-10-26）

```text
$ go test ./internal/platform/money -race -count=5
ok  internal/platform/money  1.020s
  ParseCurrency：归一化大小写/空白，拒绝 ""、"US"、"USDD"、"U1D"、"人民币"
  Validate：1..1e12 边界；0、负数、MinInt64、超上限均被拒
  Format：USD/JPY(0 位)/KWD(3 位)/负数/MinInt64 不溢出；不引入浮点

$ go test ./internal/expense -run 'Test(StatusValid|Transitions|ActionValid|ParseCategory|Authorize|CanView|ResolveScope|BudgetAvailable)' -race -count=5
ok  internal/expense  1.073s
  状态机：6x6 全矩阵比对 5 条边，与迁移种子数据一致
  禁止自审：owner/admin 审批自己提交的单 → 403 expense.self_approval_forbidden
  角色：viewer 只读；member 只能提交/撤销自己的单；审批与报销仅 admin/owner
  可见性：admin/owner 全部，其余仅本人；已软删一律 404/不可见
  scope=all 对非管理岗返回 403，不静默降级

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/expense -race -count=1
ok  internal/expense  7.417s（含 M2-1/M2-2 集成用例）
```

### M2-5 实际执行的验证（2026-10-26）

```text
$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/expense -run 'Test(CreateAndGetDraft|DraftPermission|UpdateDraftOptimisticLock|DeleteDraft|ListDrafts|DraftValidation|DraftBudgetMustBelongToTenant)' -race -count=1 -v
--- PASS: TestCreateAndGetDraft            （tenant/submitter 来自上下文；小写币种归一化；空白裁剪）
--- PASS: TestDraftPermission             （viewer 禁写；他人草稿 404；管理员可读不可改；跨租户不可见）
--- PASS: TestUpdateDraftOptimisticLock    （v1→v2；旧版本 409 expense.version_conflict；提交后 409；不存在 404）
--- PASS: TestDeleteDraft                  （版本不符 409；软删后 404 且行仍在）
--- PASS: TestListDrafts                   （mine/all、scope=all 对成员 403、状态过滤、分页、租户隔离）
--- PASS: TestDraftValidation              （金额/币种/类别/时间/长度边界，200 汉字按字符计数通过）
--- PASS: TestDraftBudgetMustBelongToTenant（跨租户预算 400；同租户可通过；草稿可清空预算）
ok  internal/expense  6.559s
```

### M2-6 实际执行的验证（2026-10-26）

`cmd/expense` 已从空壳改为真实装配：`NeedsDB` + `NeedsJWT`，启动即建立连接池、
注册 `/healthz` 的 db 检查、挂载 `/api/v1/expenses` 路由。

```text
$ gofmt -l . && go vet ./... && go build ./...
（无输出）
$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/expense -run TestHTTP -race -count=1 -v
--- PASS: TestHTTPExpenseLifecycle     （201→列表→读取→更新 v2→旧版本 409→删除 204→再读 404）
--- PASS: TestHTTPAuthAndVisibility    （无令牌/伪造令牌 401；viewer 403；他人单 404；管理员可见；scope=all 403/200）
--- PASS: TestHTTPBadRequests          （非法 ID、非对象 JSON、缺 version、零金额、非法状态、删除缺 version）
ok  internal/expense  6.700s
```

### M2-7 实际执行的验证（2026-10-26）

新增迁移 `000003_budget_version`：预算增加 `version` 列，触发器在任何 UPDATE 时自增，
用于并发编辑的乐观锁；down 会先恢复 000002 的函数体再删列。
`TestMigrationDownAndUp` 相应改为回滚 2 步（000003 + 000002）。

```text
$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/expense -run 'TestBudget|TestResolveBudget|TestMigrationDownAndUp' -race -count=1 -v
--- PASS: TestBudgetCRUDAndRoles                  （成员建预算 403；管理员建/改/删；旧版本 409；成员只读）
--- PASS: TestBudgetOverlapAndDeleteThenRecreate   （重叠 409 budget.period_overlap；相邻/异类/异币种允许；软删后重建）
--- PASS: TestBudgetCommittedMoneyProtection      （金额低于 reserved+spent 409；仍有占用不可删；清空后可删）
--- PASS: TestBudgetPeriodChangeBlockedWhenReferenced（已被费用引用时禁止改期间；改名称仍可）
--- PASS: TestResolveBudget                       （无预算租户允许 NULL；自动/显式匹配；类别/日期不符 409；跨租户预算不可见）
--- PASS: TestMigrationDownAndUp                  （回滚 2 步后表重建、RLS 与种子仍在）
--- PASS: TestBudgetHTTP                          （201→读→PUT v2→旧版本 409→成员删 403→管理员删 204→404）
--- PASS: TestBudgetHTTPBadRequests               （非法 ID、日期格式、期间倒置、零金额、缺 version、非对象体）
ok  internal/expense  6.823s（全包）
```

预算写操作权限：owner/admin；成员与 viewer 只读。期间按 UTC 日期解释，
避免同一张单因服务器时区不同匹配到不同预算。

### M2-8 实际执行的验证（2026-10-26）

审批端点：`POST /api/v1/expenses/:id/{submit,approve,reject,cancel,reimburse}`，
请求体仅有可选 `comment`；目标状态由动作决定，客户端不能直接写 `status`。
每个动作在一个事务内完成"锁单 → 鉴权 → 预算变更 → 状态变更 → 追加轨迹"：

| 动作 | 预算操作 | 状态 |
| --- | --- | --- |
| submit | 匹配预算后条件预占 `reserved += amount` | draft → submitted |
| approve | `reserved → spent`（同一条 UPDATE） | submitted → approved |
| reject | `reserved -= amount` | submitted → rejected |
| cancel | `reserved -= amount` | submitted → cancelled |
| reimburse | 不改变预算 | approved → reimbursed |

条件更新未命中（额度不足/预占不足）返回 409 `expense.budget_unavailable`；
状态机冲突返回 409 `expense.state_conflict`；自审返回 403 `expense.self_approval_forbidden`；
驳回不写理由返回 400。预算变更与状态变更同事务，任一步失败整体回滚。

```text
$ gofmt -l . && go vet ./... && go build ./...
（无输出）
$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/expense -run 'TestWorkflow|TestConcurrentSubmit|TestHTTPWorkflow' -race -count=1 -v
--- PASS: TestHTTPWorkflow                       （空体提交 200；重复提交 409；驳回无理由 400；审批/报销 200；owner 自审 403）
--- PASS: TestHTTPWorkflowBadRequests            （非法 ID、非对象 JSON、comment 类型错误 → 400）
--- PASS: TestWorkflowSubmitApproveReimburse     （submit 预占 12345；approve 后 reserved=0/spent=12345；reimburse 不改预算；轨迹 3 条）
--- PASS: TestWorkflowWithoutBudgets             （从未配置预算的租户：budget_id 为 NULL，可提交/审批）
--- PASS: TestWorkflowBudgetUnavailable          （预算 10000 提交 12345 → 409，单据仍为 draft v1，轨迹为空）
--- PASS: TestWorkflowBudgetNotMatched           （租户有 meals 预算时提交 travel → 409；显式指定不匹配预算同样 409）
--- PASS: TestWorkflowSelfApprovalForbidden      （owner 审批自己的单 → 403，状态与预占均不变）
--- PASS: TestWorkflowRejectAndCancelReleaseBudget（驳回/撤销释放预占；驳回理由必填且 ≤1000 字符；撤销不写审批人）
--- PASS: TestWorkflowInvalidTransitions         （越级审批/重复提交/提前报销/撤销已批准 → 409；被降级 viewer 403）
--- PASS: TestConcurrentSubmitRespectsBudget     （两份草稿争同额度：恰好 1 个成功、1 个 409，预占不超额度，仅 1 条轨迹）
ok  internal/expense  5.200s（定向）；全包 -race 7.020s
```

并发用例依赖 `UPDATE ... WHERE reserved + spent + amount <= amount` 的行锁语义：
两个事务先后锁同一预算行，后到者在 READ COMMITTED 下重新求值 WHERE，命中 0 行即失败。

### M2-9 实际执行的验证（2026-10-26）

```text
$ gofmt -l . && go vet ./... && go build ./...
（无输出）

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=1 -timeout 15m
ok  internal/expense   7.492s
ok  internal/gateway   3.756s
ok  internal/identity  10.590s
ok  internal/platform/*（全部通过）

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -count=5 -timeout 15m
ok  internal/expense   10.391s
ok  internal/gateway   3.647s
ok  internal/identity  13.856s
ok  internal/platform/*（全部通过）
```

网关联调：真实 PostgreSQL 16 + Redis 7；identity :8081、expense :8082、gateway :8080，
请求全部经过网关。联调前迁移版本为 identity=2、expense=3、其余=1。

```text
POST /auth/register                 201
GET  /me                            200
POST /budgets                       201
POST /expenses                      201
POST /expenses/:id/submit           200 status=submitted budget=206d1523-…
GET  /budgets/:id                   200 reserved=12345 available=87655
POST /expenses/:id/approve (self)   403 expense.self_approval_forbidden
GET  /expenses/:id (other tenant)   404
GET  /budgets/:id (other tenant)    404
GET  /expenses?scope=all (B)        200 total=0
POST submit without token           401
POST submit twice                   409 expense.state_conflict
```

M2 完成。预算预占只在提交事务内发生，审批通过时 `reserved → spent`，
驳回/撤销释放预占，报销不改预算；五个动作均留下只追加轨迹。

## 风险清单

| # | 风险 | 状态 | 结论 / 处置 |
| --- | --- | --- | --- |
| 1 | MinIO 官方镜像下架（Docker Hub 仓库 404，quay 需鉴权，ghcr 403） | **已解决** | 改用 `bitnamilegacy/minio`，其中是真实 MinIO 构建（DEVELOPMENT.2025-05-24），并用 digest 固定：`sha256:451fe685...`。代价是依赖第三方命名空间的镜像，已在 compose 注释与 ADR 中说明 |
| 2 | 原记录称保留 google/wire，但项目实际从未引入 | **已解决（偏差已记录）** | 核对 `go.mod` 与源码：没有 `google/wire`，identity 与 expense 都是手写装配。原记录与事实不符，已删除该表述；决策与后续评估时点写入 ADR-0002 |
| 3 | Makefile 在不同平台使用错误的 shell | **已解决** | Windows 需要 bash.exe 全路径；Linux/WSL 的 `/bin/sh` 是 dash，不支持 `.SHELLFLAGS` 里的 `-o pipefail`，会导致所有目标启动即失败。现按平台固定 bash，并补 `scripts/preflight.sh`；`make help/preflight` 已在 WSL2 实测 |
| 4 | Go 模块代理在本机不可直连 | **已解决（WSL2 已固化）** | `~/.config/go/env` 写入 `GOPROXY=https://goproxy.cn,direct`、`GOTOOLCHAIN=local`；Go 1.27.0 位于 `/usr/local/go`，PATH 由 `~/.bashrc` 追加。换机器需重做 |
| 5 | Docker 拉取镜像受网络代理影响 | **已解决（WSL2 环境已复核）** | Docker Desktop WSL 集成可用，daemon 29.5.3；测试镜像已本地缓存。Ryuk 默认关闭，见风险 25。网络或代理变化时仍需复核 |
| 6 | 优雅关闭偶发耗时 5 秒 | **已解决** | 根因是 net/http 把"已连接但未发请求"的连接（StateNew）视作活跃直到满 5 秒（golang/go#22682）。已在 `server.Run` 中关闭 keep-alive、超时后强制 Close，并在配置校验里禁止 `shutdown_timeout <= 5s`。测试改用禁用 keep-alive 的专用 client，40 次重复稳定 |
| 7 | 本机无法运行 `-race` | **已解决** | WSL2 具备 cgo/gcc。2026-10-26 实测 `go test ./... -race -count=5 -timeout=15m` 全部通过（identity 31.461s、gateway 4.109s），并用临时竞争用例做过 `DATA RACE` 反向验证 |
| 8 | RLS 是否真的隔离了两个租户 | **已解决** | 两租户 + 真实 HTTP 验证：acme token 见 2 名成员 / beta token 见 1 名，审计同样按租户分割；无上下文时 `current_setting` 为 NULL，策略判定不通过，查到 0 行；跨租户 INSERT 被 `WITH CHECK` 拒绝。自动化回归在 M1-6/6 补齐 |
| 9 | Kafka 端到端（生产 → 消费 → 幂等） | **已解决** | M3-5 用真实 `apache/kafka:3.9.2` broker + 真实 PostgreSQL 验证：生产者同步发送 → 消费组消费 → 幂等去重 → 乱序旧版本忽略 → 永久错误进 DLQ，全部通过。分区键为 `tenant_id`，同一租户的事件保持顺序 |
| 10 | 前端依赖版本兼容（Vue/Pinia/Router/ECharts/TS） | **已解决** | 2026-10-27 实测 `npm ls --depth=0`：vue 3.5.44、vue-router 5.4.0、pinia 4.1.0、element-plus 2.14.7、echarts 6.1.0、axios 1.20.0、vite 8.3.5、vue-tsc 3.3.13、typescript 5.9.3。`npm run build`（`vue-tsc --noEmit && vite build`）退出码 0，3.59s 完成。首屏包体见风险 34 |
| 11 | 迁移中途失败会让库进入 dirty 状态 | **已解决（有操作手册）** | 本次踩到过：基线在事务内失败 → `schema_migrations` 标记 dirty。恢复方式：修好 SQL 后重建该库（本地），生产环境只允许向前修复并手工 `force`。`make migrate` 会明确报错，不会静默跳过 |
| 12 | 并发刷新导致同一令牌被轮换两次 | **已解决** | 轮换用条件更新 `UPDATE … WHERE id=$1 AND revoked_at IS NULL` + `RowsAffected()`；抢输的一方被判为重放并撤销整族。撤销放在独立事务里提交——放在同一事务里会被随后返回的错误一起回滚 |
| 13 | 登录接口可通过耗时分出"账号是否存在" | **已解决** | 用户或租户不存在时仍对预置哈希执行一次完整 argon2id 校验，返回文案与状态码也完全一致 |
| 14 | 角色大小写两套（DB 小写 / 上下文大写） | **已解决** | 统一为小写，`tenantctx.Role` 同时承载 `Valid()` 与 `AtLeast()`；避免每层各转一次、某条路径漏转 |
| 15 | gin 默认信任 `X-Forwarded-For`，审计 IP 可伪造 | **已解决（2026-10-27 补配置）** | `server.New` 默认 `SetTrustedProxies(nil)`；新增 `EXPENSIO_HTTP_TRUSTED_PROXIES`（逗号分隔 IP / CIDR，拒绝 0 位前缀），只有来自可信代理的连接才解析 `X-Forwarded-For`。单测覆盖"默认忽略 / 可信网段采信"两条路径 |
| 16 | Windows 上无法投递 SIGTERM 验证优雅关闭 | **已解决** | WSL2 实测 billing：SIGTERM 后日志依次出现 draining → stopped → service stopped，进程退出码 0。Windows `Stop-Process` 仍是 TerminateProcess，不作为验证手段 |
| 17 | access token 用 HS256，六个服务共享同一把密钥 | **已解决（M9 迁移，M12 删除兼容路径）** | M6–M8 期间接受该取舍（同信任域、无第三方验签方）。M9 完成迁移：identity 用 Ed25519 私钥签发，header 带 RFC 7638 指纹作 `kid`；其余五个服务只挂公钥，不再持有 HMAC 密钥。identity 暴露 `/.well-known/jwks.json`。实测：JWKS `kty=OKP/crv=Ed25519/alg=EdDSA/use=sig`、`x` 32 字节且无 `d`；登录令牌 `alg=EdDSA` 且带 `kid`，expense/identity 均 200；用旧 HS256 密钥伪造的令牌被网关 401。见 ADR-0008。**M12 收尾**：删除 `HS256Secret`、`jwt.secret`、`jwt.allow_hs256` 与 compose/Helm 中的共享密钥，解析器只允许 `EdDSA`，`TestHS256TokensAreRejected` 实测 HS256 令牌被拒。轮换：M14 在 Compose 真实栈完成四阶段在线演练（加新公钥 → 切私钥 → 移除旧公钥 → 回退），旧令牌在阶段 3 立即 401，见 M14；集群内按 `docs/DEPLOYMENT.md` 的同一顺序执行，尚未在集群演练 |
| 18 | 限流器故障时放行（fail-open） | **已解决（指标+告警已落地）** | 运行期 Redis 抖动时宁可短期超额，也不让全站 5xx；但启动时连不上 Redis 直接失败，避免以"无限流"状态上线。M7 新增 `expensio.gateway.rate_limit.decisions`，按 `scope`（tenant/ip）与 `outcome`（allowed/denied/error）计数，并配 `ExpensioRateLimiterFailOpen` 告警（error 速率 >0 持续 5 分钟）。压测实测 344 次决策全部 allowed、0 denied、0 error，说明指标在正常路径也确实在采集 |
| 19 | 网关重试会不会把写请求做两遍 | **已解决** | 只对 GET/HEAD/OPTIONS、无 body、且错误属于"连接未建立/未送达"（ECONNREFUSED、DNS 等）的请求重试一次；超时与取消一律不重试，因为请求可能已经到达下游 |
| 20 | 网关是单点，且到下游走集群内明文 HTTP | **已解决（M14 真集群实测）** | Helm 提供多副本（`replicas > 1` 时自动生成 PDB）、默认拒绝入站的 NetworkPolicy（只放行 ingress-controller 与同 release）与非 root/只读根文件系统。M6 评估后不引入 mTLS：服务同处一个集群信任域，边界由 NetworkPolicy 与 Service 账户收敛。M14 在 k3d（k3s v1.31.5，双节点）实际 `helm upgrade --install --wait`：gateway 2/2、web 2/2 就绪；7 个 Deployment 实际生效的安全上下文为 `runAsNonRoot=true seccomp=RuntimeDefault ro=true ape=false`、`automountServiceAccountToken=false`、SA `can-i list/get pods` 均为 no；NetworkPolicy 实测跨命名空间与未打 release 标签的 Pod 被拒、同 release 放行。托管云与多可用区仍未验证 |
| 21 | 下游收到的 `X-Request-ID` 是否可信 | **已解决** | 网关用中间件生成/透传的 ID 覆盖入站值再转发；`X-Forwarded-*` 由 `ReverseProxy.Rewrite` 依据真实连接重写，客户端无法伪造 |
| 22 | 登录时"账号存在但不在该租户"与"凭证错误"可区分 | **已解决** | 集成测试发现：密码正确但无成员关系时返回 403 + 明确文案，等于给了一份泄露密码的攻击者一个"密码正确"的判定点。已改为与其他登录失败一致的 401 `auth.unauthorized`；成员被停用仍返回 403（需要有效密码才可探测，属账号状态而非枚举） |
| 23 | 集成测试依赖 Docker，无 Docker 时会被跳过 | **已解决（策略已落地）** | `testenv` 改为显式 `docker info` 门禁：设置 `EXPENSIO_REQUIRE_DOCKER=1` 时不可用即失败，未设置才允许跳过；`make test-integration` 固定启用。失败/跳过两条分支均已实测。CI 实际执行仍见风险 26 |
| 24 | 测试容器启动过早，迁移时报 `unexpected EOF` | **已解决** | postgres 镜像首次启动会先起临时实例跑初始化脚本再重启。testcontainers v0.44 的 postgres 模块默认不等待，必须显式 `BasicWaitStrategies()`（等第二次 ready + 端口可连） |
| 25 | Ryuk 回收器需要额外拉取镜像，代理不通时集成测试直接失败 | **已解决（明示取舍）** | 测试改为显式 `Terminate`，默认设置 `TESTCONTAINERS_RYUK_DISABLED=true`；CI 想启用 Ryuk 可显式改回。代价：测试进程被强杀时可能残留容器 |
| 26 | 集成测试在 CI（Linux）尚未执行过 | **已解决（2026-10-27 实测）** | Actions run `40487635434`（commit `c441523`，workflow `CI`）全部 success：backend（`EXPENSIO_REQUIRE_DOCKER=1` + `-race -count=1`）、frontend、helm，以及 identity/expense/billing/report/notification/gateway 六个镜像、web 镜像、migrate 镜像共 11 个 job。M13：main 分支 CI 已增加整栈 Playwright job（拉取本次推送镜像，见最终验收） |
| 27 | `.env.example` 与 compose 含固定本地口令 | **已解决** | `make env`（`scripts/env.sh`）在全新环境为 DB app/migrator/postgres、JWT、billing webhook、内部 token、MinIO、Grafana 生成随机十六进制口令，`.env` 权限 600 且不入仓；已存在 `.env` 时只补齐缺失键，不覆盖现有值。compose 对 15 处应用密钥使用 `${VAR:?}` 强制注入，缺少即拒绝启动。`.env.example` 仍保留 `expensio_local_only` 作为演示值，但推荐路径不会使用它们；仅 127.0.0.1 可达的本地基础设施保留同名 fallback |
| 28 | 技术栈声称使用 sqlc 与 wire，代码中均不存在 | **已解决（决策已记录）** | ADR-0002 明确：M1/M2 使用手写 pgx 与手工装配，M6 前决定"正式引入"或"从对外口径删除"；在此之前不得声称已使用 |
| 29 | Kafka 生产者尚未对真实 broker 端到端验证 | **已解决（M3-5）** | `TestConsumerAppliesEventsAndDeduplicates` 用 `kafka.NewProducer` 对真实 broker 同步发送，消费者从真实主题读回并落库；`TestConsumerRoutesPermanentErrorsToDLQ` 验证 DLQ 写入与位点推进。M3-7 又在 `internal/e2e` 用真实费用 API → outbox → Kafka → report 消费者 → 报表 API 跑通完整链路 |
| 30 | 业务事件主题未在 broker 上预先创建 | **已解决（本地与集群实测）** | franz-go 先取元数据，broker 的 `auto.create.topics.enable=true` 不会对这类请求生效；缺主题时投递器会持续退避重试且不丢事件（M4-6 实测）。Compose `topics` Job 实测 `Exited (0)`，创建 expense/billing 主题与 DLQ。M14 在 k3d 集群上验证 Helm `pre-install` Hook 实际执行：四个主题 `--list` 可见，成功后 Job 被 `hook-succeeded` 清理（剩余 Job 数=0） |
| 31 | notification 消费组首次启动会从最早位点回放，可能补发历史邮件 | **已解决** | 消费端新增 `OffsetReset`：report 保持 `ResetEarliest`（回放重建读模型），notification 两个主题改为 `ResetLatest`——全新消费组只消费组成立之后的事件，已有位点的重启不受影响。`TestResetLatestSkipsRecordsWrittenBeforeGroupExists` 对真实 broker 验证：组成立前写入的 `old` 不会被读到，之后写入的 `new` 正常消费；`-race -count=5` 通过。代价：重建通知库时不再自动补历史通知，需要时用专用运维消费者回放 |
| 32 | 邮件投递是至少一次，崩溃窗口可能重复发送 | **已解决（明示取舍）** | 发送成功与标记 `sent` 之间进程崩溃，会在 5 分钟后被重新领取并重发同一封邮件；优先保证不丢。通知本身不是业务事实，重复邮件可接受；若要严格一次需要与 SMTP 供应商的幂等键，成本高于收益 |
| 33 | 真实 SMTP 供应商（STARTTLS / 认证）尚未验证 | **已解决（M11 协议层 + M14 真实供应商）** | 集成测试用假 Mailer 验证退避与状态迁移，端到端联调用 Mailpit 收信（明文 1025），`RequireTLS`、`PlainAuth` 与证书校验此前确实没有对真实 SMTP 服务执行过。M11 新增 `internal/notification/mailer_test.go`：进程内起真实 TCP socket 的最小 SMTP 服务（EHLO / STARTTLS / AUTH PLAIN / MAIL / RCPT / DATA / QUIT），自签 ECDSA 证书经 `MailerOptions.TLSConfig` 注入 RootCAs，实测三条路径——STARTTLS 后 AUTH PLAIN 成功并收到正文、`RequireTLS` 下服务器不公布 STARTTLS 即拒发、自签证书不在信任链时握手失败。`go test ./internal/notification -run TestSMTPMailer -race -count=5` → `ok 1.183s`。**M14 接入 Ethereal 真实供应商**：`smtp.ethereal.email:587`（STARTTLS + AUTH PLAIN）投递成员邀请，`imap.ethereal.email:993` 读回同主题邮件且正文含一次性链接，发件箱状态 `sent`，证书链 Let's Encrypt `CN=YE1`、`TLSv1.3`、`Verify return code: 0 (ok)`。生产供应商的配额、退信与 SPF/DKIM 属运维配置 |
| 34 | 前端首屏包体偏大 | **已解决（M8，首屏）** | 原构建 `index` 865.67 KB（gzip 276.04 KB）、`ReportsView` 510.52 KB（gzip 172.62 KB）。M8 改为逐组件注册 Element Plus（不再 `app.use(ElementPlus)`），并把 ECharts 拆成按需 `import()`。实测：`index` 469.16 KB（gzip 150.49 KB，原始体积 -45.8%、gzip -45.5%），`ReportsView` 5.07 KB，ECharts 独立为 `charts` 519.31 KB（gzip 175.39 KB），只在报表页真正有数据时下载。Vite 仍对 `charts` 报 >500 KB：这是按需 chunk，不影响首屏；若继续压缩需换更小的图表实现或拆分 zrender，暂不做 |
| 35 | 没有成员邀请/入组接口，新租户只有 owner | **已解决（M10）** | M1 的"成员管理"原先只包含列表与角色调整；新租户注册后只有 owner，而自审批被禁止，真实审批流程必须先由运维写库补成员。M10 补齐：owner/admin 创建邀请（只有 owner 能邀请 owner），令牌 32 字节随机值、库内只存 SHA-256 摘要，同一租户同一邮箱最多一条待接受邀请，重复邀请撤销旧链接，默认 7 天过期；接受时必须验证密码（新邮箱设置、已有账号输入原密码），公开页与接受接口经网关 IP 限流；邮件经 notification 发件箱投递，入队失败仍返回可复制的一次性链接。详见 ADR-0010 与 M10 章节。遗留：链接是 bearer 凭据（新邮箱仍可凭链接入组）、无域名限制/批量邀请/撤销通知 |
| 36 | 前端没有浏览器级端到端测试 | **已解决（M8）** | M8 新增 `e2e/`（独立 npm 工程，Playwright 1.64.1），在官方 `mcr.microsoft.com/playwright:v1.64.1-noble` 容器里对 Compose 网络的 `web:8086` 跑真实 Chromium。覆盖：未登录路由守卫跳登录页；注册 → 建费用单 → 提交 → 报表出图 → 篡改 access token 后重载自动刷新。连续三轮全绿（2 passed；2.9s、4.4s、8.7s）。用例必须轮询 summary API 等报表读模型消费完事件，不能假设提交后立即可查（ADR-0004）。E2E 还发现并修掉一个真实缺陷，见 M8 章节。M10 追加邀请流程用例（创建 → Mailpit 收信 → 公开页接受 → 一次性链接失效 → 新成员登录 → owner 复核成员列表），全量 `--repeat-each=3` 为 9 passed（23.3s）。M13：CI 的 `e2e` job 在 main 分支用推送镜像实测通过（见最终验收） |
| 37 | 网关按 IP 限流经反代后退化为"全站一个桶" | **已解决（配置化；集群须填值）** | 邀请 E2E 第三次重复运行时接受接口返回 429：浏览器 → web（Go 反代）→ gateway，网关的 `ClientIP` 永远是 web 容器/Pod IP，所有用户共享同一个认证限流桶，审计日志也只记到代理。修复：新增 `EXPENSIO_HTTP_TRUSTED_PROXIES`（默认空、校验、拒绝 `/0`），compose 设为 Docker 网段 `172.16.0.0/12` 并把本地 auth 限流放宽到 120/min、burst 60（Helm 生产值仍为 20/min、burst 10）；网关未配置时启动打 WARN。实测网关日志 `client_ip=172.18.0.18`（E2E 容器）而不是 web 的 `172.18.0.2`，E2E 9/9。**Helm `http.trustedProxies` 默认空，部署时必须填集群 Pod/Ingress 网段**，见 DEPLOYMENT.md |

## 本次因"真的跑起来"而发现并修掉的问题

1. **psql 变量在 `DO $$...$$` 内不替换** —— 初始化脚本报语法错误，改用 `\gexec` 逐条执行。
2. **`ALTER ROLE ... SET search_path` 需要 CREATEROLE** —— migrator 只是库属主，没有该权限；改为库级 `ALTER DATABASE ... SET search_path`。
3. **迁移记录表随 search_path 漂移**，同一库里出现 `public.schema_migrations` 与 `app.schema_migrations` 两份，版本号读不到。改为在初始化阶段就建好 `app` schema 并固定库级 search_path。
4. **日志里出现重复 JSON 键** —— logger 已附加 `service`/`version`，业务代码又加了一遍；zap 不去重，下游解析结果不可预期。已统一由 logger 附加。
5. **`/healthz` 的 version 字段返回的是服务名** —— `health.New` 的参数被当成版本号用了。改为同时返回 `service` 与 `version`。

M1 期间发现并修掉（多数靠"写下去就编译不过 / 跑一遍就露"）：

6. **`Session` 内嵌 `User` 会连带序列化 `PasswordHash`** —— 改为独立 `Profile` DTO，并给哈希加 `json:"-"`。
7. **角色大小写两套** —— 数据库 CHECK 是小写，`tenantctx` 写的是大写；若不统一，`AtLeast` 会永远判 false，RBAC 静默失效。
8. **重放撤销会被回滚** —— 最初在"检测到重放 → 撤销整族 → 返回 401"写在同一个事务里，`return err` 触发 Rollback，撤销根本没落库。改为先提交撤销、再返回错误（并发路径用"提交 + 标记"）。
9. **刷新请求的租户上下文无处可取** —— 刷新时只有令牌摘要，没有 JWT，而成员关系受 RLS 约束。新增 `db.WithinTenantID`，并在注释里写死使用边界：租户 ID 只能来自数据库中的可信记录。
10. **网关测试被 gin 的 `CloseNotify` 断言打崩** —— `httputil.ReverseProxy` 会检查底层 writer 是否支持断连通知，gin 的 `responseWriter.CloseNotify()` 是无保护的类型断言，而 `httptest.ResponseRecorder` 不实现该接口，于是 panic。生产环境用的是真实 `http.response`，不受影响；测试改为 `httptest.NewServer` 走真实连接，顺带覆盖了传输层。
11. **路由指向未配置的下游会让服务起不来** —— 测试第一次只给了 identity/expense 两个下游，`gateway.New` 直接拒绝装配。这正是有意设计：漏配下游要在启动时炸，而不是等第一个请求回来 502。代价是新增路由时必须同步改部署清单，已在 `DefaultRules` 注释中写明。

M1-6/6 期间发现并修掉：

12. **登录接口的账号枚举侧信道** —— 密码正确但用户不是该租户成员时返回 403「该账号不属于此租户」，攻击者用一份泄露的密码就能确认"密码正确、账号存在"。改为统一 401，失败原因只进日志；已用集成测试锁定。
13. **集成测试连接打在 postgres 的临时实例上** —— 镜像初始化阶段会先起一个临时实例跑脚本再重启，只等端口监听就会在迁移中途收到 `unexpected EOF`。改为显式等待第二次 ready。
14. **测试容器的回收依赖 Ryuk 镜像** —— 本机代理不通时，所有集成测试在真正开始前就失败。改为测试自己 `Terminate`，把 Ryuk 变成可选。

WSL2 环境迁移后发现：

15. **Linux 下 make 所有目标启动即失败** —— Ubuntu 的 `/bin/sh` 是 dash，`-o pipefail` 是 bash 扩展；Makefile 原先只在 Windows 分支设置 `SHELL`。改为按平台固定 bash，并补 Linux 版 `preflight.sh`。
16. **"Docker 不可用就跳过"与 CI 要求冲突** —— 原实现无条件跳过，CI 会以 0 退出。改为显式 `docker info` 门禁，`EXPENSIO_REQUIRE_DOCKER=1` 时失败；失败与跳过两条分支都实测过。

M5 期间发现并修掉：

17. **后台 context 在可失败构造之前创建** —— `go vet` 报 `lostcancel`：Mailer/Worker 构造失败直接返回时，`context.WithCancel` 的 cancel 永远不会被调用。把所有可失败的构造移到 `WithCancel` 之前，再统一启动循环。
18. **`resource_type` 为 NULL 时扫描失败** —— `email_outbox` 之外的资源字段可空，`*string` 扫描报 `cannot scan NULL`。查询改为 `COALESCE(resource_type, '')`，空串表示没有可跳转资源。
19. **共享测试库让 worker 领到上一条用例的邮件** —— 同一包里的多个用例共用一个 PostgreSQL，前一条留下的待发邮件被后一条领走（断言 2 封却得到 3 封）。改为在每条用例前用迁移连接 `TRUNCATE` 三张表；TRUNCATE 不受 RLS 影响。
20. **网关没有断言 notification 路由** —— 路由表已加 `/api/v1/notifications`，但测试仍把未知下游回落到 identity，路径写错也不会被发现。补用例：无令牌 401 且不触达下游，有令牌只转发给 notification。
21. **计费消费组从最早位点回放** —— 本地首次启动 notification 时，M4 的 3 条账单事件被重放成 3 封真实邮件。回放本身是重建派生数据的正确行为，但暴露了全新部署会补发历史邮件；已记录为风险 31，M6 决定部署时的位点策略。

## 当前开发环境（换机器需重做）

- WSL2 Ubuntu 24.04，项目位于 `/home/edgerunner/projects/expensio`；
  Windows 下 `C:\Users\74021\expensio` 仅作备份。
- Go 1.27.0 安装在 `/usr/local/go`，`~/.bashrc` 追加该目录到 PATH。
- `~/.config/go/env`：`GOPROXY=https://goproxy.cn,direct`、`GOTOOLCHAIN=local`。
- `CGO_ENABLED=1`（race detector 依赖 gcc）。
- Docker Desktop 启用 WSL 集成，当前用户在 `docker` 组；
  测试容器镜像 `postgres:16.15-alpine`、`redis:7.4.11-alpine`。
- `TESTCONTAINERS_RYUK_DISABLED` 默认由 testenv 设为 true，容器由 `Cleanup` 显式回收。
- 大文件（Go 安装包、Windows 构建产物）已移出仓库到 `~/expensio-artifacts/`，
  `.gitignore`/`.dockerignore` 同时加入 `.tmp/` 与 `*.tar.gz`。

## M2 完成情况（10/10）

M2 已全部完成，全量 `-race` 与 `-count=5` 通过，网关联调见上文。

| 批次 | 内容 | 状态 |
| --- | --- | --- |
| M2-0 | Linux Makefile、Docker 门禁、仓库清理、环境记录 | 完成 |
| M2-1 | ADR-0001/0002/0003、ARCHITECTURE、expense 核心迁移 | 完成 |
| M2-2 | Schema 集成测试（RLS / 触发器 / 只追加 / 重叠约束 / down-up） | 完成 |
| M2-3 | `platform/money`：int64 最小单位、币种与金额校验 | 完成 |
| M2-4 | expense 领域模型：状态机、角色规则、禁止自审 | 完成 |
| M2-5 | 费用仓储与草稿 CRUD | 完成 |
| M2-6 | 费用 HTTP 层与 `cmd/expense` 装配 | 完成 |
| M2-7 | 预算仓储/服务/HTTP 与匹配规则 | 完成 |
| M2-8 | 审批工作流与同事务预占/释放/结转 | 完成 |
| M2-9 | 全量回归、网关联调、PROGRESS 收尾 | 完成 |

M2 明确不做 `expense_items`（明细行）与 Kafka/outbox；预算扣减时点为审批通过
（`reserved → spent`），报销不再改变预算数字。详见 ADR-0003。

## M3 完成情况（7/7）

设计已锁定在 ADR-0004：事务内 outbox、事务外投递、全量快照 + `aggregate_version`、
`tenant_id` 作为分区键、消费端按事件 ID 幂等。

| 批次 | 内容 | 状态 |
| --- | --- | --- |
| M3-1 | ADR-0004、事件契约、outbox 迁移与仓储 | 完成 |
| M3-2 | outbox 投递器 + Kafka 生产者（重试/退避；事务外发送） | 完成 |
| M3-3 | expense 五个动作同事务写 outbox + 回滚不发消息测试 | 完成 |
| M3-4 | report 迁移：幂等表、读模型、RLS | 完成 |
| M3-5 | report 消费者 + 幂等/乱序/重复投递集成测试（真实 Kafka） | 完成 |
| M3-6 | 报表查询 API + 网关路由 + HTTP 测试 | 完成 |
| M3-7 | 全量回归、跨服务联调、PROGRESS 收尾 | 完成 |

### M3-1 实际执行的验证（2026-10-26）

```text
$ gofmt -l .                       # 无输出
gofmt_exit=0
$ go vet ./...
vet_exit=0
$ go build ./...
build_exit=0

$ go test ./internal/platform/events/... -count=1
ok  github.com/edgerunner-fris/expensio/internal/platform/events  0.003s

$ go test ./internal/platform/outbox/... -count=1 -v
--- PASS: TestInsertAndClaim (3.31s)
--- PASS: TestAppRoleCannotDeleteOutboxRows (0.02s)
--- PASS: TestLeaseExpiresSoCrashedDispatcherDoesNotStrandEvents (0.23s)
--- PASS: TestMarkFailedRecordsRetry (0.02s)
--- PASS: TestConcurrentClaimersDoNotBlockOrDuplicate (0.03s)
--- PASS: TestInsertIsPartOfCallerTransaction (0.02s)
--- PASS: TestInsertRejectsInvalidEnvelope (0.01s)
--- PASS: TestClaimValidatesArguments (0.01s)
ok  github.com/edgerunner-fris/expensio/internal/platform/outbox  4.301s

$ go test ./internal/expense/ -run TestMigrationDownAndUp -count=1 -v
--- PASS: TestMigrationDownAndUp (3.14s)
ok  github.com/edgerunner-fris/expensio/internal/expense  3.804s
```

本轮验证到的事实：

- 领取使用 `FOR UPDATE SKIP LOCKED`：持有事务与行锁的投递器不会阻塞第二个投递器，
  第二个只会拿到剩余事件（`TestConcurrentClaimersDoNotBlockOrDuplicate`）。
- 租约到期后事件可以被重新领取；进程崩溃（未走 `MarkFailed`）不会把事件永久卡死。
- 失败重试会累加 `attempts`，超长错误截断到 480 个字符，未到 `available_at` 不可领取。
- 业务事务返回错误时 outbox 行一起回滚——这是 M3 最重要的不变量。
- `expense_app` 对 outbox 的 `DELETE` 被拒绝：投递成功只标记，不删行（审计）。
- 迁移 down 3 步再 up 成功，重建后 `outbox_events` 存在、为空且未启用 RLS。

### M3-2 实际执行的验证（2026-10-26）

```text
$ gofmt -l .                       # 无输出
$ go vet ./...
vet_ok
$ go build ./...
build_ok

$ go test ./internal/platform/events/... ./internal/platform/kafka/... -count=1
ok  github.com/edgerunner-fris/expensio/internal/platform/events  0.003s
ok  github.com/edgerunner-fris/expensio/internal/platform/kafka   0.003s

$ go test ./internal/platform/outbox/... -count=1
ok  github.com/edgerunner-fris/expensio/internal/platform/outbox  4.135s
```

本轮验证到的事实：

- 投递器先提交领取事务，再发送 Kafka，最后用另一个短事务标记，
  数据库事务内没有任何 broker 调用。
- 发送失败会写回 `attempts`、`last_error` 与退避后的 `available_at`；
  退避未到期时 `DispatchOnce` 返回 0，不会打满 broker。
- 同批一条事件失败不会阻塞其余事件；失败的那条留在 outbox 等重试。
- 发送内容与 `Envelope.Encode()` 完全一致，分区键为 `tenant_id`。
- `Backoff` 为 1s → 2s → 4s → … → 1m，已用表驱动测试固定。
- `cmd/expense` 启动后台投递器；Kafka 启动时探测失败只告警，
  HTTP 服务照常启动，readiness 不依赖 Kafka。
- 真实 Kafka broker 的端到端验证留到 M3-5（当前用 fake publisher 验证投递器逻辑）。

### M3-3 实际执行的验证（2026-10-26）

```text
$ gofmt -l .                       # 无输出
$ go vet ./...
vet_ok
$ go build ./...
build_ok

$ go test ./internal/expense/ -run "TestWorkflowActionsWriteOutbox|TestRejectAndCancelWriteOutboxWithoutSensitiveText|TestFailedActionsWriteNoOutbox" -count=1
ok  github.com/edgerunner-fris/expensio/internal/expense  3.818s

$ go test ./internal/expense/... -count=1
ok  github.com/edgerunner-fris/expensio/internal/expense  3.985s
```

本轮验证到的事实：

- 提交 → 批准 → 报销产生三条事件，类型依次为
  `expense.submitted/approved/reimbursed`，聚合版本 2/3/4，与费用行版本一致。
- 驳回与撤销各自产生两条事件；快照状态分别为 `rejected` / `cancelled`。
- 事件快照不含商户名、描述与驳回理由，测试直接断言这些字符串和字段名不出现。
- 预算不足、自审、重复提交、空理由驳回全部失败，outbox 行数保持不变：
  失败动作不可能留下待投递事件，更不可能发出消息。
- 事件租户、提交人、预算 ID、审批人字段与费用行一致；信封版本等于快照版本。

### M3-4 实际执行的验证（2026-10-26）

```text
$ gofmt -l .                       # 无输出
$ go vet ./...
vet_ok
$ go build ./...
build_ok

$ go test ./internal/report/... -count=1 -v
--- PASS: TestReadModelRLSIsolatesTenants (2.71s)
--- PASS: TestProcessedEventsIsAppendOnly (0.01s)
--- PASS: TestReadModelMigrationDownAndUp (0.05s)
ok  github.com/edgerunner-fris/expensio/internal/report  3.209s
```

本轮验证到的事实：

- `report_app` 在未设置 `app.tenant_id` 时读 `expense_facts` 得到零行；
  设置租户 A 后能看到自己的行，租户 B 看不到。
- `processed_events` 主键为 `(tenant_id, consumer, event_id)`，
  重复插入返回 SQLSTATE 23505；`report_app` 删除幂等记录返回 42501。
- 迁移 down 1 步再 up 成功，两张表重建且为空，`expense_facts` 仍为 FORCE RLS。

### M3-5 实际执行的验证（2026-10-26）

```text
$ gofmt -l .                       # 无输出
$ go vet ./...
vet_ok
$ go build ./...
build_ok

$ go test ./internal/platform/kafka/... -count=1
ok  github.com/edgerunner-fris/expensio/internal/platform/kafka  0.005s

$ go test ./internal/report/... -count=1 -v
--- PASS: TestConsumerAppliesEventsAndDeduplicates (9.91s)
--- PASS: TestConsumerIgnoresOlderVersionsArrivingLate (0.33s)
--- PASS: TestConsumerRoutesPermanentErrorsToDLQ (5.28s)
--- PASS: TestReadModelRLSIsolatesTenants (0.02s)
--- PASS: TestProcessedEventsIsAppendOnly (0.01s)
--- PASS: TestReadModelMigrationDownAndUp (0.09s)
ok  github.com/edgerunner-fris/expensio/internal/report  18.018s
```

本轮验证到的事实：

- 消费端是至少一次语义：业务事务提交之后才 `CommitRecords`；提交失败只记录日志，
  下次重放由幂等表吸收。测试里同一信封投递两次，`expense_facts` 只有一行、
  `processed_events` 只有一条。
- 乱序到达的旧版本被忽略：先写 v3，再写 v2，事实行保持 v3/approved，
  但两条事件都记入幂等表（幂等的是事件，不是聚合状态）。
- 永久错误（信封无法解码）原字节进 DLQ，header 带上
  `x-original-topic/partition/offset` 与 `x-error`；之后的有效事件照常处理，
  证明坏消息不会卡住分区。
- 测试 Kafka 用 `apache/kafka:3.9.2`（与 compose 同一镜像）的通用容器启动，
  没有使用官方模块——该模块只支持 confluent 镜像，测的就不是生产会跑的 broker。
  宿主端口在启动前随机分配并写进 advertised listener；测试关闭 auto-create，
  主题由 `kmsg.CreateTopicsRequest` 显式创建。
- `cmd/report` 现在装配数据库、生产者与消费者；Kafka 启动探测失败只告警，
  消费循环后台重连，readiness 仍只依赖数据库。

### M3-6 实际执行的验证（2026-10-26）

```text
$ gofmt -l .                       # 无输出
$ go vet ./...
vet_ok
$ go build ./...
build_ok

$ go test ./internal/gateway/ -count=1
ok  github.com/edgerunner-fris/expensio/internal/gateway  1.687s

$ go test ./internal/report/ -count=1
ok  github.com/edgerunner-fris/expensio/internal/report  21.466s
```

本轮验证到的事实：

- 新端点 `GET /api/v1/reports/expenses/summary`，参数
  `from`/`to`（必填，`YYYY-MM-DD`，`to` 为闭区间）、`group_by`（`category`/`status`）、
  `scope`、`status`、`category`。查询范围上限 365 天，超出返回 400。
- 币种隔离由 SQL 结构保证：`GROUPING SETS ((分组, currency), (currency))`，
  总计行只按币种分组，分组行自带币种，任何一行都不是跨币种求和。
  同一语句同时产出总计与分组，前端看到的账一定对得上。
- owner/admin 不传 `scope` 默认 `all`，member/viewer 默认 `mine`；
  member/viewer 显式请求 `all` 返回 403 `auth.forbidden`，不静默降级。
  管理员传 `scope=mine` 可以收窄到本人。
- 邻租户用合法令牌查询得到空数组而不是别人的数据；缺少令牌返回 401。
- 网关 `/api/v1/reports` 前缀转发到 report，路径与查询串原样保留，
  不会落到 identity 的兜底下游。
- 非法参数（缺 from、格式错误、from > to、超范围、未知分组/状态/类别/scope）
  统一返回 400 `common.invalid_argument`。

### M3-7 实际执行的验证（2026-10-26）

先在 `internal/e2e` 增加跨服务链路用例：费用服务写业务数据与 outbox → 投递器发到
真实 Kafka → report 消费者写读模型 → 报表 HTTP API 返回聚合。没有 fake publisher、
没有内存队列、没有直接插事实表。

```text
$ gofmt -l .                       # 无输出
$ go vet ./...
vet_ok
$ go build ./...
build_ok

$ go test ./internal/e2e -run TestExpenseEventsReachReportSummary -v -count=1
--- PASS: TestExpenseEventsReachReportSummary (9.52s)
ok  github.com/edgerunner-fris/expensio/internal/e2e  11.765s
（postgres:16.15-alpine 与 apache/kafka:3.9.2 均为容器实际启动）
```

随后跑里程碑收尾回归：

```text
$ go test ./... -race -count=5 -timeout=25m
ok  github.com/edgerunner-fris/expensio/internal/e2e        38.706s
ok  github.com/edgerunner-fris/expensio/internal/expense    23.008s
ok  github.com/edgerunner-fris/expensio/internal/gateway     5.482s
ok  github.com/edgerunner-fris/expensio/internal/identity   39.538s
ok  github.com/edgerunner-fris/expensio/internal/platform/outbox  15.878s
ok  github.com/edgerunner-fris/expensio/internal/report     72.955s
ok  github.com/edgerunner-fris/expensio/internal/platform/*（其余全部通过）
exit=0
```

本轮验证到的事实：

- 端到端产生了 5 条事件（提交/批准/报销 ×2 + 提交/批准 ×1），投递器返回后
  `outbox_events.published_at` 全部非空；没有任何事件停留在待发状态。
- 两个费用单分别收敛到 `reimbursed`/v4 与 `approved`/v3；`processed_events`
  恰好 5 行，证明每个事件只被消费一次。
- 报表 API 在同一响应里返回 USD 12345 与 EUR 6000 两行总计、以及
  `travel|USD` 与 `meals|EUR` 两行分组，币种没有相加，租户上下文来自 JWT。
- e2e 用例是异步断言：轮询事实行直到最终状态，而不是假定发完消息就立刻可查。
- 每个测试包进程各自启动容器（testenv 的 once 是包级的），跨服务用例不会
  与其他包的 Kafka/PostgreSQL 数据互相污染。

## M4 完成情况（7/7）

设计锁定在 ADR-0005：订阅与账单状态机分离、一个订阅周期一张账单、
回调按 HMAC 验签后从 `invoice_lookup` 反查租户、幂等依赖唯一约束而非先查后插、
事件继续走事务内 outbox。

| 批次 | 内容 | 状态 |
| --- | --- | --- |
| M4-1 | ADR-0005、billing 核心迁移、RLS/约束/幂等 schema 测试 | 完成 |
| M4-2 | billing 领域模型：套餐、订阅状态机、账单金额与周期规则 | 完成 |
| M4-3 | 套餐与订阅 service/repository/HTTP | 完成 |
| M4-4 | 出账（同周期幂等）与账单查询 API | 完成 |
| M4-5 | 支付回调：验签、防重放、幂等落库、outbox 事件 | 完成 |
| M4-6 | 网关 billing 路由、`cmd/billing` 装配与 HTTP 联调 | 完成 |
| M4-7 | 全量回归、跨服务联调、PROGRESS 收尾 | 完成 |

### M4-1 实际执行的验证（2026-10-26）

```text
$ gofmt -l internal/billing        # 无输出
$ go vet ./internal/billing        # 无输出
$ go build ./... && go vet ./...
ALL-OK

$ go test ./internal/billing -count=1 -v -timeout=10m
--- PASS: TestBillingSchemaApplies (3.17s)
    --- PASS: 套餐不可写 / 账单不可删 / 支付流水不可改
    --- PASS: 回调记录不可删 / 映射表不可直读 / 租户反查函数可执行
--- PASS: TestBillingRLSIsolatesTenants (0.03s)
--- PASS: TestSubscriptionConstraints (0.03s)
--- PASS: TestInvoiceConstraintsAndLookup (0.03s)
    --- PASS: 金额不可改 / 币种不可改 / 周期不可改
--- PASS: TestPaymentsWebhooksAndOutbox (0.03s)
--- PASS: TestBillingMigrationDownAndUp (0.11s)
ok  github.com/edgerunner-fris/expensio/internal/billing  3.861s
```

本轮验证到的事实：

- 四张租户表（订阅、账单、支付流水、回调记录）全部 `ENABLE + FORCE RLS`；
  无租户上下文查不到行，跨租户写入 SQLSTATE 42501，跨租户按 ID 更新 0 行。
- 同一租户第二条未取消订阅被部分唯一索引拒绝（23505）；取消后可以重新订阅。
  订阅状态只能走 `active ↔ past_due → canceled`，取消是终态。
- 同一订阅周期重复出账被唯一约束拒绝（23505）；金额、币种、周期与账单号
  出账后不可改（23514）；`paid` 必须有 `paid_at`，且不能回到 `open`。
- 支付流水与回调记录只追加：运行时角色的 UPDATE/DELETE 都是 42501；
  同一 `(provider, provider_event_id)` 与同一 `(provider, payment_id)` 只能写一次。
- 账单插入时由触发器同事务写 `invoice_lookup`；运行时角色不能直接读它，
  只能调用 `app.invoice_tenant(uuid)` 得到租户，未知账单返回 NULL。
- 迁移 down/up 后套餐种子仍在、业务表为空，说明 down SQL 可执行。

### M4-2 实际执行的验证（2026-10-26）

```text
$ gofmt -l internal/billing        # 无输出
$ go vet ./... && go build ./...
BUILD-OK

$ go test ./internal/billing -count=1 -v \
    -run '^(TestPlanValidate|TestSubscriptionTransitions|TestInvoiceTransitions|TestWebhookEventType|TestInvoiceAmount|TestCanManageBilling|TestPeriodEnd|TestPeriodEndIsDateOnly|TestPeriodEndRejectsUnknownInterval)$'
--- PASS: TestPlanValidate (0.00s)          # 12 个子用例全部 PASS
--- PASS: TestSubscriptionTransitions (0.00s)
--- PASS: TestPeriodEnd (0.00s)             # 8 个子用例全部 PASS
--- PASS: TestInvoiceTransitions (0.00s)
--- PASS: TestWebhookEventType (0.00s)
--- PASS: TestInvoiceAmount (0.00s)
--- PASS: TestCanManageBilling (0.00s)
--- PASS: TestPeriodEndIsDateOnly (0.00s)
--- PASS: TestPeriodEndRejectsUnknownInterval (0.00s)
ok  github.com/edgerunner-fris/expensio/internal/billing  0.023s
```

（M4-2 只跑单元测试；集成用例仍然真实跑容器，随 M4-7 全量回归一起复跑。）

本轮验证到的事实：

- 订阅状态机按 3×3 全矩阵逐一断言：合法的只有 `active ↔ past_due`、
  `active → canceled`、`past_due → canceled`；`canceled` 没有出边，
  未知状态的向前/向后转换都是 false。账单只有 `open → paid`。
- 套餐校验覆盖：首位必须是字母、允许字符集、2–41 位长度、名称去空白后 1–80 字符、
  单价 1..`money.MaxMinor`、三位大写币种、monthly/yearly 周期。
- 金额计算先做 `MaxMinor / seats` 再比较：`price=MaxMinor, seats=2` 被拒绝，
  `price=MaxMinor/2, seats=2` 恰好等于上限并通过，说明 int64 回绕被挡在乘法之前。
- 周期计算：月付 2026-01-31 结束于 2026-02-27；闰年 2024-01-31 结束于 2024-02-28；
  年付闰日 2024-02-29 结束于 2025-02-27；跨年与 12 月 31 日按同一规则推进。
  输入带时区与时分秒时先归一到 UTC 日期，输出恒为 00:00 UTC。
- 写测试时用了名为 `strings` 的辅助函数，`go vet` 报与 `model.go` 的 `strings`
  导入冲突（package 级声明与文件级导入同名），已改名为 `repeatA`。

### M4-3 实际执行的验证（2026-10-26）

```text
$ gofmt -l internal/billing && go vet ./internal/billing && go build ./...
BUILD-OK

$ go test ./internal/billing -count=1 -v -timeout=10m \
    -run '^(TestBillingPlansAndSubscriptionHTTP|TestBillingSubscriptionBadRequests)$'
--- PASS: TestBillingPlansAndSubscriptionHTTP (3.25s)
--- PASS: TestBillingSubscriptionBadRequests (0.01s)
    --- PASS: 套餐不存在 / 套餐为空 / 席位为零 / 席位超上限
    --- PASS: 请求体不是对象 / 非法订阅 ID / 取消缺 version
ok  github.com/edgerunner-fris/expensio/internal/billing  3.754s

# 随后整包复跑（含 M4-1 的 schema 用例）
$ go test ./internal/billing -count=1 -timeout=10m
ok  github.com/edgerunner-fris/expensio/internal/billing  3.833s
```

（PostgreSQL 16.15-alpine 容器真实启动；时钟固定为 2026-10-26 09:00 UTC，
周期断言不依赖跑测试的当天。）

本轮验证到的事实：

- `GET /billing/plans` 返回迁移里种子的 5 个套餐，按 interval、价格排序；
  查看者也能读，价格直接取自 `app.plans`，不在代码里复制一份。
- `POST /billing/subscriptions` 对 member 返回 403（中间件与 service 各挡一次）；
  admin 开通后状态 `active`、version 1，月付周期 2026-10-26..2026-11-25，
  年付 2026-10-26..2027-10-25，与 `PeriodEnd` 的单元测试一致。
- 同租户第二条生效订阅返回 409 `billing.subscription_exists`，
  由 `subscriptions_one_live_per_tenant` 唯一索引判定，没有先查后插的竞态窗口。
- 邻租户按 ID 查订阅返回 404：RLS 让查询本身没有行，而不是查到后再拒绝。
- 取消必须带匹配的 version：版本不符 409 `billing.subscription_version_conflict`；
  成功后状态 `canceled`、version 2、`canceled_at` 非空；再次取消 409
  `billing.subscription_state_conflict`，不会刷新取消时间。
- 取消后当前订阅返回 404，历史列表仍返回该条 `canceled` 记录；可立即重新订阅。
- 测试抓到一个真实缺陷：`CancelSubscription` 的 SQL 已把状态改成 `canceled`，
  但仓储没把扫描出的 status 写回结构体，响应会是 `active`。已修复（写回 Status）。

### M4-4 实际执行的验证（2026-10-26）

```text
$ gofmt -l internal/billing && go vet ./... && go build ./...
ALL-OK

$ go test ./internal/billing -count=1 -timeout=10m
ok  github.com/edgerunner-fris/expensio/internal/billing  3.351s

$ go test ./internal/platform/events ./internal/platform/outbox -count=1
ok  github.com/edgerunner-fris/expensio/internal/platform/events  0.004s
ok  github.com/edgerunner-fris/expensio/internal/platform/outbox  4.450s
```

本轮验证到的事实：

- 事件契约新增 `expensio.billing.events.v1` 与五种事件类型；
  `SubscriptionSnapshot` / `InvoiceSnapshot` 有独立校验，取消时间与取消状态、
  支付时间与 paid 状态必须同时出现，非法快照在写 outbox 前就被拒绝。
- 出账金额来自 `plans.price_minor × seats`：Growth 9900 × 5 席 = 49500；
  到期日 = 出账日 + 14 天（2026-10-26 → 2026-11-09），账单号
  `INV-202610-<8 位十六进制>`，租户内唯一。
- 同一订阅周期第二次出账返回 409 `billing.invoice_exists`；
  已取消订阅出账返回 409 `billing.subscription_state_conflict`；
  邻租户按 ID 查账单返回 404；`status=void` 这类非法过滤值是 400。
- 订阅开通、出账、取消各在同一事务内写一条 outbox 事件
  （`subscription.created` / `invoice.created` / `subscription.canceled`），
  重复出账失败后事件数不增加，证明事务回滚时事件一起消失。
- 事件信封可被 `events.Decode` 还原并通过校验；快照里的金额、状态、周期
  与实际落库行一致。
- 测试隔离修正：`outbox_events` 按设计没有 RLS（投递器跨租户扫描），
  同包用例共享一个容器，断言必须显式按 `tenant_id` 过滤，否则会读到别的
  用例产生的事件。已按租户过滤。

### M4-5 实际执行的验证（2026-10-26）

```text
$ gofmt -l internal/billing && go vet ./internal/billing && go build ./...
BUILD-OK

$ go test ./internal/billing -count=1 -timeout=15m
ok  github.com/edgerunner-fris/expensio/internal/billing  4.389s

$ go test ./internal/billing -race -count=5 -timeout=25m
ok  github.com/edgerunner-fris/expensio/internal/billing  7.437s
```

本轮验证到的事实：

- 验签基于原始请求体：`HMAC-SHA256(secret, "<timestamp>.<body>")`，十六进制比较用
  `hmac.Equal`（常量时间）；签名错误、时间戳超出 ±5 分钟、渠道名不匹配、
  请求体不是合法 JSON 分别得到 401/401/401/400，且都不写任何数据库记录。
- 租户不从请求体读取：回调只带 `invoice_id`，服务先执行 `app.invoice_tenant(uuid)`
  反查可信租户，再以 `db.WithinTenantID` 建立 RLS 上下文。未知账单返回 404，
  不可能借回调写入任意租户的行。
- `payment.failed`：账单保持 `open`，生效订阅 `active → past_due`（version 2），
  写回调记录与 `invoice.payment_failed` 事件；`payment.succeeded`：
  账单 `open → paid`（version 2、`paid_at` 非空），订阅 `past_due → active`（version 3），
  写一条支付流水与 `invoice.paid` 事件。四类写入在同一事务内完成。
- 幂等完全由唯一约束承担：同一 `(provider, provider_event_id)` 重投返回 HTTP 200
  `status=duplicate`，支付流水、回调记录、账单版本与 outbox 行数都不变；
  不同事件 ID 但同一 `payment_id` 同样判定为已处理；已支付账单收到另一笔支付返回
  409 `billing.invoice_already_paid`。
- 金额或币种与账单不符返回 409 `billing.payment_mismatch`，状态不变，
  之后合法的失败/成功回调仍可正常处理（顺序为 mismatch → failed → succeeded）。
- 重复回调用哨兵错误触发事务回滚，而不是"插到一半返回 nil"——后者会在提交时
  因事务已中止而失败，重投会变成 500。已用重投用例固定这一行为。
- 时区：账单周期与到期日是 UTC 日期；回调中金额用 `int64` 最小单位，
  不使用浮点。测试时钟固定为 2026-10-26 09:00 UTC。

### M4-6 实际执行的验证（2026-10-26）

`cmd/billing` 从空壳改为真实装配：`NeedsDB` + `NeedsJWT` + `NeedsKafka` +
`NeedsBillingWebhook`，启动即建连接池、注册 `/healthz` 的 db 检查、注入回调密钥、
挂载路由并启动 outbox 投递器。`config` 新增 `billing.webhook_secret` /
`billing.webhook_provider`，密钥无默认值。

```text
$ gofmt -l . && go vet ./... && go build ./...
（无输出）

$ go test ./internal/platform/config ./internal/gateway ./internal/billing -race -count=5 -timeout=25m
ok  internal/platform/config  1.035s
ok  internal/gateway          4.543s
ok  internal/billing          8.380s
```

真实进程联调（PostgreSQL 16 + Kafka 3.9.2 均为容器；identity :8081、billing :8083、
gateway :8080，所有业务请求从 8080 进入）：

```text
POST /api/v1/auth/register                                  201
POST /api/v1/billing/subscriptions                          201
     growth-monthly × 5，active，周期 2026-10-26..2026-11-25
POST /api/v1/billing/subscriptions/{id}/invoices            201
     INV-202610-C2D118A2，open，49500 USD，到期 2026-11-09
POST /api/v1/webhooks/payments（正确签名）                   200 status=processed
POST /api/v1/webhooks/payments（同一事件重投）                200 status=duplicate
POST /api/v1/webhooks/payments（错误签名）                    401 auth.unauthorized
GET  /api/v1/billing/invoices/{id}                           200 status=paid version=2
GET  /api/v1/billing/subscription                            200 status=active
```

数据库与 broker 侧核对：

```text
invoices: INV-202610-C2D118A2  paid  49500  version=2
payments: mockpay / pay_live_m4live… 49500
webhook_events: 1 行（重投没有新增）
outbox_events: subscription.created / invoice.created / invoice.paid   published=t（3/3）

$ kafka-console-consumer --topic expensio.billing.events.v1 --from-beginning --max-messages 3
{"event_type":"subscription.created", "aggregate_version":1, …}
{"event_type":"invoice.created",      "aggregate_version":1, "payload":{"status":"open", …}}
{"event_type":"invoice.paid",         "aggregate_version":2, "payload":{"status":"paid", …}}
```

启动期拒绝无密钥/短密钥（实测退出码非 0）：

```text
$ EXPENSIO_BILLING_WEBHOOK_SECRET=short /tmp/expensio-billing
exit=1
billing 启动失败: billing.webhook_secret 至少需要 32 字节（EXPENSIO_BILLING_WEBHOOK_SECRET）
```

本轮验证到的事实：

- 网关的公开回调路由是 `/api/v1/webhooks/payments`（`Auth=false`、按 IP 限流），
  与 billing 的注册路径一致；新增用例断言该请求只到 billing，不会落到 identity。
  之前 billing 把回调挂在 `/api/v1/billing/...` 下，会被网关的受保护前缀挡成 401，
  联调前已改正。
- 回调经网关全程无 JWT：租户来自 `invoice_lookup` 反查，HTTP 层只校验签名与时间窗。
- 出账、支付、回调记录与 outbox 在同一事务内提交；`outbox_events.published_at`
  在投递器运行后变为非空，重复回调没有产生第四条事件。
- Kafka 主题不会因为客户端元数据请求自动创建：首次联调三条事件一直失败
  （`UNKNOWN_TOPIC_OR_PARTITION`），投递器按退避重试且没有丢事件；补
  `scripts/topics.sh`（幂等，`make up` 后自动执行）并重启投递器后 3/3 发布成功。
- `billing.webhook_secret` 缺失或短于 32 字节时进程拒绝启动，而不是带病运行；
  `Config.String` 对该字段打码，日志不会带出密钥。

### M4-7 实际执行的验证（2026-10-26）

里程碑收尾回归，全量跑两遍（`-race -count=5` 与真实容器）：

```text
$ gofmt -l . && go vet ./... && go build ./...
（无输出）

$ go test ./... -race -count=5 -timeout=25m
ok  internal/billing                15.223s
ok  internal/e2e                    37.219s
ok  internal/expense                19.237s
ok  internal/gateway                 5.070s
ok  internal/identity               33.324s
ok  internal/platform/outbox        14.599s
ok  internal/report                 58.002s
ok  internal/platform/apperr / bootstrap / config / events / health
    / httpx / middleware / kafka / logger / money / server / tenantctx  全部 ok
exit=0
```

本轮验证到的事实：

- billing 的 7 个集成用例没有使用 `t.Skip` 作为通过条件；Docker 不可用时由
  `testenv` 的显式门禁失败（`EXPENSIO_REQUIRE_DOCKER=1`），不是静默跳过。
- 全量竞态回归覆盖了 M4 新增的回调路径：重复回调、金额不符、未知账单、
  坏签名、过期时间戳均被既有用例与真实联调同时覆盖。
- 跨服务链路：真实 identity → gateway → billing → PostgreSQL → outbox →
  Kafka 已跑通，Kafka 上可读到三条 billing 事件且状态与数据库一致
  （明细见 M4-6）。本次没有新增 fake publisher，也没有直接向 outbox 插行。
- M4 完成后的边界：billing 只负责订阅/账单/支付状态，不产生邮件；
  通知与前端留到 M5。

## M5 完成情况（7/7）

设计锁定在 ADR-0006：通知是派生数据，消费事件后写 `app.notifications` 与
`app.email_outbox`，邮件由独立 worker 领取并投递；收件人通过 identity 的
签名服务令牌内部接口解析；前端管理台覆盖登录、费用、审批、预算、报表、
账单、成员与通知。

| 批次 | 内容 | 状态 |
| --- | --- | --- |
| M5-1 | ADR-0006、notification 迁移：RLS、幂等约束、`claim_emails` | 完成 |
| M5-2 | identity 内部收件人接口 + `serviceauth` 签名令牌 | 完成 |
| M5-3 | 费用/账单事件消费：渲染、幂等、永久错误进 DLQ | 完成 |
| M5-4 | 站内收件箱 API：列表、未读数、标记已读/全部已读 | 完成 |
| M5-5 | 邮件发件箱 worker：跨租户领取、SMTP、退避与上限 | 完成 |
| M5-6 | `cmd/notification` 装配、网关路由、配置校验 | 完成 |
| M5-7 | Vue 管理台、依赖实测、真实联调与全量回归 | 完成 |

### M5-7 实际执行的验证（2026-10-27）

前端依赖是实际安装后的版本，构建包含类型检查：

```text
$ npm ls --depth=0
├── @element-plus/icons-vue@2.3.2
├── @vitejs/plugin-vue@6.0.10
├── axios@1.20.0
├── echarts@6.1.0
├── element-plus@2.14.7
├── pinia@4.1.0
├── typescript@5.9.3
├── vite@8.3.5
├── vue-router@5.4.0
├── vue-tsc@3.3.13
└── vue@3.5.44

$ npm run build            # vue-tsc --noEmit && vite build
✓ built in 3.59s
（仅有 index 865.67 KB / ReportsView 510.52 KB 的 >500 KB 提示，见风险 34）
```

真实进程联调：gateway 8080、identity 8081、expense 8082、notification 8085，
真实 PostgreSQL/Kafka/Mailpit（联调脚本是临时文件，不属于仓库）：

```text
probes: identity/expense/notification/gateway healthy
register: slug=m5-1793031539 tenant=9ec483a3-5d89-4417-b5d4-fd4cb63c6b02
member: 25798927-ff0e-4f0e-a850-05b80d6f2f9c <member-m5-1793031539@example.com>
submit: submitted budget=03d30c15-17e5-4b29-ac80-7390d66ec818
owner notification: expense.submitted | 新的费用待审批 | 有一笔差旅费用 234.56 USD 已提交，等待审批。
owner unread before approval: 1
POST /notifications/:id/read HTTP 204
owner unread after read: 0
approve: approved version=3
member notification: expense.approved | 费用已通过审批 | 你提交的差旅费用 234.56 USD 已通过审批。
mark all read updated: 1
member unread after read-all: 0
other tenant notifications: 0
mailpit matched messages: 2
  to: member-m5-1793031539@example.com | subject: [Expensio] 费用已通过审批
  to: owner-m5-1793031539@example.com  | subject: [Expensio] 新的费用待审批
processed=2
notifications=2
emails=2 sent=2
kinds=expense.approved,expense.submitted
notification error log lines: 0
m5 live check: OK
```

里程碑收尾回归：

```text
$ gofmt -l . && go vet ./... && go build ./...
（无输出）

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=5 -timeout=25m
ok  internal/billing                21.792s
ok  internal/e2e                    31.712s
ok  internal/expense                26.663s
ok  internal/gateway                 7.561s
ok  internal/identity               47.234s
ok  internal/notification           16.660s
ok  internal/platform/outbox        20.094s
ok  internal/report                 65.806s
ok  internal/platform/{apperr,bootstrap,config,events,health,httpx,
    httpx/middleware,kafka,logger,money,server,serviceauth,tenantctx}
exit=0
```

本轮验证到的事实：

- 提交与审批两条通知都由真实事件驱动：member 提交后 owner 收到"新的费用待审批"，
  owner 批准后 member 收到"费用已通过审批"；金额、类别与资源 ID 都正确。
- 消费端幂等：同一租户 `processed_events=2`、`notifications=2`、`emails=2`
  （按租户过滤），重复投递不会产生第二行；两封邮件均为 `sent`。
- 收件人来自 identity 内部接口，notification 没有连 identity 的库；另一租户
  用同一 API 查询得到 0 条，RLS 与用户过滤同时生效。
- 邮件经真实 SMTP（Mailpit）投递，中文主题按 RFC 2047 编码后收件箱可读；
  worker 从发件箱领取，消费事务里没有网络调用。
- 联调时 billing 消费组首次从最早位点启动，把 M4 留下的 3 条账单事件重放成
  3 条通知与 3 封邮件——回放确实能重建通知，但也说明生产全新部署会补发
  历史邮件，已记为风险 31。
- 网关新增 `/api/v1/notifications` 路由回归：无令牌 401 且不触达下游，
  有令牌只转发给 notification，请求路径与查询串原样保留。

## M6：容器化与部署（2026-10-27）

### 镜像

七个镜像全部在 WSL2 的 Docker 中构建成功（`docker compose --profile app build`
退出码 0）。六个 Go 服务共用 `Dockerfile`，管理台用 `Dockerfile.web`：

| 镜像 | 大小 | 内容大小 |
| --- | --- | --- |
| `expensio/gateway:dev` | 39.4 MB | 10.8 MB |
| `expensio/identity:dev` | 50.3 MB | 13.5 MB |
| `expensio/report:dev` | 57.1 MB | 15.3 MB |
| `expensio/expense:dev` | 57.3 MB | 15.3 MB |
| `expensio/billing:dev` | 57.3 MB | 15.3 MB |
| `expensio/notification:dev` | 58 MB | 15.5 MB |
| `expensio/web:dev` | 27.7 MB | 7.87 MB |
| `expensio/migrate:m6-dev` | 84 MB | 20.2 MB |

迁移镜像由 `Dockerfile.migrate` 单独构建（Helm Job 使用）；Compose 的迁移
Job 直接挂载 `migrations/` 到上游 `migrate` 镜像，没有再构建一份。

构建期踩到并修掉的两个问题：

- 根 `Dockerfile` 的 `ARG SERVICE` / `ARG VERSION` 若写在 `go mod download`
  之前，Docker 会把不同 SERVICE 算成不同缓存键，六个服务各下载一遍模块。
  已把这两个参数移到下载之后，六个服务共享同一层。
- `Dockerfile.web` 的 `ARG GO_VERSION` 曾写在两个 `FROM` 之间，Docker 只把
  首个 `FROM` 之前的 `ARG` 当全局参数，报 `failed to parse stage name`。
  已把 `NODE_VERSION` 与 `GO_VERSION` 都移到第一个 `FROM` 之前。

### Compose 整栈

```text
$ docker compose --profile app up -d --wait --wait-timeout 420
UP_EXIT:0

$ docker compose --profile app ps -a
migrate          Exited (0)
topics           Exited (0)
identity/expense/billing/report/notification/gateway/web   healthy
```

应用容器统一 `read_only: true`、`cap_drop: ALL`、`no-new-privileges`，
以 UID 10001 运行；只有 gateway（宿主 8080）与 web（宿主 5173）发布端口，
且都绑定 `127.0.0.1`。

容器化端到端联调（临时脚本，真实容器）：

```text
web: index 200, app mount found
web healthz: ok
web readyz: ok
web: SPA fallback /expenses/123 -> index
web: /assets/index-CgfjUPje.js cache-control:public, max-age=31536000, immutable
web: index cache-control:no-cache
web: CSP present
web: missing asset -> 404
gateway: /healthz /readyz healthy
web->gateway->identity register: slug=m6-1793033744 tenant=721e2a4c-09f0-4980-ba7f-d7036204efc3
member inserted: 37cbd902-fe6d-4bac-a6b8-b9ba46b1f750 <member-m6-1793033744@example.com>
login member: HTTP 200
billing plans: 5 | first: starter-monthly
budget: 5e588829-18f3-4781-a5bd-a0f0615731f8
expense: de3b3f05-eba6-4571-a4da-e3bfd88836d0
submit: submitted
owner notification: expense.submitted | 新的费用待审批
approve: approved
member notification: expense.approved | 费用已通过审批
report summary: all USD 23456 count 1
mailpit matched messages: 2
  to: member-m6-1793033744@example.com | subject: [Expensio] 费用已通过审批
  to: owner-m6-1793033744@example.com  | subject: [Expensio] 新的费用待审批
processed=2
notifications=2
emails=2 sent=2
report_processed=2
report_rows=1
published: gateway -> 127.0.0.1:8080 | web -> 127.0.0.1:5173
containers: all seven services healthy, rootfs read-only, uid 10001:10001
m6 live check: OK
```

### Helm 与 CI

`helm lint` 与 `helm template` 使用临时下载的 Helm v3.19.0 执行：

```text
$ helm lint deploy/helm/expensio
1 chart(s) linted, 0 chart(s) failed

$ helm template expensio deploy/helm/expensio --namespace expensio | grep -c '^kind:'
24
      1 kind: ConfigMap
      7 kind: Deployment
      1 kind: Ingress
      2 kind: Job
      3 kind: NetworkPolicy
      2 kind: PodDisruptionBudget
      7 kind: Service
      1 kind: ServiceAccount
```

CI 工作流 `.github/workflows/ci.yml` 含六个 job（backend/frontend/helm/images/
web-image/migrate-image），PyYAML 解析通过（26 个 step）。**尚未在 GitHub
runner 上执行**，也没有 `actionlint`；见风险 26。

### 里程碑收尾回归

```text
$ gofmt -l . && go vet ./... && go build ./...
（无输出）

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=1 -timeout=25m
ok  internal/billing                25.867s
ok  internal/e2e                    35.047s
ok  internal/expense                25.712s
ok  internal/gateway                10.547s
ok  internal/identity               31.515s
ok  internal/notification           22.783s
ok  internal/platform/kafka         21.816s
ok  internal/platform/outbox        23.309s
ok  internal/report                 44.726s
ok  internal/web                     1.075s
（其余基础包全部 ok；TEST_EXIT:0）

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=5 -timeout=25m
ok  internal/billing                35.871s
ok  internal/e2e                    48.359s
ok  internal/expense                42.471s
ok  internal/gateway                10.072s
ok  internal/identity               73.022s
ok  internal/notification           30.715s
ok  internal/platform/kafka         27.911s
ok  internal/platform/outbox        33.090s
ok  internal/report                 95.844s
ok  internal/web                     1.337s
（其余基础包全部 ok；TEST_EXIT:0）
```

本轮验证到的事实：

- 管理台镜像同时包含 Vite 产物与 Go 静态服务：`index` 为 `no-cache`、
  hashed asset 为一年 `immutable`、未知资源 404、SPA 深链接回退到 index，
  且响应带 CSP；`/api` 反代确实把注册请求送到 gateway → identity。
- 容器间链路完整：web → gateway → expense → outbox → Kafka → notification
  → identity 内部接口 → 站内通知 + 发件箱 → Mailpit；同时 report 消费者
  写出读模型，billing 计划接口经网关可达。
- 同一租户两个事件恰好产生 `processed=2`、`notifications=2`、`emails=2`，
  两封邮件均为 `sent`；report 侧 `processed=2`、`fact=1`，没有重复行。
- 七个服务全部 healthy，根文件系统只读、UID 10001；migrate 与 topics
  Job 均以 0 退出；宿主只监听 127.0.0.1:8080 / 5173。
- notification 改用 `ResetLatest` 后，新消费组不再补发历史邮件；report
  仍从最早位点回放。两者由同一个 `OffsetReset` 选项区分，真实 broker 测试
  在 `-race -count=5` 下通过。
- Helm 渲染出 24 个清单；但**没有可用集群**，未执行 `kubectl apply`，
  多副本、PDB、NetworkPolicy 的运行时行为仍是待验证项。

## M7：可观测性与压测（2026-10-27）

### 接入内容

七个进程（六个 Go 服务 + 管理台）统一接 OTel SDK，经 OTLP/gRPC 发往
Collector；Collector 把 trace 转 Tempo，把指标通过 Prometheus exporter
暴露给 Prometheus。Grafana 预置 Prometheus/Tempo 数据源与
`Expensio Overview` 看板。`otel.enabled` 默认关闭，Compose 显式打开。

指标覆盖 HTTP 请求数/延迟/并发、Kafka 生产与消费、outbox 领取与投递、
邮件发送、网关限流决策、pgx 连接池；健康检查不产生 span/指标，HTTP 路由
使用低基数模板。HTTP 延迟直方图边界为 5ms–10s，Kafka 为 1ms–10s。
告警规则 8 条。设计取舍、采样、outbox trace 断点见 ADR-0009。

### k6 实测

`make load` 对已启动整栈执行 3 VUs 业务循环（创建 → 提交 → 取消 → 列表 →
报表汇总 → 通知列表），每轮 sleep 1.5s。三次 30s 运行：

```text
run 1: checks 362/362 100%, http_req_failed 0.00%, p(95)=20.26ms, 60 iter, 362 req (11.47/s)
run 2: checks 362/362 100%, http_req_failed 0.00%, p(95)=25.31ms, 60 iter, 362 req (11.28/s)
run 3: checks 344/344 100%, http_req_failed 0.00%, p(95)=86.28ms, 57 iter, 344 req (10.96/s)
        avg=19.73ms med=8.09ms max=370.39ms（gateway 刚重建的冷启动）
```

三次阈值全部通过：`http_req_failed<1%`、`p(95)<500ms`、`checks>99%`。

Prometheus 对最终一轮查询（2m 窗口）：

```text
HTTP P95 overall 69.67ms
  gateway 92.5ms / expense 85.5ms / report 107.5ms
  notification 53.75ms / identity 6.25ms
直方图 le: 0.005 0.01 0.025 0.05 0.1 0.25 0.5 1 2.5
  （亚秒分桶已生效；旧的 5/10/+Inf 序列在滚动窗口内自然退场）
```

原始计数器增量与 k6 精确对账（最终一轮，57 次迭代）：

```text
gateway 200 +285 / 201 +59 = 344，与 k6 的 344 次请求一致
限流决策 344（tenant 343 + ip 1），全部 allowed，无 denied/error
expense 201 +58（57 次创建 + 1 次预算）；累计 180
Kafka producer +114；report / notification consumer 各 +114（累计各 354）
outbox delivered submitted/cancelled 各 +57（累计各 177）；claimed 累计 354
emails sent +57（只有 submit 发信，cancel 不发）；累计 177
build_info = 7 个服务；pgx 连接池 idle=total；8 条规则 health=ok，无 firing
```

关于 Prometheus 数值的一个实测结论：`increase()/rate()` 会向窗口边界外推，
短促流量下会高估（run 2 的 5m `increase` 为 472，而原始计数差值是 362）；
counter 首次导出时样本已经非零，首个窗口又会低估。因此工作量对账用原始
计数差值，延迟与错误率才用 `rate()`。

### Trace 实测

Tempo 搜索 `service.name=gateway` 返回真实 trace；同一条 trace 同时包含
生产与两个消费组：

```text
trace 1a9add9e8be1f8ac289f84070a4c368b
  expense       kafka.publish expensio.expense.events.v1      (root)
  notification  kafka.consume expensio.expense.events.v1      (child)
    notification HTTP GET
      identity GET /api/v1/internal/tenant-contacts
  report        kafka.consume expensio.expense.events.v1      (sibling)
```

说明 `traceparent` 经 Kafka 记录头传播，report 与 notification 两个消费组
落在同一条 trace；outbox 边界仍断开（HTTP 请求的上下文没有随事件落库），
这是 ADR-0009 记录的已知限制。

环境事件：Docker Desktop/WSL 在镜像构建期间冻结过一次，恢复后 Tempo
首次 `/ready` 返回 503（`waiting for 15s after being ready`），随后连续
3 次 200，搜索 API 200；日志里的 frontend RPC 错误只出现在恢复瞬间。
这是本地环境事件，不是业务缺陷，但说明本地可观测性栈不能当作生产就绪。

### Helm 与配置校验

```text
$ helm lint deploy/helm/expensio
1 chart(s) linted, 0 chart(s) failed

$ helm template ...（默认 values）
kinds=24；ConfigMap 含 EXPENSIO_OTEL_ENABLED="false"

$ helm template ... --set otel.enabled=true --set otel.endpoint=...
kinds=24；web Deployment 注入 EXPENSIO_OTEL_ENABLED/ENDPOINT/INSECURE

$ promtool check config /etc/prometheus/prometheus.yml
SUCCESS: 1 rule files found / valid config
$ promtool check rules /etc/prometheus/rules.yml
SUCCESS: 8 rules found
```

### 里程碑收尾回归（最终代码）

```text
$ gofmt -l . && go vet ./... && go build ./...
（无输出）

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=1 -timeout=25m
23 个包全部 ok；最长 internal/report 33.194s；TEST1_EXIT:0

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=5 -timeout=25m
23 个包全部 ok；最长 internal/report 89.185s；TEST5_EXIT:0
```

M7 未关闭的项：生产采样与 Tempo 对象存储、outbox trace 续接、真实集群的
Collector/Prometheus 部署（风险 20、26），以及风险 17 的 EdDSA 迁移。

## M8：前端优化与浏览器级 E2E（2026-10-27）

### 风险 34：首屏包体

`frontend/src/main.ts` 不再 `app.use(ElementPlus)`，改为逐个注册模板实际
用到的 34 个组件，`ElLoading` 指令单独 `app.use`；`App.vue` 用
`ElConfigProvider` 保留 zh-CN 语言包。报表页的 ECharts 从静态 import 改为
`@/utils/charts` 动态 `import()`，只在确实要画图时下载。

实测 `npm run build`（脚本含 `vue-tsc --noEmit`）：

```text
index-C3ahQ0WK.js        469.16 kB │ gzip: 150.49 kB   （原 865.67 / 276.04）
ReportsView-Bno0xF2Z.js    5.07 kB │ gzip:   2.54 kB   （原 510.52 / 172.62）
charts-hvYY3Ft_.js       519.31 kB │ gzip: 175.39 kB   （按需懒加载）
```

`charts` 仍触发 Vite 的 >500 KB 警告，但它是懒加载 chunk，首屏不下载；
保留并在 README 已知限制里说明。web 镜像重建后确认容器实际返回的
`index.html` 引用 `index-C3ahQ0WK.js`，不是旧产物。

### 风险 36：Playwright E2E

新增 `e2e/` 独立 npm 工程（`@playwright/test 1.64.1`）。Compose 的 `e2e`
profile 用官方 `mcr.microsoft.com/playwright:v1.64.1-noble` 镜像，跑在
Compose 网络上；`make e2e` 会带上 `--profile app --profile e2e`
（只给 e2e 会因 `web` 未定义而失败）。宿主不装浏览器：WSL 里缺
`libnspr4` 且没有免密 sudo，容器方案同时解决系统依赖与 CI 可复现性。

`e2e/playwright.config.ts`：单 worker、`baseURL` 由 `E2E_BASE_URL` 提供、
失败保留 trace、只截失败图。每个用例注册全新租户，不依赖库内既有数据。

实测（连续三轮，真实输出）：

```text
Running 2 tests using 1 worker
  ✓  1 [chromium] › tests/expense-flow.spec.ts:56:1 › 未登录访问受保护路由会跳到登录页 (240ms)
  ✓  2 [chromium] › tests/expense-flow.spec.ts:62:1 › 注册 → 建单 → 提交 → 报表 → access token 失效后自动刷新 (2.1s)
  2 passed (2.9s)

  ✓  1 … 未登录访问受保护路由会跳到登录页 (228ms)
  ✓  2 … 注册 → 建单 → 提交 → 报表 → access token 失效后自动刷新 (3.6s)
  2 passed (4.4s)

  ✓  1 … 未登录访问受保护路由会跳到登录页 (750ms)
  ✓  2 … 注册 → 建单 → 提交 → 报表 → access token 失效后自动刷新 (5.5s)
  2 passed (8.7s)   ← 与 Go 全量回归并行，耗时偏高
```

报表断言先用 `expect.poll` 轮询 summary API 的 `currency_totals` 非空（20s），
再进报表页等 `.chart canvas`：提交后事件要经 outbox → Kafka → report 消费者
才落读模型（ADR-0004），固定 sleep 会偶发失败。轮询窗口取 UTC 前后各一天，
避免浏览器时区与 UTC 跨日时漏查。

### E2E 发现并修掉的真实缺陷

首次跑通时报 `POST /api/v1/expenses` 400。原因是浏览器本地时间 07:46，
前端把日期拼成当天 12:00 再 `toISOString()`，落在后端
`maxOccurredSkew = 5m` 的未来时间窗口之外——任何用户在当地中午 12 点前
建单都会失败，编辑草稿同样受影响；而单元/集成测试直接构造 UTC 时间，
从未触发这个组合。

修复：新增 `frontend/src/utils/format.ts#occurredAtISO`，日期是当天且尚未
到 12:00 时用当前时间，其余保持中午 12:00；`ExpensesView` 创建与
`ExpenseDetailView` 编辑都改用它。这是 M8 最直接的收益：浏览器级用例覆盖了
API 测试看不到的本地时间组合。

### M8 未关闭项

风险 33（真实 SMTP STARTTLS）、风险 17（Ed25519/JWKS）本轮未做；
风险 20/26（真实集群、CI runner）仍未验证。M8 没有改 Go 代码，但仍按
里程碑收尾跑了一次全量回归，结果见下。

### 里程碑收尾回归（M8 最终代码）

```text
$ gofmt -l . && go vet ./... && go build ./...
（无输出，GOFMT_EXIT:0 VET_EXIT:0 BUILD_EXIT:0）

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=1 -timeout=25m
23 个包全部 ok；最长 internal/report 33.361s；TEST1_EXIT:0

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=5 -timeout=25m
23 个包全部 ok；最长 internal/report 80.781s；TEST5_EXIT:0
```

## M9：令牌签名迁移到 EdDSA / JWKS（2026-10-27）

### 实现

- `internal/platform/auth/jwt.go`：`IssuerConfig` / `TokenIssuer` 支持
  EdDSA 签发（header 带 `kid` = 公钥 RFC 7638 thumbprint）与按 `kid` 验签；
  解析用 `jwt.WithValidMethods` 锁定算法，`alg=none` 与 RS/HS 混淆被拒；
  未知 `kid` 直接报错，不"试所有公钥"。HS256 只在显式配置且密钥 ≥32 字节、
  `allow_hs256` 为真时可用。
- `internal/platform/auth/jwtkeys.go`：JWKS 输出（`kty=OKP`、`crv=Ed25519`、
  `use=sig`、`alg=EdDSA`，按 `kid` 排序）与私钥种子 / 公钥列表的 base64 解析。
- identity 暴露 `/.well-known/jwks.json`（`Cache-Control: public, max-age=300`）；
  `RegisterRoutes` 接收 `TokenSource`，签发票据与公布公钥共用同一份密钥配置。
- 配置：`EXPENSIO_JWT_PRIVATE_KEY`（仅 identity，缺失时 identity 启动失败）、
  `EXPENSIO_JWT_PUBLIC_KEYS`（逗号分隔多公钥，支持轮换）、
  `EXPENSIO_JWT_ALLOW_HS256`。
- 密钥分发：`scripts/env.sh` 用 openssl 生成 Ed25519 密钥对；Compose 与 Helm
  都只给 identity 注入私钥，其余五个服务只挂公钥且关闭 HS256。

### 静态检查与全量回归（M9 最终代码）

```text
$ gofmt -l . && go vet ./... && go build ./...
GOFMT_EXIT:0 lines=0
VET_EXIT:0
BUILD_EXIT:0

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=1 -timeout=25m
23 个包全部 ok（identity 22.578s、e2e 29.129s、report 27.027s）；TEST1_EXIT:0 elapsed=38s

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=5 -timeout=25m
23 个包全部 ok；最长 internal/report 81.656s；TEST5_EXIT:0 elapsed=90s
```

### 运行期验证（六服务均为 m9 镜像且 healthy）

```text
=== JWKS（从 compose 网络内取；网关不代理该路径）===
FETCH_EXIT:0 bytes=168
  Cache-Control: public, max-age=300
keys: 1
kty=OKP crv=Ed25519 alg=EdDSA use=sig kid=7PJeSZ06dFOV… xlen=32 has_d=False

=== 通过网关注册新租户 ===
REGISTER_HTTP:201
alg: EdDSA | kid: 7PJeSZ06dFOV… | has_kid: True

=== EdDSA 令牌被验签服务接受 ===
expense:  200
identity: 200

=== 用旧 HS256 密钥伪造的令牌必须被网关拒绝 ===
gateway with HS256: 401
```

### 部署面

```text
$ helm lint deploy/helm/expensio
1 chart(s) linted, 0 chart(s) failed

$ helm template expensio deploy/helm/expensio --namespace expensio | grep -c '^kind:'
24

环境变量计数（默认 values）：PUBLIC_KEYS=6、PRIVATE_KEY=1、SECRET=1、ALLOW_HS256=6
identity：PRIVATE_KEY + PUBLIC_KEYS + SECRET（optional）+ ALLOW_HS256=true
其余五个：PUBLIC_KEYS + ALLOW_HS256=false
```

Compose 侧用 `docker compose --profile app config` 验证：identity 独有
`EXPENSIO_JWT_PRIVATE_KEY` 与旧 `EXPENSIO_JWT_SECRET`，其余五个服务只有公钥。

### 浏览器级 E2E

```text
✓ 未登录访问受保护路由会跳到登录页 (432ms)
✓ 注册 → 建单 → 提交 → 报表 → access token 失效后自动刷新 (3.1s)
2 passed (4.5s)
```

### 过程中发现的问题

- Compose v5.1.4 的 `config` 只渲染已启用 profile 的服务：不给 `--profile`
  时输出 `services: {}` 且退出码 0，容易被误读成"配置为空"。验收命令改为
  `--profile app`。
- Helm 的 identity 原先以非 optional 方式引用 `EXPENSIO_JWT_SECRET`，与
  NOTES 里"迁移期可选"矛盾；已加 `optional: true`。
- 并发跑六个镜像构建与 testcontainers 集成测试把 WSL2 / Docker 引擎拖垮
  （引擎 500、`Wsl/Service/0x8007274c`）；终止后台任务、`wsl --shutdown`
  后改为串行执行，全部检查通过。这是环境问题，不是代码问题。

### M9 未关闭项

- identity 的 HS256 兼容路径仍在（`allow_hs256=true` + 旧 secret），要等一个
  access TTL 后关闭并删除该密钥与代码分支；
  **M12 已删除该路径，见下文 M12 章节**；
- 多公钥并存的轮换演练未做（配置与 `kid` 选择已由单测覆盖）；
- JWKS 由 identity 直接提供，网关不代理 `/.well-known/jwks.json`；
- 风险 33（真实 SMTP）、风险 20/26（真实集群、CI runner）状态不变。

## M10 完成情况：成员邀请

设计取舍见 [ADR-0010](adr/0010-member-invitations.md)，本节只记录做了什么与
实际跑出了什么。

| 项 | 状态 | 说明 |
| --- | --- | --- |
| 邀请令牌 | 完成 | 32 字节 `crypto/rand` → base64url；库内只存 SHA-256；部分唯一索引限制每租户每邮箱一条待接受邀请 |
| 状态机 | 完成 | 待接受 / 已接受 / 已撤销 / 已过期；分别返回 `410 invitation.accepted|revoked|expired`，未知令牌 `404` |
| 接受流程 | 完成 | 新邮箱设置密码；已有账号必须输入原密码，复用原用户；租户事务内 `FOR UPDATE` 保证只成功一次 |
| 权限 | 完成 | owner/admin 可邀请，只有 owner 能邀请 owner；member/viewer 无创建/列表权限 |
| 邮件 | 完成 | identity → notification `POST /api/v1/internal/emails`（HMAC 短时令牌）→ `email_outbox` → worker；失败不回滚邀请 |
| 网关 | 完成 | `/api/v1/invitations` 公开 + IP 限流；`/api/v1/internal/*` 不转发 |
| 管理台 | 完成 | 成员页邀请弹窗、一次性链接复制、待接受列表与撤销；公开 `/invite/:token` 接受页。typecheck 与生产构建通过；邀请页已纳入 Playwright E2E（经 Mailpit API 取链），见本节末尾 |
| 审计 | 完成 | `member.invited`、`member.invitation_accepted`；元数据不含令牌/链接 |

### 自动化测试

```text
$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/identity/ -run TestInvitation -race -count=1 -v
--- PASS: TestInvitationLifecycleForNewUser (6.11s)
--- PASS: TestInvitationPermissions (1.20s)
--- PASS: TestInvitationReinviteAndRevoke (0.41s)
--- PASS: TestInvitationExpires (0.31s)
--- PASS: TestInvitationForExistingAccountRequiresPassword (1.01s)
--- PASS: TestInvitationTargetRules (0.62s)
--- PASS: TestInvitationUnknownToken (0.18s)
ok  	github.com/edgerunner-fris/expensio/internal/identity	11.398s

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/notification/ -run TestInternalEmail -race -count=1 -v
--- PASS: TestInternalEmailRequiresSignedTenantToken (2.84s)
--- PASS: TestInternalEmailQueuesUnderTenantFromToken (0.04s)
--- PASS: TestInternalEmailValidatesPayload (0.03s)
ok  	github.com/edgerunner-fris/expensio/internal/notification	4.416s

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=1 -p 1
24 个包 ok；EXIT=0

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=5 -p 1 -timeout=40m
24 个包 ok，FAIL 0 行；EXIT=0（report 59.014s）

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=5 -p 1 -timeout=30m
24 个包 ok；EXIT=0（identity 53.176s、report 53.450s）
```

`identity` 用例覆盖：新用户完整生命周期、owner/admin/member/viewer 权限、
重复邀请使旧链接失效、撤销 204/409/404、过期 410、已有账号的错误密码
401 与正确密码复用用户、已是成员/非法角色/非法邮箱、未知令牌。
`notification` 用例覆盖：无令牌/伪造/篡改签名均 401、签名中的租户决定
`email_outbox.tenant_id`、同租户同幂等键只写一行、跨租户互不可见、
非法收件人/空主题/空幂等键/非 JSON 均 400。

### 前端

```text
$ docker run --rm --network none -v .../frontend:/app -w /app node:24 \
    ./node_modules/.bin/vue-tsc --noEmit
TYPECHECK_EXIT=0

$ docker run --rm --network none -v .../frontend:/app -w /app node:24 \
    ./node_modules/.bin/vite build
dist/assets/AcceptInviteView-BIalC-12.js    3.50 kB │ gzip: 1.88 kB
dist/assets/MembersView-BHpd6kMI.js         7.74 kB │ gzip: 3.24 kB
✓ built in 2.00s

# 容器内的 web 服务实际返回了邀请页与对应 chunk
GET /invite/probe-token                      → 200 text/html
GET /assets/AcceptInviteView-BIalC-12.js     → 200 text/javascript
```

### 配置与部署面

```text
$ docker run --rm -v .../deploy/helm/expensio:/chart alpine/helm lint /chart
1 chart(s) linted, 0 chart(s) failed

$ helm template expensio ... | grep EXPENSIO_INVITE
  EXPENSIO_INVITE_BASE_URL: "https://expensio.example.com"
  EXPENSIO_INVITE_TTL: "168h"
模板 kind 数：24（与 M9 相同）

$ docker compose --profile app config（--format json 读取 identity.environment）
EXPENSIO_INVITE_BASE_URL = http://localhost:5173
EXPENSIO_NOTIFICATION_SERVICE_URL = http://notification:8085
```

### 整栈实跑（13 项全过）

脚本从 compose 网络内访问 `gateway:8080` 与 `mailpit:8025`，不打印任何
令牌或口令：

```text
PASS  注册新租户并拿到 owner 会话  status=201
PASS  创建邀请返回 201 且邮件已入队  status=201 email_queued=True
PASS  邀请链接只出现在创建响应里  link_present=True
PASS  Mailpit 收到邀请邮件并能取出链接
PASS  网关公开路由可查询邀请  status=200
PASS  接受邀请后拿到 member 会话  status=200 role=member
PASS  已接受的链接返回 410  status=410
PASS  新成员可用设置的密码登录  status=200
PASS  成员列表包含被邀请人  status=200 count=2
PASS  创建待撤销邀请  status=201
PASS  撤销邀请返回 204  status=204
PASS  撤销后的链接返回 410  status=410
PASS  网关不转发内部邮件接口  status=404

13 passed, 0 failed
```

### 过程中发现的问题

- **只重建部分镜像会留下旧二进制**：本地 `expense`/`billing`/`report` 镜像
  还是 2026-10-26 构建的版本，重启后不认识 EdDSA 公钥配置，报
  `jwt.secret 至少需要 32 字节` 并不断重启。重新构建这三个镜像后全部
  healthy。结论：本地整栈验收前用 `make app-up`（会构建全部服务），
  不要只 `docker compose build <个别服务>` 后重启。
- **集成测试的固定邮箱在 `-count=5` 下暴露**：`TestInvitationLifecycleForNewUser`
  第一轮把 `newbie@example.com` 注册成全局账号，第二轮同一邮箱就不再是
  "新用户"，`account_exists` 断言失败（identity 包 `-count=5` 失败）。
  改为每轮生成唯一邮箱后，该包 7 个邀请用例 `-count=5` 全绿（36.030s）。
  教训：共享 Postgres 的包内用例不能用固定邮箱表示"新用户"。
- **`--count=5` 全量回归**：修正测试邮箱后重跑，
  `24 个包 ok；EXIT=0`，无失败（identity 53.176s、report 53.450s）。
  第一次运行只失败 identity 一项，原因就是上面的固定邮箱。
- **前端 typecheck 的容器选择**：挂载的 `node_modules` 是在 glibc 环境安装的，
  `node:24-alpine` 缺 `@rolldown/binding-linux-x64-musl`，改用 `node:24`
  后构建成功。这是本地验收命令的细节，不影响 `Dockerfile.web`（它自己
  `npm ci`）。
- **E2E 连续运行暴露按 IP 限流失真**：邀请用例写完后用
  `--repeat-each=3` 连跑，第三轮的接受请求返回 429。查网关访问日志与
  trace 后确认：请求经 web 反代进入，网关 `ClientIP` 恒为 web 容器 IP，
  认证类限流实际是"整栈一个桶"；同时审计日志记不到真实来源。修复方式与
  实测见风险 37，最终 `client_ip` 变为 E2E 容器地址，9 个用例全绿。
- **member 角色不能读成员管理接口**：邀请用例最初在接受后直接用新会话
  打开 `/members` 断言邀请人邮箱，实际该接口只对 owner/admin 开放，
  页面拿到 403 并显示空表。改为退回 owner 登录后复核成员列表——这同时
  验证了两条会话路径。

### 浏览器级 E2E（2026-10-27）

`e2e/tests/support.ts` 提供每次运行唯一的租户注册与邀请链接路径提取；
`e2e/tests/invite-flow.spec.ts` 覆盖：

1. owner 在成员页创建邀请，结果弹窗给出一次性链接；
2. Mailpit（`E2E_MAILPIT_URL`）收到邮件，正文包含同一个链接；
3. 新用户在公开页设置姓名/密码后加入，会话切到被邀请租户且角色为成员；
4. 再次打开同一链接返回 410 "邀请已被使用"；
5. 退出后用新邮箱/密码/租户标识登录成功；
6. owner 重新登录后成员列表包含被邀请人。

```text
$ docker compose --profile app --profile e2e run --rm e2e \
    "npm ci --no-audit --no-fund && npx playwright test --repeat-each=3"
Running 9 tests using 1 worker
  ✓  1 [chromium] › tests/expense-flow.spec.ts:56:1 › 未登录访问受保护路由会跳到登录页 (364ms)
  ...
  ✓  9 [chromium] › tests/invite-flow.spec.ts:39:1 › 邀请成员 → 邮件送达 → 公开页接受 → 新成员可登录 (3.5s)

  9 passed (23.3s)
```

本地 `vite dev` 场景没有 Mailpit 时，用例仍跑完整 UI 流程，只跳过邮件
送达断言（`E2E_MAILPIT_URL` 为空）。

## M11 完成情况：SMTP STARTTLS、证书校验与认证

风险 33 此前的空白是协议层：`net/smtp` 的 STARTTLS、`PlainAuth` 与证书校验
从未对真实 socket 跑过，只有 Mailpit 明文收信。M11 用进程内真实 SMTP 服务
补测，不依赖外部供应商，也不打桩绕过 TLS。

| 项 | 状态 | 说明 |
| --- | --- | --- |
| 测试服务器 | 完成 | `fakeSMTPServer` 起真实 `127.0.0.1:0` 监听，实现 EHLO / STARTTLS / AUTH PLAIN / MAIL / RCPT / DATA / QUIT；AUTH 只在 TLS 之后公布，与真实服务器行为一致 |
| 证书 | 完成 | 每个用例生成自签 ECDSA P-256 证书（SAN = 127.0.0.1），测试通过 `MailerOptions.TLSConfig` 注入 RootCAs |
| 业务改动 | 完成 | `MailerOptions.TLSConfig` 可选；`startTLSConfig()` 在调用方配置上补齐 `ServerName` 与最低 TLS 1.2，传 `&tls.Config{}` 也不会降级 |
| 覆盖场景 | 完成 | ① STARTTLS → AUTH PLAIN → 正文送达；② `RequireTLS` 且服务器不公布 STARTTLS → 拒发；③ 证书不受信 → 握手失败 |

```text
$ go test ./internal/notification -run "TestSMTPMailer" -race -count=5 -v
--- PASS: TestSMTPMailerSendsOverSTARTTLSWithAuth (0.00s)
--- PASS: TestSMTPMailerRequiresStartTLSWhenConfigured (0.00s)
--- PASS: TestSMTPMailerRejectsUntrustedCertificate (0.00s)
...
--- PASS: TestSMTPMailerSendsOverSTARTTLSWithAuth (0.00s)
ok  	github.com/edgerunner-fris/expensio/internal/notification	1.183s
EXIT=0
```

用例断言的是错误文案与服务器侧事实（`gotAuth`、DATA 正文包含收件人与
中文主题），不是"调用没报错"。第三条用例同时确认客户端拒绝证书后服务端
goroutine 正常退出，不留悬挂连接。

仍未覆盖：真实供应商（SES / Postmark / 企业 SMTP）的地址、凭据、证书链
与反垃圾策略，属部署配置。接入时填 `EXPENSIO_NOTIFICATION_SMTP_ADDR` /
`_FROM` / `_USERNAME` / `_PASSWORD` 并把 `EXPENSIO_NOTIFICATION_SMTP_USE_TLS`
设为 `true`（Helm 对应 `notification.smtpAddr` / `smtpFrom` / `smtpUsername` /
`smtpUseTLS` 与 Secret 里的 `smtpPassword`）。

## M12 完成情况：删除 HS256 兼容路径

M9 迁移到 EdDSA/JWKS 时保留了迁移期的 HS256 开关，`jwt.allow_hs256`
的默认值甚至是 `true`。这个项目从未签发给旧版 HS256 令牌的用户，
"迁移窗口"没有实际对象，只留下一把共享密钥和一条对称验签分支——
任何拿到该密钥的服务都能伪造任意租户的令牌。M12 把这条路径删除。

| 项 | 状态 | 说明 |
| --- | --- | --- |
| 校验器 | 完成 | `IssuerConfig` 删除 `HS256Secret`；`ParseAccess` 的 `WithValidMethods` 只允许 `EdDSA`，HS256 令牌在算法检查处即失败 |
| 签发器 | 完成 | 没有 `PrivateKey` 时 `IssueAccess` 直接报错，不再退回 HMAC |
| 配置 | 完成 | 删除 `jwt.secret` / `jwt.allow_hs256`；验签方缺少 `jwt.public_keys` 拒绝启动，签发方缺少 `jwt.private_key` 拒绝启动 |
| 部署面 | 完成 | Compose、Helm、`.env.example`、`scripts/env.sh` 移除 `EXPENSIO_JWT_SECRET` / `EXPENSIO_JWT_ALLOW_HS256`；Helm `secretKeys` 删除 `jwt` 键 |
| 测试 | 完成 | `TestHS256TokensAreRejected` 用真实 HS256 签名令牌验证被拒；`TestMissingEd25519KeyMaterialIsRejected` 验证只设置共享密钥无法启动；五个集成 harness 改为临时 Ed25519 私钥 |

```text
$ go test ./internal/platform/auth/ ./internal/platform/config/ -race -count=1
ok  	github.com/edgerunner-fris/expensio/internal/platform/auth	1.031s
ok  	github.com/edgerunner-fris/expensio/internal/platform/config	1.033s
EXIT=0

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=1 -p 1
24 个包 ok；EXIT=0

$ docker run --rm -v .../deploy/helm/expensio:/chart alpine/helm lint /chart
1 chart(s) linted, 0 chart(s) failed

$ docker compose --profile app config -q
COMPOSE_EXIT=0

$ docker compose --profile app up -d --wait --wait-timeout 420
migrate / topics 退出 0；Postgres、Redis、Kafka、MinIO、六个服务与 web 全部 Healthy

$ docker compose --profile app --profile e2e run --rm e2e
  ✓  1 [chromium] › tests/expense-flow.spec.ts:56:1 › 未登录访问受保护路由会跳到登录页 (477ms)
  ✓  2 [chromium] › tests/expense-flow.spec.ts:62:1 › 注册 → 建单 → 提交 → 报表 → access token 失效后自动刷新 (3.9s)
  ✓  3 [chromium] › tests/invite-flow.spec.ts:39:1 › 邀请成员 → 邮件送达 → 公开页接受 → 新成员可登录 (5.4s)
  3 passed (10.7s)
```

整栈起来后还做了一次线上伪造验证：用 `.env` 里遗留的旧共享密钥按
`alg=HS256` 自签一个 claims 齐全的 access token，打
`GET /api/v1/expenses?limit=1`，网关返回 `401 auth.unauthorized`。
这证明新镜像里既没有该密钥，也没有对称验签分支——不是"代码里删了但
配置还在"。

升级提示：删除后旧的 HS256 access token 立即失效；access TTL 只有 15 分钟，
客户端用 refresh token 换新令牌即可（新令牌一定是 EdDSA），不需要强制
重新登录。仓库自建以来没有对外签发过 HS256 令牌，因此这里只影响假想的
迁移期部署。

## M13 完成情况：最终验收

### CI 整栈 Playwright（风险 26/36 的最后一环）

`ci.yml` 新增 `e2e` job，只在 main 分支、等 backend/frontend/images/web-image
全部成功后执行：登录 GHCR 拉取本次提交的七个镜像并打
`expensio/<svc>:<sha>` 本地标签 → `bash scripts/env.sh` 生成 `.env` →
`docker compose --profile app up -d --wait` → `docker compose run e2e`。
测的是即将部署的那批镜像，不在 runner 上重新构建。

实测 run `40494336695`（commit `556567a`）12 个 job 全部 success，其中
`浏览器端到端（整栈）` 从 11:16:06Z 到 11:17:54Z（1m48s）。收尾提交
`342b1f6` 的 run `40495335477` 同样 12 个 job 全部 success（含 kubeconform
与修复后的 web 镜像；e2e 11:23:56Z → 11:26:12Z，2m16s）。本地同一命令
序列的输出见 M12 章节。

### Helm 渲染结果的 schema 校验

`helm lint` / `helm template` 只能证明模板可渲染；字段名写错时 API server
会静默忽略未知字段。CI 的 helm job 追加 kubeconform（K8s 1.31 schema）：

```text
$ helm template expensio deploy/helm/expensio --namespace expensio \
    | kubeconform -strict -summary -kubernetes-version 1.31.0
Summary: 24 resources found parsing stdin - Valid: 24, Invalid: 0, Errors: 0, Skipped: 0
EXIT=0
```

这只证明字段合法，**不证明** RBAC、NetworkPolicy、Ingress 与 Hook Job 在
集群里的实际行为；集群 apply 仍未做。

### 界面预览与由此发现的一个缺陷

`e2e/capture/preview.spec.ts`（独立 config，`npx playwright test` 不会执行）
在真实整栈上注册租户、建 5 条预算、6 张费用单（提交 5 张、保留 1 张草稿）、
邀请 admin 并完成 3 通过 1 驳回，然后截取 8 个页面到 `docs/screenshots/`：

```text
$ make preview
Running 1 test using 1 worker
  ✓  1 [chromium] › capture/preview.spec.ts:74:1 › 生成界面预览图 (6.4s)
  1 passed
```

首次生成时截图里出现红色 toast「订阅不存在」。原因是前端 `BillingView` 把
404 的 `code` 写成了 `'not_found'`，而 `apperr.CodeNotFound` 实际是
`common.not_found`，于是新租户打开账单页会误报错。已改为
`common.not_found` 并加注释；重新构建 web 镜像后 toast 消失，截图已更新。

### 全量回归

```text
$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=5 -p 1 -timeout=40m
24 个包 ok，FAIL 0 行；EXIT=0
最慢：identity 81.371s、report 69.320s、expense 15.837s、web 1.114s
```

### 结论

- 本地整栈（六个服务 + web + PostgreSQL/Redis/Kafka/MinIO + Mailpit +
  观测）：健康检查全绿，Playwright 3 passed，`-race -count=5` 全量回归绿；
- CI：两次 main 运行各 12 个 job 全绿，包含整栈浏览器 E2E 与 kubeconform；
- 仍未验证：真实集群 apply、真实 SMTP 供应商、公钥在线轮换（见下一步）。

## M14 完成情况：三项最终验收

M13 留下的三项"未验证"都是"代码路径已有、但没对真实外部系统跑过"。这一轮
分别用真实第三方 SMTP 服务、真实 Compose 栈上的在线轮换、真实 k3d 集群上的
`helm upgrade --install` 补齐。三项都以脚本断言，不以人工观察为准。

### 1. 真实第三方 SMTP 供应商（风险 33）

用 Ethereal 一次性账号（`smtp.ethereal.email:587`，STARTTLS + AUTH PLAIN）
替换 Mailpit，向该账号的收件箱发一封真实的成员邀请邮件，再用
`imap.ethereal.email:993`（SSL）读回：

```text
register: 201
create_invitation: 201
email_queued: True
recipient: if***@ethereal.email
IMAP_RESULT: found
imap_server: imap.ethereal.email:993
uid: 1
subject: 加入 SMTP 验收 1793119493 的 Expensio 团队
from_domain: ethereal.email
link_in_body: True
date: Tue, 27 Oct 2026 16:44:59 +0000
```

数据库侧：notification 发件箱该行状态为 `sent`；服务日志出现
`"msg":"email sent"`。证书链与协议另用 openssl 对同一主机核验：

```text
subject=CN = ethereal.email
issuer=C = US, O = Let's Encrypt, CN = YE1
Protocol  : TLSv1.3
Cipher    : TLS_AES_256_GCM_SHA384
Verify return code: 0 (ok)
```

脚本只打印掩码后的收件人与邮件元数据，不打印口令、令牌或链接原文。验收后把
notification 切回本地 Mailpit（明文 1025），日常开发不依赖外网。这证明的是
`net/smtp` 的 STARTTLS、证书校验、AUTH PLAIN 与真实供应商可用；配额、退信、
SPF/DKIM 属运维配置，仍未验证。

### 2. 公钥在线轮换（风险 17）

在运行中的 Compose 整栈上做四阶段演练（步骤已写入
`docs/DEPLOYMENT.md` 的公钥轮换一节）。每一步用同一枚旧令牌和一枚新令牌打
六个入口（网关 `/me`、identity `/me`、expense、billing、report、
notification），要求全部一致：

| 阶段 | 变更 | JWKS | 旧令牌 | 新令牌 |
| --- | --- | --- | --- | --- |
| 0 | 基线：只有 A | `[A]` | 200 | — |
| 1 | 五个验签端加 B | `[A]`（identity 未动） | 200 | — |
| 2 | identity 私钥切 B | `[A,B]` | 200 | 200 |
| 3 | 全部只留 B | `[B]` | **401** | 200 |
| 回退 | 恢复原 `.env` | `[A]` | 200 | **401** |

实测 kid：A=`7PJeSZ06…`，B=`7fvHSEqx…`；`DRILL_RESULT: PASS`。阶段 1 的
顺序（先让所有验签方认识新公钥，再切签发方）避免了"签出来了但没人认"的窗口；
阶段 3 移除旧公钥后旧令牌立刻 401，说明不是靠 TTL 自然过期掩盖问题。收尾把
`.env` 恢复原状并重启六个服务，确认演练可回退。

### 3. 真实集群 Helm 安装（风险 20 / 30）

k3d v5.8.3（k3s v1.31.5，1 server + 1 agent）导入本地构建的八个镜像，
PostgreSQL / Redis / Kafka 以 `deploy/cluster/acceptance/deps.yaml` 部署，
再用 `helm upgrade --install --wait` 安装 chart：

```text
$ helm upgrade --install expensio deploy/helm/expensio -n expensio \
    --wait --timeout 15m -f deploy/cluster/acceptance/values.yaml ...
NAME: expensio
STATUS: deployed
REVISION: 1
```

随后运行 `deploy/cluster/acceptance/smoke.sh`，断言的是集群里的实际行为：

```text
expensio-gateway=2/2  expensio-web=2/2  其余 5 个 1/1
7 个 Deployment 全部 runAsNonRoot=true seccomp=RuntimeDefault ro=true ape=false
automountServiceAccountToken=false；can-i list/get pods = no
identity_db versions=1 … notification_db versions=1   （五个库）
expensio.expense.events.v1(+dlq)、expensio.billing.events.v1(+dlq) 已创建
剩余 Job 数=0（hook-succeeded 已清理成功的迁移/建主题 Job）
Ingress expensio CLASS=traefik HOSTS=expensio.test；GET /healthz -> 200
注册 → 邀请 → 接受 → 费用 → 提交 → 审批 → report currency_totals=1
default 命名空间 Pod → identity: blocked
同命名空间未打 release 标签的 Pod → identity: blocked
同 release 标签的 Pod → identity: {"status":"ok","service":"identity","version":"dev"}
SMOKE_RESULT: PASS
```

这次真装暴露了两个模板缺陷，都只有实际 `helm install` 才会出现：

1. 两个 Hook Job 引用了 chart 自己的 ServiceAccount。`pre-install` Hook
   早于普通资源创建，Pod 被拒：`serviceaccount "expensio" not found`。
   Hook 本身不需要 API 权限，已移除 `serviceAccountName`，保留
   `automountServiceAccountToken: false`。
2. Hook Job 没带 `imagePullSecrets`，私有仓库（GHCR）会拉取失败；已补齐。

验收所依赖的清单、values 与 smoke 脚本放在
[`deploy/cluster/acceptance/`](../deploy/cluster/acceptance/)，CI 的 `cluster`
job 在 main 分支用刚推送的镜像跑同一套流程。实测 run `40547326540`
（commit `9c3c0af`）13 个 job 全部 success，其中
`真实集群 Helm 安装（k3d）` 17:09:44Z → 17:11:40Z（1m56s），
`浏览器端到端（整栈）` 17:09:44Z → 17:11:47Z（2m03s），互不依赖、并行执行。

CI 的第一次集群运行（run `40546365312`）在“生成密钥并创建 Secret”步骤失败：
密钥只写进了 `$GITHUB_ENV`，而该文件里的值只在后续 step 生效，当前 step 在
`set -u` 下引用 `$JWT_PRIVATE_KEY` 立即以 unbound variable 退出。改为普通
shell 变量后通过——这也是本地手工演练不可能暴露的缺陷。

集群验收用的是 k3d 与单节点依赖；托管云、Ingress TLS 终止与多可用区仍未验证，
见 `docs/DEPLOYMENT.md`。

## M15 设计：附件、评论与分级审批

原始 brief 里还有三块没有实现：发票附件、审批沟通、按金额分级审批。
M15 补这三块，并加一个服务端过滤的审批待办。设计见
[ADR-0011](adr/0011-expense-attachments-comments-and-approval-levels.md)。

### API 契约

| 方法与路径 | 说明 |
| --- | --- |
| `POST /api/v1/expenses/:id/attachments/presign` | 签发 15 分钟 PUT URL |
| `POST /api/v1/expenses/:id/attachments` | 确认对象已上传并落元数据 |
| `GET /api/v1/expenses/:id/attachments` | 列表 + 10 分钟下载 URL |
| `DELETE /api/v1/expenses/:id/attachments/:attachmentID` | 软删（仅草稿提交人） |
| `GET /api/v1/expenses/:id/comments` | 评论列表 |
| `POST /api/v1/expenses/:id/comments` | 新增评论 |
| `DELETE /api/v1/expenses/:id/comments/:commentID` | 作者或管理员软删 |
| `GET /api/v1/expenses/:id/approvals` | 各级批准记录 |
| `GET /api/v1/expenses?inbox=true` | 审批待办（仅 admin/owner） |
| `GET /api/v1/approval-policies` | 审批策略（仅 admin/owner） |
| `PUT /api/v1/approval-policies/:currency` | 设置二级门槛 |
| `DELETE /api/v1/approval-policies/:currency` | 取消分级 |

费用单响应新增 `required_approval_levels` 与 `approval_level`，前端据此显示
"1/2 已批准"；状态机与事件类型不变。

### 验收计划

1. 迁移 000005 的 up/down 都真实执行，回滚后重建表结构；
2. 附件：真实 MinIO 容器，presign → PUT → confirm → list → GET 下载内容
   一致；非提交人、已提交单据、伪对象键、超限大小都被拒；
3. 评论：可见性、作者/管理员删除、长度边界；
4. 分级审批：门槛以下 1 级、以上 2 级且必须不同人；中间级不发事件、
   最后一级才结转预算；策略变更不影响在途单据；待办过滤正确；
5. 前端 `npm run build` 通过；完整 Go 回归 `-race -count=5`。

## M15 完成情况：附件、评论与分级审批（2026-10-28）

### 1. 迁移与数据约束

`TestMigrationDownAndUp` 对 000002–000005 连续回滚 4 步再重新 up：业务表
被删除后由迁移重建，`expense_status_transitions` 仍有 5 条种子；重新 up 后
`expense_approvals`、`expense_comments`、`expense_attachments` 三张表都实测
`relrowsecurity=true`、`relforcerowsecurity=true`；新建费用单的
`required_approval_levels=1`、`approval_level=0`。`000005` 的
`expenses_approval_level_consistency` 与独立触发器
`expenses_guard_approval_progress` 由数据库测试直接触发验证，不依赖服务层。

### 2. 附件（真实 MinIO 容器）

测试使用 testcontainers 启动真实 MinIO，完整链路
presign → 浏览器 PUT → confirm → list → GET → delete：

```text
--- PASS: TestAttachmentLifecycle (0.04s)
--- PASS: TestAttachmentRejectsInvalidMetadata (0.02s)
--- PASS: TestAttachmentAuthorizeAndLimits (0.04s)
--- PASS: TestAttachmentRejectsForeignObjectKey (0.02s)
--- PASS: TestAttachmentWithoutStoreReturns503 (0.01s)
--- PASS: TestHTTPAttachmentsAndCollaboration (0.83s)
--- PASS: TestHTTPAttachmentsWithoutStore (0.02s)
```

覆盖的断言：presigned PUT 的 `Content-Type` 进入签名；确认接口用
`StatObject` 核对大小与类型，声明 12 MiB、非白名单类型、含路径分隔符的
文件名都在落库前被拒；确认后列表返回可下载 URL，`GET` 到的是原始字节且
带 `Content-Disposition: attachment`；删除元数据后再取下载 URL 得到 404；
非提交人 403、已提交单据 409、第 11 个附件 409、伪造其他单据前缀的对象键
409；未配置对象存储的部署返回 503 `attachment_storage_unavailable`。

部署侧的关键修正：presigned URL 的 SigV4 签名包含 Host，容器内的
`minio:9000` 在浏览器里不可用。`objectstore.Config` 增加
`PublicEndpoint`，用独立客户端给浏览器地址签名；Compose 显式使用
`minio:9000` 作为内部地址、`localhost:9000` 作为签名地址。Helm 渲染实测
（`helm template | grep EXPENSIO_MINIO`）：

```text
EXPENSIO_MINIO_ENDPOINT: minio:9000
EXPENSIO_MINIO_PUBLIC_ENDPOINT: minio:9000
EXPENSIO_MINIO_BUCKET: expensio-invoices
EXPENSIO_MINIO_USE_SSL: "false"
```

### 3. 评论与审批待办

`TestCommentsLifecycle` / `TestCommentLengthAndVisibility`：正文首尾空白被
裁剪；提交后的单据仍可评论；作者和管理员可软删，其他成员删除 403；空正文
与 2001 字符被拒；不能查看该单据的成员看不到评论。

`TestHTTPAttachmentsAndCollaboration` 从 HTTP 层再走一遍评论、审批记录、
策略管理：普通成员写策略 403，管理员 upsert 后列表出现该币种，删除返回
204，评论删除返回 204。

### 4. 分级审批

`TestMultiLevelApproval` 用 50,000 的二级门槛提交 60,000 的单据：

- 提交后 `required_approval_levels=2`、`approval_level=0`；
- 第一位管理员批准后状态仍是 `submitted`、`approval_level=1`，
  `approval_steps` 没有 approve 记录，outbox 仍只有提交事件；
- 同一个人再批一级被 409 `duplicate_approver` 拒绝（数据库唯一约束）；
- 第二位管理员批准后才 `approved`、预算 `reserved → spent`、
  `expense_approvals` 两行、outbox 追加最终批准事件（共 2 条）。

`TestApprovalRejectsSelfApprovalAndBadLevels`：提交人自审 403
`self_approval`；直接 INSERT 第 2 级（跳过第 1 级）被触发器以 23514 拒绝。
`TestApprovalPolicySnapshotAndInbox`：策略只影响之后提交的单据；待办排除
自己提交的、排除自己已批过任何一级的、非管理岗访问 403；分页总数与列表
使用同一过滤条件。

### 5. 前端与全量回归

审批页（待办 + 策略管理）与费用详情页（审批时间线、附件上传/下载/删除、
评论区）通过 `vue-tsc --noEmit && vite build`，产物包含新的
`ApprovalsView-*.js` 与 `ExpenseDetailView-*.js`；构建在 `node:24`
容器内执行（本机 WSL 无 node，`node:24-alpine` 缺 glibc 版 rolldown 绑定）。

里程碑关门回归：

```text
$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/expense/... \
    ./internal/platform/objectstore/... ./internal/platform/config/... \
    -race -count=5
ok  github.com/edgerunner-fris/expensio/internal/expense            15.418s
?   github.com/edgerunner-fris/expensio/internal/platform/objectstore [no test files]
ok  github.com/edgerunner-fris/expensio/internal/platform/config     1.108s
```

第一次跑 `-count=5` 时暴露了一个真实回归：`TestStatusTransitionGuard`
直接用 SQL 把单据改成 `approved`，没有同步 `approval_level`，被 000005 的
一致性约束以 23514 拒绝。修正测试为同时写入级别，并为"跳过状态"断言
`invalid expense status transition` 报文，避免新约束掩盖状态机本身的失败。
这正是 `-count=5` 需要保留的原因——单次运行没有暴露，因为它依赖测试执行
顺序之外的迁移状态。

### 风险状态

| 风险 | 状态 | 说明 |
| --- | --- | --- |
| presigned URL 主机不可达 | 已解决 | 独立 `PublicEndpoint` 签名客户端；Compose 与 Helm 都实测渲染出正确值 |
| 同一人批两级 / 同一级批两次 | 已解决 | 两条唯一约束 + 触发器，集成测试实测 409 与 23514 |
| 中间级批准破坏读模型 | 已解决 | 中间级只推进 `approval_level`，不发事件、不写 `approval_steps`；最终级才结转预算 |
| 附件大小/类型/数量绕过 | 已解决 | 白名单 + 确认时 `StatObject` 复核；集成测试覆盖超限与伪键 |
| 孤儿对象 | 未解决 | 删除是尽力而为；生产需给桶配置生命周期规则，本地与验收环境未配置 |
| 附件内容安全扫描 | 未解决 | 仅按声明类型与大小校验，未接入病毒扫描；超出本阶段范围 |
| 租户只有一名管理员时分级的单据会停在 1/2 | 待验证 | 属运营约束，UI 已显示进度；未在真实租户上演练 |
| MinIO 故障时的启动/运行边界 | 待验证 | 启动 fail-fast、readiness 不探测 MinIO；运行中故障只影响附件接口，尚未做故障注入演练 |

## M16 设计：仪表盘、CSV 导出与 Redis 缓存

管理台目前只有报表页，没有一眼可见的运营概览；财务只能看聚合，拿不走明细；
summary 每次刷新都在事实表上现算。设计见
[ADR-0012](adr/0012-dashboard-export-and-report-cache.md)。

### API 契约

| 方法与路径 | 说明 |
| --- | --- |
| `GET /api/v1/reports/dashboard` | 币种/状态/类别/按日聚合，一次事务内四条查询 |
| `GET /api/v1/reports/expenses.csv` | 与 summary 相同过滤条件的明细导出，50,000 行上限 |

仪表盘与 summary 共用 `resolveScope`：管理岗默认全租户、成员默认本人、
非管理岗显式请求 `all` 返回 403。

### 缓存与失效

- 仪表盘结果缓存 5 分钟；键包含租户级版本号 `report:version:<tenant>`；
- 报表消费者成功处理事件后 `INCR` 版本号，旧键自然失效；
- Redis 读/写/取版本失败都回退到现算，只记 WARN；
- summary 与 CSV 不缓存，保持"提交后立即可查"的既有 E2E 断言。

### 预算告警

预算 API 已返回整数金额，前端据此计算已用比例：≥90% 显示"接近上限"，
≥100% 显示"已超限"，并汇总到仪表盘。提交时的条件 UPDATE 仍是唯一防线，
告警不参与额度判断。

### 验收计划

1. 仪表盘：两种币种、三种状态、跨日数据的金额与笔数逐项断言；成员只看
   自己，非管理岗请求 `all` 403；范围参数与 summary 同样校验；
2. CSV：表头、BOM、金额主单位与最小单位、过滤条件、行数上限与 403；
3. 缓存：真实 Redis，命中后不再查库（改库后仍返回缓存值）；消费者
   `INCR` 版本后读到新值；Redis 不可用时接口仍返回正确结果；
4. 前端仪表盘与预算告警通过 `vue-tsc --noEmit && vite build`；
5. 报表包 `-race -count=5`。

## M16 完成情况：仪表盘、CSV 导出与 Redis 缓存（2026-10-28）

### 交付内容

后端（`internal/report`）：

- [dashboard.go](../internal/report/dashboard.go)：`GET /api/v1/reports/dashboard`，
  一次事务内四条聚合（币种 / 状态 / 类别 / UTC 自然日），空结果返回 `[]`；
  校验与可见范围完全复用 summary 的 `newSummaryFilter`；
- [export.go](../internal/report/export.go)：`GET /api/v1/reports/expenses.csv`，
  与 summary 同过滤器，50,000 行上限（第 50,001 行触发 400），UTF-8 BOM
  `EF BB BF`，金额列同时给出主单位字符串与 `amount_minor`；
- [cache.go](../internal/report/cache.go)：租户版本号失效的仪表盘缓存，
  5 分钟 TTL，所有 Redis 错误回退现算；
- 消费者在事务提交后 `INCR report:version:<tenant>`；summary/CSV 不缓存。

其他：

- [money.go](../internal/platform/money/money.go)：新增 `FormatNumber`
  （只输出数字，不带币种），`Format` 改为委托它，既有输出不变；
- [docker-compose.yml](../docker-compose.yml)：report 服务补上
  `EXPENSIO_REDIS_ADDR: redis:6379`；Helm 的 ConfigMap 早已全局注入该键
  （`deploy/helm/expensio/templates/configmap.yaml:16`，Deployment 统一
  `envFrom`），`.env.example:31` 也已包含，无需改动；
- 前端：新增 `DashboardView.vue` 与 `/dashboard` 路由（`/` 仍重定向到
  `/expenses`，不改变既有 E2E 的落点）、菜单「概览」、报表页「导出 CSV」、
  预算页 ≥90% 告警横幅；折线图走已有的按需 `@/utils/charts`；
  导出请求单独 60s 超时，并把 400 返回的 JSON 信封从 Blob 里解出来，
  提示仍是服务端原文（如「超过 50000 行」）。

### 验收证据（实测）

测试清单：report 包共 19 个测试函数，其中 M16 新增 6 个——

| 用例 | 覆盖 |
| --- | --- |
| `TestDashboardAggregates` | 双币种、三状态、跨日数据的金额与笔数；成员 `scope=all` 403；缺参 400；空数组 |
| `TestExportCSV` | BOM、表头、主单位/最小单位、status 过滤、成员 403、非法参数返回 JSON 400 |
| `TestExportRejectsTooManyRows` | 50,001 行 → 400 且消息含 `50000` |
| `TestDashboardCacheInvalidationAndFallback` | 缓存命中（绕过消费者改库仍返回旧值）；`Bump` 后读到新值；Redis 不可达时回退现算 |
| `TestDashboardCacheSeparatesMembers` | 同租户成员 A/B 的 `scope=mine` 缓存互不串读（回归） |
| `TestConsumerBumpsDashboardCacheVersion` | 消费成功（含重复投递）后版本号 1 → 2 |

```text
$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/report/... ./internal/platform/money/... -race -count=5
ok  	github.com/edgerunner-fris/expensio/internal/report	54.992s
ok  	github.com/edgerunner-fris/expensio/internal/platform/money	1.013s
```

```text
$ gofmt -l ./internal ./cmd     # 无输出
$ go build ./...                # BUILD_OK
$ go vet ./internal/report/...  # VET_OK
```

前端（node:24 容器内，无 node 的 WSL 环境）：

```text
$ ./node_modules/.bin/vue-tsc --noEmit && ./node_modules/.bin/vite build
dist/assets/DashboardView-*.js   8.42 kB │ gzip:  3.76 kB
dist/assets/ReportsView-*.js     5.59 kB │ gzip:  2.74 kB
dist/assets/BudgetsView-*.js     9.11 kB │ gzip:  3.50 kB
✓ built in 623ms
```

### 过程中发现并修复的缺陷

仪表盘缓存键最初是 `report:dashboard:v<v>:<tenant>:<scope>:<from>:<to>`，
`scope=mine` 时**没有包含提交人**：同租户两个成员用同一时间范围查询会命中同一
键，后者的请求会读到前者的单据——这是跨用户的数据泄露，且版本号失效掩盖不了它
（键本身就不该共享）。

修复：`<scope>` 段改为 `<view>`，`mine` 时为 `mine:<user_id>`，`all` 时为
`all`。回归用例先对旧调用点执行，确认它确实失败：

```text
$ go test ./internal/report/ -run TestDashboardCacheSeparatesMembers -count=1
Error: Not equal:
  expected: 3000
  actual  : 10000
FAIL	github.com/edgerunner-fris/expensio/internal/report	5.366s
```

恢复修复后该用例通过，并包含在 `-race -count=5` 的 54.992s 里。ADR-0012
的缓存键说明与风险清单已同步更新。

### 风险状态

| 风险 | 状态 | 依据 |
| --- | --- | --- |
| 成员视图缓存串读 | 已解决 | 键含提交人；回归用例对旧实现实测失败、新实现通过 |
| Redis 故障阻断仪表盘 | 已解决 | 所有 Redis 错误回退现算；不可达地址实测返回 17000 而非错误 |
| CSV 悄悄截断 | 已解决 | 第 50,001 行返回 400，消息含 `50000` |
| Excel 打开中文乱码 | 已解决 | 响应体前 3 字节为 `EF BB BF`，用例断言 |
| 大租户下四条聚合的延迟 | 待验证 | 未压测，不宣称高并发指标；命中缓存后不再扫表 |
| 多副本 report 的缓存一致性 | 待验证 | 依赖所有副本连同一个 Redis；Helm 上是同一 `redis.addr`，未做集群实测 |
| 预算告警被误当成额度校验 | 设计如此 | 告警仅前端展示，提交时仍是条件 UPDATE 唯一防线 |

## M17：租户切换、个人设置、审计日志页与登录失败锁定

账号本来就是全局的（`users`）+ 多租户成员关系（`memberships`），但登录只按
slug 进入，没有切换入口；审计接口已有、页面缺失；没有自助改资料/改密码；
登录失败只有网关按 IP 限流。设计见
[ADR-0013](adr/0013-tenant-switch-settings-audit-lockout.md)。

### API 契约

| 方法与路径 | 说明 |
| --- | --- |
| `GET /api/v1/auth/tenants` | 我的全部成员关系（含角色、状态、是否当前） |
| `POST /api/v1/auth/switch-tenant` | `{tenant_id}` → 新租户的完整会话 |
| `PATCH /api/v1/me` | `{display_name}`，全局账号资料 |
| `POST /api/v1/auth/change-password` | 校验当前密码；成功后撤销全部刷新链 |
| `GET /api/v1/audit-logs` | 已有；补 `ip` / `user_agent` 读字段与前端页面 |

### 关键约束

- 「列出租户」走 memberships 上仅 SELECT 的 `self_membership_read` 策略，
  键 `app.user_id` 只由 `db.WithinUser` 从 JWT 的 sub 写入。初版尝试的
  SECURITY DEFINER 函数在 `FORCE RLS` 下对表属主同样失效（集成测试实测返回
  0 行），已放弃；写入仍只受 `tenant_isolation` 约束；
- 切换不撤销旧刷新链（其他设备上的会话不受影响），登出仍按链撤销；
- 改密成功撤销该用户全部刷新链，前端回登录页；
- 锁定：15 分钟内 5 次凭证错误 → 429，锁 15 分钟；不存在的邮箱同样计数，
  避免把锁定状态变成账号探测点；成功登录清零。

### 验收计划

1. 双租户用户：列表两条、当前标记正确；切换到另一租户后 `/me` 的租户与角色
   随成员关系变化；非成员 403、切到当前 400、暂停租户 403；审计可见；
2. 改名：`/me` 生效、审计有记录、超长/空白 400；
3. 改密：错误当前密码 401；成功后旧刷新令牌 401、旧密码 401、新密码可登录；
4. 锁定：连续 5 次错误后 429、锁定期内正确密码仍 429、用 SQL 拨回过期时间后
   可登录、4 次失败后成功会清零；
5. 前端（切换菜单、设置页、审计页）`vue-tsc --noEmit && vite build`；
6. identity 包 `-race -count=5`。

### 实现

| 层 | 文件 | 内容 |
| --- | --- | --- |
| 迁移 | `migrations/identity/000004_tenant_switch` | memberships 上仅 SELECT 的 `self_membership_read` 策略 |
| 迁移 | `migrations/identity/000005_login_lockout` | `app.login_failures`（无 RLS、邮箱主键、锁定部分索引、`touch_updated_at` 触发器） |
| 平台 | `internal/platform/db/db.go` | `WithinUser`：事务级设置 `app.user_id`；`inTx` 同时携带 tenant/user 两个 GUC |
| identity | `account.go` / `account_repository.go` | 列表（普通 join，靠策略收敛）、切换、改名、改密（两段事务 + CAS）、登录失败计数 SQL |
| identity | `lockout.go` | 5 次 / 15 分钟窗口 → 15 分钟锁定；429 `auth.too_many_attempts`；计数失败只记日志 |
| identity | `handler_account.go` / `routes.go` | 三个新端点 + `PATCH /me`；`GET /auth/tenants` 返回 `{"items": [...]}` |
| identity | `service.go` | 锁定检查前置于密码校验（仍跑 dummy argon2）、成功登录清零、注册复用展示名校验 |
| identity | `model.go` / `repository.go` | 审计读模型补 `ip`（`host(ip)`）与 `user_agent` |
| 网关 | `internal/gateway/gateway.go` | 三条已认证、按租户限流的规则排在 `/api/v1/auth` 之前 |
| 前端 | `AppLayout.vue` | 租户下拉（当前项与停用项禁用）、`router-view :key="tenant_id"` 强制重建、账号设置入口 |
| 前端 | `SettingsView.vue` / `AuditLogView.vue` / `router` | 资料与改密表单、审计表格（展开 metadata）、路由与管理员菜单 |

### 验收（2026-10-28 实测）

```text
$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/identity/ \
    -run 'TestTenantSwitch|TestTenantListRequiresAuth|TestUpdateProfile|TestChangePassword|TestLoginLockout' -count=1
ok  	github.com/edgerunner-fris/expensio/internal/identity	7.151s

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/identity/ -race -count=5 -timeout 20m
ok  	github.com/edgerunner-fris/expensio/internal/identity	88.466s

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/gateway/ ./internal/platform/db/ -count=1
ok  	github.com/edgerunner-fris/expensio/internal/gateway	3.185s
?   	github.com/edgerunner-fris/expensio/internal/platform/db	[no test files]

$ gofmt -l internal/identity internal/platform/db && go build ./... \
    && go vet ./internal/identity ./internal/platform/db
（无输出）

# 前端在 node:24 容器内执行（WSL 里的 node 是 Windows 版，原生绑定不可用）
$ docker run --rm --network none -v .../frontend:/app -w /app node:24 \
    ./node_modules/.bin/vue-tsc --noEmit
TYPECHECK_EXIT=0
$ docker run --rm --network none -v .../frontend:/app -w /app node:24 \
    ./node_modules/.bin/vite build
dist/assets/AuditLogView-DW1lyo0s.js              … kB
dist/assets/SettingsView-BPbUloM-.js              … kB
✓ built in 1.18s
```

新增 5 个集成用例：`TestTenantSwitch`（列表、切换、`/me`、审计、旧链仍可刷新、
切回当前 400、非成员 / 暂停租户 / 停用成员关系 403）、`TestTenantListRequiresAuth`、
`TestUpdateProfile`（含审计 `ip`/`user_agent`）、`TestChangePassword`（旧链撤销、
旧密码失效、新密码可登录）、`TestLoginLockout`（第 5 次 429、锁定期内正确密码
仍 429、不存在的邮箱同样锁定、SQL 拨回解锁时间、成功清零）。

### 失败与修正：FORCE RLS 让 definer 函数失效

初版按 `app.find_invitation` 的模式实现 `app.user_tenants(uuid)`（SECURITY
DEFINER，属主 `identity_migrator`）。`TestTenantSwitch` 在
`require.Len(t, list.Items, 2)` 处实际拿到 0 条：`memberships` 是 `ENABLE + FORCE`
RLS，**FORCE 对表属主同样生效**，definer 函数也读不到跨租户的行；`invitations`
只 `ENABLE`，所以 `find_invitation` 能用，两者前提不同。

修正为：migration `000004` 只增加一条仅 SELECT 的宽松策略
`self_membership_read`（`user_id = app.user_id`），新增 `db.WithinUser` 在事务内
从 JWT 的 `sub` 设置 `app.user_id`。因为没有同时设置 `app.tenant_id`，该事务里
其他受 RLS 约束的表（如 `audit_logs`）仍然读不到任何行；写入仍只受
`tenant_isolation` 的 `WITH CHECK` 约束。改完后同一用例通过。

### 风险

| 风险 | 状态 | 说明 |
| --- | --- | --- |
| FORCE RLS 让 definer 函数跨租户读失效 | 已解决 | 集成测试实测 0 行；改为 SELECT-only 策略 + `WithinUser` |
| 账号级锁定可被用于拒绝服务 | 待验证 | 15 分钟自动解锁、网关按 IP 20/min、成功清零；未做并发压测 |
| `login_failures` 无清理任务 | 未解决 | 不存在的邮箱也会留行；网关限流约束增长速度，清理任务待排期 |
| `change-password` 不累计锁定 | 未解决 | 持有 access token 仍可试当前密码，受 argon2 与 1200/min 约束 |
| 同一浏览器多标签共享会话 | 设计如此 | 切换后以最后一次为准；服务端不撤销旧链，其他设备不受影响 |
| 账号级操作只写当前租户审计 | 设计如此 | 跨租户账号审计表未做 |
| 锁定与网关限流的叠加行为 | 待验证 | 只做了功能测试，未做 5 次锁定 × 20/min 的联合压测 |

## M18：自动续费、模拟支付、租户删除、熔断与日志聚合

M17 之后 billing 只有"创建订阅 + 首期账单 + 回调"，周期不会前进；支付只能
手工构造 HMAC；租户无法注销；网关持续向已挂掉的下游转发；日志只进 Docker
json-file、无法按 trace 检索。设计见
[ADR-0014](adr/0014-renewal-mockpay-deletion-circuitbreaker-loki.md)。

### 交付内容

| 层 | 文件 | 内容 |
| --- | --- | --- |
| 迁移 | `migrations/billing/000003_renewal.{up,down}.sql` | `anchor_day` 列与填充触发器、只允许连续前进的周期守卫、无 RLS 的 `app.renewal_schedule` 目录表 + 同步触发器、`app.due_subscriptions()` definer 函数 |
| billing | `internal/billing/period.go` | `NextPeriodStart`：锚点日按 `min(anchor_day, 月末)` 落到目标月；`PeriodEnd` 复用它 |
| billing | `internal/billing/repository.go` | `ListDueSubscriptions` / `HasOpenInvoice` / `RolloverSubscription`（CAS on version） |
| billing | `internal/billing/renewal.go` | `Service.RunRenewal`、`RenewalJob`（启动即跑 + ticker，多副本安全）、`expensio.billing.renewal.results{outcome}` 指标 |
| billing | `internal/billing/service.go` / `cmd/billing/main.go` | 创建订阅写入 `AnchorDay`；装配续费任务并随进程优雅停止 |
| 平台 | `internal/platform/config/config.go` / `cmd/gateway/main.go` | `billing.renewal_interval` / `renewal_batch`、`circuit_breaker.*` 配置与校验 |
| CLI | `cmd/mockpay/` | 可重复的支付回调模拟器：仅从环境变量读密钥、时间戳 + 原始报文 HMAC、非 2xx 退出码 1 |
| identity | `internal/identity/tenant_delete.go` / `account_repository.go` / `handler.go` / `routes.go` | `DELETE /tenants/current`：owner + slug 二次确认，软删租户与成员、撤销全部刷新令牌与待接受邀请、审计 `tenant.deleted` |
| 网关 | `internal/gateway/circuitbreaker.go` / `gateway.go` | 每下游独立 `gobreaker`：连续 5 次失败打开、30s 半开、503 + `Retry-After`、状态/拒绝指标 |
| 平台 | `internal/platform/httpx/middleware/accesslog.go` | 访问日志写入 `trace_id` / `span_id`（来自 OTel span 上下文） |
| 观测 | `deploy/local/loki/config.yaml`、`deploy/local/alloy/config.alloy`、`deploy/cluster/logging/` | 本地 Loki + Alloy；生产 Loki（S3 后端）+ Alloy DaemonSet values；Grafana 数据源与双向跳转 |
| 前端 | `SettingsView.vue` / `api/auth.ts` / `stores/auth.ts` | 所有者专属"删除租户"危险区：slug 回写 + 二次确认，成功后清本地会话回登录页 |

### API / CLI 契约

| 接口 | 说明 |
| --- | --- |
| `DELETE /api/v1/tenants/current` | 仅 owner；body `{"slug": "<当前租户标识>"}`；成功 `{"sessions_revoked": N}`；不匹配 400、重复删除 404 |
| `mockpay --url --invoice --event --payment-id --event-id --amount --currency` | `--event` 取 `succeeded` / `failed`；密钥 `EXPENSIO_BILLING_WEBHOOK_SECRET`、provider `EXPENSIO_BILLING_WEBHOOK_PROVIDER` |

### 关键约束

- 锚点日由首期起点决定并冻结；周期只允许 `new_start = old_end + 1` 的连续前进；
- 当前周期有未支付账单时**不续费**，避免每轮叠加新账单；补缴后由后续轮次追平，
  每轮每个订阅只前进一期；
- 跨租户到期扫描不绕过 RLS：目录表无 RLS，只读函数 `REVOKE ALL` + 仅
  `GRANT EXECUTE` 给 `billing_app`，处理后仍以 `WithinTenantID` 进入租户上下文；
- 熔断只对网络错误与 502/503/504 计数；500 与客户端取消不计入；每个下游独立；
- 删除租户不物理删除数据：软删除 + 断链，清理属于运维流程；
- 日志采集不参与告警，告警仍全部基于指标。

### 验收（2026-10-28 实测）

```text
$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/billing/... ./internal/identity/... \
    -race -count=5 -timeout 30m
ok  	github.com/edgerunner-fris/expensio/internal/billing	36.582s
ok  	github.com/edgerunner-fris/expensio/internal/identity	150.807s
RACE_EXIT=0

$ go test ./internal/gateway/... ./internal/platform/config/... \
    ./internal/platform/httpx/... ./cmd/mockpay/... -race -count=1
ok  	github.com/edgerunner-fris/expensio/internal/gateway	5.769s
ok  	github.com/edgerunner-fris/expensio/internal/platform/config	1.113s
ok  	github.com/edgerunner-fris/expensio/internal/platform/httpx/middleware	1.028s
ok  	github.com/edgerunner-fris/expensio/cmd/mockpay	1.048s

$ gofmt -l internal cmd   # 无输出
$ go build ./... && go vet ./...
BUILD_OK
VET_OK

$ helm lint deploy/helm/expensio
1 chart(s) linted, 0 chart(s) failed

$ docker compose --profile app run --rm migrate
==> migrate identity
4/u tenant_switch (54.841026ms)
5/u login_lockout (150.47931ms)
==> migrate billing
3/u renewal (128.644906ms)

$ curl -s http://127.0.0.1:3100/ready
ready

$ curl -s http://127.0.0.1:3100/loki/api/v1/label/service/values
{"status":"success","data":["alloy","billing","expense","gateway","grafana",
 "identity","kafka","loki","mailpit","minio","notification","otel-collector",
 "postgres","prometheus","redis","report","tempo","web"]}
```

Loki 查询到重建后的 identity 访问日志，`trace_id` 与 Tempo 中的同一条
trace 一致（Tempo `GET /api/traces/<id>` 返回 200，span 名
`POST /api/v1/*path`）：

```text
trace_id=7a61cefb9ea7f5dceb28c88ec76225cd span_id=170065e28a2c0b58 status=401 path=/api/v1/auth/login
```

前端：`npm run typecheck` 退出码 0，`npm run build` 输出 `✓ built`
（Node 24 容器；WSL 内的 `node` 是 Windows 二进制，不能直接跑）。

### 过程中发现并修复的问题

1. **Loki 3.5.0 拒收全部日志**：ingester 持续报
   `negative structured metadata bytes received`，`/ready` 永不就绪。换成
   3.5.1 后同一配置 0 条该错误，日志正常写入；镜像标签已固定为 3.5.1。
2. **访问日志测试对"字段缺席"的断言写错**：`observer.ContextMap()` 对缺失
   键返回 `nil` 而不是空字符串，改为 `NotContains`；实现本身无需改动。
3. **删掉的目录表状态**：运行中的 compose 数据库只到 identity 4 / billing 2，
   新代码启动后登录报 500（`relation "app.login_failures" does not exist`）。
   这个问题正是通过 Loki 里的 `error` 字段定位的；重跑迁移后恢复 401。
4. **`go mod download` 首次失败**：容器内下载 `gobreaker` 时瞬时失败，
   复跑即成功，未改构建逻辑；构建失败时 `BUILD_EXIT` 非 0，不会被当成成功。

### 风险

| 风险 | 状态 | 说明 |
| --- | --- | --- |
| 续费每轮只追平一期 | 设计如此 | 长期停机需多轮；每期独立账单，不会漏账 |
| 欠费阻塞续费 | 设计如此 | 补缴后下一轮追平；`outcome=unpaid` 指标可发现长期欠费 |
| 删除租户后 access token 最长 15 分钟仍可用 | 未解决 | 无状态验签与"立即失效"冲突；刷新链已全部撤销 |
| 软删除不等于合规擦除 | 未解决 | 物理清理/导出保留策略属于运维流程，本期未实现 |
| `renewal_schedule` 目录可能漂移 | 待验证 | 与订阅同事务由触发器同步；尚无定期对账任务 |
| 熔断阈值是常量 | 待验证 | 5 次 / 30s 未按流量自适应；调参依据是 `circuit.transitions` |
| 本地 Alloy 挂载 `docker.sock` | 设计如此 | 仅开发；生产 values 不挂套接字或宿主目录 |
| Loki 生产单副本 | 设计如此 | 数据在对象存储，Pod 重建不丢；查询会短暂不可用 |
| `mockpay` 不做自动重试 | 设计如此 | 非 2xx 退出码 1，是否重试由调用脚本决定 |

## M19：OpenAPI 契约、种子数据、安全扫描与发布流水线

M18 结束时，接口只存在于 Go 路由和前端调用里：调用方没有可读的契约，路由
漂移也没有测试能发现。演示数据要靠手工点页面，无法在 CI 或新环境重现。
依赖漏洞只在本地偶尔跑一次，镜像里的旧 Go 标准库和 grpc 则一直带着已知
CVE。发布只有 CI，没有 tag 触发的镜像与部署。

设计取舍见
[ADR-0015](adr/0015-openapi-seed-security-and-release.md)。

### 交付内容

| 层 | 文件 | 内容 |
| --- | --- | --- |
| 契约 | `api/openapi.yaml` | OpenAPI 3.0.3：50 条路径 / 62 个操作 / 92 个 schema，统一成功与错误信封、Bearer 安全方案、分页参数 |
| 契约测试 | `internal/contract/openapi_test.go` | 从真实 gin 路由（含五个服务的 `RegisterRoutes`）提取端点，与规范双向比对：文档多写的、代码多写的都失败 |
| 契约测试 | `internal/contract/gateway_test.go` | 规范中的每个路径都必须被网关 `DefaultRules()` 转发；每条网关规则也必须对应文档化端点，防止"服务健康但网关 404"和残留规则 |
| 种子 | `cmd/seed/main.go` | 全部经公开 HTTP API 造数（真实鉴权、RLS、状态机、预算预占），口令只读 `EXPENSIO_SEED_PASSWORD`；预算按名称+期间、费用单按 seed 标记、成员按邮箱幂等匹配，账单已有 open 时复用 |
| 种子 | `cmd/seed/main_test.go`、`Makefile` | 单元测试覆盖匹配与幂等判定；`make seed` 入口 |
| 安全 | `.github/workflows/ci.yml` | 新增 `security` job：`govulncheck ./...`，任何**代码可达**漏洞让流水线失败 |
| 发布 | `.github/workflows/cd.yml` | tag `v*` 触发：verify（gofmt/vet/build/全量 race 测试/前端构建/OpenAPI lint）→ images（八镜像矩阵，SBOM）→ deploy（Helm，`environment: production`，KUBE_CONFIG 未配置时明确跳过并说明原因） |
| 镜像 | `Dockerfile.migrate` | 改为从源码编译 golang-migrate v4.18.3，去掉上游镜像里旧 Go 构建的标准库 CVE；运行时 alpine 3.22 + `USER 65534` |
| 镜像 | `Dockerfile` / `Dockerfile.web` / `go.mod` | Go 1.27.3；otel 1.45.0、grpc 1.83.2、x/net 0.62.0、quic-go 0.59.1、moby/go-archive 0.3.0 |
| 运维 | `scripts/backup-drill.sh` | 逐库 `pg_dump` → 恢复到 `<db>_drill` → 比对表集合、总行数、迁移版本 → 删除临时库；失败退出码非 0 |
| 文档 | `docs/INTERVIEW.md`、`docs/DEPLOYMENT.md`、`README.md`、`docs/ARCHITECTURE.md` | 项目讲解与演示脚本、tag 发布流程与回滚、演示数据说明、M19 里程碑状态 |

### 关键约束

- OpenAPI 是**被测对象**，不是注释：契约测试直接调用各服务的路由注册函数，
  规范与代码任一方向漂移都会失败；
- 种子数据必须走公开 API，不允许直连数据库，否则绕过 RLS 与状态机的演示
  数据没有说服力；
- 种子口令只从环境变量读取，不提供命令行参数（避免进入 shell 历史与 `ps`）；
- CD 的 deploy 段在 `KUBE_CONFIG` 缺失时不会假装成功：跳过并输出原因；
- 备份脚本只验证逻辑备份可恢复，不等于生产备份方案（生产由对象存储 + PITR
  负责）。

### 验收（2026-10-28 实测）

```text
$ go version
go version go1.27.3 linux/amd64

$ gofmt -l internal cmd          # 无输出
$ go build ./... && go vet ./...
BUILD_OK
VET_OK

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=1 -timeout 30m
（全部包 ok，含 internal/contract、internal/e2e、internal/gateway、cmd/seed）
FULL_EXIT=0

$ govulncheck ./...
No vulnerabilities found.
Your code is affected by 0 vulnerabilities.

$ actionlint                     # 三个 workflow，退出码 0
$ docker run --rm -v "$PWD:/spec" -w /spec redocly/cli:1.34.3 lint api/openapi.yaml
Woohoo! Your API description is valid. 🎉
paths=50 operations=62 schemas=92
```

镜像扫描（`trivy image --severity HIGH,CRITICAL --ignore-unfixed`）：

```text
expensio/identity:dev      (alpine 3.22.7)  Total: 0 (HIGH: 0, CRITICAL: 0)
expensio/expense:dev       (alpine 3.22.7)  Total: 0 (HIGH: 0, CRITICAL: 0)
expensio/billing:dev       (alpine 3.22.7)  Total: 0 (HIGH: 0, CRITICAL: 0)
expensio/report:dev        (alpine 3.22.7)  Total: 0 (HIGH: 0, CRITICAL: 0)
expensio/notification:dev  (alpine 3.22.7)  Total: 0 (HIGH: 0, CRITICAL: 0)
expensio/gateway:dev       (alpine 3.22.7)  Total: 0 (HIGH: 0, CRITICAL: 0)
expensio/web:dev           (alpine 3.22.7)  Total: 0 (HIGH: 0, CRITICAL: 0)
expensio-migrate:m19                        Total: 0 (HIGH: 0, CRITICAL: 0)
```

重建前后对比：旧镜像有 `google.golang.org/grpc v1.82.0` 的
`CVE-2026-84304`（HTTP/2 DATA 帧碎片化 DoS，HIGH），升级到 1.83.2 并
用 Go 1.27.3 重建后消失。重建后的整栈以新镜像重启，七个服务全部 healthy，
种子数据重跑输出现有账单号未变、新建 0：

```text
$ docker compose --profile app up -d --no-build --wait --wait-timeout 420
compose_exit=0
expensio-identity-1 ... (healthy)   expensio-gateway-1 ... (healthy)
expensio-expense-1 ... (healthy)    expensio-web-1 ... (healthy)
expensio-billing-1 ... (healthy)    expensio-report-1 ... (healthy)
expensio-notification-1 ... (healthy)

$ EXPENSIO_SEED_PASSWORD=*** go run ./cmd/seed
seed: 完成
  租户      demo-seed（demo-seed）
  预算      4 个（本次新建 0）
  费用单    4 张（本次新建 0）：已报销 1、已批准 1、待审批 1、已驳回 1
  账单      INV-202610-2B753BAD / USD 14500 / 到期 2026-11-11（open）
```

文件系统扫描（`trivy fs --scanners vuln,misconfig,secret --severity
HIGH,CRITICAL --ignore-unfixed`）：仓库自身 Go 依赖与密钥检查无发现，剩余
12 条 HIGH 全部在 `deploy/cluster/acceptance/deps.yaml`：

```text
deploy/cluster/acceptance/deps.yaml (kubernetes)
Tests: 35 (SUCCESSES: 23, FAILURES: 12)
AVD-KSV-0014 (HIGH) ×4：postgres / redis / kafka / minio 未设 readOnlyRootFilesystem
AVD-KSV-0118 (HIGH) ×8：上述 Deployment 与容器未设 securityContext
```

这 12 条是**已接受**的：该文件只是 k3d 验收用的第三方演示依赖，生产由托管
PostgreSQL / Kafka / 对象存储替代；应用自身 chart 的 pod 与容器安全上下文
（`runAsUser: 10001`、禁止提权、`readOnlyRootFilesystem: true`）在
`deploy/helm/expensio/values.yaml` 中已配置。

备份恢复演练：

```text
$ bash scripts/backup-drill.sh
==> identity_db
    OK dump=159128B tables=8 rows=466 schema_version=5:false
==> expense_db
    OK dump=746891B tables=10 rows=1306 schema_version=5:false
==> billing_db
    OK dump=31242B tables=10 rows=25 schema_version=3:false
==> report_db
    OK dump=145805B tables=3 rows=757 schema_version=2:false
==> notification_db
    OK dump=312287B tables=4 rows=1072 schema_version=2:false
BACKUP_DRILL: PASS
```

### 过程中发现并修复的问题

1. **网关漏配 `/api/v1/approval-policies` 导致 404**：服务健康、OpenAPI 有
   文档、直连 expense 正常，只有经过网关才 404，最容易被忽略。补规则的同时
   新增规范 ↔ 网关的双向契约测试；测试还发现一条早已删除的
   `/api/v1/invoices` 残留规则，一并删除。
2. **镜像里的旧标准库与 grpc**：Trivy 在旧镜像上报 Go 标准库与 grpc 1.82.0
   的 HIGH（有修复版本）。把所有镜像的 Go 固定到 1.27.3 并升级模块后重建，
   八镜像扫描归零；`Dockerfile.migrate` 不再使用上游预编译二进制。
3. **备份脚本的口令变量被外层 shell 吞掉**：嵌套 `bash -c` 里 `"$SUPERUSER"`
   被外层展开成空串，`psql -U ""` 报错。改为脚本文件传参后复现并修正；
   同时把 psql 的 NOTICE 噪音重定向，结果行不再与数据库名交错。
4. **Trivy fs 扫描超时**：直接扫 WSL 9p 挂载下的仓库时卡在 `node_modules`
   的目录遍历，不是扫描本身的问题；改为先 `tar` 排除重目录再扫。
5. **种子脚本重复执行**：第一次 404 是因为运行中的 expense 容器还是旧代码
   （`approval-policies` 路由是新加的），重建容器后通过；第二次执行新建 0，
   账单号不变，确认幂等。

### 风险

| 风险 | 状态 | 说明 |
| --- | --- | --- |
| `cd.yml` 的 deploy 段未在真实集群执行 | 待验证 | 需要 `KUBE_CONFIG` 与目标集群；verify 与 images 两段可在任何 tag 上运行 |
| 验收依赖清单 12 条 HIGH 未修 | 已接受 | 第三方演示镜像需要可写根目录；生产用托管服务替代，应用 chart 已加固 |
| 镜像扫描使用 `--ignore-unfixed` | 设计如此 | 没有修复版本的 CVE 无法通过升级消除；`govulncheck` 单独保证代码可达漏洞为零 |
| 备份演练只覆盖逻辑备份 | 待验证 | 未覆盖 PITR、对象存储跨区与恢复演练；属于托管云环境（M20） |
| 种子数据是演示用 | 设计如此 | 依赖运行中的完整栈；口令共享，不适合真实租户环境 |
| OpenAPI 只描述 HTTP 契约 | 设计如此 | 不描述 Kafka 事件 schema；事件结构由共享的 `internal/platform/events` 锁定 |

## M20：托管 Kafka/PostgreSQL 认证、HPA 与 Ingress 证书

M19 之后，本地与 k3d 已经能完整运行，但离托管云还差四处：Kafka 客户端只支持
PLAINTEXT（MSK / Event Hubs / Confluent 都要求 TLS + SASL）；PostgreSQL 只能
到 `sslmode=require`，私有 CA 无法做 `verify-full`；chart 只有固定副本数；
Ingress 只声明 `tls.secretName`，没有证书签发的接线。M20 补上代码与模板，
并把无法在本仓库验证的部分明确标出来。

设计取舍见 [ADR-0016](adr/0016-cloud-kafka-hpa-certmanager.md)。

### 交付内容

| 层 | 文件 | 内容 |
| --- | --- | --- |
| 配置 | `internal/platform/config/config.go` | `kafka.tls` / `tls_ca_file` / `sasl_mechanism` / `sasl_username` / `sasl_password`；机制白名单、凭据成对、SASL 必须配 TLS、CA 文件必须配 TLS |
| 配置 | `internal/platform/config/config.go` | `db.ssl_root_cert`：写入 `sslrootcert`；只允许配 `verify-ca` / `verify-full`，且不得与 `db.dsn` 同时设置 |
| Kafka | `internal/platform/kafka/security.go` | `Security.options()`：TLS 1.2+、私有 CA、PLAIN / SCRAM-SHA-256 / SCRAM-SHA-512；构造客户端前再校验一次 |
| Kafka | `producer.go` / `consumer.go` / report / notification | 生产者用可变参数接收 `Security`，消费选项新增同名字段；四个需要 Kafka 的服务在 `cmd/*` 装配处传入 |
| 测试 | `internal/platform/testenv/kafka_sasl.go` | 生成一次性 CA/服务端证书，启动 SASL_SSL broker；真实握手/生产/消费/负例集成测试 |
| 测试 | `internal/platform/testenv/postgres_tls.go` | 用同一套证书启动 TLS PostgreSQL；`verify-full` 建连与错误 CA 两个方向 |
| 测试 | `internal/platform/db/tls_integration_test.go` | `db.Open` 走 `verify-full` + 自定义 CA 成功；换一张 CA 必须握手失败 |
| Chart | `templates/hpa.yaml`、`_helpers.tpl` | `autoscaling/v2` HPA；全局值 + 按服务覆盖；开启后 Deployment 不渲染 `replicas` |
| Chart | `templates/ingress.yaml` | cert-manager 注解（clusterIssuer 优先），缺少 TLS 或 issuer 时模板失败 |
| Chart | `templates/topics-job.yaml` | `kafka.cliImage` 可覆盖主题 Hook 的 Kafka CLI 镜像；默认值不变，供 ACR/离线环境使用（0.3.2） |
| Chart | `values.yaml`、`deployments.yaml`、`migration-job.yaml` | Kafka TLS/SASL/私有 CA 只注入四个 Kafka 服务，口令与 CA 来自 Secret；`database.tlsCA` 把私有 CA 挂给五个数据库服务与迁移 Job，并在迁移 DSN 加 `sslrootcert`；gateway 可选 Redis 口令 |
| 文档 | `docs/DEPLOYMENT.md` | HPA、cert-manager、EKS/AKS/GKE 依赖与网络对照、迁移顺序 |

### 关键约束

- SASL 必须同时开启 TLS：SASL 只保护认证，不保护数据；拒绝
  SASL_PLAINTEXT 是有意的；
- Kafka 口令只从 Secret 注入，且只挂给真正使用 Kafka 的服务；私有 CA 的
  PEM 同样走 Secret，不写进 values；
- PostgreSQL 的私有 CA 同理：只挂给五个数据库服务与迁移 Job，`tlsCA` 与
  `sslmode` 必须同为校验模式，避免"挂了 CA 却只加密不验证"；
- HPA 开启后副本数由 HPA 独占，避免 Helm 与 HPA 互相覆盖；
- Chart 不创建 Issuer / Certificate：ACME 账号与 solver 属于集群管理员；
- 托管云差异通过 values 注入，不为三家云各维护一套 manifest。

### 验收（2026-10-28 实测）

```text
$ gofmt -l internal cmd                 # 无输出
$ go build ./... && go vet ./...
BUILD_OK
VET_OK

$ go test ./internal/platform/config/... ./internal/platform/kafka/... -count=1
ok  	github.com/edgerunner-fris/expensio/internal/platform/config	0.013s
ok  	github.com/edgerunner-fris/expensio/internal/platform/kafka	12.005s

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/platform/kafka/ \
    -run 'TestSASLPlainOverTLSRoundTrip|TestSASLWrongPasswordIsRejected|TestPlainClientRejectedBySASLBroker' \
    -race -count=1 -v
=== RUN   TestSASLPlainOverTLSRoundTrip
--- PASS: TestSASLPlainOverTLSRoundTrip (11.70s)
=== RUN   TestSASLWrongPasswordIsRejected
--- PASS: TestSASLWrongPasswordIsRejected (0.31s)
=== RUN   TestPlainClientRejectedBySASLBroker
--- PASS: TestPlainClientRejectedBySASLBroker (0.01s)
ok  	github.com/edgerunner-fris/expensio/internal/platform/kafka	14.922s

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/platform/db/ \
    -run 'TestOpenVerifyFull' -race -count=1 -v
=== RUN   TestOpenVerifyFullWithPrivateCA
--- PASS: TestOpenVerifyFullWithPrivateCA (4.14s)
=== RUN   TestOpenVerifyFullRejectsWrongCA
--- PASS: TestOpenVerifyFullRejectsWrongCA (0.03s)
PASS
ok  	github.com/edgerunner-fris/expensio/internal/platform/db	5.791s

$ EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=5 -p 2 -timeout 40m
ok  	github.com/edgerunner-fris/expensio/internal/billing	11.885s
ok  	github.com/edgerunner-fris/expensio/internal/contract	1.648s
ok  	github.com/edgerunner-fris/expensio/internal/e2e	19.709s
ok  	github.com/edgerunner-fris/expensio/internal/expense	21.583s
ok  	github.com/edgerunner-fris/expensio/internal/gateway	4.583s
ok  	github.com/edgerunner-fris/expensio/internal/identity	106.898s
ok  	github.com/edgerunner-fris/expensio/internal/notification	7.568s
ok  	github.com/edgerunner-fris/expensio/internal/platform/db	6.516s
ok  	github.com/edgerunner-fris/expensio/internal/platform/kafka	31.955s
ok  	github.com/edgerunner-fris/expensio/internal/platform/outbox	8.065s
ok  	github.com/edgerunner-fris/expensio/internal/report	61.148s
... 其余有测试的包均 ok；无测试文件的包显示 [no test files]
FULL5_EXIT=0

$ helm lint deploy/helm/expensio
1 chart(s) linted, 0 chart(s) failed
```

`-p 2` 是有意限制包级并发：默认并行度会同时起十余个 PostgreSQL / Redis /
Kafka 容器，在 7.75 GB 的 Docker Desktop 上曾让 Redis 的健康检查因 daemon
排队而超时（断言本身没有失败）。串行度调低后 `-count=5` 与 `-race` 不变。

HPA 渲染（`autoscaling.enabled=true`，`min=3/max=12/CPU=65`，gateway 覆盖
`max=20`）：

```text
hpa_count=7
deploy_replicas=0
expensio-billing       min 3 max 12 metrics [('cpu', 65)]
expensio-expense       min 3 max 12 metrics [('cpu', 65)]
expensio-gateway       min 3 max 20 metrics [('cpu', 65)]
expensio-identity      min 3 max 12 metrics [('cpu', 65)]
expensio-notification  min 3 max 12 metrics [('cpu', 65)]
expensio-report        min 3 max 12 metrics [('cpu', 65)]
expensio-web           min 3 max 12 metrics [('cpu', 65)]
```

负例必须失败：

```text
minReplicas=10 > maxReplicas=5
Error: execution error at (expensio/templates/hpa.yaml:9:3):
autoscaling: billing 的 minReplicas(10) 大于 maxReplicas(5)

certManager.enabled=true 且 tls.enabled=false
Error: execution error at (expensio/templates/ingress.yaml:4:4):
ingress.certManager.enabled=true 需要同时开启 ingress.tls.enabled

database.tlsCA=true 且 sslmode=require
Error: execution error at (expensio/templates/deployments.yaml:36:28):
database.tlsCA 需要 database.sslmode 为 verify-ca 或 verify-full：require 只加密不校验证书
```

Kafka SASL 渲染（`tls=true`、`scram-sha-512`）：四个 Kafka 服务拿到
`EXPENSIO_KAFKA_TLS`、`EXPENSIO_KAFKA_SASL_MECHANISM` 与
`EXPENSIO_KAFKA_SASL_PASSWORD`（Secret 引用）；gateway / identity / web
没有任何 Kafka 认证变量。私有 CA（`kafka.tlsCA=true`）时同一组服务额外挂载
Secret 的 `ca.pem` 并注入 `EXPENSIO_KAFKA_TLS_CA_FILE`，topics Job 也挂载
CA 并把 `ssl.truststore.*` 写进 `client.properties`；默认渲染下这些都不存在，
`tlsCA` 未开 TLS 时模板直接失败。

```text
$ helm template ... --set autoscaling.enabled=true | kubeconform -strict -kubernetes-version 1.31.0
Summary: 31 resources found in 1 file - Valid: 31, Invalid: 0, Errors: 0, Skipped: 0

$ helm template ... --set kafka.tls=true --set kafka.tlsCA=true \
    --set kafka.saslMechanism=scram-sha-512 | kubeconform -strict
Summary: 24 resources found in 1 file - Valid: 24, Invalid: 0, Errors: 0, Skipped: 0
```

PostgreSQL 私有 CA 渲染（`database.tlsCA=true`、`sslmode=verify-full`，同时
开启 Kafka TLS/CA）：ConfigMap 出现 `EXPENSIO_DB_SSL_ROOT_CERT`，五个数据库
服务与迁移 Job 各挂载一个 `postgres-ca` Secret 卷；迁移 DSN 带上
`&sslrootcert=/etc/expensio/postgres/ca.pem`。默认渲染中这些都不存在（
`postgres/ca.pem` 出现 0 次），同一份渲染 kubeconform 仍是 24/24 通过。

### k3d 全链路彩排（2026-10-28）

把 `docs/EXTERNAL_VERIFICATION.md` 的命令顺序在当前提交上完整走了一遍：
k3d v5.8.3（k3s v1.31.5，1 server + 1 agent），宿主 `18080` → 集群 80，
依赖用 `deploy/cluster/acceptance/deps.yaml`，镜像从当前提交重建后导入。

安装与两轮 smoke：

```text
22:20:10 创建 namespace / ConfigMap / Secret，应用 deps
22:21:42 helm upgrade --install --wait（chart 0.3.0，导入镜像 tag=dev）
22:23:29 STATUS: deployed；7 个服务 + web 全部 Running；GET /healthz -> 200
22:23:42 smoke.sh（集群内依赖）：SMOKE_RESULT: PASS
22:25:16 MANAGED_DEPS=1 smoke.sh：SMOKE_RESULT: PASS
```

两轮 smoke 都校验了实际 `helm` Hook（五个库 `schema_migrations`、四个主题、
成功 Job 被清理）、完整注册→邀请→费用→审批→Kafka→报表链路，以及
NetworkPolicy 放行/拒绝的三个方向。`MANAGED_DEPS=1` 分支跳过两条直连
`exec`，迁移与主题改由功能链路证明，行为与本地模式一致。

**HPA 真实扩缩容。** 只对 identity 开启 HPA（`min=1 max=3`，CPU 目标 50%，
缩容稳定窗口 30s），压测用 busybox Pod 直连 identity Service 持续做
argon2 登录（绕开网关限流，单并发，避免把 CPU limit 打满）：

```text
22:30:45  cpu: 2%/50%    replicas 1
22:31:15  cpu: 606%/50%  pods 3（扩容约 30s）
22:31:25  cpu: 998%/50%  replicas 3
          identity pods CPU 499m / 160m / 140m，内存各 134Mi
22:31:25  停止压测
22:32:37  cpu: 2%/50%（指标窗口滞后约 1 分钟）
22:33:07  replicas 1（停止后约 1m40s 回落）
HPA_SCALE_UP_OK
HPA_SCALE_DOWN_OK
```

扩缩容事件在 HPA 的 Events 里可见（`SuccessfulRescale New size: 3`）。本轮
没有制造节点级资源不足，也没有验证"扩容期间消费者再平衡/连接排空"；后者
已在本文件后面的「滚动与中断演练」中用 ACK 现场补测。

**彩排暴露并修复的两个问题：**

1. `smoke.sh` 连续跑两次时，第一轮用固定名字创建的 NetworkPolicy 测试 Pod
   还在 Terminating，第二轮 `kubectl run` 直接 `AlreadyExists`。测试 Pod
   名改为带 PID 后缀，删除逻辑同步使用变量；改完两轮连续 smoke 通过。
2. 默认 `resources.limits.memory: 256Mi` 对 identity 不够：argon2id 每次
   哈希固定占 64 MiB，两个并发登录就触发 `OOMKilled`（exit 137），随后
   CrashLoopBackOff，HPA 连续报 `FailedGetResourceMetric`。把 chart 默认
   上限调到 512 MiB（`0.3.1`），同一压测在 512 MiB 下单并发稳定在
   134 MiB，扩缩容恢复正常。

最终用 chart `0.3.1` 和验收 values 重新安装（不再显式设内存），确认 identity
实际拿到 `limits.memory=512Mi`，再跑一次 smoke：`SMOKE_RESULT: PASS`。

k3d 彩排未覆盖（需要外部环境）：真实云集群的托管依赖、各 CNI 的
NetworkPolicy 行为、负载均衡器、云上 HPA 的节点扩容延迟与事件消费再平衡、
cert-manager 的 ACME 签发与续期，以及各云厂商 Kafka/PostgreSQL 端点的差异。

### 阿里云 ACK 阶段 A（2026-10-29）

在真实的 ACK 托管集群上做平台适配验收，目标是把“能渲染”推进到“能在云端
部署并跑通”。这一阶段**没有**接入阿里云托管 PostgreSQL / Redis / Kafka /
OSS：为了先验证 chart、Ingress、ACR、HPA 与平台网络，依赖仍是
`deploy/cluster/acceptance/deps.yaml` 渲染出的四个单副本 Pod。因此下面的结果
只证明“应用在 ACK 上可运行”，不能证明托管依赖、备份或高可用。

环境与镜像：

```text
集群 ID   c772e4d3c99764bdf906ce05a63e559e0（cn-hangzhou）
节点      4 × Alibaba Cloud Linux Lifsea 3 / containerd 2.1.9
K8s       server v1.37.1-aliyun.1；本地 kubectl v1.31.0（存在版本偏差，命令可用）
CNI       Terway v1.17.7，ENIMultiIP；验收后 disable_network_policy=false，节点切到 DataPath V2 + cilium-cni
入口      ingress-nginx（ACK 托管版 v1.14.5），Service nginx-ingress-lb 为公网 NLB
指标      metrics-server v0.3.16
应用镜像  ACR 个人版 crpi-0z2i4q13886a2ief.cn-hangzhou.personal.cr.aliyuncs.com/expensio/*:63a3707
依赖镜像  ACR 同步的 postgres:16.15-alpine / redis:7.4.11-alpine / kafka:3.9.2 / minio:451fe68
chart     0.3.2
```

安装与结构性证据：

```text
$ helm -n expensio status expensio
STATUS: deployed（revision 2，HPA 开启后）

7 个 Deployment：首次安装 gateway 2/2、web 2/2，其余 1/1；
启用 HPA（identity min=1）后网关/web 缩为 1/1，当前共 7 个应用 Pod；依赖 Pod 全部 Running
迁移 Job：五个库 schema_migrations 版本数均为 1
主题 Job：四个业务主题均已创建，成功 Hook Job 已被清理
GET /healthz（NLB 地址 + Host: expensio.test）-> 200
```

smoke 结果（同一份 `deploy/cluster/acceptance/smoke.sh`）：

```text
Pod 就绪、7 个 Deployment、安全上下文、RBAC、五个库迁移、四个主题、Hook 清理、
Ingress /healthz、注册→邀请→接受→费用→审批→Kafka→报表：通过
NetworkPolicy：default -> identity 被拒
NetworkPolicy：未打 release 标签的 Pod 被拒
NetworkPolicy：同 release 的 Pod 放行（identity 返回 200）
SMOKE_RESULT: PASS
```

NetworkPolicy 的开启过程（本轮最费时的一项，按实际排查顺序记录）：

1. 首次 smoke 失败在 `default -> identity`，当时的
   `eni-config.disable_network_policy` 为 `true`。
2. 改为 `false` 并重启 `terway-eniip` 后仍然放行。根因不是 RBAC：被重启的
   策略容器只打印 `Health check TCP server started on 127.0.0.1:9099`，
   `/var/run/cilium` 为空，`cilium-dbg status` 报
   `cilium.sock: no such file or directory`。节点能力文件
   `/var/run/eni/node_capabilities` 记录的是
   `datapath = veth`、`network_policy_provider = ebpf`、
   `has_cilium_chainer = false`；Terway v1.17.7 的 `terway-cli policy` 在
   veth + ebpf 但 `has_cilium_chainer=false` 时只启动健康检查服务器，
   不会启动 cilium-agent。
3. `terway-cli cni` 中的 `allowEBPFNetworkPolicy()` 对已有节点是粘性的：
   节点能力一旦写成 `false`，之后即使关闭 `disable_network_policy` 也保持
   veth 旧行为，不追加 `cilium-cni` 链路。这是 ACK 在“创建时未启用
   NetworkPolicy”的节点上的设计，单个 ConfigMap 开关无法热开启。
4. 处理方式（已获集群所有者确认）：备份每台节点的能力文件与 CNI 配置，
   删除 4 台节点的 `/var/run/eni/node_capabilities`，滚动重启
   `terway-eniip`。重新初始化后节点能力变为
   `has_cilium_chainer = true`、`datapath = datapathv2`，主机
   `/etc/cni/net.d/10-terway.conflist` 追加 `cilium-cni`，
   策略容器开始运行
   `cilium-agent --cni-chaining-mode=terway-chainer --enable-policy=default --datapath-mode=veth`。
5. 切换到 DataPath V2 后，旧 Pod 的 veth 上没有 cilium 程序，也不会出现在
   `CiliumEndpoint` 里：此时外部流量会因“未知来源 + 默认拒绝”而超时。因此
   滚动重启了 7 个 `expensio-*` Deployment 与
   `kube-system/nginx-ingress-controller`；重启后
   `kubectl get ciliumendpoints -n expensio` 的 7 个端点全部 `ready`，
   策略才真正生效。临时依赖（postgres/redis/kafka/minio）保持原状，
   它们只作为访问目标，不参与 NetworkPolicy 断言。

HPA 实测（identity，`min=1 max=3`，CPU 50%，缩容稳定窗口 60s，压测为两个
busybox Pod 持续做 argon2id 登录）：

```text
07:45:53  cpu: 18%/50%   replicas 1
07:45:54  开始压测
07:46:54  cpu: 908%/50%  replicas 3；identity Pods 454m/…，分布 .117/.119
07:49:06  cpu: 2%/50%    replicas 1
HPA_SCALE_UP_OK
HPA_SCALE_DOWN_OK
HPA Events: SuccessfulRescale New size: 3；SuccessfulRescale New size: 1
```

扩容约 60s、停压后约 2m10s 回落；扩容期间 3 个 identity Pod 均 Ready。

#### 滚动与中断演练（2026-10-29）

Docker 在本机 WSL 不可用，k3d 彩排无法进行，因此三项演练直接在 ACK 现场
执行。脚本在 `deploy/cluster/acceptance/drills/`（`lib.sh` +
`rebalance.sh` / `drain.sh` / `pdb.sh`），与 smoke 共用
`NAMESPACE` / `INGRESS_HOST` / `BASE_URL`；HPA 开启时通过 patch HPA 调整
副本数，未开启时直接 scale Deployment，两种部署方式都能跑。

**消费者再平衡**（`rebalance.sh`）。主题 3 分区，report 消费组从 1 副本扩到 2：

```text
baseline        parts=3 consumers=1 current=10 end=10 lag=0
扩到 2 副本     readyReplicas=2 -> consumers=2，分区 2/1 分配，min per consumer=1
造 4 条事件     produced=4 consumed=4 lag=0
报表读模型      CNY amount_minor=24690（2 × 12345，无丢失/重复）
缩回 1 副本     consumers=1，current=14 end=14 lag=0
DRILL_RESULT: PASS rebalance
```

**连接排空**（`drain.sh`）。滚动重启 gateway，100ms 间隔经 NLB →
ingress → gateway → identity 请求 `/api/v1/me`：

```text
strategy=RollingUpdate maxUnavailable=0 maxSurge=1 grace=30
probe requests=78 ok=78 bad=0（前后三次运行 71/71、77/77、78/78 均为 200）
Pod UID 前后不同：8cfc556f7-x28kh -> 65fd466dc6-kvv72
DRILL_RESULT: PASS drain
```

**PDB 驱逐**（`pdb.sh`）。Eviction 是 Pod 子资源，`kubectl create -f` 无法
映射，脚本通过 `kubectl proxy` 并发 POST 驱逐请求：

```text
1 副本   currentHealthy=1 disruptionsAllowed=0
         Eviction HTTP 429 "Cannot evict pod as it would violate the pod's disruption budget."
2 副本   currentHealthy=2 disruptionsAllowed=1
并发驱逐 1 × 201 + 1 × 429（429 原因为 DisruptionBudget）
驱逐期间 probe requests=43 ok=43 bad=0；副本补齐后 allowed=1
恢复 1 副本 allowed=0
DRILL_RESULT: PASS pdb
```

仍未覆盖的是云上节点级扩容（cluster-autoscaler 加节点）、多可用区与托管
依赖；消费者再平衡、连接排空与 PDB 驱逐已在本集群实测。

**ACK 暴露并修复的问题：**

1. 主题 Hook 的 Kafka CLI 镜像原先硬编码 `apache/kafka:3.9.2`，ACK 节点访问
   Docker Hub 超时，`pre-install` Hook 卡在 `ImagePullBackOff`，Helm 停在
   `pending-install`。chart 0.3.2 新增 `kafka.cliImage`（默认值不变），ACK
   values 指向 ACR 同步镜像后安装成功。验收脚本的 busybox 也改为可配置，
   ACK 使用 `registry.cn-hangzhou.aliyuncs.com/google_containers/busybox:1.27`
   公共镜像，避免把 ACR 凭据复制到其他命名空间。
2. ACK 的 ACR 免密组件没有实际注入（节点上没有 credential provider
   DaemonSet/Webhook），首次拉取 `ImagePullBackOff`；改为命名空间级
   `acr-pull` Secret，应用 Pod 通过 `imagePullSecrets` 继承，验收依赖的
   `expensio/default` ServiceAccount 也挂载该 Secret。凭据不写入仓库。

仍未执行（需要托管依赖或集群管理员）。每一步的控制台参数、白名单网段、
Secret 键名与验收命令见 `docs/ACK_MANAGED_RESOURCES.md`：

- OSS（S3 兼容接入点已实测通过，`OSS_VERIFY: PASS`；HeadBucket 在 OSS 上
  恒为 403，启动探测已改用 `ListObjects`，见 `EXTERNAL_VERIFICATION` §13。
  待集群恢复后把 release 切到 OSS 并跑完整 smoke）；
- cert-manager / ACME 与 TLS；本次 Ingress 只走 HTTP；
- 云上节点级扩容（cluster-autoscaler）与多可用区；
- 对象存储生命周期与 PITR/备份恢复。

### 托管 RDS PostgreSQL 接管（2026-10-29）

release revision 4 起数据库指向阿里云 RDS（`pgm-bp122vf7jr65sz8y`，SSL
`verify-full`，库与角色由 `bootstrap-rds.sql` 建立）。证据链：

- `verify-rds.sh`：五个库的 `*_app` / `*_migrator` 都能用阿里云 CA 建连并
  `ssl=on`；`identity_app` 建表被拒、`rolbypassrls=f`；
- `init-rds-schema.sh`：修复"版本表落在 public"的缺陷后，五个库均为
  `schema=app search_path=app, public version_table=app`；
- 迁移 Job 重跑五个库全部 `no change`（version 5/5/3/2/2，dirty=false）；
- `MANAGED_DEPS=1` 的 smoke 在 NLB 入口跑通：`SMOKE_RESULT: PASS`。

完整输出见 `docs/EXTERNAL_VERIFICATION.md` 第 10 节。**待用户处理**：RDS 白名单
当前是"全部开放"（0.0.0.0/0），建议收窄到集群 vSwitch 网段。

### 托管 Tair Redis 接管（2026-10-30）

release revision 6 起 `redis.addr` 指向
`r-bp1vv55oefnvdvpvci.redis.rds.aliyuncs.com:6379`（Redis 7.0.15，standalone，
SSL 关闭——客户端只支持明文 + 口令）。

- `verify-redis.sh`：PING、版本/角色、无口令 `NOAUTH`、错误口令 `WRONGPASS`、
  SET/GET/TTL 全部符合预期，`REDIS_VERIFY: PASS`；
- 应用侧证据：smoke 之后实例里存在 `expensio:rl:ip:*`（gateway 令牌桶）与
  `report:version:*`（report 仪表盘缓存），不是"进程起来了"；
- 顺带修复 chart 缺陷：`services.report.redis` 原先没开，report 拿不到
  `EXPENSIO_REDIS_PASSWORD`，托管 Redis 上启动即 `NOAUTH`、缓存被静默禁用。
  chart 0.3.3 起默认打开。

白名单最初没放行集群网段，现象是 TCP 超时而不是认证失败；加入
`10.75.0.0/16` 后通过。完整输出见 `docs/EXTERNAL_VERIFICATION.md` 第 11 节。

### 托管 ApsaraMQ Kafka 接管（2026-10-30）

release revision 13 起四个 `EXPENSIO_KAFKA_*` 指向 Serverless 实例的
SASL_SSL 接入点（9095，三台 broker），`tls=true`、`tlsCA=true`（私有 CA
`CN=AliKafka`）、`saslMechanism=plain`、`saslUsername=expensio_test`。

- 端口按实测选：9093 无监听；9094 是 SASL_PLAINTEXT（口令明文过网，配置层
  拒绝）；9095 是 SASL_SSL，必须挂控制台下载的 CA；
- `verify-kafka.sh`（TLS_CA=1）：认证通过、错误口令被拒、四个业务 Topic
  齐全、三个消费组由应用真实 join，`KAFKA_VERIFY: PASS`；
- 切换后 `MANAGED_DEPS=1` smoke 通过（`report currency_totals=1`），链路为
  提交费用 → outbox → Kafka → report 读模型；
- Serverless 禁止客户端建 Topic，`topics.enabled=true` 会让 Hook 30×5s
  重试并把 `helm upgrade` 卡成 `pending-upgrade`，托管环境必须设为 false。

这一步暴露并修复了一个只在托管 Kafka 上出现的代码缺陷：四个服务的
`cmd/*/main.go` 手拼 `kafka.Security` 时漏了 `TLS`，本地明文 Kafka 恰好
`tls=false`，单测与本地联调都发现不了，切 SASL_SSL 后四个服务全部
CrashLoop。修复为统一的 `kafka.SecurityFromConfig` 加回归测试。

完整输出见 `docs/EXTERNAL_VERIFICATION.md` 第 12 节。

本轮保留了 ACK 现场便于复核：`expensio` 命名空间、四个临时依赖 Pod、
`acr-pull` Secret 与 smoke 产生的测试租户都未删除。复核后先
`helm -n expensio uninstall expensio`，再删除临时依赖清单与命名空间；
`creds.sh` 只保存在 WSL 本地（mode 600），没有进入仓库。

### 风险

| 风险 | 状态 | 说明 |
| --- | --- | --- |
| 托管云只完成 ACK 阶段 A | 待验证 | ACK 上已用真实 ACR 镜像、NLB Ingress、metrics-server 完成安装、全链路 smoke 与 HPA 1→3→1；RDS、Tair 与 ApsaraMQ 已接管（三套 verify 脚本 PASS、迁移 no change、MANAGED_DEPS=1 smoke PASS、Kafka 消费组由应用真实创建）；OSS 的 S3 兼容接入点与 CORS 已实测、`OSS_VERIFY: PASS`，待集群恢复后把 release 切过去；ACME 仍未接入 |
| ACK NetworkPolicy 未生效 | 已解决（ACK 实测） | 初始 `disable_network_policy=true`；关闭后仍不生效，根因是节点能力 `has_cilium_chainer=false` 的粘性设计。重置节点能力并重启 Terway 后节点切到 DataPath V2 + cilium-cni；重启应用与 ingress 后 smoke 三条断言全部通过（`SMOKE_RESULT: PASS`） |
| ACK 主题 Hook 拉不到 Docker Hub | 已解决 | chart `0.3.2` 新增 `kafka.cliImage`，指向 ACR 镜像后安装成功；默认值仍是 `apache/kafka:3.9.2` |
| ACK ACR 免密组件未生效 | 已解决 | 没有 credential provider DaemonSet/Webhook；使用命名空间级 `acr-pull` Secret 与 `imagePullSecrets`，凭据不入库 |
| Kafka SASL 未连过真实 broker | 已解决 | testcontainers 启动真实 `apache/kafka:3.9.2`（SASL/PLAIN + TLS），握手、生产、消费、位点提交与两个负例均通过 |
| 云厂商 Kafka 端点差异 | 待验证 | 阿里云 ApsaraMQ 已实测：9093 无监听、9094 是 SASL_PLAINTEXT、9095 是 SASL_SSL 且证书签自私有 CA（CN=AliKafka），需 `tlsCA=true`；MSK 的 SCRAM 与 Event Hubs 的连接串形态仍未接入 |
| OSS S3 兼容差异 | 已解决（OSS 实测） | HeadBucket 即使授予 `oss:GetBucketInfo` 仍返回 403（EC 0003-00000001），启动探测改用 `MaxKeys=1` 的 ListObjects；OSS 拒绝路径风格（0003-00001401），必须虚拟主机风格，minio-go 对 `aliyuncs.com` 自动选择；`aws-chunked` 分片上传需退到 unsigned payload |
| HPA 未做真实扩缩容 | 已解决（k3d + ACK 实测） | identity 在 k3d 与 ACK 的 metrics-server 下均实测 1→3→1：ACK 上 CPU 908%/50% 触发扩容约 60s，停压后约 2m10s 回落；压测还暴露并修复了 256Mi 内存上限导致的 OOMKilled（0.3.1 起默认 512Mi）。云上节点级扩容仍未验证 |
| 滚动与中断行为未实测 | 已解决（ACK 实测） | 三项演练均通过：report 1→2→1 触发分区再平衡（3 分区、2 消费者，produced=4/consumed=4，报表金额 24690）；gateway 滚动重启期间 78/78 请求 200；并发驱逐 1×201 + 1×429，驱逐期间 43/43 请求 200。脚本在 `deploy/cluster/acceptance/drills/` |
| cert-manager 未走完整 ACME | 待验证 | 需要真实域名与公网入口；模板负例已覆盖 |
| PostgreSQL `verify-full` 未连过真实实例 | 已解决 | testcontainers 启动真实 TLS PostgreSQL（自签 CA），`verify-full` 建连与错误 CA 拒绝均通过；chart 的 CA 挂载与迁移 DSN 已渲染校验 |
| 云厂商 PostgreSQL 证书差异 | 已解决（RDS 实测） | 阿里云 RDS 内网域名与实例证书 SAN 一致，`verify-full` + ApsaraDB CA 链从 Pod 内建连成功；CA 链最早到期 2032-04，轮换仍需运维跟进 |
| Redis 口令为可选键 | 设计如此 | 本地/验收无认证；托管 Redis 缺键时请求会显式失败，不会静默降级。chart 0.3.3 起 report 也注入该键（此前只有 gateway 有，report 的仪表盘缓存会静默禁用） |

## 下一步

项目的代码、文档、本地/k3d 验收与阿里云 ACK 阶段 A（平台适配）已收口。
M20 之后剩下的都是需要外部资源的验证工作，继续做必须提供相应环境。执行顺序、
证据模板与常见失败见 [EXTERNAL_VERIFICATION.md](EXTERNAL_VERIFICATION.md)。

1. 把 ACK 最后一个临时依赖 MinIO 换成 OSS（ApsaraMQ 与 RDS/Tair 已完成；
   OSS 验收已通过，剩余是改 `minio.*` values 并重跑 `MANAGED_DEPS=1`）；
2. 真实域名 + cert-manager：验证 ACME HTTP-01/DNS-01 的签发与续期；
3. 云上 HPA：CPU 扩缩容已在 k3d 与 ACK 实测，事件消费再平衡、连接排空与
   PDB 驱逐已在 ACK 实测，剩下节点级扩容（cluster-autoscaler）延迟与多可用区；
4. 各云厂商 Kafka 端点（MSK SCRAM / Event Hubs PLAIN）的连接与事件链路；
   本地真实 broker 的 SASL/PLAIN + TLS 已验证，供应商差异仍需实测；
5. 托管 PostgreSQL 的 `verify-full` 已在阿里云 RDS 实测通过；剩下 CA 轮换与
   备份恢复；
6. 生产备份：对象存储 + PITR，把 `scripts/backup-drill.sh` 扩展为云端恢复演练。



