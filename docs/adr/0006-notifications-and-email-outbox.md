# ADR-0006：站内通知、邮件发件箱与服务间取数

- 状态：已接受
- 日期：2026-10-27
- 相关：notification 服务、expense/billing 事件消费、identity 内部接口、SMTP 投递

## 背景

M3/M4 之后，费用与账单的状态变化已经以事件形式发布到 Kafka。用户需要
在站内看到"我的费用被批准/驳回""账单已支付"之类的提醒，管理员需要收到
待审批与支付结果。这些提醒需要送达用户邮箱，但邮件不能成为业务链路的一部分：

1. 通知不是业务事实。费用是否批准只由 expense 的数据库决定，通知丢失或延迟
   不能让业务状态不一致；重放事件必须能重建收件箱。
2. 邮件外发依赖外部 SMTP，随时可能慢或失败。**绝不能在业务事务里发信**，
   也不能在事件消费事务里同步发信——那会把 Kafka 位点推进与外部网络耦合。
3. 收件人邮箱属于 identity。notification 不能直连 identity 的库，
   更不能跨库 JOIN；而消费事件时只有 `tenant_id`，没有调用方的用户 JWT。
4. 同一事件可能被重复投递；同一租户可能同时有多个 notification 副本。
   幂等必须落在一个明确的位置，而不是靠"先查后插"。

## 决策

### 1. 数据归属与派生性

notification 独占 `notification_db`，表：

| 表 | 作用 | RLS |
| --- | --- | --- |
| `app.notifications` | 每个用户自己的站内通知 | `ENABLE + FORCE` |
| `app.processed_events` | 消费幂等记录 | `ENABLE + FORCE` |
| `app.email_outbox` | 待发邮件（事务内写入） | `ENABLE`（非 FORCE，见 §4） |

通知是**派生数据**：在 Kafka 保留期内重放两个事件主题即可重建。它不参与
任何业务判定，也不提供"通知已读状态"给其他服务使用。`email_outbox` 同理，
但已发送的邮件无法撤回，因此发送侧的语义是至少一次（见 §7）。

租户上下文一律来自 JWT 或事件信封中的 `tenant_id`；HTTP 层不从
body/query/path 读取租户。事件信封由本系统服务产生，仍在消费前校验
`tenant_id / aggregate_id / aggregate_version / event_type / status` 的一致性，
不一致按契约错误处理，而不是"尽力而为"。

### 2. 消费、幂等与错误分类

两个独立的消费组，手动提交位点：

| 主题 | 消费组 | 事件 |
| --- | --- | --- |
| `expensio.expense.events.v1` | `notification-expense-v1` | `expense.submitted / approved / rejected / reimbursed / canceled` |
| `expensio.billing.events.v1` | `notification-billing-v1` | `subscription.created / canceled`、`invoice.created / paid / payment_failed` |

幂等由数据库约束兜底，而不是应用先查：

- `processed_events` 主键 `(tenant_id, consumer, event_id)`；
- `notifications` 唯一约束 `(tenant_id, user_id, event_id, kind)`；
- `email_outbox` 唯一约束 `(tenant_id, dedupe_key)`，键为
  `<事件类型>:<event_id>:<user_id>`。

处理顺序：解码信封 → 校验快照 → 取收件人 → 开租户事务 →
`INSERT processed_events ... ON CONFLICT DO NOTHING` → 若已存在则提交并跳过 →
否则写通知与（可选的）邮件行 → 提交 → 提交位点。

错误分两类：瞬时错误（数据库连接、identity 暂时不可用）不提交位点，
原地退避重试；契约错误（未知事件类型、快照不一致）连同原字节写入
`<topic>.dlq` 后再提交位点，避免毒消息卡住整个分区。数据库的
SQLSTATE 22/23/42 也按永久错误处理——重试不会让约束冲突变得合法。

### 3. 收件人解析：签名令牌 + 内部接口

identity 增加 `GET /api/v1/internal/tenant-contacts`，返回该租户成员
（用户 ID、邮箱、显示名、角色）。信任模型：

1. `Authorization: Bearer <serviceauth token>`；
2. 令牌是 HMAC-SHA256 签名的 `{tenant_id, exp}`，有效期 2 分钟，密钥与
   用户 JWT 密钥分开，至少 32 字节，只从环境变量 / K8s Secret 读取；
3. identity 只信任签名，**租户 ID 只存在于签名载荷中**，不接受 query/path/body；
4. 网关不转发 `/api/v1/internal/*`，外部客户端即使拿到路径也无法到达；
5. notification 在每次取数时现签令牌，调用超时 3 秒，响应体上限 1 MiB；
   非 200 时不读取、不回显响应体，避免把内部错误细节写进日志。

