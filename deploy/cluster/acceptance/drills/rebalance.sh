#!/usr/bin/env bash
# 演练：Kafka 消费者再平衡
#
# 验证 report 消费组在副本数变化时重新分配分区，且再平衡期间产生的事件
# 不丢不重（位点前移量 = 日志增量、LAG 归零、报表读模型金额正确）。
#
# 用法：
#   NAMESPACE=expensio INGRESS_HOST=expensio.test BASE_URL=http://127.0.0.1:8080 \
#     bash deploy/cluster/acceptance/drills/rebalance.sh
set -uo pipefail
cd "$(dirname "$0")" || exit 2
# shellcheck source=lib.sh
source ./lib.sh

GROUP=report-expense-v1
TOPIC=expensio.expense.events.v1
DEPLOY=$RELEASE-report
EXPECTED_EVENTS=4       # 2 张费用单的 submitted + approved
EXPECTED_CNY=24690      # 2 x 12345

KP=$(kafka_pod)
[ -n "$KP" ] || fail "命名空间 $NS 里找不到 kafka Pod（托管 Kafka 不适用本演练）"

log "基线：$GROUP（1 副本）"
kafka_group "$KP" --describe --group "$GROUP"
snapshot_scale "$DEPLOY"
echo "scale mode=$ORIG_MODE replicas=${ORIG_REPLICAS:-?} hpa=${ORIG_MIN:-?}..${ORIG_MAX:-?}"
if [ "$(deploy_ready "$DEPLOY")" -ne 1 ]; then
  log "规整基线：$DEPLOY 缩到 1 副本"
  scale_to "$DEPLOY" 1 || fail "缩到 1 副本失败"
  wait_ready "$DEPLOY" 1 240 || fail "240s 内未缩到 1 个副本"
fi
[ "$(deploy_ready "$DEPLOY")" -eq 1 ] || fail "演练开始前 $DEPLOY 应为 1 个就绪副本"
read -r parts cons cur0 end0 lag0 <<<"$(group_summary "$KP" "$GROUP" "$TOPIC")"
[ "${parts:-0}" -ge 2 ] || fail "主题只有 ${parts:-0} 个分区，无法验证多消费者分配"
[ "$cons" -eq 1 ] || fail "基线应只有 1 个 consumer，实际 $cons"
[ "$lag0" -eq 0 ] || fail "基线已有积压 lag=$lag0"
echo "baseline parts=$parts consumers=$cons current=$cur0 end=$end0 lag=$lag0"

log "扩容到 2 副本"
scale_to "$DEPLOY" 2 || fail "扩容失败"
wait_ready "$DEPLOY" 2 240 || fail "240s 内未扩到 2 个副本"
echo "$DEPLOY readyReplicas=$(deploy_ready "$DEPLOY")"

log "等待再平衡到 2 个 consumer"
deadline=$((SECONDS + 180))
while [ $SECONDS -lt $deadline ]; do
  read -r parts cons _ _ lag <<<"$(group_summary "$KP" "$GROUP" "$TOPIC")"
  [ "${cons:-0}" -eq 2 ] && [ "${lag:-1}" -eq 0 ] && break
  sleep 3
done
kafka_group "$KP" --describe --group "$GROUP"
read -r parts cons cur1 end1 lag1 <<<"$(group_summary "$KP" "$GROUP" "$TOPIC")"
echo "after-scale parts=$parts consumers=$cons current=$cur1 end=$end1 lag=$lag1"
[ "$cons" -eq 2 ] || fail "180s 内未观察到 2 个 consumer"
[ "$lag1" -eq 0 ] || fail "再平衡后有积压 lag=$lag1"
per=$(kafka_group "$KP" --describe --group "$GROUP" 2>/dev/null \
  | awk -v t="$TOPIC" 'NR>1 && $2==t && $7!="-"{print $7}' | sort | uniq -c | awk '{print $1}' | sort -n | head -1)
echo "min partitions per consumer=${per:-0}"
[ "${per:-0}" -ge 1 ] || fail "有 consumer 没有分到分区"

log "产生真实事件：注册 -> 邀请 admin -> 建 2 张费用单 -> 提交 -> 审批"
slug="rebalance-$(date +%s)"
reg=$(req -H 'Content-Type: application/json' -X POST "$BASE/api/v1/auth/register" \
  -d "{\"tenant_slug\":\"$slug\",\"tenant_name\":\"Rebalance $slug\",\"email\":\"$slug@example.com\",\"password\":\"Drill-pass-123\",\"display_name\":\"Drill Owner\"}") \
  || fail "注册失败"
owner=$(printf '%s' "$reg" | jq -r '.data.access_token')
[ -n "$owner" ] && [ "$owner" != "null" ] || fail "注册未返回 access_token"
owner_auth=(-H "Authorization: Bearer $owner" -H 'Content-Type: application/json')

invite=$(req "${owner_auth[@]}" -X POST "$BASE/api/v1/tenants/current/invitations" \
  -d "{\"email\":\"admin-$slug@example.com\",\"role\":\"admin\"}") || fail "创建邀请失败"
