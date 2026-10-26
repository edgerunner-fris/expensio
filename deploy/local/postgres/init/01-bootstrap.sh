#!/usr/bin/env bash
# 首次创建数据卷时执行一次：建五个库，每库一对角色。
#
# 为什么要成对：
#   {service}_migrator —— 库与表的属主，golang-migrate 用它执行 DDL；
#   {service}_app      —— 运行时账号，NOBYPASSRLS，只能按策略访问本租户数据。
# 应用绝不能用属主账号连库：属主默认绕过 RLS，多租户隔离会直接失效。
#
# 整段放在子 shell 里：docker entrypoint 在没有执行位时会 source 本文件，
# 若直接 set -euo pipefail 会污染 entrypoint 自身的 shell 选项。
(
  set -euo pipefail

  : "${EXPENSIO_DB_APP_PASSWORD:?缺少 EXPENSIO_DB_APP_PASSWORD}"
  : "${EXPENSIO_DB_MIGRATOR_PASSWORD:?缺少 EXPENSIO_DB_MIGRATOR_PASSWORD}"

  for svc in identity expense billing report notification; do
    psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
      -v svc="$svc" \
      -v app_pw="$EXPENSIO_DB_APP_PASSWORD" \
      -v migrator_pw="$EXPENSIO_DB_MIGRATOR_PASSWORD" <<'SQL'
-- 用 \gexec 而不是 DO 块：psql 的 :'var' 只在本层 SQL 里替换，
-- 放进 $$ ... $$ 就不再插值（dollar-quoted 字符串按字面处理）。
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L',
              :'svc' || '_migrator', :'migrator_pw')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'svc' || '_migrator')
\gexec

-- NOBYPASSRLS 是默认值，这里显式写出来：运行时角色一旦能绕过 RLS，
-- 多租户隔离就形同虚设，这个属性必须是有人刻意改才会失去。
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L NOBYPASSRLS',
              :'svc' || '_app', :'app_pw')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'svc' || '_app')
\gexec

-- CREATE DATABASE 不能进事务，所以同样用 \gexec 逐条执行。
SELECT format('CREATE DATABASE %I OWNER %I', :'svc' || '_db', :'svc' || '_migrator')
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = :'svc' || '_db')
\gexec
SQL
  done

  # schema 与 search_path 必须在 golang-migrate 第一次连库之前就定好。
  # 原因：schema_migrations 是"未限定 schema 名的建表语句"，落在 search_path 的
  # 第一个 schema 里。若不在这里固定，基线迁移改了库级 search_path 之后，
  # 下一次连接会把表建到另一个 schema，出现两份迁移记录、版本号对不上。
  for svc in identity expense billing report notification; do
    psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "${svc}_db" \
      -c "CREATE SCHEMA IF NOT EXISTS app AUTHORIZATION ${svc}_migrator" \
      -c "ALTER DATABASE ${svc}_db SET search_path = app, public"
  done

  echo "expensio: 五个数据库与角色已就绪"
)
