#!/usr/bin/env bash
# 备份恢复演练：对本地 compose 的 PostgreSQL 逐库 pg_dump，恢复到临时库，
# 比对表集合、迁移版本与总行数，然后删除临时库。
#
# 这是一个"能证明恢复可用"的演练，不是备份方案本身：生产备份由平台的
# 对象存储 + PITR 负责，本脚本只验证逻辑备份能被完整恢复。
#
# 用法：
#   ./scripts/backup-drill.sh                 # 默认容器 expensio-postgres-1
#   POSTGRES_CONTAINER=my-pg ./scripts/backup-drill.sh
set -euo pipefail

CONTAINER="${POSTGRES_CONTAINER:-expensio-postgres-1}"
# compose 的初始化脚本把超级用户名设为 expensio（镜像默认的 postgres 不存在）。
SUPERUSER="${POSTGRES_SUPERUSER:-expensio}"
DATABASES="${DATABASES:-identity_db expense_db billing_db report_db notification_db}"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

psql_db() { # psql_db <database> <sql>
  # NOTICE（如 "database does not exist, skipping"）只是噪音，会打乱逐库的
  # 结果行；真正的错误仍由 ON_ERROR_STOP 的退出码暴露。
  docker exec "$CONTAINER" psql -U "$SUPERUSER" -At -v ON_ERROR_STOP=1 -d "$1" -c "$2" 2>/dev/null
}

table_list() { # table_list <database>
  psql_db "$1" "select quote_ident(schemaname)||'.'||quote_ident(tablename)
                 from pg_tables
                 where schemaname not in ('pg_catalog','information_schema')
                 order by 1"
}

row_total() { # row_total <database>
  local total=0 table count
  while IFS= read -r table; do
    [ -n "$table" ] || continue
    count=$(psql_db "$1" "select count(*) from $table")
    total=$((total + count))
  done < <(table_list "$1")
  echo "$total"
}

migration_state() { # migration_state <database>
  # golang-migrate 的表在 app schema 里（search_path=app,public），必须显式限定。
  psql_db "$1" "select coalesce(string_agg(version::text||':'||dirty::text, ',' order by version), 'none')
                from app.schema_migrations" 2>/dev/null || echo "none"
}

fail=0
for db in $DATABASES; do
  scratch="${db}_drill"
  dump="$WORK_DIR/$db.sql"

  echo "==> $db"
  docker exec "$CONTAINER" pg_dump -U "$SUPERUSER" --no-owner --no-privileges "$db" > "$dump"

  src_tables=$(table_list "$db" | wc -l)
  src_rows=$(row_total "$db")
  src_mig=$(migration_state "$db")

  docker exec "$CONTAINER" psql -U "$SUPERUSER" -d postgres -v ON_ERROR_STOP=1 -q -c "drop database if exists $scratch" >/dev/null 2>&1
  docker exec "$CONTAINER" createdb -U "$SUPERUSER" --maintenance-db=postgres "$scratch"
  docker exec -i "$CONTAINER" psql -U "$SUPERUSER" -v ON_ERROR_STOP=1 -q -d "$scratch" < "$dump" >/dev/null 2>&1

  dst_tables=$(table_list "$scratch" | wc -l)
  dst_rows=$(row_total "$scratch")
  dst_mig=$(migration_state "$scratch")

  docker exec "$CONTAINER" psql -U "$SUPERUSER" -d postgres -v ON_ERROR_STOP=1 -q -c "drop database $scratch" >/dev/null 2>&1

  size=$(wc -c < "$dump")
  if [ "$src_tables" = "$dst_tables" ] && [ "$src_rows" = "$dst_rows" ] && [ "$src_mig" = "$dst_mig" ]; then
    echo "    OK dump=${size}B tables=${src_tables} rows=${src_rows} schema_version=${src_mig}"
  else
    echo "    FAIL dump=${size}B src(tables=${src_tables},rows=${src_rows},ver=${src_mig}) dst(tables=${dst_tables},rows=${dst_rows},ver=${dst_mig})"
    fail=1
  fi
done

if [ "$fail" -ne 0 ]; then
  echo "BACKUP_DRILL: FAIL"
  exit 1
fi
echo "BACKUP_DRILL: PASS"
