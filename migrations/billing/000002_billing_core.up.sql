-- billing 核心表：套餐、订阅、账单、支付流水、回调幂等与 outbox。
-- 设计见 ADR-0005。
--
-- 关键不变量尽量放在数据库层：
--   * 一个租户最多一条未取消订阅（部分唯一索引）
--   * 一个订阅周期最多一张账单（唯一约束），重复出账不可能发生
--   * 同一支付渠道事件只处理一次（唯一约束）
--   * 账单金额/币种/周期出账后不可改（触发器）
--   * 订阅与账单的租户隔离（FORCE RLS）
BEGIN;

CREATE TABLE app.plans (
    code        text PRIMARY KEY,
    name        text NOT NULL,
    price_minor bigint NOT NULL,
    currency    text NOT NULL,
    interval    text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT plans_code_check CHECK (code ~ '^[a-z][a-z0-9-]{1,40}$'),
    CONSTRAINT plans_name_check CHECK (char_length(btrim(name)) BETWEEN 1 AND 80),
    CONSTRAINT plans_price_check CHECK (price_minor > 0 AND price_minor <= 1000000000000),
    CONSTRAINT plans_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT plans_interval_check CHECK (interval IN ('monthly', 'yearly'))
);

-- 套餐目录是全局参考数据，运行时只能读；改价必须走新迁移，历史账单不受影响。
REVOKE INSERT, UPDATE, DELETE ON app.plans FROM billing_app;

INSERT INTO app.plans (code, name, price_minor, currency, interval) VALUES
    ('starter-monthly', 'Starter',  2900, 'USD', 'monthly'),
    ('growth-monthly',  'Growth',   9900, 'USD', 'monthly'),
    ('scale-monthly',   'Scale',   29900, 'USD', 'monthly'),
    ('starter-yearly',  'Starter', 29000, 'USD', 'yearly'),
    ('growth-yearly',   'Growth',  99000, 'USD', 'yearly');

CREATE TABLE app.subscriptions (
    id                   uuid PRIMARY KEY,
    tenant_id            uuid NOT NULL,
    plan_code            text NOT NULL REFERENCES app.plans (code),
    seats                integer NOT NULL,
    status               text NOT NULL DEFAULT 'active',
    current_period_start date NOT NULL,
    current_period_end   date NOT NULL,
    version              integer NOT NULL DEFAULT 1,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    created_by           uuid NOT NULL,
    updated_by           uuid NOT NULL,
    canceled_at          timestamptz,
    CONSTRAINT subscriptions_tenant_id_key UNIQUE (tenant_id, id),
    CONSTRAINT subscriptions_seats_check CHECK (seats BETWEEN 1 AND 1000),
    CONSTRAINT subscriptions_status_check CHECK (status IN ('active', 'past_due', 'canceled')),
    CONSTRAINT subscriptions_period_check CHECK (current_period_end >= current_period_start),
    CONSTRAINT subscriptions_version_check CHECK (version > 0),
    -- 取消时间与取消状态必须同时出现，不能有"已取消但没有取消时间"的行。
    CONSTRAINT subscriptions_canceled_consistency CHECK (
        (status = 'canceled') = (canceled_at IS NOT NULL)
    )
);

-- 一个租户最多一条活跃或欠费订阅；取消后可以重新订阅。
CREATE UNIQUE INDEX subscriptions_one_live_per_tenant
    ON app.subscriptions (tenant_id)
    WHERE status <> 'canceled';

CREATE TABLE app.subscription_status_transitions (
    from_status text NOT NULL,
    to_status   text NOT NULL,
    PRIMARY KEY (from_status, to_status),
    CONSTRAINT subscription_transitions_from_check CHECK (
        from_status IN ('active', 'past_due', 'canceled')
    ),
    CONSTRAINT subscription_transitions_to_check CHECK (
        to_status IN ('active', 'past_due', 'canceled')
    ),
    CONSTRAINT subscription_transitions_distinct CHECK (from_status <> to_status)
);

INSERT INTO app.subscription_status_transitions (from_status, to_status) VALUES
    ('active', 'past_due'),
    ('past_due', 'active'),
    ('active', 'canceled'),
    ('past_due', 'canceled');

