#!/usr/bin/env bash
# 冒烟：编译六个服务、逐个拉起、验证探针与统一错误格式，然后全部关掉。
#
# M0 阶段不依赖外部基础设施；M1 起 identity 会在启动时连数据库并要求迁移已执行，
# 因此现在需要先 `make up && make migrate`。这一步验证的仍然是"装配与生命周期"：
# 配置加载、日志、探针、404 信封、退出——只是探针背后多了真实的数据库连接。
set -euo pipefail

cd "$(dirname "$0")/.."

# 与 make 的目标保持一致：配置全部来自 .env，脚本里不写死任何地址或口令。
if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

EXT=""
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) EXT=".exe" ;;
esac

SERVICES="gateway:8080 identity:8081 expense:8082 billing:8083 report:8084 notification:8085"

echo "==> 编译"
go build -trimpath -o bin/ ./cmd/...

pids=""
cleanup() {
  for pid in $pids; do
    kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "==> 启动"
for entry in $SERVICES; do
  name="${entry%%:*}"
  # 库名与账号按服务注入，和 Makefile 的 run-% 一致；gateway 用不到这些变量。
  EXPENSIO_DB_NAME="${name}_db" EXPENSIO_DB_USER="${name}_app" \
    "./bin/${name}${EXT}" >"bin/${name}.smoke.log" 2>&1 &
  pids="$pids $!"
done

fail=0

for entry in $SERVICES; do
  name="${entry%%:*}"
  port="${entry##*:}"
  base="http://127.0.0.1:${port}"

  # --retry-connrefused 等待端口就绪，不用固定睡眠。
  if ! curl -fsS --retry 20 --retry-connrefused --retry-delay 1 "${base}/healthz" >/dev/null; then
    echo "FAIL ${name}: /healthz 不可达（日志：bin/${name}.smoke.log）"
    fail=1
    continue
  fi

  if ! curl -fsS --retry 5 --retry-delay 1 "${base}/readyz" >/dev/null; then
    echo "FAIL ${name}: /readyz 未就绪（日志：bin/${name}.smoke.log）"
    fail=1
    continue
  fi

  # 未知路由必须走统一信封，而不是 gin 默认的纯文本 404。
  resp="$(curl -sS -w '\n%{http_code}' "${base}/api/v1/__smoke__" || true)"
  code="${resp##*$'\n'}"
  body="${resp%$'\n'*}"
  if [ "$code" != "404" ] || ! printf '%s' "$body" | grep -q '"code":"common.not_found"'; then
    echo "FAIL ${name}: 未知路由响应不符合统一信封（status=${code} body=${body}）"
    fail=1
    continue
  fi

  echo "OK   ${name} :${port}"
done

if [ "$fail" -ne 0 ]; then
  echo "冒烟未通过"
  exit 1
fi

echo "冒烟通过：6/6"
