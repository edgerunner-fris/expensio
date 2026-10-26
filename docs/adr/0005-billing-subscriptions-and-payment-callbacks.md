# ADR-0005：订阅计费、出账与支付回调

- 状态：已接受
- 日期：2026-10-26
- 相关：billing 服务、订阅生命周期、支付回调幂等、outbox

## 背景

M3 已把"费用发生 → 报表"的异步链路跑通。M4 要补的是另一条资金链路：
租户订阅套餐、按周期出账、支付渠道回调付款结果。

这条链路与报销链路的差异在于：

1. 入站回调来自外部支付渠道，没有本系统的 JWT；
2. 支付渠道会重复投递同一回调，且可能乱序；
3. 账单金额一旦出账就不能被后续改价影响；
4. 不能因为回调处理失败而丢钱，也不能把同一笔钱记两次。

同时必须继续遵守既有约束：金额只用 `int64` 最小单位、不同币种不求和、
租户数据经 FORCE RLS 隔离、事务内不直接发 Kafka。

## 决策

### 1. 数据归属

billing 独占 `billing_db`，不读 identity/expense 的库，也不做跨库 JOIN。
租户与用户在 billing 里只是 UUID 引用：是否真实存在由 identity 负责，
billing 不在自己的库里复制一份用户表。

表：

| 表 | 作用 | RLS |
| --- | --- | --- |
| `plans` | 全局套餐目录（只读，迁移内种子） | 否（无 tenant_id） |
| `subscriptions` | 租户订阅，含套餐、席位数、状态与当前周期 | 是 |
| `invoices` | 每个订阅周期一张账单 | 是 |
| `payments` | 成功的支付流水（只追加） | 是 |
| `webhook_events` | 回调幂等记录（只追加） | 是 |
| `invoice_lookup` | 回调按账单找租户的最小映射 | 否（不对运行时开放） |
| `outbox_events` | 订阅/账单事件 | 否（投递器跨租户扫描） |

`plans` 是全局目录，不按租户隔离，但运行时角色只有 `SELECT`；
价格与币种在迁移里种子化，改价只能通过新迁移，历史账单因此不会被改价污染。

### 2. 订阅状态机

```text
active ──支付失败──▶ past_due ──支付成功──▶ active
   │                    │
   └────取消────────────┴────▶ canceled
```

- 同一租户最多一条未取消订阅（部分唯一索引）。
- 订阅创建时固定套餐、席位与当前周期；**不做就地升级/降级**。
  换套餐 = 取消旧订阅 + 新建订阅，历史账单与订阅一一对应。
- 取消是终态，不能恢复；取消不删除任何账单。
- 状态迁移由 `subscription_status_transitions` 表与触发器共同约束，
  与费用状态机使用同一套写法。

### 3. 账单与金额

- 一个订阅的一个周期只有一张账单：唯一约束 `(tenant_id, subscription_id, period_start)`。
  重复出账在数据库层就不可能发生，不依赖应用先查后插。
- 金额 = 套餐单价 × 席位数，全部 `int64` 最小单位；账单落库后金额、
  币种、周期不可改。
- 账单状态只有 `open → paid`；支付失败不关闭账单，订阅进入 `past_due`，
  账单仍可继续收款。没有 `void`：作废需要人工修数，M4 不提供半套流程。
- 账单号由服务生成 `INV-<yyyymm>-<8 位随机>`，租户内唯一。

### 4. 支付回调的信任模型

支付渠道没有本系统的 JWT。回调按以下顺序建立信任：

1. **验签**：`X-Billing-Signature` = HMAC-SHA256(`secret`, `<timestamp>.<raw body>`).
   密钥只从环境变量 / K8s Secret 读取；使用常量时间比较。
2. **防重放**：`X-Billing-Timestamp` 与服务器时间相差超过 5 分钟直接拒绝。
3. **确定租户**：请求体只有 `invoice_id`，没有 `tenant_id`。
   服务调用 `app.invoice_tenant(uuid)`（SECURITY DEFINER，只读取
   `invoice_lookup` 映射表）得到租户，再以该租户建立 RLS 事务上下文。

