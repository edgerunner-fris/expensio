-- M15：附件元数据、评论、按币种的审批策略与各级批准记录。
--
-- 设计见 docs/adr/0011-expense-attachments-comments-and-approval-levels.md。
-- 与 000002 一样，能放进数据库的规则就不只写在 service 里：
--   * 分级批准只追加，同级别、同审批人都有唯一约束；
--   * 审批人不能是提交人，级别必须逐级递增；
--   * 附件对象键必须落在本租户本单据的前缀下，类型与大小有 CHECK；
--   * 四张新表全部启用并强制 RLS。
BEGIN;

-- 审批策略：每个租户、每个币种至多一条；金额达到门槛即需要两级审批。
CREATE TABLE app.approval_policies (
    tenant_id                    uuid NOT NULL,
    currency                     text NOT NULL,
    second_level_threshold_minor bigint NOT NULL,
    created_at                   timestamptz NOT NULL DEFAULT now(),
    updated_at                   timestamptz NOT NULL DEFAULT now(),
    created_by                   uuid NOT NULL,
    updated_by                   uuid NOT NULL,
    CONSTRAINT approval_policies_pkey PRIMARY KEY (tenant_id, currency),
    CONSTRAINT approval_policies_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT approval_policies_threshold_check CHECK (
        second_level_threshold_minor > 0 AND second_level_threshold_minor <= 1000000000000
    )
);

-- 提交时把要求级数快照到单据上；之后改策略不影响在途单据。
ALTER TABLE app.expenses
    ADD COLUMN required_approval_levels smallint NOT NULL DEFAULT 1,
    ADD COLUMN approval_level smallint NOT NULL DEFAULT 0;

-- 迁移以表属主身份执行，但 FORCE RLS 对属主同样生效；不暂时关掉它，
-- 这个没有 app.tenant_id 的 UPDATE 会匹配 0 行而不报错，随后加约束才暴露。
ALTER TABLE app.expenses NO FORCE ROW LEVEL SECURITY;
UPDATE app.expenses
SET approval_level = required_approval_levels
WHERE status IN ('approved', 'reimbursed');
ALTER TABLE app.expenses FORCE ROW LEVEL SECURITY;

ALTER TABLE app.expenses
    ADD CONSTRAINT expenses_required_levels_check CHECK (required_approval_levels BETWEEN 1 AND 2),
    ADD CONSTRAINT expenses_approval_level_check CHECK (approval_level BETWEEN 0 AND required_approval_levels),
    -- 已批准/已报销必须恰好走完所有级别；其他状态必须还没走完。
    ADD CONSTRAINT expenses_approval_level_consistency CHECK (
        (status IN ('approved', 'reimbursed')) = (approval_level = required_approval_levels)
    );

-- 独立触发器：不改写 000002 的 expenses_guard_update，回滚只需删本函数。
CREATE FUNCTION app.expenses_guard_approval_progress() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    IF OLD.status <> 'draft'
       AND NEW.required_approval_levels IS DISTINCT FROM OLD.required_approval_levels THEN
        RAISE EXCEPTION 'expense %: required_approval_levels is immutable after submission', OLD.id
            USING ERRCODE = '23514';
    END IF;

    IF NEW.approval_level IS DISTINCT FROM OLD.approval_level
       AND NEW.approval_level <> OLD.approval_level + 1 THEN
        RAISE EXCEPTION 'expense %: approval_level can only advance by one (from % to %)',
            OLD.id, OLD.approval_level, NEW.approval_level
            USING ERRCODE = '23514';
    END IF;

    RETURN NEW;
END
$$;

CREATE TRIGGER expenses_guard_approval_progress BEFORE UPDATE ON app.expenses
    FOR EACH ROW EXECUTE FUNCTION app.expenses_guard_approval_progress();

-- 各级批准：只追加。level 与 actor 的唯一约束把'同一级批两次'和
-- '同一个人批两级'都挡在数据库层。
CREATE TABLE app.expense_approvals (
    id         uuid PRIMARY KEY,
    tenant_id  uuid NOT NULL,
    expense_id uuid NOT NULL,
    level      smallint NOT NULL,
    actor_id   uuid NOT NULL,
    comment    text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT expense_approvals_level_check CHECK (level BETWEEN 1 AND 2),
    CONSTRAINT expense_approvals_comment_check CHECK (char_length(comment) <= 1000),
    CONSTRAINT expense_approvals_expense_fk FOREIGN KEY (tenant_id, expense_id)
        REFERENCES app.expenses (tenant_id, id),
    CONSTRAINT expense_approvals_level_key UNIQUE (tenant_id, expense_id, level),
    CONSTRAINT expense_approvals_actor_key UNIQUE (tenant_id, expense_id, actor_id)
);

