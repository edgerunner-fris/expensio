#!/usr/bin/env bash
# 三个演练共用的环境与辅助函数。被 rebalance.sh / drain.sh / pdb.sh source。
#
# 与 smoke.sh 一致地接受这些环境变量，便于 k3d 与托管集群共用：
#   NAMESPACE（默认 expensio）、RELEASE（默认 expensio）、
#   INGRESS_HOST（默认 expensio.test）、BASE_URL（默认 http://127.0.0.1:8080）、
#   KUBECTL（默认 kubectl）、KAFKA_BOOTSTRAP（默认 localhost:9092）、
#   KAFKA_CLI（默认 /opt/kafka/bin）。

NS=${NAMESPACE:-expensio}
RELEASE=${RELEASE:-expensio}
INGRESS_HOST=${INGRESS_HOST:-expensio.test}
BASE=${BASE_URL:-http://127.0.0.1:8080}
KUBECTL=${KUBECTL:-kubectl}
KAFKA_BOOTSTRAP=${KAFKA_BOOTSTRAP:-localhost:9092}
KAFKA_CLI=${KAFKA_CLI:-/opt/kafka/bin}

log() { printf '\n== %s\n' "$*"; }
fail() { printf 'DRILL FAIL: %s\n' "$*" >&2; exit 1; }
req() { curl -fsS --max-time 20 -H "Host: $INGRESS_HOST" "$@"; }

deploy_ready() {
  $KUBECTL -n "$NS" get deploy "$1" -o jsonpath='{.status.readyReplicas}' 2>/dev/null \
    | grep -E '^[0-9]+$' || echo 0
}

wait_ready() { # $1=deployment $2=期望副本 $3=超时秒
  local deadline=$((SECONDS + $3))
  while [ $SECONDS -lt $deadline ]; do
    [ "$(deploy_ready "$1")" -ge "$2" ] && return 0
    sleep 3
  done
  return 1
}

# HPA 开启时 Deployment 不渲染 replicas，副本数由 HPA 独占；
# 关闭时只能直接 scale Deployment。演练必须两种模式都能跑。
ORIG_MODE=""; ORIG_MIN=""; ORIG_MAX=""; ORIG_REPLICAS=""

snapshot_scale() { # $1=deployment
  ORIG_REPLICAS=$($KUBECTL -n "$NS" get deploy "$1" -o jsonpath='{.spec.replicas}' 2>/dev/null)
  if $KUBECTL -n "$NS" get hpa "$1" >/dev/null 2>&1; then
    ORIG_MODE=hpa
    ORIG_MIN=$($KUBECTL -n "$NS" get hpa "$1" -o jsonpath='{.spec.minReplicas}' 2>/dev/null)
    ORIG_MAX=$($KUBECTL -n "$NS" get hpa "$1" -o jsonpath='{.spec.maxReplicas}' 2>/dev/null)
  else
    ORIG_MODE=deploy
  fi
}

scale_to() { # $1=deployment $2=副本数
  if [ "$ORIG_MODE" = hpa ]; then
    local max=$2
    [ "${ORIG_MAX:-0}" -gt "$max" ] && max=$ORIG_MAX
    $KUBECTL -n "$NS" patch hpa "$1" --type=merge \
      -p "{\"spec\":{\"minReplicas\":$2,\"maxReplicas\":$max}}" >/dev/null
  else
    $KUBECTL -n "$NS" scale deploy "$1" --replicas="$2" >/dev/null
  fi
}

restore_scale() { # $1=deployment
  if [ "$ORIG_MODE" = hpa ]; then
    $KUBECTL -n "$NS" patch hpa "$1" --type=merge \
      -p "{\"spec\":{\"minReplicas\":$ORIG_MIN,\"maxReplicas\":$ORIG_MAX}}" >/dev/null
  else
    $KUBECTL -n "$NS" scale deploy "$1" --replicas="$ORIG_REPLICAS" >/dev/null
  fi
}

kafka_pod() {
  $KUBECTL -n "$NS" get pods -l app.kubernetes.io/name=kafka \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

kafka_group() { # $1=kafka pod，其余参数透传给 kafka-consumer-groups.sh
  local pod=$1; shift
  timeout 25 $KUBECTL -n "$NS" exec "$pod" -- \
    "$KAFKA_CLI/kafka-consumer-groups.sh" --bootstrap-server "$KAFKA_BOOTSTRAP" "$@"
}

# 打印消费组摘要："分区数 消费者数 提交位点合计 日志末端合计 积压合计"。
group_summary() { # $1=kafka pod $2=group $3=topic
  kafka_group "$1" --describe --group "$2" 2>/dev/null | awk -v t="$3" '
    NR>1 && $2==t {
      parts++
      if ($7 != "-") cons[$7]=1
      if ($4 != "-") cur += $4
      if ($5 != "-") end += $5
      if ($6 != "-") lag += $6
    }
    END {
      n=0; for (c in cons) n++
      printf "%d %d %d %d %d\n", parts, n, cur, end, lag
    }'
}

# 注册一个租户并返回 access token（只用于演练探针/造事件）。
register_token() { # $1=slug 前缀
  local slug="$1-$(date +%s)"
  local reg tok
  reg=$(req -H 'Content-Type: application/json' -X POST "$BASE/api/v1/auth/register" \
    -d "{\"tenant_slug\":\"$slug\",\"tenant_name\":\"Drill $slug\",\"email\":\"$slug@example.com\",\"password\":\"Drill-pass-123\",\"display_name\":\"Drill Owner\"}") \
    || return 1
  tok=$(printf '%s' "$reg" | jq -r '.data.access_token')
  [ -n "$tok" ] && [ "$tok" != "null" ] || return 1
  printf '%s' "$tok"
}