这样租户仍然来自数据库中的可信记录，而不是调用方可控的 body/query/path。
`invoice_lookup` 由 `invoices` 的 AFTER INSERT 触发器写入，与账单同事务，
运行时角色对它没有任何直接权限；它只暴露"账单 → 租户"这一条最小信息。

**为什么不接受回调 body 里的 tenant_id**：那等于让外部调用方选择租户边界，
即便有签名，也多了一个需要永久保持正确的信任假设。body 只携带
`invoice_id / payment_id / 金额 / 币种 / 事件 ID`，租户由数据库反查。

### 5. 回调幂等

`webhook_events` 以 `(tenant_id, provider, provider_event_id)` 唯一。
处理顺序：

```text
验签 → 反查租户 → BEGIN(tenant)
   INSERT webhook_events ... ON CONFLICT DO NOTHING
   冲突 → COMMIT，返回 duplicate=true（不重复改账单）
   未冲突 → 锁定账单 → 校验金额/币种 → 写 payments → 标记 paid
            → 更新订阅状态 → 写 outbox → COMMIT
```

- 同一 `event_id` 重复投递：第二次不产生任何状态变化。
- 不同 `event_id` 携带同一 `payment_id`：`payments` 的唯一约束吸收，
  同样按重复处理。
- 账单已是 `paid`：视为幂等成功，不再写第二条支付流水。
- 金额或币种与账单不一致：永久错误，返回 409，且**不**标记账单已付；
  请求进入日志与告警，等待人工核对。回调记录仍保留，避免无限重试。
- 处理成功/失败都在本地事务提交后返回 2xx/4xx；渠道重试由幂等表吸收。

### 6. 事件

主题 `expensio.billing.events.v1`，分区键 `tenant_id`，DLQ 同名 `.dlq`。
事件类型：

- `subscription.created`
- `subscription.canceled`
- `invoice.created`
- `invoice.paid`
- `invoice.payment_failed`

信封复用 `internal/platform/events` 的通用结构，payload 为全量快照。
事件在业务事务内写 `outbox_events`，由独立投递器发送（ADR-0004）。
M4 只负责生产；notification 在 M5 消费。

### 7. 权限

- 套餐列表：所有已认证角色可读。
- 创建/取消订阅、出账：owner/admin。
- 查看当前订阅与账单：owner/admin 看全租户；member/viewer 只能看自己的？
  账单属于租户而非个人，因此本服务对账单列表不做 `scope` 区分——
  能读到账单的前提是已是该租户成员，这由 JWT 与 RLS 保证。
  这个取舍写入 ARCHITECTURE，避免与费用列表的 `scope` 语义混淆。

## 影响

- 支付回调是唯一不使用 JWT 的写路径；它的信任来源是签名 + 数据库反查租户，
  需要单独监控验签失败率与 409 金额不一致。
- 订阅不做就地换套餐，前端必须走"取消再订阅"。这是有意的：
  按比例折算（proration）会把金额与周期计算变成另一个状态机，M4 不做。
- 账单与支付流水只追加，不提供删除；冲正需要后续里程碑的贷项单。

## 被否决的方案

1. **回调 body 携带 tenant_id**：把租户选择权交给外部输入。
2. **回调直接连数据库查 invoice**：跨不过 RLS，且会把查询路径绕过服务边界。
3. **先查后插做幂等**：并发回调会双写；唯一约束才是幂等来源。
4. **支付成功后再异步改账单**：钱已到账但账单长时间 open，对账无法收敛；
   支付流水、账单状态与 outbox 必须在同一事务。
5. **订阅就地升降级 + 按比例折算**：需要额外记录变更历史与折算规则，
   M4 的范围是"订阅、出账、回调幂等"，不做半套 proration。
