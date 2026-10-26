#!/usr/bin/env bash
# Linux / WSL 工具链自检：只检查项目实际用到的命令，缺一即失败。
# 与 scripts/preflight.ps1 保持同一目的：在开始构建之前暴露环境问题。
set -euo pipefail

fail=0

ok()   { printf '  OK   %-10s %s\n' "$1" "$2"; }
bad()  { printf '  FAIL %-10s %s\n' "$1" "$2"; fail=1; }
warn() { printf '  WARN %-10s %s\n' "$1" "$2"; }

echo "Expensio 工具链自检"

if command -v go >/dev/null 2>&1; then
  ok go "$(go version)"
else
  bad go "未安装；安装 Go 1.27+ 并把 /usr/local/go/bin 加入 PATH"
fi

if command -v git >/dev/null 2>&1; then
  ok git "$(git --version)"
else
  bad git "未安装"
fi

if command -v make >/dev/null 2>&1; then
  ok make "$(make --version | head -n1)"
else
  bad make "未安装"
fi

if command -v curl >/dev/null 2>&1; then
  ok curl "$(curl --version | head -n1)"
else
  bad curl "未安装"
fi

if ! command -v docker >/dev/null 2>&1; then
  bad docker "未安装，或在 WSL 中未启用 Docker Desktop 集成"
elif docker info >/dev/null 2>&1; then
  ok docker "$(docker --version)"
else
  # daemon 未启动不算工具链缺失；集成测试会由 EXPENSIO_REQUIRE_DOCKER 决定是否失败。
  warn docker "命令存在，但 daemon 当前不可用"
fi

if docker compose version >/dev/null 2>&1; then
  ok compose "$(docker compose version --short)"
else
  bad compose "docker compose 插件不可用"
fi

exit "$fail"
