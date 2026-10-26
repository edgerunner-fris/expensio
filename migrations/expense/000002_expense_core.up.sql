-- expense 核心表：预算、费用单、审批步骤、状态迁移白名单。
--
-- 关键约束都尽量放在数据库层，而不是只写在 service 里：
--   * 预算不超占：CHECK reserved + spent <= amount
--   * 预算期间不重叠：EXCLUDE USING gist
--   * 状态只能走白名单：触发器 + expense_status_transitions
--   * 提交后金额/类别/预算不可改：触发器
--   * 审批记录只追加：REVOKE UPDATE, DELETE
-- 这些约束在集成测试里用 {service}_app 角色逐条验证。
BEGIN;

-- 区间重叠约束需要 GiST 的等值操作符类；btree_gist 在 PG16 中是 trusted extension，
-- 库属主 expense_migrator 不需要超级用户即可创建。
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE app.budgets (
    id             uuid PRIMARY KEY,
    tenant_id      uuid NOT NULL,
    name           text NOT NULL,
    category       text NOT NULL,
    currency       text NOT NULL,
    period_start   date NOT NULL,
    period_end     date NOT NULL,
    amount_minor   bigint NOT NULL,
    reserved_minor bigint NOT NULL DEFAULT 0,
    spent_minor    bigint NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    created_by     uuid NOT NULL,
    updated_by     uuid NOT NULL,
    deleted_at     timestamptz,
    CONSTRAINT budgets_tenant_id_key UNIQUE (tenant_id, id),
    CONSTRAINT budgets_name_check CHECK (char_length(btrim(name)) BETWEEN 1 AND 120),
    CONSTRAINT budgets_category_check CHECK (category IN ('travel', 'meals', 'office', 'software', 'other')),
    CONSTRAINT budgets_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT budgets_period_check CHECK (period_end >= period_start),
    CONSTRAINT budgets_amount_check CHECK (amount_minor > 0 AND amount_minor <= 1000000000000),
    CONSTRAINT budgets_reserved_nonneg CHECK (reserved_minor >= 0),
    CONSTRAINT budgets_spent_nonneg CHECK (spent_minor >= 0),
    -- 任何代码路径都不可能把预算写超；条件 UPDATE 只是第一道拦截。
    CONSTRAINT budgets_committed_within_amount CHECK (reserved_minor + spent_minor <= amount_minor),
    -- 同租户、同类别、同币种的有效期不得重叠，否则"匹配哪一条预算"没有确定答案。
    CONSTRAINT budgets_period_no_overlap EXCLUDE USING gist (
        tenant_id WITH =,
        category WITH =,
        currency WITH =,
        daterange(period_start, period_end, '[]') WITH &&
    ) WHERE (deleted_at IS NULL)
);

CREATE INDEX budgets_match_idx
    ON app.budgets (tenant_id, category, currency, period_start DESC, period_end)
    WHERE deleted_at IS NULL;

CREATE TABLE app.expenses (
    id               uuid PRIMARY KEY,
    tenant_id        uuid NOT NULL,
    submitter_id     uuid NOT NULL,
    status           text NOT NULL DEFAULT 'draft',
    category         text NOT NULL,
    currency         text NOT NULL,
    amount_minor     bigint NOT NULL,
    merchant         text NOT NULL DEFAULT '',
    description      text NOT NULL DEFAULT '',
    occurred_at      timestamptz NOT NULL,
    budget_id        uuid,
    decided_by       uuid,
    decided_at       timestamptz,
    rejection_reason text NOT NULL DEFAULT '',
    version          integer NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    created_by       uuid NOT NULL,
    updated_by       uuid NOT NULL,
    deleted_at       timestamptz,
    CONSTRAINT expenses_tenant_id_key UNIQUE (tenant_id, id),
    CONSTRAINT expenses_status_check CHECK (
        status IN ('draft', 'submitted', 'approved', 'rejected', 'cancelled', 'reimbursed')
    ),
    CONSTRAINT expenses_category_check CHECK (category IN ('travel', 'meals', 'office', 'software', 'other')),
    CONSTRAINT expenses_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT expenses_amount_check CHECK (amount_minor > 0 AND amount_minor <= 1000000000000),
    CONSTRAINT expenses_merchant_check CHECK (char_length(merchant) <= 200),
    CONSTRAINT expenses_description_check CHECK (char_length(description) <= 2000),
    CONSTRAINT expenses_rejection_reason_check CHECK (char_length(rejection_reason) <= 1000),
    CONSTRAINT expenses_version_check CHECK (version > 0),
    -- 审批决定字段与状态必须一致：不能出现"已批准但没有审批人"的行。
    CONSTRAINT expenses_decision_consistency CHECK (
        (status IN ('approved', 'reimbursed', 'rejected'))
        = (decided_by IS NOT NULL AND decided_at IS NOT NULL)
    ),
    CONSTRAINT expenses_rejected_reason_check CHECK (
        status <> 'rejected' OR char_length(btrim(rejection_reason)) > 0
    ),
    -- 软删只允许草稿；提交后的单据只能走撤销/驳回，不能直接删掉。
    CONSTRAINT expenses_deleted_only_draft CHECK (deleted_at IS NULL OR status = 'draft'),
    -- 预算与费用必须在同一租户；budget_id 为空时按 MATCH SIMPLE 跳过检查。
    CONSTRAINT expenses_budget_fk FOREIGN KEY (tenant_id, budget_id)
        REFERENCES app.budgets (tenant_id, id)
);

