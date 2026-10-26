-- expense 基线：建 app schema、给运行时账号授权、固定 search_path。
-- 预算表也在这个库里：预算占用必须与费用写入同事务完成。
BEGIN;

CREATE SCHEMA IF NOT EXISTS app;

GRANT USAGE ON SCHEMA app TO expense_app;

ALTER DEFAULT PRIVILEGES FOR ROLE expense_migrator IN SCHEMA app
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO expense_app;
ALTER DEFAULT PRIVILEGES FOR ROLE expense_migrator IN SCHEMA app
  GRANT USAGE, SELECT ON SEQUENCES TO expense_app;

DO $do$
BEGIN
  EXECUTE format('ALTER DATABASE %I SET search_path = app, public', current_database());
END
$do$;

COMMIT;
