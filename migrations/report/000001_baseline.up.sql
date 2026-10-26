-- report 基线：建 app schema、给运行时账号授权、固定 search_path。
-- 这里只放读模型；读模型可随时重建，不作为业务事实来源。
BEGIN;

CREATE SCHEMA IF NOT EXISTS app;

GRANT USAGE ON SCHEMA app TO report_app;

ALTER DEFAULT PRIVILEGES FOR ROLE report_migrator IN SCHEMA app
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO report_app;
ALTER DEFAULT PRIVILEGES FOR ROLE report_migrator IN SCHEMA app
  GRANT USAGE, SELECT ON SEQUENCES TO report_app;

DO $do$
BEGIN
  EXECUTE format('ALTER DATABASE %I SET search_path = app, public', current_database());
END
$do$;

COMMIT;