CREATE FUNCTION app.expense_approvals_guard() RETURNS trigger
    LANGUAGE plpgsql AS
$$
DECLARE
    v_submitter uuid;
    v_status    text;
    v_required  smallint;
    v_done      integer;
BEGIN
    -- 锁单：并发两级批准必须串行，否则两个请求会读到同一个 count。
    SELECT submitter_id, status, required_approval_levels
      INTO v_submitter, v_status, v_required
      FROM app.expenses
     WHERE tenant_id = NEW.tenant_id AND id = NEW.expense_id
       FOR UPDATE;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'expense % not found in tenant', NEW.expense_id
            USING ERRCODE = '23503';
    END IF;
    IF v_status <> 'submitted' THEN
        RAISE EXCEPTION 'expense % is %, cannot record approval', NEW.expense_id, v_status
            USING ERRCODE = '23514';
    END IF;
    IF v_submitter = NEW.actor_id THEN
        RAISE EXCEPTION 'submitter cannot approve own expense %', NEW.expense_id
            USING ERRCODE = '23514';
    END IF;

    SELECT count(*) INTO v_done FROM app.expense_approvals
     WHERE tenant_id = NEW.tenant_id AND expense_id = NEW.expense_id;

    IF NEW.level <> v_done + 1 THEN
        RAISE EXCEPTION 'expense %: approval level must be %, got %',
            NEW.expense_id, v_done + 1, NEW.level
            USING ERRCODE = '23514';
    END IF;
    IF NEW.level > v_required THEN
        RAISE EXCEPTION 'expense %: level % exceeds required %',
            NEW.expense_id, NEW.level, v_required
            USING ERRCODE = '23514';
    END IF;

    RETURN NEW;
END
$$;

CREATE TRIGGER expense_approvals_guard BEFORE INSERT ON app.expense_approvals
    FOR EACH ROW EXECUTE FUNCTION app.expense_approvals_guard();

-- 审批沟通记录。软删，保留审计价值。
CREATE TABLE app.expense_comments (
    id         uuid PRIMARY KEY,
    tenant_id  uuid NOT NULL,
    expense_id uuid NOT NULL,
    author_id  uuid NOT NULL,
    body       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    CONSTRAINT expense_comments_body_check CHECK (char_length(body) BETWEEN 1 AND 2000),
    CONSTRAINT expense_comments_expense_fk FOREIGN KEY (tenant_id, expense_id)
        REFERENCES app.expenses (tenant_id, id)
);

CREATE INDEX expense_comments_expense_idx
    ON app.expense_comments (tenant_id, expense_id, created_at)
    WHERE deleted_at IS NULL;

-- 附件只存对象键与元数据，文件本体在对象存储里。
CREATE TABLE app.expense_attachments (
    id           uuid PRIMARY KEY,
    tenant_id    uuid NOT NULL,
    expense_id   uuid NOT NULL,
    object_key   text NOT NULL,
    file_name    text NOT NULL,
    content_type text NOT NULL,
    size_bytes   bigint NOT NULL,
    uploaded_by  uuid NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz,
    CONSTRAINT expense_attachments_expense_fk FOREIGN KEY (tenant_id, expense_id)
        REFERENCES app.expenses (tenant_id, id),
    CONSTRAINT expense_attachments_key_key UNIQUE (tenant_id, object_key),
    CONSTRAINT expense_attachments_object_key_check CHECK (
        object_key ~ '^tenants/[0-9a-f-]{36}/expenses/[0-9a-f-]{36}/[0-9a-f-]{36}\.(jpg|png|webp|pdf)$'
    ),
    CONSTRAINT expense_attachments_content_type_check CHECK (
        content_type IN ('image/jpeg', 'image/png', 'image/webp', 'application/pdf')
    ),
    CONSTRAINT expense_attachments_file_name_check CHECK (
        char_length(btrim(file_name)) BETWEEN 1 AND 200
    ),
    CONSTRAINT expense_attachments_size_check CHECK (
        size_bytes > 0 AND size_bytes <= 10485760
    )
);

CREATE INDEX expense_attachments_expense_idx
    ON app.expense_attachments (tenant_id, expense_id, created_at)
    WHERE deleted_at IS NULL;

-- 只追加 / 只软删：运行时角色不能硬删审计记录，也不能改各级批准。
REVOKE UPDATE, DELETE ON app.expense_approvals FROM expense_app;
REVOKE DELETE ON app.expense_comments FROM expense_app;
REVOKE DELETE ON app.expense_attachments FROM expense_app;

ALTER TABLE app.approval_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.approval_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.approval_policies
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE app.expense_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.expense_approvals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.expense_approvals
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE app.expense_comments ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.expense_comments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.expense_comments
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE app.expense_attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.expense_attachments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.expense_attachments
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

COMMIT;
