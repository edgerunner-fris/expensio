# ADR-0013：租户切换、个人设置、审计日志页与登录失败锁定

- 状态：已接受（M17）
- 日期：2026-10-28

## 背景

`users` 是全局账号、`memberships` 是"用户 × 租户"关系，一个邮箱本来就可以
属于多个租户；但登录接口只接受 slug，用户没有任何入口查看或切换到自己的
其他租户。审计接口 `GET /api/v1/audit-logs` 已经存在，前端却没有页面。
账号没有自助修改资料或密码的入口；登录对暴力破解只有网关的按 IP 限流，
换 IP 即可绕过。M17 补齐这四块。

## 决策

### 1. 租户列表与切换：一条"只看自己"的 SELECT 策略

`memberships` 是 `ENABLE + FORCE ROW LEVEL SECURITY`，按 `app.tenant_id`
隔离。"列出我的全部租户"天然发生在当前租户之外，RLS 会让它返回 0 行。

第一版实现与 `app.find_invitation` 同一模式：`SECURITY DEFINER` 函数
`app.user_tenants(uuid)`，属主是 `identity_migrator`。集成测试当场暴露它返回
0 行——**`FORCE` 对表属主同样生效**，definer 函数以属主身份执行也绕不过策略。
`find_invitation` 能工作只是因为 `invitations` 只 `ENABLE`、没有 `FORCE`；
两者不是同一前提，不能照抄。

最终方案（migration `000004_tenant_switch`）：在 `memberships` 上增加一条
仅 SELECT 的宽松策略

```sql
CREATE POLICY self_membership_read ON app.memberships
    FOR SELECT
    USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid);
```

它与 `tenant_isolation` 是 OR 关系，因此只在本事务显式设置了 `app.user_id`
时才生效；而 `app.user_id` 的唯一设置点是 `db.WithinUser`，其入参只来自
access token 的 `sub`，不接受任何请求参数。边界因此收敛为：

- 只影响 SELECT，写入仍必须满足 `tenant_isolation` 的 `WITH CHECK`；
- 只能看到 `user_id` 等于自己的行，看不到同租户其他成员、也看不到其他租户；
- 不设置 `app.tenant_id`，同一事务里其他受 RLS 约束的表（如 `audit_logs`）
  仍然一行都读不到。

被否决的方案：**BYPASSRLS 专用角色**（每个环境都要额外建角色，绕过能力一旦
被误用就是全库可读）；**definer 函数**（在 FORCE 下不成立，见上）；
**触发器同步的目录表**（多一处会漂移的状态，且要维护删除/恢复）。

端点：

| 方法与路径 | 说明 |
| --- | --- |
| `GET /api/v1/auth/tenants` | 当前用户的全部非删除成员关系，标记当前租户 |
| `POST /api/v1/auth/switch-tenant` | 凭当前 access token 换取目标租户的新会话 |

切换规则：

- 目标租户必须是**未删除、成员状态 active** 的成员关系，否则 403；
- 目标就是当前租户 → 400（不做无意义的刷新）；
- 租户被暂停 → 403，与登录时一致；
- 新会话是**新的刷新令牌链**，旧链保持有效："切换"不是"撤销凭据"，
  其他设备上的会话不受影响，撤销仍然走登出；
- 审计写在目标租户：`auth.tenant_switched`，metadata 只含来源租户 ID。

### 2. 个人设置

- `PATCH /api/v1/me` 只接受 `display_name`（去空白后 1–64 个字符，拒绝控制
  字符）。账号是全局的，
  改名对所有租户生效——这是全局账号模型的直接结果，不提供"每个租户不同昵称"；
- `POST /api/v1/auth/change-password` 需要当前密码；成功后**撤销该用户的全部
  刷新令牌链**，前端清空会话并回到登录页。旧 access token 最多存活其剩余
  TTL（15 分钟），与"改密后旧会话尽快失效"的取舍一致；