这个接口只暴露"该租户里谁是谁"，不暴露密码、令牌或其他租户；它不新增
用户身份能力，也不能代替用户 JWT 调用业务 API。

### 4. 邮件发件箱与跨租户领取

消费事务内只写 `app.email_outbox`，不碰网络。后台 worker 负责发送：

- 领取通过 `app.claim_emails(p_limit)`（`SECURITY DEFINER`，所有者为迁移账号），
  `FOR UPDATE SKIP LOCKED`，一次领取一批，多副本不会重复领取；
- `email_outbox` 上只有 `ENABLE RLS`，没有 `FORCE`：表所有者（迁移账号）
  可以跨租户扫描，而运行时账号 `notification_app` 仍受租户策略约束，
  只能在已建立的租户上下文里把行标记为 `sent/failed`；
- 运行时账号对 `claim_emails` 只有 `EXECUTE`，对 `email_outbox` 的直接读写
  仍被 RLS 限制；这比给运行时账号 `BYPASSRLS` 小得多。

领取条件：`attempts < 5`，且处于 `pending/failed` 已到重试时间，或处于
`sending` 但 `last_attempt_at` 超过 5 分钟（进程崩溃后重新领取）。

发送成功后在新的事务里、以该行租户为上下文标记 `sent`；失败标记 `failed`
并写 `next_attempt_at`。退避为 1、2、4、8、16 分钟，最长 1 小时；达到 5 次后
不再领取，保留 `failed` 供人工排查，而不是静默删除。

### 5. SMTP 传输

使用标准库 `net/smtp`，不引入第三方邮件 SDK：

- 服务器支持 `STARTTLS` 时升级，最低 `tls.VersionTLS12`；
- `smtp_use_tls=true` 时，服务器不支持 STARTTLS 直接拒绝发送（生产默认）；
  本地 Mailpit 显式设为 `false`，避免本机演示被 TLS 卡住；
- 主题按 RFC 2047 编码，正文统一 CRLF；收件人与主题拒绝 CRLF 与控制字符，
  防止邮件头注入；
- 设置用户名时才做 `PlainAuth`，空用户名允许匿名中继（仅本地）；
- 每次发送有 30 秒上限，worker 不因单封邮件卡死。

### 6. 权限

- 站内通知只属于用户本人：列表、未读数、标记已读全部以 JWT 的 `uid` 过滤，
  租户来自 `tid`；没有"管理员查看他人通知"的接口。
- 通知页只提供提示与跳转；费用/账单的可见性仍由各自服务判定，
  用户点开链接后若无权访问会得到对应的 403/404。
- 通知 API 不提供删除与导出，M5 不做保留期策略。

### 7. 已知取舍

- **邮件至少一次**：发送成功但与"标记已发送"之间崩溃，会重发同一封邮件。
  收件人可能看到重复邮件；这与"通知不是业务事实"一致，优先保证不丢。
- **身份服务是收件人的唯一来源**：identity 不可用时，事件消费会退避重试，
  通知与邮件都会延迟，但不会丢；不会把邮箱复制到 notification 库。
- **本地默认关闭 TLS 强制**：`smtp_use_tls=false` 只用于 Mailpit；
  生产必须设为 `true`，否则配置审查应判失败。
- **没有已读状态回写**：通知已读只存在 notification 库，不产生事件，
  其他服务不应依赖它。

## 被否决的方案

1. **消费事务内直接发邮件**：外部 SMTP 抖动会拖住 Kafka 位点，且事务回滚
   无法撤回已发出的邮件。
2. **notification 直连 identity 库取邮箱**：违反"禁止跨服务直连数据库"，
   也会把 RLS 边界绕开。
3. **事件 payload 携带收件人邮箱**：邮箱会进入所有消费方与日志，且成员
   变更后事件里的快照立刻过期。
4. **把 `tenant_id` 放在内部接口的 query/path 里**：调用方可选择租户；
   即使有网段限制，也多一个永久需要正确的假设。
5. **给运行时账号 `BYPASSRLS` 来跨租户发信**：权限过大；`SECURITY DEFINER`
   函数只暴露"领取一批待发邮件"这一条能力。
6. **每个租户一个 worker/连接**：租户数量增长后不可运维；单 worker 顺序
   领取一批已经足够，横向扩展靠副本 + `SKIP LOCKED`。
7. **先查 `processed_events` 再插入**：并发重复投递会双写；唯一约束才是
   幂等来源。
