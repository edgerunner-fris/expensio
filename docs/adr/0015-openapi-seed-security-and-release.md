# ADR-0015：OpenAPI 契约、种子数据、安全扫描与发布流水线

- 状态：已接受（M19）
- 日期：2026-10-28

## 背景

M18 结束时功能已经闭环，但"可交付"还差四件事：

1. 调用方没有可读的接口契约，接口只存在于 Go 路由和前端调用里；新端点漏写
   文档、删掉的端点留在文档里，都不会让测试失败；
2. 演示环境要靠手工在页面上点出数据，新机器上无法重现，也无法在演示前快速
   判断"整栈是否真的能用"；
3. 依赖漏洞只在本地偶尔扫一次，镜像里还带着旧 Go 标准库和 grpc 1.82.0 的
   已知 HIGH，迁移镜像用的是上游旧工具链编译的二进制；
4. 只有 CI，没有 tag 触发的镜像发布与部署，也没有证明"备份能恢复"的手段。

M19 只补这四块，不改业务语义。

## 决策

### 1. OpenAPI 是契约，也是被测对象

`api/openapi.yaml` 手写（不是从代码生成）：生成器只能描述它看到的结构，
无法表达"这个字段在什么状态下必填"、错误码含义与示例。规范用 3.0.3，
覆盖 50 条路径 / 62 个操作 / 92 个 schema，统一 `{code,message,request_id,
data,details}` 信封、分页参数与 Bearer 安全方案。

关键在 `internal/contract`：

- `TestRegisteredRoutesDocumented`：真实调用 identity / expense / billing /
  report / notification 的 `RegisterRoutes`，从 gin 引擎提取"方法 + 规范化
  路径"，与规范双向比对。代码新增但没写文档 → 失败；文档写了但代码没有 →
  失败。路径参数名（`:id` 与 `{expenseId}`）在比对前统一。
- `TestOpenAPIPathsRoutedByGateway`：规范里的每个路径都必须被
  `gateway.DefaultRules()` 覆盖，且每条网关规则必须对应一个文档化端点。
  这条测试是为一个真实缺陷加的：`/api/v1/approval-policies` 在服务里注册了、
  在规范里写了，但网关漏了转发规则，表现为"服务健康、直连正常、走网关 404"。
  反向检查还清掉了一条早已删除的 `/api/v1/invoices` 残留规则。

为什么不用代码生成：生成器会把实现细节（比如 gin 的内部结构）带进契约，
且无法阻止"代码改了、契约跟着自动改"的无声漂移。手写 + 双向测试把契约
当成独立产物来维护，漂移必须显式修一处。

`redocly.yaml` 只关掉 `no-server-example.com`（本地 server 就是 localhost），
其余 recommended 规则保留，`make openapi-lint` 与 CI 共用。

### 2. 种子数据走公开 API，而不是直连数据库

`cmd/seed` 用 owner / approver 两个账号登录网关，通过公开接口创建租户成员、
四个预算、四种状态的费用单、订阅与一张待支付账单。

走 API 而不是 SQL 的理由：种子数据要证明的是"这套系统能跑通"，直连数据库
写入会绕过鉴权、RLS、状态机、预算预占与事件派发——恰恰是最容易坏的部分。
代价是它依赖完整栈（`make app-up`），这是演示工具而不是生产工具。

幂等规则：

| 对象 | 匹配方式 |
| --- | --- |
| 预算 | 名称 + 期间 |
| 费用单 | 描述里的 seed 标记（四种状态各一张） |
| 审批人 | 租户成员列表里的邮箱 |
| 账单 | 已有 open 账单时复用，不重复开具 |

口令只从 `EXPENSIO_SEED_PASSWORD` 读取，不提供命令行参数：参数会进 shell
历史与 `ps`，而演示环境往往多人共用。输出只打印账号、数量与账单号，不打印
口令或令牌。第二次执行输出"本次新建 0"，账单号不变。

### 3. 安全扫描：代码可达漏洞为零，镜像 HIGH/CRITICAL 为零

- CI 新增 `security` job：`govulncheck ./...`，只对**代码实际可达**的漏洞
  失败。当前结果 `No vulnerabilities found` / `Your code is affected by 0
  vulnerabilities`。
- 镜像扫描用 Trivy `--severity HIGH,CRITICAL --ignore-unfixed`。没有修复
  版本的 CVE 无法通过升级消除，用 `--ignore-unfixed` 明确排除，而不是让
  流水线长期红着被忽略。
