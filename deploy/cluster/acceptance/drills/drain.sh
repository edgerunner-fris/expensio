#!/usr/bin/env bash
# 演练：网关连接排空
#
# 滚动重启 gateway，期间以 100ms 间隔经 Ingress 打真实请求
# （/api/v1/me 走 gateway -> identity），断言没有非 200 响应。
# 这验证 SIGTERM -> readiness=false -> 在途请求完成 -> 退出 的关闭顺序，
# 以及 RollingUpdate 的 maxUnavailable=0 / maxSurge=1。
#
# 用法：
#   NAMESPACE=expensio INGRESS_HOST=expensio.test BASE_URL=http://127.0.0.1:8080 \
#     bash deploy/cluster/acceptance/drills/drain.sh
set -uo pipefail
cd "$(dirname "$0")" || exit 2
# shellcheck source=lib.sh
source ./lib.sh

DEPLOY=$RELEASE-gateway

pod_state() {
  $KUBECTL -n "$NS" get pods -l app.kubernetes.io/component=gateway \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}{"\n"}{end}' | sort
}

log "基线：$DEPLOY 副本与滚动策略"
snapshot_scale "$DEPLOY"
echo "scale mode=$ORIG_MODE replicas=${ORIG_REPLICAS:-?} hpa=${ORIG_MIN:-?}..${ORIG_MAX:-?}"
if [ "$(deploy_ready "$DEPLOY")" -ne 1 ]; then
  log "规整基线：$DEPLOY 缩到 1 副本（单副本时排空窗口最能暴露问题）"
  scale_to "$DEPLOY" 1 || fail "缩到 1 副本失败"
  wait_ready "$DEPLOY" 1 240 || fail "240s 内未缩到 1 个副本"
fi
$KUBECTL -n "$NS" get deploy "$DEPLOY" \
  -o jsonpath='replicas={.spec.replicas} ready={.status.readyReplicas} strategy={.spec.strategy.type}/{.spec.strategy.rollingUpdate.maxUnavailable},{.spec.strategy.rollingUpdate.maxSurge} grace={.spec.template.spec.terminationGracePeriodSeconds}{"\n"}'
before=$(pod_state)
echo "before: $before"
[ "$(printf '%s' "$before" | grep -c .)" -eq 1 ] || fail "演练要求 $DEPLOY 缩到 1 个副本"

log "注册探针租户"
tok=$(register_token drain) || fail "注册失败"
[ -n "$tok" ] || fail "注册未返回 access_token"

STOP=$(mktemp -u); OUT=$(mktemp); : >"$OUT"
trap 'touch "$STOP" 2>/dev/null || true' EXIT
probe() {
  while [ ! -f "$STOP" ]; do
    ts=$(date +%s%3N)
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
      -H "Host: $INGRESS_HOST" -H "Authorization: Bearer $tok" \
      "$BASE/api/v1/me" 2>/dev/null) || code=000
    printf '%s %s\n' "$ts" "$code" >>"$OUT"
    sleep 0.1
  done
}

log "启动持续探针（每 100ms 一次 /api/v1/me）"
probe & PROBE_PID=$!
sleep 3

log "滚动重启 $DEPLOY"
$KUBECTL -n "$NS" rollout restart "deploy/$DEPLOY" || fail "rollout restart 失败"
$KUBECTL -n "$NS" rollout status "deploy/$DEPLOY" --timeout=300s || fail "rollout 未在 300s 内完成"
sleep 8

touch "$STOP"
wait "$PROBE_PID" 2>/dev/null || true

log "结果统计"
total=$(wc -l <"$OUT")
ok=$(awk '$2=="200"' "$OUT" | wc -l)
bad=$((total - ok))
echo "probe requests=$total ok=$ok bad=$bad"
echo "状态码分布："
awk '{print $2}' "$OUT" | sort | uniq -c | sed 's/^/  /'
if [ "$bad" -gt 0 ]; then
  echo "失败请求（毫秒时间戳 状态码）："
  awk '$2!="200"' "$OUT" | head -20
fi

after=$(pod_state)
echo "after: $after"
[ "$before" != "$after" ] || fail "Pod 没有被替换，滚动重启未生效"

[ "$total" -ge 50 ] || fail "探针请求只有 $total 次，不足以覆盖滚动窗口"
[ "$bad" -eq 0 ] || fail "滚动重启期间出现 $bad 次非 200 响应"

log "恢复副本数"
restore_scale "$DEPLOY" || fail "恢复副本数失败"
want=${ORIG_REPLICAS:-1}
[ "$ORIG_MODE" = hpa ] && want=${ORIG_MIN:-1}
wait_ready "$DEPLOY" "$want" 240 || echo "WARN: 副本未在 240s 内回到 $want（HPA 缩容窗口）"

echo
echo "DRILL_RESULT: PASS drain"