CREATE TABLE app.invoices (
    id              uuid PRIMARY KEY,
    tenant_id       uuid NOT NULL,
    subscription_id uuid NOT NULL,
    number          text NOT NULL,
    status          text NOT NULL DEFAULT 'open',
    period_start    date NOT NULL,
    period_end      date NOT NULL,
    currency        text NOT NULL,
    amount_minor    bigint NOT NULL,
    due_on          date NOT NULL,
    issued_at       timestamptz NOT NULL DEFAULT now(),
    paid_at         timestamptz,
    version         integer NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT invoices_tenant_id_key UNIQUE (tenant_id, id),
    CONSTRAINT invoices_number_key UNIQUE (tenant_id, number),
    -- 一个订阅周期只有一张账单，重复出账在数据库层被拒绝。
    CONSTRAINT invoices_period_key UNIQUE (tenant_id, subscription_id, period_start),
    CONSTRAINT invoices_status_check CHECK (status IN ('open', 'paid')),
    CONSTRAINT invoices_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT invoices_amount_check CHECK (amount_minor > 0 AND amount_minor <= 1000000000000),
    CONSTRAINT invoices_period_check CHECK (period_end >= period_start),
    CONSTRAINT invoices_due_check CHECK (due_on >= period_start),
    CONSTRAINT invoices_version_check CHECK (version > 0),
    CONSTRAINT invoices_paid_consistency CHECK ((status = 'paid') = (paid_at IS NOT NULL)),
    CONSTRAINT invoices_subscription_fk FOREIGN KEY (tenant_id, subscription_id)
        REFERENCES app.subscriptions (tenant_id, id)
);

CREATE INDEX invoices_tenant_status_idx
    ON app.invoices (tenant_id, status, issued_at DESC);

-- 订阅与账单是财务记录：取消/欠费都保留行，任何路径都不删除。
REVOKE DELETE ON app.subscriptions, app.invoices FROM billing_app;

CREATE TABLE app.payments (
    id                 uuid PRIMARY KEY,
    tenant_id          uuid NOT NULL,
    invoice_id         uuid NOT NULL,
    provider           text NOT NULL,
    provider_payment_id text NOT NULL,
    amount_minor       bigint NOT NULL,
    currency           text NOT NULL,
    paid_at            timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT payments_provider_key UNIQUE (tenant_id, provider, provider_payment_id),
    CONSTRAINT payments_provider_check CHECK (provider ~ '^[a-z][a-z0-9_-]{1,31}$'),
    CONSTRAINT payments_amount_check CHECK (amount_minor > 0 AND amount_minor <= 1000000000000),
    CONSTRAINT payments_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT payments_invoice_fk FOREIGN KEY (tenant_id, invoice_id)
        REFERENCES app.invoices (tenant_id, id)
);

-- 支付流水只追加：冲正需要后续的贷项单，不允许改写历史。
REVOKE UPDATE, DELETE ON app.payments FROM billing_app;

CREATE TABLE app.webhook_events (
    id                uuid PRIMARY KEY,
    tenant_id         uuid NOT NULL,
    provider          text NOT NULL,
    provider_event_id text NOT NULL,
    event_type        text NOT NULL,
    invoice_id        uuid NOT NULL,
    payload_sha256    bytea NOT NULL,
    received_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT webhook_events_provider_key UNIQUE (tenant_id, provider, provider_event_id),
    CONSTRAINT webhook_events_type_check CHECK (
        event_type IN ('payment.succeeded', 'payment.failed')
    ),
    CONSTRAINT webhook_events_sha_check CHECK (octet_length(payload_sha256) = 32),
    CONSTRAINT webhook_events_invoice_fk FOREIGN KEY (tenant_id, invoice_id)
        REFERENCES app.invoices (tenant_id, id)
);

-- 回调记录同样只追加；去重完全依赖唯一约束，而不是"先查后插"。
REVOKE UPDATE, DELETE ON app.webhook_events FROM billing_app;

-- 回调只带 invoice_id，不带 tenant_id。这张表是"账单 → 租户"的唯一映射，
-- 由 invoices 的触发器在同事务写入；运行时角色没有直接读写权限，
-- 只能通过 SECURITY DEFINER 函数查询结果的 tenant_id。
CREATE TABLE app.invoice_lookup (
    invoice_id uuid PRIMARY KEY,
    tenant_id  uuid NOT NULL
);

REVOKE ALL ON app.invoice_lookup FROM billing_app;

CREATE TABLE app.outbox_events (
    id                uuid PRIMARY KEY,
    tenant_id         uuid NOT NULL,
    aggregate_id      uuid NOT NULL,
    aggregate_version integer NOT NULL,
    event_type        text NOT NULL,
    topic             text NOT NULL,
    payload           jsonb NOT NULL,
    attempts          integer NOT NULL DEFAULT 0,
    available_at      timestamptz NOT NULL DEFAULT now(),
    last_error        text NOT NULL DEFAULT '',
    published_at      timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT outbox_events_type_check CHECK (event_type IN (
        'subscription.created', 'subscription.canceled',
        'invoice.created', 'invoice.paid', 'invoice.payment_failed'
    )),
    CONSTRAINT outbox_events_version_check CHECK (aggregate_version > 0),
    CONSTRAINT outbox_events_attempts_check CHECK (attempts >= 0),
    CONSTRAINT outbox_events_topic_check CHECK (char_length(topic) BETWEEN 1 AND 200),
    CONSTRAINT outbox_events_payload_check CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT outbox_events_last_error_check CHECK (char_length(last_error) <= 500)
);

