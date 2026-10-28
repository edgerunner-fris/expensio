-- 租户切换所需的"我的成员关系"读写路径。
--
-- 为什么不是 SECURITY DEFINER 函数：memberships 是 ENABLE + FORCE RLS，
-- FORCE 对表属主同样生效，definer 函数以属主身份执行也查不到跨租户的行。
-- 要绕过只有两条路：引入 BYPASSRLS 专用角色（每个环境都要额外建角色，
-- 且该角色的绕过能力必须被严格限制），或复制一份目录表（触发器等同步，
-- 多一处会漂移的状态）。两者都比下面的策略重。
--
-- 这里选择：为 SELECT 增加一条"只看自己"的策略。它与租户策略是 OR 关系，
-- 因此不会放宽跨租户读取——只有 user_id 等于当前事务 app.user_id 的行可见，
-- 而且只对 SELECT 生效，写入仍必须满足租户隔离策略。
-- app.user_id 只由 db.WithinUser 从 JWT 的 sub 写入，是代码里唯一的设置点。
BEGIN;

CREATE POLICY self_membership_read ON app.memberships
    FOR SELECT
    USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid);

COMMIT;
