#!/usr/bin/env bash
# 演练：PodDisruptionBudget 驱逐
#
# 断言：
#   1. 1 副本时 disruptionsAllowed=0，Eviction API 返回 429；
#   2. 扩到 2 副本后 disruptionsAllowed=1；
#   3. 同时驱逐两个 Pod：恰好 1 个 201、1 个 429，服务请求全程 200；
#   4. 副本自动补齐，恢复副本数后 disruptionsAllowed 回到 0。
#
# Eviction 是 Pod 的子资源，不在 discovery 里，`kubectl create -f` 无法映射；
# 因此通过 kubectl proxy 直接 POST .../pods/<pod>/eviction。
#
# 用法：
#   NAMESPACE=expensio INGRESS_HOST=expensio.test BASE_URL=http://127.0.0.1:8080 \
#     bash deploy/cluster/acceptance/drills/pdb.sh
set -uo pipefail
cd "$(dirname "$0")" || exit 2
# shellcheck source=lib.sh
source ./lib.sh

DEPLOY=$RELEASE-gateway
PDB=$RELEASE-gateway
PROXY_PORT=${PROXY_PORT:-18001}

ready() { deploy_ready "$DEPLOY"; }
allowed() {
  $KUBECTL -n "$NS" get pdb "$PDB" -o jsonpath='{.status.disruptionsAllowed}' 2>/dev/null \
    | grep -E '^[0-9]+$' || echo -1
}
pdb_state() {
  $KUBECTL -n "$NS" get pdb "$PDB" -o jsonpath='currentHealthy={.status.currentHealthy} desiredHealthy={.status.desiredHealthy} disruptionsAllowed={.status.disruptionsAllowed}{"\n"}'
}

$KUBECTL -n "$NS" get pdb "$PDB" >/dev/null 2>&1 \
  || fail "找不到 PDB $PDB（chart 只在副本数 > 1 时渲染 PDB）"

log "启动 kubectl proxy（127.0.0.1:$PROXY_PORT）"
$KUBECTL proxy --port="$PROXY_PORT" --address=127.0.0.1 >/tmp/expensio-drill-pdb-proxy.log 2>&1 &
PROXY_PID=$!
STOP=$(mktemp -u); PROBE=$(mktemp); : >"$PROBE"
cleanup() {
  touch "$STOP" 2>/dev/null || true
  kill "$PROXY_PID" 2>/dev/null || true
}
trap cleanup EXIT

deadline=$((SECONDS + 30))
until curl -s -o /dev/null "http://127.0.0.1:$PROXY_PORT/version"; do
  [ $SECONDS -ge $deadline ] && fail "kubectl proxy 未就绪"
  sleep 0.5
done
echo "proxy ready: $(curl -s "http://127.0.0.1:$PROXY_PORT/version" | jq -r '.gitVersion')"

# evict_pod 打印 HTTP 状态码，响应体写入 $2。
evict_pod() {
  curl -s -o "$2" -w '%{http_code}' --max-time 30 \
    -X POST -H 'Content-Type: application/json' \
    -d "{\"apiVersion\":\"policy/v1\",\"kind\":\"Eviction\",\"metadata\":{\"name\":\"$1\",\"namespace\":\"$NS\"}}" \
    "http://127.0.0.1:$PROXY_PORT/api/v1/namespaces/$NS/pods/$1/eviction"
}

log "基线：1 副本，PDB 应阻止任何驱逐"
snapshot_scale "$DEPLOY"
echo "scale mode=$ORIG_MODE replicas=${ORIG_REPLICAS:-?} hpa=${ORIG_MIN:-?}..${ORIG_MAX:-?}"
if [ "$(ready)" -ne 1 ]; then
  log "规整基线：$DEPLOY 缩到 1 副本"
  scale_to "$DEPLOY" 1 || fail "缩到 1 副本失败"
  wait_ready "$DEPLOY" 1 240 || fail "240s 内未缩到 1 个副本"
  deadline=$((SECONDS + 120))
  while [ $SECONDS -lt $deadline ]; do
    [ "$(allowed)" -eq 0 ] && break
    sleep 3
  done
fi
r=$(ready); echo "$DEPLOY readyReplicas=$r"
[ "$r" -eq 1 ] || fail "演练要求 $DEPLOY 当前为 1 个副本，实际 $r"
pdb_state
[ "$(allowed)" -eq 0 ] || fail "1 副本时 disruptionsAllowed 应为 0，实际 $(allowed)"

pod=$($KUBECTL -n "$NS" get pods -l app.kubernetes.io/component=gateway -o jsonpath='{.items[0].metadata.name}')
echo "唯一 Pod: $pod"
out=$(mktemp)
code=$(evict_pod "$pod" "$out")
body=$(cat "$out"); rm -f "$out"
echo "Eviction HTTP $code: $body"
[ "$code" = "429" ] || fail "1 副本时 Eviction 期望 429，实际 $code"
printf '%s' "$body" | grep -qi 'disruption budget' || fail "429 的原因不是 PDB：$body"

