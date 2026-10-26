-- notification 基线：建 app schema、给运行时账号授权、固定 search_path。
BEGIN;

CREATE SCHEMA IF NOT EXISTS app;

GRANT USAGE ON SCHEMA app TO notification_app;

ALTER DEFAULT PRIVILEGES FOR ROLE notification_migrator IN SCHEMA app
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO notification_app;
ALTER DEFAULT PRIVILEGES FOR ROLE notification_migrator IN SCHEMA app
  GRANT USAGE, SELECT ON SEQUENCES TO notification_app;

DO $do$
BEGIN
  EXECUTE format('ALTER DATABASE %I SET search_path = app, public', current_database());
END
$do$;

COMMIT;
