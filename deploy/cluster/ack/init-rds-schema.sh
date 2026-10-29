#!/usr/bin/env bash
# 在迁移之前把五个库的 app schema 与库级 search_path 准备好。
#
# 为什么不能交给迁移本身做：
#   golang-migrate 在应用第一个迁移**之前**就建版本表 schema_migrations，
#   用的是连接当时的 search_path。如果库级 search_path 是 000001 迁移里才
#   设置的，版本表会落在 public，业务表落在 app；下一次 migrate 在 app 里
#   找不到版本表，会把所有迁移重跑一遍（多数会在 CREATE TABLE 处失败并被
#   标记 dirty）。本地 compose 没有这个问题，因为初始化脚本先设了
#   search_path，迁移才以 app 为默认 schema 建表。
#
# 本脚本以 *migrator 身份运行（库属主，有权建 schema 与 ALTER DATABASE），
# 幂等；如果发现版本表已经在 public，会把它移到 app，修复已经跑歪的库。
#
# 用法：
#   HOST=pgm-xxxx.pg.rds.aliyuncs.com bash deploy/cluster/ack/init-rds-schema.sh
set -uo pipefail

NS=${NAMESPACE:-expensio}
SECRET=${SECRET:-expensio-secrets}
HOST=${HOST:?请设置 HOST=<RDS 内网地址>}
PORT=${PORT:-5432}
IMAGE=${IMAGE:-postgres:16.15-alpine}
POD=${POD:-rds-schema-init}
KUBECTL=${KUBECTL:-kubectl}
DBS=${DBS:-"identity_db expense_db billing_db report_db notification_db"}

fail=0
cleanup() {
  $KUBECTL -n "$NS" delete pod "$POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

$KUBECTL -n "$NS" delete pod "$POD" --ignore-not-found >/dev/null 2>&1 || true

cat <<EOF | $KUBECTL -n "$NS" apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $POD
  labels:
    app.kubernetes.io/name: rds-schema-init
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 70
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: psql
      image: $IMAGE
      command: ["sleep", "1800"]
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: creds
          mountPath: /secrets
          readOnly: true
        - name: ca
          mountPath: /certs
          readOnly: true
        - name: tmp
          mountPath: /tmp
  volumes:
    - name: creds
      secret:
        secretName: $SECRET
        items:
          - key: EXPENSIO_DB_MIGRATOR_PASSWORD
            path: migrator
    - name: ca
      secret:
        secretName: $SECRET
        items:
          - key: EXPENSIO_DB_TLS_CA
            path: ca.pem
    - name: tmp
      emptyDir: {}
EOF

if ! $KUBECTL -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=180s >/dev/null; then
  $KUBECTL -n "$NS" describe pod "$POD" | tail -15
  echo "FAIL: 临时 Pod 未就绪" >&2
  exit 1
fi

for db in $DBS; do
  role=${db%_db}
  out=$($KUBECTL -n "$NS" exec -i "$POD" -- sh -s 2>&1 <<EOF
set -eu
export PGPASSWORD="\$(cat /secrets/migrator)"
psql "host=$HOST port=$PORT dbname=$db user=${role}_migrator sslmode=verify-full sslrootcert=/certs/ca.pem connect_timeout=10" \
  -v ON_ERROR_STOP=1 -q <<'SQL'
CREATE SCHEMA IF NOT EXISTS app AUTHORIZATION CURRENT_USER;
DO \$\$
BEGIN
  EXECUTE format('ALTER DATABASE %I SET search_path = app, public', current_database());
  IF EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
             WHERE c.relname = 'schema_migrations' AND n.nspname = 'public')
     AND NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
             WHERE c.relname = 'schema_migrations' AND n.nspname = 'app') THEN
    ALTER TABLE public.schema_migrations SET SCHEMA app;
    RAISE NOTICE 'schema_migrations: public -> app';
  END IF;
END \$\$;
SQL
psql "host=$HOST port=$PORT dbname=$db user=${role}_migrator sslmode=verify-full sslrootcert=/certs/ca.pem connect_timeout=10" \
  -tAc "select 'schema=' || (select nspname from pg_namespace where nspname='app') || ' search_path=' || current_setting('search_path') || ' version_table=' || coalesce((select n.nspname from pg_class c join pg_namespace n on n.oid=c.relnamespace where c.relname='schema_migrations'), 'none')"
EOF
)
  if [ $? -eq 0 ]; then
    printf '  %-18s %s\n' "$db" "$(printf '%s' "$out" | tail -1)"
  else
    printf 'FAIL: %s\n%s\n' "$db" "$out" >&2
    fail=1
  fi
done

if [ "$fail" -eq 0 ]; then
  echo "RDS_SCHEMA_INIT: PASS"
else
  echo "RDS_SCHEMA_INIT: FAIL" >&2
fi
exit "$fail"
