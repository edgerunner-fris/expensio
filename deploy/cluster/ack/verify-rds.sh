#!/usr/bin/env bash
# 从集群内部验证 RDS PostgreSQL：网络、TLS（verify-full + 阿里云 CA）、
# 五个库、两类账号的权限边界。
#
# 为什么在集群里跑：白名单是按 vSwitch 网段放的，从跳板机验证通不代表
# Pod 能连；而 Pod 里没有 psql，也不该为了这条检查给业务镜像装客户端。
# 这里起一个临时 Pod，口令只以文件形式挂在 /secrets，不出现在命令行参数里。
#
# 用法：
#   HOST=pgm-xxxx.pg.rds.aliyuncs.com bash deploy/cluster/ack/verify-rds.sh
#   HOST=... PORT=5432 NAMESPACE=expensio bash deploy/cluster/ack/verify-rds.sh
#
# 前置：expensio-secrets 里已有 EXPENSIO_DB_APP_PASSWORD、
#       EXPENSIO_DB_MIGRATOR_PASSWORD、EXPENSIO_DB_TLS_CA。
set -uo pipefail

NS=${NAMESPACE:-expensio}
SECRET=${SECRET:-expensio-secrets}
HOST=${HOST:?请设置 HOST=<RDS 内网地址>}
PORT=${PORT:-5432}
IMAGE=${IMAGE:-postgres:16.15-alpine}
POD=${POD:-rds-verify}
KUBECTL=${KUBECTL:-kubectl}
DBS=${DBS:-"identity_db expense_db billing_db report_db notification_db"}

fail=0
say() { printf '%s\n' "$*"; }
bad() { printf 'FAIL: %s\n' "$*" >&2; fail=1; }

