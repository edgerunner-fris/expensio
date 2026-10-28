-- 登录失败计数与临时锁定。
--
-- 无 RLS，与 users 同理：第一次凭证校验发生在"租户上下文建立之前"，
-- 此时还不知道（也不该先知道）租户是谁。保护来自另外两层：
-- 只有 identity_app 能访问本库，且本表只存归一化邮箱与计数。
--
-- 键不含租户：同一账号在不同租户的失败合并计数，换 slug 不能绕过锁定。
BEGIN;

CREATE TABLE app.login_failures (
    email           text PRIMARY KEY,
    failed_count    integer NOT NULL DEFAULT 0,
    first_failed_at timestamptz NOT NULL DEFAULT now(),
    locked_until    timestamptz,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT login_failures_email_check CHECK (
        char_length(email) BETWEEN 3 AND 254 AND email = lower(email)
    ),
    CONSTRAINT login_failures_count_check CHECK (failed_count >= 0)
);

-- 只有被锁定的行才有清理价值；部分索引避免给正常流量加无用条目。
CREATE INDEX login_failures_locked_idx
    ON app.login_failures (locked_until)
    WHERE locked_until IS NOT NULL;

CREATE TRIGGER login_failures_touch BEFORE UPDATE ON app.login_failures
    FOR EACH ROW EXECUTE FUNCTION app.touch_updated_at();

COMMIT;
