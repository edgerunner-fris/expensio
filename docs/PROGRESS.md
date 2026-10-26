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
| 10 | 前端依赖版本兼容（Vue/Pinia/Router/ECharts/TS） | **待验证** | 版本表已核对，但未真正装过一次。M5 落地时以 `npm ls` 与构建结果为准 |
| 11 | 迁移中途失败会让库进入 dirty 状态 | **已解决（有操作手册）** | 本次踩到过：基线在事务内失败 → `schema_migrations` 标记 dirty。恢复方式：修好 SQL 后重建该库（本地），生产环境只允许向前修复并手工 `force`。`make migrate` 会明确报错，不会静默跳过 |
| 12 | 并发刷新导致同一令牌被轮换两次 | **已解决** | 轮换用条件更新 `UPDATE … WHERE id=$1 AND revoked_at IS NULL` + `RowsAffected()`；抢输的一方被判为重放并撤销整族。撤销放在独立事务里提交——放在同一事务里会被随后返回的错误一起回滚 |
| 13 | 登录接口可通过耗时分出"账号是否存在" | **已解决** | 用户或租户不存在时仍对预置哈希执行一次完整 argon2id 校验，返回文案与状态码也完全一致 |
| 14 | 角色大小写两套（DB 小写 / 上下文大写） | **已解决** | 统一为小写，`tenantctx.Role` 同时承载 `Valid()` 与 `AtLeast()`；避免每层各转一次、某条路径漏转 |
| 15 | gin 默认信任 `X-Forwarded-For`，审计 IP 可伪造 | **已解决** | `server.New` 里 `SetTrustedProxies(nil)`；部署到 ingress 之后再按实际网段显式配置 |
| 16 | Windows 上无法投递 SIGTERM 验证优雅关闭 | **已解决** | WSL2 实测 billing：SIGTERM 后日志依次出现 draining → stopped → service stopped，进程退出码 0。Windows `Stop-Process` 仍是 TerminateProcess，不作为验证手段 |
| 17 | access token 用 HS256，六个服务共享同一把密钥 | **未解决（已知取舍）** | 任何一个服务被攻陷都能签发任意租户的令牌。M1 阶段接受（本地单体部署、密钥只走环境变量）；M6 上 K8s 之前评估改为 EdDSA/RS256（identity 持私钥，其余服务只持公钥）。在 ADR 中记录 |
| 18 | 限流器故障时放行（fail-open） | **已解决（明示取舍）** | 运行期 Redis 抖动时宁可短期超额，也不让全站 5xx；但启动时连不上 Redis 直接失败，避免以"无限流"状态上线。两类分支都有日志，M7 补指标与告警 |
| 19 | 网关重试会不会把写请求做两遍 | **已解决** | 只对 GET/HEAD/OPTIONS、无 body、且错误属于"连接未建立/未送达"（ECONNREFUSED、DNS 等）的请求重试一次；超时与取消一律不重试，因为请求可能已经到达下游 |
| 20 | 网关是单点，且到下游走集群内明文 HTTP | **未解决（留到 M6）** | 本地与 kind 单副本可接受。上 K8s 后：多副本 + PDB + NetworkPolicy 限制只有网关能访问业务服务；是否需要 mTLS 在 M6 评估 |
| 21 | 下游收到的 `X-Request-ID` 是否可信 | **已解决** | 网关用中间件生成/透传的 ID 覆盖入站值再转发；`X-Forwarded-*` 由 `ReverseProxy.Rewrite` 依据真实连接重写，客户端无法伪造 |
| 22 | 登录时"账号存在但不在该租户"与"凭证错误"可区分 | **已解决** | 集成测试发现：密码正确但无成员关系时返回 403 + 明确文案，等于给了一份泄露密码的攻击者一个"密码正确"的判定点。已改为与其他登录失败一致的 401 `auth.unauthorized`；成员被停用仍返回 403（需要有效密码才可探测，属账号状态而非枚举） |
| 23 | 集成测试依赖 Docker，无 Docker 时会被跳过 | **已解决（策略已落地）** | `testenv` 改为显式 `docker info` 门禁：设置 `EXPENSIO_REQUIRE_DOCKER=1` 时不可用即失败，未设置才允许跳过；`make test-integration` 固定启用。失败/跳过两条分支均已实测。CI 实际执行仍见风险 26 |
| 24 | 测试容器启动过早，迁移时报 `unexpected EOF` | **已解决** | postgres 镜像首次启动会先起临时实例跑初始化脚本再重启。testcontainers v0.44 的 postgres 模块默认不等待，必须显式 `BasicWaitStrategies()`（等第二次 ready + 端口可连） |
| 25 | Ryuk 回收器需要额外拉取镜像，代理不通时集成测试直接失败 | **已解决（明示取舍）** | 测试改为显式 `Terminate`，默认设置 `TESTCONTAINERS_RYUK_DISABLED=true`；CI 想启用 Ryuk 可显式改回。代价：测试进程被强杀时可能残留容器 |
| 26 | 集成测试在 CI（Linux）尚未执行过 | **待验证** | WSL2 本机（Linux 内核）已通过；GitHub Actions 尚未配置，M6 落地时以 runner 结果为准 |
| 27 | `.env.example` 与 compose 含固定本地口令 | **未解决（已知取舍）** | 均为仅在 127.0.0.1 生效的本地演示值，不是生产密钥；但公开仓库中仍显眼。计划改为 `make env` 生成随机口令、compose 用 `:?` 强制注入，M6 前完成 |
| 28 | 技术栈声称使用 sqlc 与 wire，代码中均不存在 | **已解决（决策已记录）** | ADR-0002 明确：M1/M2 使用手写 pgx 与手工装配，M6 前决定"正式引入"或"从对外口径删除"；在此之前不得声称已使用 |
| 29 | Kafka 生产者尚未对真实 broker 端到端验证 | **已解决（M3-5）** | `TestConsumerAppliesEventsAndDeduplicates` 用 `kafka.NewProducer` 对真实 broker 同步发送，消费者从真实主题读回并落库；`TestConsumerRoutesPermanentErrorsToDLQ` 验证 DLQ 写入与位点推进。M3-7 又在 `internal/e2e` 用真实费用 API → outbox → Kafka → report 消费者 → 报表 API 跑通完整链路 |

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

## M4 进行中（3/7）

设计锁定在 ADR-0005：订阅与账单状态机分离、一个订阅周期一张账单、
回调按 HMAC 验签后从 `invoice_lookup` 反查租户、幂等依赖唯一约束而非先查后插、
事件继续走事务内 outbox。

| 批次 | 内容 | 状态 |
| --- | --- | --- |
| M4-1 | ADR-0005、billing 核心迁移、RLS/约束/幂等 schema 测试 | 完成 |
| M4-2 | billing 领域模型：套餐、订阅状态机、账单金额与周期规则 | 完成 |
| M4-3 | 套餐与订阅 service/repository/HTTP | 完成 |
| M4-4 | 出账（同周期幂等）与账单查询 API | 未开始 |
| M4-5 | 支付回调：验签、防重放、幂等落库、outbox 事件 | 未开始 |
| M4-6 | 网关 billing 路由、`cmd/billing` 装配与 HTTP 联调 | 未开始 |
| M4-7 | 全量回归、跨服务联调、PROGRESS 收尾 | 未开始 |

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

## 下一步：M4-4

出账与账单查询：按订阅周期生成账单（同订阅同周期唯一，重复出账返回 409），
金额 = 套餐单价 × 席位，使用 `InvoiceAmount` 的溢出保护；
账单列表与详情对全租户可读，写操作仍限 owner/admin。



