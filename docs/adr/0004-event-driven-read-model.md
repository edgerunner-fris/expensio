# ADR-0004：事务内 outbox 与报表读模型

- 状态：已接受
- 日期：2026-10-26
- 相关：expense 审批工作流、report 读模型、Kafka

## 背景

M2 结束时，费用的真相只在 `expense_db`。报表如果直接读 expense 的库，
就违反了"禁止跨服务直连数据库、禁止跨库 JOIN"；如果同步调用 expense API，
一张报表会退化成 N 次远程查询，且无法承受历史数据增长。

M3 需要一条异步链路：费用状态变化 → 事件 → report 读模型。
必须同时满足：

1. 业务事务提交前，事件不能离开数据库（否则回滚后消息无法撤回）；
2. 事件不能丢（业务成功但消息丢失，报表会永久少一笔）；
3. 消费必须幂等（Kafka 至少一次投递，重复是常态）；
4. 同一租户的事件顺序必须稳定，否则旧状态会覆盖新状态。

## 决策

### 1. 事务内写 outbox，事务外投递

`expense_db` 新增 `app.outbox_events`。五个审批动作在各自的事务里，
与费用状态、预算变更、审批轨迹一起插入一行 outbox；提交后由进程内的
投递器发送到 Kafka。

```text
BEGIN
  锁单 → 鉴权 → 预算变更 → 状态变更 → approval_steps → outbox_events
COMMIT
         ↓（后台投递器，独立事务）
      Kafka
```

数据库事务内绝不调用 Kafka。投递失败不回滚业务：业务已经提交，
事件留在 outbox 里重试。

### 2. 投递语义：至少一次

投递器用租约领取事件：

```sql
SELECT id FROM app.outbox_events
WHERE published_at IS NULL AND available_at <= now()
ORDER BY created_at
LIMIT $1
FOR UPDATE SKIP LOCKED;
```

领取时把 `available_at` 推到未来（租约），提交后发送 Kafka，成功则写
`published_at`，失败则写 `last_error` 并按 `1s → 2s → 4s …（上限 1m）` 退避。
进程在"已发送、未标记"之间崩溃会产生重复事件，这是可接受的：
消费端必须幂等，而不是追求端到端 exactly-once。

`outbox_events` 是全服务共用的基础设施表，投递器需要跨租户扫描，
因此**不启用 RLS**；它没有对外 HTTP 读取路径，运行时只有 expense 的
数据库账号可访问。租户隔离仍由事件信封中的 `tenant_id` 与消费端 RLS 保证。

### 3. 事件契约

主题：`expensio.expense.events.v1`；分区键：`tenant_id`（同一租户落到同一分区）。
死信主题：`expensio.expense.events.v1.dlq`。

```json
{
  "event_id": "uuid",
  "event_type": "expense.approved",
  "schema_version": 1,
  "tenant_id": "uuid",
  "aggregate_id": "uuid",
  "aggregate_version": 7,
  "occurred_at": "2026-10-26T12:00:00Z",
  "payload": { "...费用快照..." }
}
```

- 事件类型只有五个：`expense.submitted/approved/rejected/cancelled/reimbursed`。
- `payload` 是**全量快照**，不是状态增量：消费端可以独立重建，不需要回放历史；
  乱序或重放时用 `aggregate_version` 比较，旧版本直接忽略。
- 快照不含商户名、描述、驳回理由：报表只需要聚合维度，
  事件总线不应承载不必要的个人/文本数据。
- `schema_version` 不兼容变更时升版本、换主题；同一主题内只做向后兼容的字段追加。

### 4. 消费端幂等与读模型

report 库有两张表：

- `app.processed_events`：`(tenant_id, consumer, event_id)` 主键，
  消费事务内先插入；冲突即重复事件，直接提交 offset。
- `app.expense_facts`：`(tenant_id, expense_id)` 主键的费用事实表。
  upsert 条件为 `excluded.version >= 当前 version`，旧事件不覆盖新状态。

两条写入与 offset 提交的顺序：

```text
消费一条 → 开启带 tenant_id 的事务 → 幂等表 → 事实表 → COMMIT → 提交 Kafka offset
```

数据库提交成功但 offset 未提交时，消息会重投；幂等表把它挡住。
事件解码失败属于永久错误，投递到 DLQ 后提交 offset；数据库暂时不可用属于
瞬时错误，不提交并持续退避重试，宁可堆积也不丢。

报表查询直接聚合事实表，M3 不建按日预聚合表。

### 5. 报表权限与金额

- owner/admin 可选择 `scope=all`（默认）或 `scope=mine`；
  member/viewer 只能 `scope=mine`，请求 `all` 返回 403，
  与费用列表的语义保持一致，不做静默降级。
- 金额继续使用 `int64` 最小单位；**不同币种绝不求和**。
  聚合结果按 `(分组键, currency)` 分行返回。

## 影响

- 报表是最终一致的：审批完成后存在毫秒到秒级延迟，接受这一点。
- outbox 表会随未投递事件增长；投递成功后不删除（保留审计），
  后续在 M7 增加归档作业。
- Kafka 不可用时业务仍可提交，事件在 outbox 中堆积；
  readiness 不依赖 Kafka，避免消息系统故障拖垮报销流程。
- report 的读模型可以随时清空重建：重新消费主题即可恢复。

## 被否决的方案

1. **事务内直接发 Kafka**：消息可能先于提交被消费，回滚后无法撤回。
2. **定时扫表比对**：需要额外的"已发送"状态列与全表比较，延迟不可控。
3. **消费端存每条事件明细再查询时去重**：存储量随投递次数增长，无法约束重复。
4. **报表服务直连 expense 库**：违反跨服务边界，且把两个服务的 schema 耦死。