- 两个动作都写审计（`user.profile_updated` / `auth.password_changed`），
  元数据不含任何密码或摘要。

### 3. 审计日志页

接口本身不变，但读模型补上了 `ip` 与 `user_agent`（`ip` 列是 `inet`，
查询用 `host()` 去掉 `/32` 前缀）。前端加管理员菜单「审计日志」，表格列
（时间、动作、操作者、对象、来源 IP/UA）+ 可展开的 metadata JSON，分页 20。
普通成员看不到菜单，服务端本来也返回 403。

### 4. 登录失败锁定

新表 `app.login_failures`（无 RLS，与 `users` 同理：失败计数发生在租户
上下文建立之前）：

- 键是归一化邮箱，不含租户——同一账号在不同租户的失败合并计数；
- 15 分钟窗口内累计 5 次凭证错误 → 锁定 15 分钟；
- 锁定期内一律返回 429 `auth.too_many_attempts`，不校验真实密码；
  仍执行一次对 dummy hash 的 argon2，保持与"账号不存在"相同的耗时；
- **不存在的邮箱同样计数并锁定**：否则"是否被锁"本身就成了账号存在性探测点；
- 成功登录在同一事务里删除计数行；
- 窗口外或锁过期后的下一次失败从 1 重新计数。

阈值与时长是常量，测试里通过 SQL 直接把 `locked_until` 拨到过去验证解锁，
不等待真实时间。

## 备选方案

- **切换租户时撤销旧刷新链**：更接近"单活跃会话"，但会让另一个标签页在
  刷新时直接掉线；切换不是高风险动作，保留旧链更符合多标签使用习惯。
- **用 Redis 存失败计数**：identity 目前不依赖 Redis；锁定是低频写，
  放数据库可以少一个部署依赖，也避免 Redis 重启后计数清零。
- **只按 IP 限流**：现有网关策略在换 IP 后失效，账号级锁定是必要的补充。
- **每个租户独立昵称**：需要新的"租户内资料"表，收益不抵复杂度。

## 风险

- 账号级锁定可被用来拒绝服务（故意输错把别人锁住）。缓解：15 分钟上限、
  网关按 IP 限流、成功登录清零；不做永久锁定。
- `login_failures` 对不存在的邮箱也写行（这是反枚举的代价），且没有定时清理；
  网关按 IP 限流限制了增长速度，但长期仍会缓慢增长。清理任务列入后续工作。
- `display_name` 全局生效，用户在不同租户下看到同一个名字。
- `change-password` 里输错当前密码不计入登录失败锁定：持有 access token 的
  攻击者仍可反复尝试当前密码，受 argon2 成本与网关按租户限流（1200/min）约束。
  若要收紧，应复用 `login_failures` 并按账号邮箱计数。
- 资料与密码是账号级操作，审计却写进当前租户：同一账号在多个租户的管理员
  看不到彼此的记录。跨租户账号审计需要独立的全局审计表，本期不做。
- 浏览器只保存一份会话（localStorage），同一浏览器的多个标签页共享它：
  切换后其他标签页在下一次令牌刷新时会跟随到新租户。服务端保留旧链是为了
  不把"切换"等同于"撤销"，不承诺同一浏览器内并行停留多个租户。
- 改密后旧 access token 在剩余 TTL（≤15 分钟）内仍可调用只读接口；
  刷新链已全部撤销，无法续期。若要立即失效，需要引入令牌黑名单。
- `self_membership_read` 是一张表上的第二条策略，安全依赖"`app.user_id`
  只由 `WithinUser` 从 JWT 的 `sub` 写入"这一约定。集成测试覆盖了列表只返回
  本人、非成员切换 403；新增调用点必须走 `WithinUser`，不得手工
  `set_config('app.user_id', ...)`。
- 列表查询跨租户，无法命中 `memberships(tenant_id, ...)` 的主键前缀；已有
  `memberships_user_idx (user_id) WHERE deleted_at IS NULL` 正是它的索引。
