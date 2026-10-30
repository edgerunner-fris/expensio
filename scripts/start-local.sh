#!/usr/bin/env bash
# 本地整栈一键启动（Windows 桌面快捷方式调用；也可直接手动执行）。
# 幂等：已经起来时几十秒内返回。
set -u
cd "$(dirname "$0")/.." || exit 1
export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
DOCKER_DESKTOP="/mnt/c/Program Files/Docker/Docker/resources/bin/docker.exe"

echo "=============================================="
echo " Expensio 本地整栈启动"
echo "=============================================="

# 1) Docker 引擎：没起来就尝试拉起 Docker Desktop，最多等 5 分钟
if ! docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then
  echo "[1/3] Docker 未运行，正在尝试启动 Docker Desktop…"
  if [ -x "$DOCKER_DESKTOP" ]; then
    "$DOCKER_DESKTOP" desktop start >/dev/null 2>&1 || true
  fi
  for _ in $(seq 1 60); do
    docker version --format '{{.Server.Version}}' >/dev/null 2>&1 && break
    sleep 5
    printf '.'
  done
  echo
fi
ver=$(docker version --format '{{.Server.Version}}' 2>/dev/null) || {
  echo "[失败] Docker 没起来。手动打开 Docker Desktop，或检查"
  echo "       Settings → Resources → WSL integration 是否勾选 Ubuntu-24.04。"
  exit 1
}
echo "[1/3] Docker server $ver"

# 2) .env
if [ ! -f .env ]; then
  echo "[2/3] 首次运行，生成 .env…"
  make env || exit 1
else
  echo "[2/3] .env 已存在"
fi

# 3) 整栈
echo "[3/3] 启动整栈（首次会构建镜像，可能几分钟）…"
if ! make app-up; then
  echo
  echo "[失败] 未完成。排查：docker compose --profile app ps ；S=gateway make logs"
  exit 1
fi

echo
echo "----------------------------------------------"
echo " 已就绪："
echo "   管理台   http://127.0.0.1:5173/"
echo "   网关     http://127.0.0.1:8080/healthz"
echo "   Grafana  http://127.0.0.1:3000/"
echo "   Mailpit  http://127.0.0.1:8025/"
echo "----------------------------------------------"
