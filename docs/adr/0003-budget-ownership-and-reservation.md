# ADR-0003：预算归属与预占生命周期

- 状态：已接受
- 日期：2026-10-26
- 相关：费用提交、审批状态机、预算报表

## 背景

报销系统里最容易被做坏的一致性问题是预算：

1. 员工提交费用时，预算必须立刻"看得见地"被占用，否则两个人可以同时花掉同一笔钱；
2. 审批通过后，占用要转成实际支出，不能重复计一次；
3. 驳回或撤销后，占用必须释放；
4. 费用被删除、审批卡住、服务重启，都不能留下"幽灵占用"。

如果预算放在独立服务，费用写入与预算扣减就只能靠分布式事务、Saga 或
"先写费用、再发事件扣预算"。这些方案都会出现一个中间状态：费用已经存在，
预算还没扣；或者预算扣了，费用没写成。对一个以"可验证的一致性"为目标的
项目，这个窗口不可接受。

同时，项目约束明确禁止跨服务直连数据库、禁止跨库 JOIN，也禁止在数据库
事务内直接发 Kafka 消息。

## 决策

### 1. 预算归 expense-service 所有

`budgets`、`expenses`、`approval_steps` 三张表都在 `expense_db`，
由 expense-service 独占读写。费用提交、审批、预算预占在**同一个本地事务**里完成。
其他服务只能通过 API 或后续的事件读模型看到结果，不能直接碰表。

这样做的代价是 expense-service 同时承担费用与预算两块职责；收益是
"提交失败不会留下已预占额度"可以由数据库事务直接保证，而不是靠补偿逻辑。

### 2. 预算数字

- `amount_minor`：预算总额，最小货币单位，`bigint`。
- `reserved_minor`：已提交但未审批通过的费用占用。
- `spent_minor`：已审批通过（含已报销）的费用支出。
- 不变式（数据库 CHECK）：`reserved_minor >= 0`、`spent_minor >= 0`、
  `reserved_minor + spent_minor <= amount_minor`。

所有金额都是 `int64` 最小单位，禁止浮点。币种为三位大写 ISO-4217；
不做汇率换算，费用与预算币种必须一致。

### 3. 状态与预算的对应关系

| 动作 | 状态变化 | 预算变化 |
| --- | --- | --- |
| 提交 | draft → submitted | `reserved += amount` |
| 审批通过 | submitted → approved | `reserved -= amount`，`spent += amount` |
| 驳回 | submitted → rejected | `reserved -= amount` |
| 撤销 | submitted → cancelled | `reserved -= amount` |
| 报销 | approved → reimbursed | 不变（审批时已计入 spent） |

扣减使用条件更新，例如：

```sql
UPDATE app.budgets
SET reserved_minor = reserved_minor + $2
WHERE id = $1 AND reserved_minor + spent_minor + $2 <= amount_minor;
```

影响行数为 0 即预算不足，返回 409，而不是先查后写。审批与释放同理，
用 `reserved_minor >= $2` 保护，避免出现负数。

### 4. 事务与锁顺序

提交/审批/驳回/撤销的事务内顺序固定：

1. 锁定费用单（`SELECT ... FOR UPDATE`）；
2. 锁定并条件更新预算；
3. 条件更新费用状态（`WHERE status = $from`）；
4. 插入 `approval_steps`；
5. 提交。

固定"费用单 → 预算"的顺序，避免两个请求反向加锁造成死锁。
任一步失败整体回滚。Kafka 事件在 M3 引入时，只允许在同一事务内写 outbox 行，
提交后再投递，绝不在事务内直接发消息。

### 5. 预算匹配与租户策略

- 费用单可以显式指定 `budget_id`；提交时校验该预算属于本租户、类别一致、
  币种一致，且 `occurred_at` 的 UTC 日期落在预算期内。
- 未指定时，按 `(tenant, category, currency, 日期区间)` 匹配，
  取 `period_start` 最新的一条，并加行锁。
- **租户只要存在任何未删除的预算，提交就必须匹配到预算**，否则 409
  `budget.not_found`。完全没有配置预算的租户允许无预算提交（`budget_id` 为空），
  避免新租户连一张费用都提交不了。
- 同一租户、同类别、同币种的预算有效期不得重叠，由 `EXCLUDE` 约束在
  数据库层拒绝，而不是靠应用层"记得查一下"。

### 6. 审批权限

- 提交人只能提交自己的草稿。
- 审批人必须是 admin/owner，且**不能是提交人本人**（owner 也不例外）。
  代价是单管理员租户需要再邀请一个管理员才能完成审批；这是有意的，
  否则"审批"退化成"自己点两下"。
- 驳回必须填写理由；所有动作写入只追加的 `approval_steps`。

## 影响

- 报表服务（M3）不能在 SQL 里 JOIN 预算表，只能消费事件或调用 expense API；
  预算口径统一由 expense-service 给出。
- 预算增减必须走工作流或受控的预算更新接口，禁止直接改 `reserved/spent`。
- 预算与费用的金额、币种、类别一致性由服务层 + 数据库约束双重保证。
- 失败语义统一为 409：`budget.exceeded`、`budget.not_found`、
  `budget.currency_mismatch`、`budget.in_use`、`budget.below_committed`、
  `expense.invalid_transition`。

## 被否决的方案

1. **独立 budget-service + 分布式事务**：本地与 kind 环境没有可靠的事务协调器；
   引入 Saga 后仍要面对补偿失败，且无法在数据库层证明不变式。
2. **先写费用，再异步扣预算**：中间窗口会超支；事件丢失时永久不一致。
3. **用浮点存金额**：二进制浮点无法精确表示 0.01，累计误差不可接受。
4. **在数据库事务内直接发 Kafka**：消息可能先于事务提交被消费，回滚后无法撤回。
