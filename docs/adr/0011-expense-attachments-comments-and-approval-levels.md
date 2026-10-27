# ADR-0011：附件、评论与按金额分级审批

- 状态：已接受（M15）
- 日期：2026-10-28

## 背景

最初的 brief 要求费用单支持发票附件、审批沟通，以及"金额越大、审批层级越多"
的分级审批。M2 只实现了单级审批与单据 CRUD；本次补齐这三块，并新增一个
审批待办入口。约束仍然是既有的几条：租户只能来自 JWT、金额不用浮点、
业务写入与事件在同一个事务、数据库层要能独立挡住脏数据。

## 决策

### 1. 附件用对象存储直传（presigned PUT）

服务端不中转文件字节：

1. `POST /expenses/:id/attachments/presign` 校验权限与元数据后，签发一个
   15 分钟的 PUT URL 和一个随机对象键；
2. 浏览器直接把文件 PUT 到 MinIO；
3. `POST /expenses/:id/attachments` 确认：服务端 `StatObject` 校验对象确实
   存在、大小与声明一致，然后才写入元数据行；
4. `GET /expenses/:id/attachments` 每次现签 10 分钟的 GET URL。

对象键为 `tenants/<tenant_id>/expenses/<expense_id>/<uuid>.<ext>`，扩展名由
内容类型决定，不使用用户文件名。确认接口校验键的租户/单据前缀，避免把
同租户另一张单的对象挂到本单上。

限制与权限：

| 项 | 取值 |
| --- | --- |
| 允许类型 | `image/jpeg`、`image/png`、`image/webp`、`application/pdf` |
| 单文件大小 | 1 B – 10 MiB |
| 每单数量 | 最多 10 个 |
| 写权限 | 仅草稿的提交人（`draft` + `submitter`） |
| 读权限 | 能查看该费用单的人（`CanView`） |
| 下载 | presigned GET，`response-content-disposition=attachment` |

附件在提交后冻结：审批阶段不能再增删，因为附件是报销证据。删除采用
"先软删元数据行、后尽力删除对象"，对象删除失败只记日志，不会让已经删掉的
附件重新出现在列表里；残留对象由桶生命周期规则清理（部署侧配置）。

服务端**不保存下载 URL**，只保存 `object_key`。URL 是短期凭据，落库会变成
长期可用的越权入口，也会写进备份。

### 2. 评论

`expense_comments`：`body` 1–2000 字符，任何能查看该单的成员可写，作者或
管理员可软删。评论不改变单据状态，也不发事件；它只是审批沟通记录。

### 3. 按金额分级审批

策略按"租户 + 币种"配置一个二级门槛：

- 没有策略，或金额 < 门槛 → 1 级审批；
- 金额 ≥ 门槛 → 2 级审批，且两级必须是**不同的人**。

`required_approval_levels` 在提交时按当时的策略计算并**快照**到单据上。
之后管理员改门槛，不会追溯改变在途单据的审批要求——否则同一张单的
审批路径会随管理员操作变化，审计上无法解释。

每一级批准写入只追加的 `expense_approvals`：

- `UNIQUE (tenant_id, expense_id, level)`：同一级不能批两次；
- `UNIQUE (tenant_id, expense_id, actor_id)`：同一个人不能批两级；
- 触发器再校验：单据必须是 `submitted`、审批人不能是提交人、级别必须
  从 1 开始逐级递增且不超过要求级数。

状态机不变：中间级批准只把 `approval_level` 加一，状态仍是 `submitted`；
最后一级才执行"预占转支出 → `approved` → 追加审批轨迹 → 写 outbox"。
因此 report 的读模型和事件消费者完全不需要感知分级。中间级批准不发事件，
因为它没有改变任何消费者关心的状态。

`approval_steps` 保持"只记录状态变化"的语义，中间级批准不写进去；它的
`action_transition_check` 因此不需要放宽。审批历史由 `expense_approvals`
（各级批准）与 `approval_steps`（提交/最终批准/驳回/撤销/报销）共同表达。

### 4. 审批待办

`GET /expenses?inbox=true` 在服务端过滤，而不是让前端拉全量再筛：

- 只有 admin/owner 能访问，其他角色 403；
- 排除自己提交的单（禁止自审）；
- 排除自己已经批准过任何一级的单（两级必须不同人）。

分页与总数在同一套 `WHERE` 下计算，翻页不会出现"第一页有、第二页重复"。

## 备选方案

- **服务端中转上传**：实现简单，但文件字节要经过 API 进程，内存、超时和
  带宽都被放大；对象存储直传是这类需求的标准做法。
- **决策时实时读策略**：在途单据的审批路径会被管理员改配置影响，无法解释
  历史。选择提交时快照。
- **中间级批准也写 `approval_steps`**：需要放宽该表的
  `action_transition_check`，而这条约束正是状态机的数据库防线；为了一条
  审计记录削弱它不划算。
- **前端过滤待办**：分页会错，且全量拉取会把"谁能看什么"泄漏到客户端逻辑。

## 迁移与回滚

`000005_expense_collaboration`：

- 新建 `approval_policies`、`expense_approvals`、`expense_comments`、
  `expense_attachments`；四张表都启用并强制 RLS；
- `expenses` 增加 `required_approval_levels`（1–2）与 `approval_level`；
  历史 `approved`/`reimbursed` 行回填 `approval_level = required_approval_levels`；
- 新增独立触发器函数 `expenses_guard_approval_progress`，不替换 000002 的
  `expenses_guard_update`，回滚时只需删除新函数与列；
- down 按依赖倒序删表、删列。

## 风险

- 启动时检查桶，MinIO 不可达则进程启动失败（fail-fast），避免带着未配置的
  存储静默上线；expense 的 readiness 不探测 MinIO，因此运行中 MinIO 故障只
  让附件接口报 503/500，费用单与审批继续可用。
- 阈值生效后租户至少需要两名不同的 admin/owner，否则单据会停在第一级；
  UI 显示"1/2 已批准"，管理台可改门槛。
- 对象删除是尽力而为，可能残留孤儿对象；生产需给桶配生命周期规则。
- presigned URL 是短期 bearer 凭据，不写日志、不落库。
