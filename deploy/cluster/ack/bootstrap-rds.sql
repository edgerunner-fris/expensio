-- 在阿里云 RDS PostgreSQL 上用**高权限账号**执行一次。
--
--   PGPASSWORD 不落 shell history 的写法见 docs/ACK_MANAGED_RESOURCES.md：
--   psql "host=pgm-xxxx.pg.rds.aliyuncs.com port=5432 dbname=postgres \
--         user=expensio_admin sslmode=verify-full \
--         sslrootcert=$HOME/expensio-ack/creds/EXPENSIO_DB_TLS_CA" \
--        -v app_pw="$APP_PW" -v migrator_pw="$MIGRATOR_PW" \
--        -f deploy/cluster/ack/bootstrap-rds.sql
--
-- 与 deploy/local/postgres/init/01-bootstrap.sh 的差别只有两处，原因都写在这里：
--
--   1. RDS 没有真正的超级用户。高权限账号（pg_rds_superuser）可以建角色，
--      但 PostgreSQL 16 起 CREATE DATABASE ... OWNER 还要求执行者能
--      SET ROLE 到目标属主，所以先显式 GRANT 成员关系。
--   2. schema、默认权限、库级 search_path 不在这里做，但**也不能留给迁移
--      自己做**：golang-migrate 在应用第一个迁移之前就建版本表
--      schema_migrations，用的是连接当时的 search_path。若 search_path 到
--      000001 才设置，版本表会落在 public 而业务表落在 app，下一次 migrate
--      在 app 里找不到版本表，会把所有迁移重跑一遍。
--      所以顺序是：先跑完本脚本，再跑 init-rds-schema.sh 建 app schema 与
--      库级 search_path，最后才执行迁移。init-rds-schema.sh 也负责把已经
--      落在 public 的版本表移回 app。
--
-- 幂等：角色或库已存在时跳过；口令不会被回显，但也不会被修改。

\set ON_ERROR_STOP on

-- 五个业务库。与 internal/platform/config 的 serviceName 一一对应。
\set svcs identity,expense,billing,report,notification

SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', s || '_migrator', :'migrator_pw')
FROM unnest(string_to_array(:'svcs', ',')) AS s
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = s || '_migrator')
\gexec

-- NOBYPASSRLS 是默认值，这里显式写出来：运行时角色一旦能绕过 RLS，
-- 多租户隔离就形同虚设，这个属性必须是有人刻意改才会失去。
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L NOBYPASSRLS', s || '_app', :'app_pw')
FROM unnest(string_to_array(:'svcs', ',')) AS s
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = s || '_app')
\gexec

-- PG16+：CREATE DATABASE ... OWNER 要求执行者是目标属主的成员。
-- 角色由本账号创建时自动带 ADMIN OPTION；若报权限错误，说明这些角色
-- 不是本账号建的，改用控制台把库授权给对应 migrator 账号。
SELECT format('GRANT %I TO %I', s || '_migrator', current_user)
FROM unnest(string_to_array(:'svcs', ',')) AS s
WHERE NOT pg_has_role(current_user, s || '_migrator', 'MEMBER')
\gexec

SELECT format('CREATE DATABASE %I OWNER %I', s || '_db', s || '_migrator')
FROM unnest(string_to_array(:'svcs', ',')) AS s
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = s || '_db')
\gexec

-- 回执：只打印名字与属主，不打印任何口令。
SELECT rolname FROM pg_roles
WHERE rolname LIKE '%\_migrator' OR rolname LIKE '%\_app'
ORDER BY 1;

SELECT datname, pg_get_userbyid(datdba) AS owner FROM pg_database
WHERE datname LIKE '%\_db'
ORDER BY 1;
