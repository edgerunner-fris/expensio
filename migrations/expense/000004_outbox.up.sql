-- 事务内 outbox：业务事务提交前，事件只存在于这张表里。
-- 设计见 ADR-0004。
--
-- 这张表刻意**不启用 RLS**：投递器需要跨租户扫描待发事件，
-- 而它没有任何对外 HTTP 读取路径。事件本身仍带 tenant_id，
-- 消费端在写入自己的库时会重新建立租户上下文。
BEGIN;

CREATE TABLE app.outbox_events (
    id                uuid PRIMARY KEY,
    tenant_id         uuid NOT NULL,
    aggregate_id      uuid NOT NULL,
    aggregate_version integer NOT NULL,
    event_type        text NOT NULL,
    topic             text NOT NULL,
    -- 完整信封（events.Envelope）的 JSON 表示。使用 jsonb 便于人工排查；
    -- 消费者只依赖字段含义，不依赖键顺序。
    payload           jsonb NOT NULL,
    attempts          integer NOT NULL DEFAULT 0,
    available_at      timestamptz NOT NULL DEFAULT now(),
    last_error        text NOT NULL DEFAULT '',
    published_at      timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT outbox_events_type_check CHECK (event_type IN (
        'expense.submitted', 'expense.approved', 'expense.rejected',
        'expense.cancelled', 'expense.reimbursed'
    )),
    CONSTRAINT outbox_events_version_check CHECK (aggregate_version > 0),
    CONSTRAINT outbox_events_attempts_check CHECK (attempts >= 0),
    CONSTRAINT outbox_events_topic_check CHECK (char_length(topic) BETWEEN 1 AND 200),
    CONSTRAINT outbox_events_payload_check CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT outbox_events_last_error_check CHECK (char_length(last_error) <= 500)
);

-- 投递器只扫未发布的事件；部分索引让扫描量与历史总量无关。
CREATE INDEX outbox_events_pending_idx
    ON app.outbox_events (available_at, created_at)
    WHERE published_at IS NULL;

-- 审计与排障：按聚合查一张单的事件序列。
CREATE INDEX outbox_events_aggregate_idx
    ON app.outbox_events (tenant_id, aggregate_id, aggregate_version);

-- outbox 是只追加的审计记录：投递器只更新状态，任何路径都不应删除。
REVOKE DELETE ON app.outbox_events FROM expense_app;

COMMIT;
