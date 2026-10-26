-- report 读模型：幂等表 + 费用事实表。
-- 设计见 ADR-0004。这里的数据都可以从事件主题重建，不是业务事实来源。
BEGIN;

-- 每个消费者自己的去重窗口。主键含 consumer，便于以后多个消费组共用一张表。
CREATE TABLE app.processed_events (
    tenant_id    uuid NOT NULL,
    consumer     text NOT NULL,
    event_id     uuid NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT processed_events_pkey PRIMARY KEY (tenant_id, consumer, event_id),
    CONSTRAINT processed_events_consumer_check CHECK (char_length(consumer) BETWEEN 1 AND 100)
);

-- 幂等记录只追加：处理过的事件不允许被改写或删除。
REVOKE UPDATE, DELETE ON app.processed_events FROM report_app;

-- 费用事实表：每个费用一行，保存事件里的全量快照。
-- 所有聚合都在查询时完成（M3 不建预聚合表），因此这里只保留报表需要的维度。
CREATE TABLE app.expense_facts (
    tenant_id          uuid NOT NULL,
    expense_id         uuid NOT NULL,
    submitter_id       uuid NOT NULL,
    status             text NOT NULL,
    category           text NOT NULL,
    currency           text NOT NULL,
    amount_minor       bigint NOT NULL,
    occurred_at        timestamptz NOT NULL,
    budget_id          uuid,
    decided_by         uuid,
    decided_at         timestamptz,
    version            integer NOT NULL,
    expense_created_at timestamptz NOT NULL,
    expense_updated_at timestamptz NOT NULL,
    -- 最后写入本行的事件 ID，排障时可以从事实行追到具体事件。
    last_event_id      uuid NOT NULL,
    CONSTRAINT expense_facts_pkey PRIMARY KEY (tenant_id, expense_id),
    CONSTRAINT expense_facts_status_check CHECK (
        status IN ('draft', 'submitted', 'approved', 'rejected', 'cancelled', 'reimbursed')
    ),
    CONSTRAINT expense_facts_category_check CHECK (
        category IN ('travel', 'meals', 'office', 'software', 'other')
    ),
    CONSTRAINT expense_facts_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT expense_facts_amount_check CHECK (amount_minor > 0 AND amount_minor <= 1000000000000),
    CONSTRAINT expense_facts_version_check CHECK (version > 0)
);

-- 报表按租户 + 发生时间过滤，再按类别/币种分组。
CREATE INDEX expense_facts_report_idx
    ON app.expense_facts (tenant_id, occurred_at, category, currency);
CREATE INDEX expense_facts_submitter_idx
    ON app.expense_facts (tenant_id, submitter_id, occurred_at);

-- 读模型同样启用 FORCE RLS：消费端每条事件用信封里的 tenant_id 建立事务上下文，
-- 查询端从 JWT 建立上下文。没有任何路径能跨租户读取。
ALTER TABLE app.processed_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.processed_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.processed_events
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE app.expense_facts ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.expense_facts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.expense_facts
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

COMMIT;
