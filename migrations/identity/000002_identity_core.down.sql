BEGIN;

DROP POLICY IF EXISTS tenant_isolation ON app.audit_logs;
ALTER TABLE app.audit_logs DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON app.memberships;
ALTER TABLE app.memberships DISABLE ROW LEVEL SECURITY;

DROP TRIGGER IF EXISTS memberships_touch ON app.memberships;
DROP TRIGGER IF EXISTS users_touch ON app.users;
DROP TRIGGER IF EXISTS tenants_touch ON app.tenants;
DROP FUNCTION IF EXISTS app.touch_updated_at();

DROP TABLE IF EXISTS app.audit_logs;
DROP TABLE IF EXISTS app.refresh_tokens;
DROP TABLE IF EXISTS app.memberships;
DROP TABLE IF EXISTS app.users;
DROP TABLE IF EXISTS app.tenants;

COMMIT;