cleanup() {
  $KUBECTL -n "$NS" delete pod "$POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

$KUBECTL -n "$NS" delete pod "$POD" --ignore-not-found >/dev/null 2>&1 || true

say "== 起临时 Pod $POD（镜像 $IMAGE）=="
cat <<EOF | $KUBECTL -n "$NS" apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $POD
  labels:
    app.kubernetes.io/name: rds-verify
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
    # 只挂两个口令文件与 CA，不把整个 Secret（含 JWT 私钥）铺进临时 Pod。
    - name: creds
      secret:
        secretName: $SECRET
        items:
          - key: EXPENSIO_DB_APP_PASSWORD
            path: app
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
  $KUBECTL -n "$NS" describe pod "$POD" | tail -20
  bad "临时 Pod 未就绪（镜像拉取失败？换 IMAGE= 指向 ACR 的同版本镜像）"
  exit 1
fi
say "Pod 就绪"

say
say "== 1. DNS 与 TCP 连通（白名单是否放行这个网段）=="
if $KUBECTL -n "$NS" exec -i "$POD" -- sh -s <<<"getent hosts '$HOST'"; then
  say "DNS 解析：通过"
else
  bad "Pod 内无法解析 $HOST"
fi

say
say "== 2. 应用账号：五个库，sslmode=verify-full =="
for db in $DBS; do
  role=${db%_db}
  out=$($KUBECTL -n "$NS" exec -i "$POD" -- sh -s 2>&1 <<EOF
PGPASSWORD=\$(cat /secrets/app) psql "host=$HOST port=$PORT dbname=$db user=${role}_app sslmode=verify-full sslrootcert=/certs/ca.pem connect_timeout=10" -tAc "select current_user || '|' || current_database() || '|' || current_setting('ssl')"
EOF
)
  want="${role}_app|$db|on"
  if [ "$(printf '%s' "$out" | tr -d '[:space:]')" = "$want" ]; then
    say "  $db: $out"
  else
    bad "$db 应用账号连接失败：$out"
  fi
done

say
say "== 3. 迁移账号：五个库，sslmode=verify-full =="
for db in $DBS; do
  role=${db%_db}
  out=$($KUBECTL -n "$NS" exec -i "$POD" -- sh -s 2>&1 <<EOF
PGPASSWORD=\$(cat /secrets/migrator) psql "host=$HOST port=$PORT dbname=$db user=${role}_migrator sslmode=verify-full sslrootcert=/certs/ca.pem connect_timeout=10" -tAc "select current_user || '|' || pg_get_userbyid(datdba) from pg_database where datname = current_database()"
EOF
)
  want="${role}_migrator|${role}_migrator"
  if [ "$(printf '%s' "$out" | tr -d '[:space:]')" = "$want" ]; then
    say "  $db: $out"
  else
    bad "$db 迁移账号连接或库属主不符：$out"
  fi
done

say
say "== 4. 权限边界：app 不能建表、不能绕过 RLS =="
out=$($KUBECTL -n "$NS" exec -i "$POD" -- sh -s 2>&1 <<EOF
PGPASSWORD=\$(cat /secrets/app) psql "host=$HOST port=$PORT dbname=identity_db user=identity_app sslmode=verify-full sslrootcert=/certs/ca.pem connect_timeout=10" -c "create table app.verify_should_fail(id int)" 2>&1 | head -1
EOF
)
if printf '%s' "$out" | grep -q 'permission denied'; then
  say "  建表被拒：$out"
elif printf '%s' "$out" | grep -q 'does not exist'; then
  # 迁移前 app schema 还不存在，这不构成失败；部署后再跑一次这条会变成
  # "permission denied for schema app"。
  say "  app schema 尚未创建（迁移前正常）：$out"
else
  bad "identity_app 竟然能建表（或报错不是权限问题）：$out"
fi

out=$($KUBECTL -n "$NS" exec -i "$POD" -- sh -s 2>&1 <<EOF
PGPASSWORD=\$(cat /secrets/app) psql "host=$HOST port=$PORT dbname=identity_db user=identity_app sslmode=verify-full sslrootcert=/certs/ca.pem connect_timeout=10" -tAc "select rolbypassrls from pg_roles where rolname = current_user"
EOF
)
if [ "$(printf '%s' "$out" | tr -d '[:space:]')" = "f" ]; then
  say "  rolbypassrls：f"
else
  bad "identity_app 的 rolbypassrls 不是 f：$out"
fi

say
say "== 5. 迁移记录表的位置（必须在 app schema）=="
# golang-migrate 在建版本表时用连接的 search_path。若库级 search_path 是
# 迁移过程中才设置的，表会落在 public；下一次 migrate 到 app 里找不到它，
# 会把所有迁移重跑一遍。位置不对就是失败，不是提示。
for db in $DBS; do
  role=${db%_db}
  out=$($KUBECTL -n "$NS" exec -i "$POD" -- sh -s 2>&1 <<EOF
PGPASSWORD=\$(cat /secrets/migrator) psql "host=$HOST port=$PORT dbname=$db user=${role}_migrator sslmode=verify-full sslrootcert=/certs/ca.pem connect_timeout=10" -tAc "select n.nspname from pg_class c join pg_namespace n on n.oid = c.relnamespace where c.relname = 'schema_migrations'"
EOF
)
  loc=$(printf '%s' "$out" | tr -d '[:space:]')
  case "$loc" in
    "")
      say "  $db: 版本表不存在（尚未迁移）"
      ;;
    app)
      ver=$($KUBECTL -n "$NS" exec -i "$POD" -- sh -s 2>&1 <<EOF
PGPASSWORD=\$(cat /secrets/migrator) psql "host=$HOST port=$PORT dbname=$db user=${role}_migrator sslmode=verify-full sslrootcert=/certs/ca.pem connect_timeout=10" -tAc "select version || '|dirty=' || dirty from app.schema_migrations"
EOF
)
      say "  $db: app（version=$(printf '%s' "$ver" | tr -d '[:space:]')）"
      ;;
    *)
      bad "$db: schema_migrations 在 $loc，应在 app——下一次 migrate 会把所有迁移重跑"
      ;;
  esac
done

say
if [ "$fail" -eq 0 ]; then
  say "RDS_VERIFY: PASS"
else
  say "RDS_VERIFY: FAIL"
fi
exit "$fail"