log "扩到 2 副本"
scale_to "$DEPLOY" 2 || fail "扩容失败"
wait_ready "$DEPLOY" 2 240 || fail "240s 内未扩到 2 个副本"
deadline=$((SECONDS + 120))
while [ $SECONDS -lt $deadline ]; do
  [ "$(allowed)" -eq 1 ] && break
  sleep 3
done
r=$(ready); a=$(allowed)
echo "readyReplicas=$r disruptionsAllowed=$a"
pdb_state
[ "$r" -ge 2 ] || fail "未扩到 2 个副本"
[ "$a" -eq 1 ] || fail "2 副本时 disruptionsAllowed 应为 1，实际 $a"

log "注册探针租户并开始持续请求"
tok=$(register_token pdb) || fail "注册失败"
[ -n "$tok" ] || fail "注册未返回 access_token"

probe() {
  while [ ! -f "$STOP" ]; do
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
      -H "Host: $INGRESS_HOST" -H "Authorization: Bearer $tok" \
      "$BASE/api/v1/me" 2>/dev/null) || code=000
    printf '%s\n' "$code" >>"$PROBE"
    sleep 0.2
  done
}
probe & PROBE_PID=$!
sleep 2

pods=$($KUBECTL -n "$NS" get pods -l app.kubernetes.io/component=gateway \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
echo "并发驱逐以下 Pod："
printf '%s\n' "$pods" | sed 's/^/  /'
[ "$(printf '%s' "$pods" | grep -c .)" -ge 2 ] || fail "少于 2 个 Pod，无法做并发驱逐"

RES=$(mktemp)
pids=()
for p in $pods; do
  (
    o=$(mktemp)
    c=$(evict_pod "$p" "$o")
    printf '%s\t%s\t%s\n' "$p" "$c" "$(tr '\n' ' ' <"$o")" >>"$RES"
    rm -f "$o"
  ) &
  pids+=("$!")
done
for pid in "${pids[@]}"; do wait "$pid"; done

log "并发驱逐结果"
cat "$RES"
succeeded=$(awk -F'\t' '$2=="201"' "$RES" | wc -l)
blocked=$(awk -F'\t' '$2=="429"' "$RES" | wc -l)
blocked_pdb=$(awk -F'\t' '$2=="429" {print $3}' "$RES" | grep -ci 'disruption budget' || true)
echo "succeeded=$succeeded blocked=$blocked blocked_by_pdb=$blocked_pdb"
[ "$succeeded" -eq 1 ] || fail "并发驱逐成功了 $succeeded 个，期望恰好 1 个"
[ "$blocked" -eq 1 ] || fail "被拒绝 $blocked 个，期望恰好 1 个（429）"
[ "${blocked_pdb:-0}" -ge 1 ] || fail "429 的原因不是 PDB"

log "等待副本补齐与 PDB 恢复"
deadline=$((SECONDS + 240))
while [ $SECONDS -lt $deadline ]; do
  [ "$(ready)" -ge 2 ] && [ "$(allowed)" -eq 1 ] && break
  sleep 3
done
r=$(ready); a=$(allowed)
echo "readyReplicas=$r disruptionsAllowed=$a"
pdb_state
[ "$r" -ge 2 ] || fail "被驱逐的副本没有被补齐"
[ "$a" -eq 1 ] || fail "PDB 未恢复到 1"

sleep 2
touch "$STOP"; wait "$PROBE_PID" 2>/dev/null || true
total=$(wc -l <"$PROBE")
ok=$(grep -c '^200$' "$PROBE" || true)
bad=$((total - ${ok:-0}))
echo "驱逐期间探针：requests=$total ok=${ok:-0} bad=$bad"
echo "状态码分布："
sort "$PROBE" | uniq -c | sed 's/^/  /'
[ "$total" -ge 20 ] || fail "探针请求只有 $total 次"
[ "$bad" -eq 0 ] || fail "驱逐期间出现 $bad 次非 200 响应"

log "恢复副本数"
restore_scale "$DEPLOY" || fail "恢复副本数失败"
want=${ORIG_REPLICAS:-1}
[ "$ORIG_MODE" = hpa ] && want=${ORIG_MIN:-1}
deadline=$((SECONDS + 360))
while [ $SECONDS -lt $deadline ]; do
  [ "$(ready)" -eq "$want" ] && [ "$(allowed)" -eq $((want - 1)) ] && break
  sleep 5
done
r=$(ready); a=$(allowed)
echo "readyReplicas=$r disruptionsAllowed=$a (expected $want / $((want - 1)))"
pdb_state
[ "$r" -eq "$want" ] || fail "未恢复到 $want 副本"
[ "$a" -eq $((want - 1)) ] || fail "disruptionsAllowed 未恢复到 $((want - 1))"

echo
echo "DRILL_RESULT: PASS pdb"
