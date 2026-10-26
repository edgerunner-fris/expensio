-- identity 基线：建 app schema、给运行时账号授权、固定 search_path。
--
-- 约定（五个库一致）：
--   属主   identity_migrator —— 只有它能执行 DDL
--   运行时 identity_app      —— NOBYPASSRLS，只有 DML 权限
-- 业务表都建在 app schema 下，public 留给扩展。
BEGIN;

CREATE SCHEMA IF NOT EXISTS app;

GRANT USAGE ON SCHEMA app TO identity_app;

-- 之后新建的表/序列自动授权。少了这一条，每加一张表都要记得手写 GRANT，
-- 漏一次就是线上 500，而且只有跑到的接口才会暴露。
ALTER DEFAULT PRIVILEGES FOR ROLE identity_migrator IN SCHEMA app
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO identity_app;
ALTER DEFAULT PRIVILEGES FOR ROLE identity_migrator IN SCHEMA app
  GRANT USAGE, SELECT ON SEQUENCES TO identity_app;

-- search_path 设在库级别：migrator 是库属主，有权 ALTER DATABASE；
-- 但它没有 CREATEROLE，改不了 identity_app 的角色属性——也不该有。
-- 新会话生效，所以本文件里仍需显式限定 schema。
DO $do$
BEGIN
  EXECUTE format('ALTER DATABASE %I SET search_path = app, public', current_database());
END
$do$;

COMMIT;
