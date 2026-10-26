-- identity 核心表。
--
-- RLS 边界（有意为之，不是遗漏）：
--   有 RLS：memberships、audit_logs   —— 纯租户数据
--   无 RLS：tenants、users、refresh_tokens
-- 理由：登录与刷新发生在"租户上下文建立之前"——系统必须先按 slug 找到租户、
-- 按邮箱找到用户、按摘要找到刷新令牌，才能知道租户是谁。对这些表盲目套
-- tenant_id 策略只会让登录永远查不到数据。它们的保护来自另外两层：
-- 只有 identity 的 app 角色能访问本库，且刷新令牌只存 sha256 摘要。
BEGIN;

CREATE TABLE app.tenants (
    id         uuid PRIMARY KEY,
    slug       text NOT NULL,
    name       text NOT NULL,
    plan       text NOT NULL DEFAULT 'free',
    status     text NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid,
    updated_by uuid,
    deleted_at timestamptz,
    CONSTRAINT tenants_slug_key UNIQUE (slug),
    CONSTRAINT tenants_slug_format CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$'),
    CONSTRAINT tenants_plan_check CHECK (plan IN ('free', 'pro', 'enterprise')),
    CONSTRAINT tenants_status_check CHECK (status IN ('active', 'suspended'))
);

CREATE TABLE app.users (
    id            uuid PRIMARY KEY,
    email         text NOT NULL,
    password_hash text NOT NULL,
    display_name  text NOT NULL DEFAULT '',
    status        text NOT NULL DEFAULT 'active',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_by    uuid,
    deleted_at    timestamptz,
    CONSTRAINT users_status_check CHECK (status IN ('active', 'disabled'))
);

-- 邮箱唯一性按小写判定：统一在应用层归一化，索引兜底，避免 Alice@ 与 alice@ 并存。
CREATE UNIQUE INDEX users_email_lower_key ON app.users (lower(email));

CREATE TABLE app.memberships (
    tenant_id  uuid NOT NULL REFERENCES app.tenants (id),
    user_id    uuid NOT NULL REFERENCES app.users (id),
    role       text NOT NULL,
    status     text NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid,
    updated_by uuid,
    deleted_at timestamptz,
    PRIMARY KEY (tenant_id, user_id),
    CONSTRAINT memberships_role_check CHECK (role IN ('owner', 'admin', 'member', 'viewer')),
    CONSTRAINT memberships_status_check CHECK (status IN ('active', 'invited', 'disabled'))
);

CREATE INDEX memberships_user_idx ON app.memberships (user_id) WHERE deleted_at IS NULL;

-- 刷新令牌：只存 sha256 摘要。库被读走也拿不到可用令牌。
-- family_id 把一次登录派生出的令牌串成一条链，用于重放检测：链上任何旧令牌
-- 再次出现，说明它已被窃取，整族立即作废。
CREATE TABLE app.refresh_tokens (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES app.tenants (id),
    user_id     uuid NOT NULL REFERENCES app.users (id),
    token_hash  bytea NOT NULL,
    family_id   uuid NOT NULL,
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    replaced_by uuid,
    user_agent  text NOT NULL DEFAULT '',
    ip          inet,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT refresh_tokens_hash_key UNIQUE (token_hash)
);

CREATE INDEX refresh_tokens_family_idx ON app.refresh_tokens (family_id);
CREATE INDEX refresh_tokens_user_idx ON app.refresh_tokens (tenant_id, user_id);

CREATE TABLE app.audit_logs (
    id            uuid PRIMARY KEY,
    tenant_id     uuid NOT NULL,
    actor_user_id uuid,
    action        text NOT NULL,
    target_type   text NOT NULL DEFAULT '',
    target_id     text NOT NULL DEFAULT '',
    ip            inet,
    user_agent    text NOT NULL DEFAULT '',
    metadata      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_tenant_time_idx ON app.audit_logs (tenant_id, created_at DESC);

-- updated_at 由触发器维护：靠应用层"记得写"迟早会漏，审计字段不能依赖自觉。
CREATE FUNCTION app.touch_updated_at() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END
$$;

CREATE TRIGGER tenants_touch BEFORE UPDATE ON app.tenants
    FOR EACH ROW EXECUTE FUNCTION app.touch_updated_at();
CREATE TRIGGER users_touch BEFORE UPDATE ON app.users
    FOR EACH ROW EXECUTE FUNCTION app.touch_updated_at();
CREATE TRIGGER memberships_touch BEFORE UPDATE ON app.memberships
    FOR EACH ROW EXECUTE FUNCTION app.touch_updated_at();

-- RLS：未设置 app.tenant_id 时 nullif(...) 为 NULL，比较结果恒为 NULL，
-- 策略判定为不通过——即"忘记设上下文"的默认行为是查不到任何数据，而不是全量可见。
ALTER TABLE app.memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.memberships
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE app.audit_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.audit_logs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.audit_logs
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

COMMIT;
