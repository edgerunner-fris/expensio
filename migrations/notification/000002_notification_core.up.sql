-- notification 核心表：站内通知、邮件发件箱、事件幂等窗口。
--
-- 设计原则：
--   * 通知是事件的派生物，不是业务事实来源；重复事件由 processed_events 吸收。
--   * 邮件投递用事务内 outbox：先与通知同事务落库，再由后台 worker 发送，
--     绝不在数据库事务里直接连 SMTP。
--   * notifications 对运行时账号启用 FORCE RLS；email_outbox 只允许通过
--     SECURITY DEFINER 的 claim 函数跨租户领取，API 永远不读这张表。
BEGIN;

CREATE TABLE app.processed_events (
    tenant_id    uuid NOT NULL,
    consumer     text NOT NULL,
    event_id     uuid NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT processed_events_pkey PRIMARY KEY (tenant_id, consumer, event_id),
    CONSTRAINT processed_events_consumer_check CHECK (char_length(consumer) BETWEEN 1 AND 100)
);

-- 幂等记录只追加。
REVOKE UPDATE, DELETE ON app.processed_events FROM notification_app;

CREATE TABLE app.notifications (
    id            uuid PRIMARY KEY,
    tenant_id     uuid NOT NULL,
    user_id       uuid NOT NULL,
    kind          text NOT NULL,
    title         text NOT NULL,
    body          text NOT NULL,
    resource_type text,
    resource_id   uuid,
    event_id      uuid NOT NULL,
    read_at       timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- 同一事件对同一用户只产生一条同类通知：消费端重放不会刷屏。
    CONSTRAINT notifications_event_key UNIQUE (tenant_id, user_id, event_id, kind),
    CONSTRAINT notifications_kind_check CHECK (char_length(kind) BETWEEN 1 AND 80),
    CONSTRAINT notifications_title_check CHECK (char_length(btrim(title)) BETWEEN 1 AND 200),
    CONSTRAINT notifications_body_check CHECK (char_length(body) <= 2000)
);

CREATE INDEX notifications_inbox_idx
    ON app.notifications (tenant_id, user_id, created_at DESC);
CREATE INDEX notifications_unread_idx
    ON app.notifications (tenant_id, user_id, created_at DESC)
    WHERE read_at IS NULL;

CREATE TABLE app.email_outbox (
    id              uuid PRIMARY KEY,
    tenant_id       uuid NOT NULL,
    recipient       text NOT NULL,
    subject         text NOT NULL,
    body            text NOT NULL,
    dedupe_key      text NOT NULL,
    status          text NOT NULL DEFAULT 'pending',
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_attempt_at timestamptz,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz,
    CONSTRAINT email_outbox_dedupe_key UNIQUE (tenant_id, dedupe_key),
    CONSTRAINT email_outbox_status_check CHECK (status IN ('pending', 'sending', 'sent', 'failed')),
    CONSTRAINT email_outbox_attempts_check CHECK (attempts >= 0),
    CONSTRAINT email_outbox_recipient_check CHECK (char_length(recipient) BETWEEN 3 AND 320),
    CONSTRAINT email_outbox_subject_check CHECK (char_length(btrim(subject)) BETWEEN 1 AND 200),
    CONSTRAINT email_outbox_body_check CHECK (char_length(body) <= 10000)
);

CREATE INDEX email_outbox_due_idx
    ON app.email_outbox (next_attempt_at)
    WHERE status IN ('pending', 'failed');

-- claim_emails 是 worker 唯一的跨租户读路径。
--
-- 表上启用了 RLS（不是 FORCE）：函数以迁移账号身份执行，能够领取任意租户的
-- 待发邮件；运行时账号 notification_app 只能看到当前租户上下文里的行。
-- 这样既不需要给运行时账号 BYPASSRLS，又不会把发件箱暴露成全局可读表。
CREATE FUNCTION app.claim_emails(p_limit integer)
RETURNS TABLE (
    id         uuid,
    tenant_id  uuid,
    recipient  text,
    subject    text,
    body       text,
    attempts   integer
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = app, pg_temp
AS $$
    UPDATE app.email_outbox AS e
    SET status = 'sending',
        attempts = e.attempts + 1,
        last_attempt_at = now()
    WHERE e.id IN (
        SELECT o.id
        FROM app.email_outbox AS o
        WHERE o.attempts < 5
          AND (
                (
                    o.status IN ('pending', 'failed')
                    AND o.next_attempt_at <= now()
                )
                OR (
                    -- 进程崩溃后遗留的 sending 行，超时后允许重新领取。
                    o.status = 'sending'
                    AND o.last_attempt_at < now() - interval '5 minutes'
                )
              )
        ORDER BY o.created_at
        LIMIT greatest(1, least(coalesce(p_limit, 20), 100))
        FOR UPDATE SKIP LOCKED
    )
    RETURNING e.id, e.tenant_id, e.recipient, e.subject, e.body, e.attempts;
$$;

REVOKE ALL ON FUNCTION app.claim_emails(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.claim_emails(integer) TO notification_app;

ALTER TABLE app.processed_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.processed_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.processed_events
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE app.notifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.notifications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.notifications
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE app.email_outbox ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.email_outbox
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

COMMIT;