- 修复路径不是忽略告警：`go.mod` 升到 Go 1.27.3（标准库补丁），otel 1.45.0、
  grpc 1.83.2、x/net 0.62.0、quic-go 0.59.1、moby/go-archive 0.3.0；七个
  应用镜像全部重建。旧镜像上的 `CVE-2026-84304`（grpc 1.82.0，HTTP/2 DATA
  帧碎片化 DoS）随升级消失，八个镜像扫描均为 `Total: 0`。
- `Dockerfile.migrate` 改为从源码编译 golang-migrate v4.18.3：上游
  `migrate/migrate` 镜像的二进制由旧 Go 构建，标准库 HIGH 没有修复 tag；
  与业务服务共用同一条 Go 工具链后告警归零，运行时用 `USER 65534`。
- 文件系统扫描余下 12 条 HIGH 全在 `deploy/cluster/acceptance/deps.yaml`
  （k3d 验收用的 postgres/redis/kafka/minio 演示部署，未设 securityContext
  与只读根文件系统）。这些第三方 stateful 镜像需要可写根目录，强行只读会
  破坏验收环境；生产由托管服务替代，应用 chart 的
  `securityContext` 已设置 `runAsUser: 10001` 与
  `readOnlyRootFilesystem: true`。因此标记为**已接受**并记录在 PROGRESS。

### 4. 发布：tag 触发、先验证再出镜像、没有集群就不假装部署

`.github/workflows/cd.yml` 在 `v*` tag 上分三段：

1. **verify**：gofmt、vet、build、`EXPENSIO_REQUIRE_DOCKER=1 go test ./...
   -race -count=1 -timeout=30m`、前端构建、OpenAPI lint。任何一步失败都
   不会出镜像。
2. **images**：八个镜像的矩阵（identity/expense/billing/report/
   notification/gateway/web/migrate），按服务选择 Dockerfile，推
   `${{ github.ref_name }}` 与 `${{ github.sha }}` 两个 tag，并生成 SBOM。
3. **deploy**：`environment: production`，从 `secrets.KUBE_CONFIG` 写临时
   kubeconfig，`helm upgrade --install --wait --timeout 15m`，再检查
   rollout。没有配置 `KUBE_CONFIG` 时**明确跳过并输出原因**，不把"没部署"
   伪装成成功。

这一段没有在真实云集群执行过（没有目标集群与凭据），在 PROGRESS 与
DEPLOYMENT 中都标为待验证；verify 与 images 两段可以在任何 tag 上真实运行。

### 5. 备份演练：证明逻辑备份可恢复，不冒充备份方案

`scripts/backup-drill.sh` 对五个库逐一 `pg_dump --no-owner --no-privileges`，
恢复到 `<db>_drill`，比对表集合、总行数与 `app.schema_migrations` 的版本，
全部一致才 PASS，最后删除临时库。

它验证的是"备份文件能完整恢复"，不是"生产备份可用"：生产还需要对象存储、
跨区、PITR 与定期恢复演练，属于 M20 的托管云工作。把它做成脚本而不是一次性
操作，是为了让每次 schema 变更后都能用一条命令回归。

## 风险

- CD 的 deploy 段未在真实集群执行；Helm 参数与 `--wait` 行为只能等有集群时
  验证。
- 验收依赖清单的 12 条 HIGH 是接受而不是修复，理由是演示用途；如果有人把
  `deps.yaml` 直接用到生产，这些告警就是真实风险。
- 镜像扫描依赖 Trivy 漏洞库的更新；`--ignore-unfixed` 意味着"当前没有修复
  版本"的漏洞不会出现在结果里，需要靠 govulncheck 与订阅公告补充。
- `cmd/seed` 的幂等依赖"seed 标记"文本；如果用户手工改了这些费用单的描述，
  下一次 seed 会认为它们不存在而补造一张。这是演示工具的已知边界。
- 备份演练只覆盖逻辑备份与本地 compose；WAL 归档、PITR、跨区副本都未覆盖。

## 测试

- `internal/contract/openapi_test.go`：真实路由 ↔ 规范双向一致；
- `internal/contract/gateway_test.go`：规范 ↔ 网关规则双向覆盖；
- `cmd/seed/main_test.go`：口令校验、幂等匹配与请求构造；
- `EXPENSIO_REQUIRE_DOCKER=1 go test ./... -race -count=1 -timeout 30m`：
  全量通过，含 e2e；
- `scripts/backup-drill.sh`：五库恢复后表数、行数、迁移版本全部一致。
