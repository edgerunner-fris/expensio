-- 成员邀请：管理员创建，被邀请人凭链接接受后成为租户成员。
--
-- 令牌只存 sha256 摘要：数据库被读走也拿不到可用链接。
-- RLS 用 ENABLE 而不是 FORCE，原因与 notification.email_outbox 相同：
-- 接受邀请发生在"租户上下文建立之前"——调用方只有链接里的令牌，
-- 必须先凭摘要找到邀请，才知道 tenant_id 是谁。find_invitation 是唯一的
-- 跨租户读路径，以迁移账号身份执行；运行时账号 identity_app 仍然受 RLS 约束。
BEGIN;

CREATE TABLE app.invitations (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES app.tenants (id),
    email       text NOT NULL,
    role        text NOT NULL,
    token_hash  bytea NOT NULL,
    invited_by  uuid NOT NULL REFERENCES app.users (id),
    expires_at  timestamptz NOT NULL,
    accepted_at timestamptz,
    revoked_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT invitations_token_key UNIQUE (token_hash),
    CONSTRAINT invitations_role_check CHECK (role IN ('owner', 'admin', 'member', 'viewer')),
    -- 邮箱统一在应用层归一化成小写，约束负责挡住旁路写入。
    CONSTRAINT invitations_email_check CHECK (
        char_length(email) BETWEEN 3 AND 254 AND email = lower(email)
    )
);

-- 同一租户同一邮箱最多一条待接受邀请。重复邀请必须先撤销旧的，
-- 这样"最新一封邮件里的链接"是唯一有效的，旧链接自动失效。
CREATE UNIQUE INDEX invitations_pending_email_key
    ON app.invitations (tenant_id, lower(email))
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE INDEX invitations_tenant_created_idx
    ON app.invitations (tenant_id, created_at DESC);

CREATE TRIGGER invitations_touch BEFORE UPDATE ON app.invitations
    FOR EACH ROW EXECUTE FUNCTION app.touch_updated_at();

ALTER TABLE app.invitations ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app.invitations
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- 按令牌摘要查找邀请。返回的 tenant_id 来自数据库记录，而不是请求参数，
-- 因此调用方可以安全地用它建立后续的租户上下文（见 db.WithinTenantID）。
CREATE FUNCTION app.find_invitation(p_hash bytea)
RETURNS TABLE (
    id          uuid,
    tenant_id   uuid,
    tenant_slug text,
    tenant_name text,
    email       text,
    role        text,
    expires_at  timestamptz,
    accepted_at timestamptz,
    revoked_at  timestamptz,
    created_at  timestamptz
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = app, pg_temp
AS $$
    SELECT i.id, i.tenant_id, t.slug, t.name, i.email, i.role,
           i.expires_at, i.accepted_at, i.revoked_at, i.created_at
    FROM app.invitations AS i
    JOIN app.tenants AS t ON t.id = i.tenant_id
    WHERE i.token_hash = p_hash
      AND t.deleted_at IS NULL;
$$;

REVOKE ALL ON FUNCTION app.find_invitation(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.find_invitation(bytea) TO identity_app;

COMMIT;
