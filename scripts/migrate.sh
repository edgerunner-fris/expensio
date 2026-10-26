#!/usr/bin/env bash
# 对五个库执行 golang-migrate。
#
# 用法：
#   ./scripts/migrate.sh up         # 升到最新
#   ./scripts/migrate.sh down 1     # 回退 1 个版本
#   ./scripts/migrate.sh version    # 查看当前版本
#
# 为什么不用本机安装的 migrate 二进制：
#   版本随仓库固定（MIGRATE_IMAGE），换机器、换 CI 都不会因为工具版本差异出现
#   "本地能过、CI 报 migration dirty"。迁移账号是 {service}_migrator，
#   不是应用账号——应用账号没有 DDL 权限，也就没有绕过 RLS 的机会。
set -euo pipefail

cd "$(dirname "$0")/.."

if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

MIGRATE_IMAGE="${MIGRATE_IMAGE:-migrate/migrate:v4.18.3}"
COMPOSE_NETWORK="${COMPOSE_NETWORK:-expensio_default}"
PG_HOST="${PG_HOST:-postgres}"
PG_PORT="${PG_PORT:-5432}"
MIGRATOR_PW="${EXPENSIO_DB_MIGRATOR_PASSWORD:-expensio_local_only}"

# 可通过 SERVICES 环境变量只对单个库操作，例如：
#   SERVICES=identity ./scripts/migrate.sh force 0
SERVICES="${SERVICES:-identity expense billing report notification}"
ACTION="${1:-up}"
EXTRA="${2:-}"

# 口令要拼进连接串，含 URL 保留字符会被解析错位。
if printf '%s' "$MIGRATOR_PW" | grep -qE '[^A-Za-z0-9._~-]'; then
  echo "错误：EXPENSIO_DB_MIGRATOR_PASSWORD 含 URL 保留字符，当前脚本不支持，请改用仅含 [A-Za-z0-9._~-] 的口令" >&2
  exit 1
fi

# postgres 必须已经健康。make up 使用 `up --wait`，正常路径下这里一定通过；
# 保留检查是为了在容器被手动停掉时给出可执行的提示，而不是抛一个连接错误。
cid="$(docker compose ps -q postgres 2>/dev/null || true)"
if [ -z "$cid" ]; then
  echo "错误：postgres 未运行，请先执行 make up" >&2
  exit 1
fi

status="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$cid")"
if [ "$status" != "healthy" ]; then
  echo "错误：postgres 健康状态为 $status，请先执行 make up" >&2
  exit 1
fi

for svc in $SERVICES; do
  echo "==> $svc"
  # MSYS_NO_PATHCONV：Git Bash 会把容器内路径 "/migrations/x" 改写成
  # "C:/Program Files/Git/migrations/x"，导致 migrate 找不到源目录。
  MSYS_NO_PATHCONV=1 docker run --rm \
    --network "$COMPOSE_NETWORK" \
    -v "$(pwd)/migrations:/migrations:ro" \
    "$MIGRATE_IMAGE" \
    -path "/migrations/$svc" \
    -database "postgres://${svc}_migrator:${MIGRATOR_PW}@${PG_HOST}:${PG_PORT}/${svc}_db?sslmode=disable" \
    $ACTION $EXTRA
done

echo "迁移完成：$ACTION"