CREATE INDEX outbox_events_pending_idx
    ON app.outbox_events (available_at, created_at)
    WHERE published_at IS NULL;
CREATE INDEX outbox_events_aggregate_idx
    ON app.outbox_events (tenant_id, aggregate_id, aggregate_version);

REVOKE DELETE ON app.outbox_events FROM billing_app;

CREATE FUNCTION app.touch_updated_at() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END
$$;

CREATE FUNCTION app.subscriptions_guard_update() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    IF NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.plan_code <> OLD.plan_code OR NEW.seats <> OLD.seats
       OR NEW.current_period_start <> OLD.current_period_start
       OR NEW.current_period_end <> OLD.current_period_end
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'subscription identity columns are immutable'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status AND NOT EXISTS (
        SELECT 1 FROM app.subscription_status_transitions t
        WHERE t.from_status = OLD.status AND t.to_status = NEW.status
    ) THEN
        RAISE EXCEPTION 'invalid subscription status transition % -> %', OLD.status, NEW.status
            USING ERRCODE = '23514';
    END IF;

    -- 取消时间是取消动作的一部分；进入 canceled 却不写时间会被下面的 CHECK 拒绝，
    -- 这里再补一条：非取消状态不允许提前写取消时间。
    IF NEW.status <> 'canceled' AND NEW.canceled_at IS NOT NULL THEN
        RAISE EXCEPTION 'canceled_at is only allowed on canceled subscriptions'
            USING ERRCODE = '23514';
    END IF;

    NEW.version := OLD.version + 1;
    RETURN NEW;
END
$$;

CREATE FUNCTION app.invoices_guard_update() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    IF NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.subscription_id <> OLD.subscription_id OR NEW.number <> OLD.number
       OR NEW.period_start <> OLD.period_start OR NEW.period_end <> OLD.period_end
       OR NEW.currency <> OLD.currency OR NEW.amount_minor <> OLD.amount_minor
       OR NEW.due_on <> OLD.due_on OR NEW.issued_at <> OLD.issued_at
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'invoice financial columns are immutable'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status
       AND NOT (OLD.status = 'open' AND NEW.status = 'paid') THEN
        RAISE EXCEPTION 'invalid invoice status transition % -> %', OLD.status, NEW.status
            USING ERRCODE = '23514';
    END IF;

    IF OLD.status = 'paid' AND NEW.paid_at IS DISTINCT FROM OLD.paid_at THEN
        RAISE EXCEPTION 'paid_at is immutable once paid'
            USING ERRCODE = '23514';
    END IF;

    NEW.version := OLD.version + 1;
    RETURN NEW;
END
$$;

-- 账单插入时同步写回调映射。SECURITY DEFINER 让运行时角色无需拥有
-- invoice_lookup 的写权限；触发器在业务事务内执行，回滚时映射一起消失。
CREATE FUNCTION app.invoices_record_lookup() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, app AS
$$
BEGIN
    INSERT INTO app.invoice_lookup (invoice_id, tenant_id)
    VALUES (NEW.id, NEW.tenant_id);
    RETURN NEW;
END
$$;

REVOKE ALL ON FUNCTION app.invoices_record_lookup() FROM PUBLIC;

CREATE FUNCTION app.invoice_tenant(invoice uuid) RETURNS uuid
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, app AS
$$
    SELECT tenant_id FROM app.invoice_lookup WHERE invoice_id = $1
$$;

REVOKE ALL ON FUNCTION app.invoice_tenant(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.invoice_tenant(uuid) TO billing_app;

CREATE TRIGGER subscriptions_touch BEFORE UPDATE ON app.subscriptions
    FOR EACH ROW EXECUTE FUNCTION app.touch_updated_at();
CREATE TRIGGER invoices_touch BEFORE UPDATE ON app.invoices
    FOR EACH ROW EXECUTE FUNCTION app.touch_updated_at();

CREATE TRIGGER subscriptions_guard_update BEFORE UPDATE ON app.subscriptions
    FOR EACH ROW EXECUTE FUNCTION app.subscriptions_guard_update();
CREATE TRIGGER invoices_guard_update BEFORE UPDATE ON app.invoices
    FOR EACH ROW EXECUTE FUNCTION app.invoices_guard_update();
CREATE TRIGGER invoices_record_lookup AFTER INSERT ON app.invoices
    FOR EACH ROW EXECUTE FUNCTION app.invoices_record_lookup();

ALTER TABLE app.subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.subscriptions
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE app.invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.invoices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.invoices
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE app.payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.payments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.payments
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE app.webhook_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.webhook_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.webhook_events
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

COMMIT;