link=$(printf '%s' "$invite" | jq -r '.data.link')
token=$(printf '%s' "$link" | sed -E 's#.*/invite/([^/?]+).*#\1#')
[ -n "$token" ] && [ "$token" != "$link" ] || fail "邀请链接里没有一次性令牌"
accepted=$(curl -fsS --max-time 20 -H "Host: $INGRESS_HOST" -H 'Content-Type: application/json' \
  -X POST "$BASE/api/v1/invitations/$token/accept" \
  -d '{"password":"Admin-pass-123","display_name":"Drill Admin"}') || fail "接受邀请失败"
admin=$(printf '%s' "$accepted" | jq -r '.data.access_token')
[ -n "$admin" ] && [ "$admin" != "null" ] || fail "接受邀请未返回 access_token"
admin_auth=(-H "Authorization: Bearer $admin" -H 'Content-Type: application/json')

occurred=$(date -u +%Y-%m-%dT%H:%M:%SZ)
for i in 1 2; do
  created=$(req "${owner_auth[@]}" -X POST "$BASE/api/v1/expenses" \
    -d "{\"category\":\"travel\",\"currency\":\"CNY\",\"amount_minor\":12345,\"merchant\":\"Rebalance Air $i\",\"description\":\"drill $i\",\"occurred_at\":\"$occurred\"}") \
    || fail "创建第 $i 张费用单失败"
  expense=$(printf '%s' "$created" | jq -r '.data.id')
  [ -n "$expense" ] && [ "$expense" != "null" ] || fail "创建第 $i 张费用单未返回 id"
  req "${owner_auth[@]}" -X POST "$BASE/api/v1/expenses/$expense/submit" >/dev/null || fail "提交第 $i 张失败"
  req "${admin_auth[@]}" -X POST "$BASE/api/v1/expenses/$expense/approve" -d '{"comment":"drill ok"}' >/dev/null || fail "审批第 $i 张失败"
  echo "expense[$i]=$expense submitted+approved"
done

log "等待积压归零并核对位点"
deadline=$((SECONDS + 180))
while [ $SECONDS -lt $deadline ]; do
  read -r parts cons cur2 end2 lag2 <<<"$(group_summary "$KP" "$GROUP" "$TOPIC")"
  [ "${lag2:-1}" -eq 0 ] && [ $(( ${cur2:-0} - cur1 )) -ge $EXPECTED_EVENTS ] && break
  sleep 2
done
kafka_group "$KP" --describe --group "$GROUP"
read -r parts cons cur2 end2 lag2 <<<"$(group_summary "$KP" "$GROUP" "$TOPIC")"
produced=$((end2 - end1))
consumed=$((cur2 - cur1))
echo "produced=$produced consumed=$consumed lag=$lag2 consumers=$cons"
[ "$produced" -ge "$EXPECTED_EVENTS" ] || fail "日志只增加 $produced 条，少于预期的 $EXPECTED_EVENTS 条"
[ "$lag2" -eq 0 ] || fail "仍有积压 lag=$lag2"
[ "$consumed" -eq "$produced" ] || fail "位点前移 $consumed 与日志增量 $produced 不一致"

log "核对报表读模型：金额必须等于 $EXPECTED_CNY CNY"
from=$(date -u -d '2 months ago' +%Y-%m-%d)
to=$(date -u +%Y-%m-%d)
amt=0
for _ in $(seq 1 30); do
  summary=$(req "${owner_auth[@]}" \
    "$BASE/api/v1/reports/expenses/summary?from=$from&to=$to&group_by=category&scope=all" 2>/dev/null || true)
  amt=$(printf '%s' "$summary" | jq -r '[.data.currency_totals[]? | select(.currency=="CNY") | .amount_minor] | add // 0' 2>/dev/null || echo 0)
  [ "${amt:-0}" -eq "$EXPECTED_CNY" ] && break
  sleep 2
done
echo "report CNY amount_minor=$amt (expected $EXPECTED_CNY)"
[ "${amt:-0}" -eq "$EXPECTED_CNY" ] || fail "报表读模型金额 $amt 不等于 $EXPECTED_CNY，事件可能丢失或重复"

log "缩到 1 副本，验证再次再平衡"
scale_to "$DEPLOY" 1 || fail "缩容失败"
deadline=$((SECONDS + 360))
while [ $SECONDS -lt $deadline ]; do
  read -r parts cons _ _ lag <<<"$(group_summary "$KP" "$GROUP" "$TOPIC")"
  [ "${cons:-0}" -eq 1 ] && [ "${lag:-1}" -eq 0 ] && break
  sleep 3
done
kafka_group "$KP" --describe --group "$GROUP"
read -r parts cons cur3 end3 lag3 <<<"$(group_summary "$KP" "$GROUP" "$TOPIC")"
echo "after-scale-down parts=$parts consumers=$cons current=$cur3 end=$end3 lag=$lag3"
[ "$cons" -eq 1 ] && [ "${lag3:-1}" -eq 0 ] || fail "缩容后分配或积压异常"

log "恢复初始副本策略"
restore_scale "$DEPLOY" || fail "恢复副本策略失败"
want=${ORIG_REPLICAS:-1}
[ "$ORIG_MODE" = hpa ] && want=${ORIG_MIN:-1}
wait_ready "$DEPLOY" "$want" 300 || echo "WARN: 副本未在 300s 内回到 $want"

echo
echo "DRILL_RESULT: PASS rebalance"