CREATE INDEX expenses_tenant_submitter_idx
    ON app.expenses (tenant_id, submitter_id, created_at DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX expenses_tenant_status_idx
    ON app.expenses (tenant_id, status, created_at DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX expenses_tenant_budget_idx
    ON app.expenses (tenant_id, budget_id)
    WHERE budget_id IS NOT NULL;

-- 审批步骤只追加：没有 updated_at / deleted_at，且运行时角色没有 UPDATE/DELETE 权限。
CREATE TABLE app.approval_steps (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL,
    expense_id  uuid NOT NULL,
    actor_id    uuid NOT NULL,
    action      text NOT NULL,
    from_status text NOT NULL,
    to_status   text NOT NULL,
    comment     text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT approval_steps_action_check CHECK (
        action IN ('submit', 'approve', 'reject', 'cancel', 'reimburse')
    ),
    CONSTRAINT approval_steps_from_check CHECK (
        from_status IN ('draft', 'submitted', 'approved', 'rejected', 'cancelled', 'reimbursed')
    ),
    CONSTRAINT approval_steps_to_check CHECK (
        to_status IN ('draft', 'submitted', 'approved', 'rejected', 'cancelled', 'reimbursed')
    ),
    CONSTRAINT approval_steps_comment_check CHECK (char_length(comment) <= 1000),
    -- 动作与状态边必须一一对应，避免写入"approve 但 from=approved"这种脏记录。
    CONSTRAINT approval_steps_action_transition_check CHECK (
        (action = 'submit'    AND from_status = 'draft'     AND to_status = 'submitted') OR
        (action = 'approve'   AND from_status = 'submitted' AND to_status = 'approved') OR
        (action = 'reject'    AND from_status = 'submitted' AND to_status = 'rejected') OR
        (action = 'cancel'    AND from_status = 'submitted' AND to_status = 'cancelled') OR
        (action = 'reimburse' AND from_status = 'approved'  AND to_status = 'reimbursed')
    ),
    CONSTRAINT approval_steps_reject_reason_check CHECK (
        action <> 'reject' OR char_length(btrim(comment)) > 0
    ),
    CONSTRAINT approval_steps_expense_fk FOREIGN KEY (tenant_id, expense_id)
        REFERENCES app.expenses (tenant_id, id)
);

CREATE INDEX approval_steps_expense_idx
    ON app.approval_steps (tenant_id, expense_id, created_at, id);

-- 合法的状态边单独成表：service 与触发器读取同一份白名单，
-- 新增状态时必须同时改这里，避免两份硬编码悄悄分叉。
CREATE TABLE app.expense_status_transitions (
    from_status text NOT NULL,
    to_status   text NOT NULL,
    PRIMARY KEY (from_status, to_status),
    CONSTRAINT expense_status_transitions_from_check CHECK (
        from_status IN ('draft', 'submitted', 'approved', 'rejected', 'cancelled', 'reimbursed')
    ),
    CONSTRAINT expense_status_transitions_to_check CHECK (
        to_status IN ('draft', 'submitted', 'approved', 'rejected', 'cancelled', 'reimbursed')
    ),
    CONSTRAINT expense_status_transitions_distinct CHECK (from_status <> to_status)
);

INSERT INTO app.expense_status_transitions (from_status, to_status) VALUES
    ('draft', 'submitted'),
    ('submitted', 'approved'),
    ('submitted', 'rejected'),
    ('submitted', 'cancelled'),
    ('approved', 'reimbursed');

CREATE FUNCTION app.touch_updated_at() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END
$$;

-- 新单据只能以 draft 落库；"插入一张已批准的单"必须被数据库拒绝。
CREATE FUNCTION app.expenses_guard_insert() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    IF NEW.status <> 'draft' THEN
        RAISE EXCEPTION 'expense must be created as draft, got %', NEW.status
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION app.expenses_guard_update() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    IF NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'expense identity columns are immutable'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NOT EXISTS (
            SELECT 1 FROM app.expense_status_transitions t
            WHERE t.from_status = OLD.status AND t.to_status = NEW.status
        ) THEN
            RAISE EXCEPTION 'invalid expense status transition % -> %', OLD.status, NEW.status
                USING ERRCODE = '23514';
        END IF;
    END IF;

    -- 提交之后，影响预算/金额的字段全部冻结。草稿阶段允许反复修改。
    IF OLD.status <> 'draft' AND (
        NEW.amount_minor IS DISTINCT FROM OLD.amount_minor
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.category IS DISTINCT FROM OLD.category
        OR NEW.occurred_at IS DISTINCT FROM OLD.occurred_at
        OR NEW.budget_id IS DISTINCT FROM OLD.budget_id
        OR NEW.submitter_id IS DISTINCT FROM OLD.submitter_id
    ) THEN
        RAISE EXCEPTION 'expense % is immutable after submission', OLD.id
            USING ERRCODE = '23514';
    END IF;

    IF OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL THEN
        RAISE EXCEPTION 'expense % is soft-deleted and cannot be restored', OLD.id
            USING ERRCODE = '23514';
    END IF;

    RETURN NEW;
END
$$;

CREATE FUNCTION app.budgets_guard_update() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    IF NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'budget identity columns are immutable'
            USING ERRCODE = '23514';
    END IF;

    IF OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL THEN
        RAISE EXCEPTION 'budget % is soft-deleted and cannot be restored', OLD.id
            USING ERRCODE = '23514';
    END IF;

    -- 还有占用或支出时不允许软删，否则报表会出现"花掉的钱没有预算"。
    IF NEW.deleted_at IS NOT NULL AND OLD.deleted_at IS NULL
       AND (OLD.reserved_minor > 0 OR OLD.spent_minor > 0) THEN
        RAISE EXCEPTION 'budget % still has committed money', OLD.id
            USING ERRCODE = '23514';
    END IF;

    RETURN NEW;
END
$$;

CREATE TRIGGER budgets_touch BEFORE UPDATE ON app.budgets
    FOR EACH ROW EXECUTE FUNCTION app.touch_updated_at();
CREATE TRIGGER expenses_touch BEFORE UPDATE ON app.expenses
    FOR EACH ROW EXECUTE FUNCTION app.touch_updated_at();

CREATE TRIGGER expenses_guard_insert BEFORE INSERT ON app.expenses
    FOR EACH ROW EXECUTE FUNCTION app.expenses_guard_insert();
CREATE TRIGGER expenses_guard_update BEFORE UPDATE ON app.expenses
    FOR EACH ROW EXECUTE FUNCTION app.expenses_guard_update();
CREATE TRIGGER budgets_guard_update BEFORE UPDATE ON app.budgets
    FOR EACH ROW EXECUTE FUNCTION app.budgets_guard_update();

-- 参考表只读；审批记录只追加。默认权限已在基线里授予，这里做减法。
REVOKE INSERT, UPDATE, DELETE ON app.expense_status_transitions FROM expense_app;
REVOKE UPDATE, DELETE ON app.approval_steps FROM expense_app;

-- RLS：与 identity 使用同一套策略表达式。
-- 未设置 app.tenant_id 时 nullif(...) 为 NULL，策略不通过，默认零行。
ALTER TABLE app.budgets ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.budgets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.budgets
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE app.expenses ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.expenses FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.expenses
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE app.approval_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.approval_steps FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.approval_steps
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

COMMIT;
