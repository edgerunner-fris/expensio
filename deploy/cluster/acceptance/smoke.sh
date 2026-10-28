#!/usr/bin/env bash
# 在真实集群里验收 chart 安装结果。CI 的 cluster job 与本机演练共用。
#
# 覆盖什么：
#   1. Helm Hook 真的执行了：五个库有迁移记录、四个主题已创建、Job 已因成功被删除；
#   2. Ingress → web → gateway → 身份/费用/报表/计费 的真实请求链路；
#   3. 事件链路：提交 → 审批 → Kafka → report 读模型可查到聚合；
#   4. NetworkPolicy 的实际放行/拒绝（同 release 放行、跨命名空间与未打标签的
#      Pod 拒绝、ingress controller 可访问 web）；
#   5. RBAC 与容器安全上下文（SA 无权限、不挂载令牌、只读根文件系统）。
#
# 依赖：kubectl、curl、jq；入口地址默认是 k3d 映射到宿主 8080 的负载均衡器。
#
# 托管依赖（RDS / ElastiCache / MSK 等）没有命名空间内的 postgres / kafka Pod：
#   MANAGED_DEPS=1 ... bash deploy/cluster/acceptance/smoke.sh
# 会跳过两条结构性检查，迁移与主题由功能链路证明；需要直连证据时按
# docs/EXTERNAL_VERIFICATION.md 单独渲染迁移/主题 Hook Job。
set -euo pipefail

NS=${NAMESPACE:-expensio}
INGRESS_HOST=${INGRESS_HOST:-expensio.test}
BASE=${BASE_URL:-http://127.0.0.1:8080}
KUBECTL=${KUBECTL:-kubectl}
RELEASE=${RELEASE:-expensio}
# 托管依赖（RDS / ElastiCache / MSK 等）没有 deploy/postgres、deploy/kafka
# 可供 exec，结构性断言改为"功能链路 + 单独渲染 Hook Job"。本地 k3d 保持 0。
MANAGED_DEPS=${MANAGED_DEPS:-0}

log() { printf '\n== %s\n' "$*"; }
fail() { printf 'SMOKE FAIL: %s\n' "$*" >&2; exit 1; }

req() {
  curl -fsS --max-time 20 -H "Host: $INGRESS_HOST" "$@"
}

log "Pod 就绪"
$KUBECTL -n "$NS" get pods -o wide
$KUBECTL -n "$NS" wait --for=condition=Ready \
  pod -l app.kubernetes.io/part-of=expensio --timeout=300s
ready=$($KUBECTL -n "$NS" get deploy -l app.kubernetes.io/part-of=expensio \
  -o jsonpath='{range .items[*]}{.metadata.name}={.status.readyReplicas}/{.spec.replicas}{"\n"}{end}')
echo "$ready"
[ "$(echo "$ready" | grep -c '^[a-z-]*=[0-9]*/[0-9]*$')" -eq 7 ] || fail "应有 6 个服务 + web 共 7 个 Deployment"

log "安全上下文（集群中实际生效的值）"
sec=$($KUBECTL -n "$NS" get deploy -l app.kubernetes.io/part-of=expensio \
  -o jsonpath='{range .items[*]}{.metadata.name}{" runAsNonRoot="}{.spec.template.spec.securityContext.runAsNonRoot}{" seccomp="}{.spec.template.spec.securityContext.seccompProfile.type}{" ro="}{.spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem}{" ape="}{.spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation}{"\n"}{end}')
echo "$sec"
[ "$(echo "$sec" | grep -c 'runAsNonRoot=true seccomp=RuntimeDefault ro=true ape=false')" -eq 7 ] \
  || fail "安全上下文不符合预期"

log "RBAC：ServiceAccount 不应有 API 权限，也不应挂载令牌"
sa_token=$($KUBECTL -n "$NS" get deploy -l app.kubernetes.io/part-of=expensio \
  -o jsonpath='{range .items[*]}{.spec.template.spec.automountServiceAccountToken}{"\n"}{end}' | sort -u)
echo "automountServiceAccountToken=$sa_token"
[ "$sa_token" = "false" ] || fail "Pod 仍挂载 ServiceAccount 令牌"
for verb in list get; do
  # can-i 在答案为 no 时退出码为 1，这里必须吞掉，否则 set -e 会提前终止。
  can=$($KUBECTL auth can-i "$verb" pods -n "$NS" --as=system:serviceaccount:"$NS":expensio || true)
  echo "can-i $verb pods=$can"
  [ "$can" = "no" ] || fail "ServiceAccount 不应能 $verb pods"
done

if [ "$MANAGED_DEPS" = "1" ]; then
  # 托管数据库没有命名空间内的 deploy/postgres 可 exec；迁移是否真正执行，
  # 由后面的注册/建单/报表链路证明。需要结构性证据时用
  # docs/EXTERNAL_VERIFICATION.md 里的"单独渲染 Hook Job"步骤。
  log "迁移 Hook：MANAGED_DEPS=1，跳过直连检查（功能链路会验证迁移）"
else
  log "迁移 Hook：五个库都应存在 schema_migrations"
  for db in identity_db expense_db billing_db report_db notification_db; do
    n=$($KUBECTL -n "$NS" exec deploy/postgres -- \
      psql -U postgres -d "$db" -tAc 'select count(*) from app.schema_migrations')
    echo "$db versions=$n"
    [ "${n:-0}" -gt 0 ] || fail "$db 没有迁移记录"
  done
fi

if [ "$MANAGED_DEPS" = "1" ]; then
  log "主题 Hook：MANAGED_DEPS=1，跳过直连检查（事件链路会验证主题）"
else
  log "主题 Hook：四个主题都应已创建"
  topics=$($KUBECTL -n "$NS" exec deploy/kafka -- \
    /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --list)
  echo "$topics"
  for topic in expensio.expense.events.v1 expensio.expense.events.v1.dlq \
               expensio.billing.events.v1 expensio.billing.events.v1.dlq; do
    echo "$topics" | grep -qx "$topic" || fail "缺少主题 $topic"
  done
  jobs=$($KUBECTL -n "$NS" get jobs --no-headers 2>/dev/null | wc -l)
  echo "剩余 Job 数=$jobs（hook-succeeded 会删除成功的 Job）"
  [ "$jobs" -eq 0 ] || fail "仍有未清理的 Hook Job"
fi

log "Ingress"
$KUBECTL -n "$NS" get ingress "$RELEASE"
[ "$($KUBECTL -n "$NS" get ingress "$RELEASE" -o jsonpath='{.spec.rules[0].host}')" = "$INGRESS_HOST" ] \
  || fail "Ingress host 不匹配"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 -H "Host: $INGRESS_HOST" "$BASE/healthz")
echo "GET /healthz -> $code"
[ "$code" = "200" ] || fail "Ingress 到 web 的 /healthz 不是 200"

log "API 链路：注册 → 邀请 → 接受 → 费用 → 审批 → 报表"
slug="smoke-$(date +%s)"
reg=$(req -H 'Content-Type: application/json' -X POST "$BASE/api/v1/auth/register" \
  -d "{\"tenant_slug\":\"$slug\",\"tenant_name\":\"Smoke $slug\",\"email\":\"$slug@example.com\",\"password\":\"Smoke-pass-123\",\"display_name\":\"Smoke Owner\"}")
owner=$(printf '%s' "$reg" | jq -r '.data.access_token')
[ -n "$owner" ] && [ "$owner" != "null" ] || fail "注册未返回 access_token"
owner_auth=(-H "Authorization: Bearer $owner" -H 'Content-Type: application/json')

me=$(req "${owner_auth[@]}" "$BASE/api/v1/me")
[ "$(printf '%s' "$me" | jq -r '.data.role')" = "owner" ] || fail "/me 角色不对"
req "${owner_auth[@]}" "$BASE/api/v1/expenses" >/dev/null
req "${owner_auth[@]}" "$BASE/api/v1/billing/plans" >/dev/null
members=$(req "${owner_auth[@]}" "$BASE/api/v1/tenants/current/members")
echo "members=$(printf '%s' "$members" | jq -r '.data | length')"

invite=$(req "${owner_auth[@]}" -X POST "$BASE/api/v1/tenants/current/invitations" \
  -d "{\"email\":\"admin-$slug@example.com\",\"role\":\"admin\"}")
link=$(printf '%s' "$invite" | jq -r '.data.link')
token=$(printf '%s' "$link" | sed -E 's#.*/invite/([^/?]+).*#\1#')
[ -n "$token" ] && [ "$token" != "$link" ] || fail "邀请链接里没有一次性令牌"
accepted=$(curl -fsS --max-time 20 -H "Host: $INGRESS_HOST" -H 'Content-Type: application/json' \
  -X POST "$BASE/api/v1/invitations/$token/accept" \
  -d '{"password":"Admin-pass-123","display_name":"Smoke Admin"}')
admin=$(printf '%s' "$accepted" | jq -r '.data.access_token')
[ -n "$admin" ] && [ "$admin" != "null" ] || fail "接受邀请未返回 access_token"
admin_auth=(-H "Authorization: Bearer $admin" -H 'Content-Type: application/json')

occurred=$(date -u +%Y-%m-%dT%H:%M:%SZ)
created=$(req "${owner_auth[@]}" -X POST "$BASE/api/v1/expenses" \
  -d "{\"category\":\"travel\",\"currency\":\"CNY\",\"amount_minor\":12345,\"merchant\":\"Smoke Air\",\"description\":\"cluster smoke\",\"occurred_at\":\"$occurred\"}")
expense=$(printf '%s' "$created" | jq -r '.data.id')
[ -n "$expense" ] && [ "$expense" != "null" ] || fail "创建费用单失败"
req "${owner_auth[@]}" -X POST "$BASE/api/v1/expenses/$expense/submit" >/dev/null
req "${admin_auth[@]}" -X POST "$BASE/api/v1/expenses/$expense/approve" -d '{"comment":"ok"}' >/dev/null
echo "expense $expense submitted and approved"

from=$(date -u -d '2 months ago' +%Y-%m-%d)
to=$(date -u +%Y-%m-%d)
totals=0
for _ in $(seq 1 30); do
  summary=$(req "${owner_auth[@]}" \
    "$BASE/api/v1/reports/expenses/summary?from=$from&to=$to&group_by=category&scope=all" 2>/dev/null || true)
  totals=$(printf '%s' "$summary" | jq -r '.data.currency_totals | length' 2>/dev/null || echo 0)
  [ "${totals:-0}" -gt 0 ] && break
  sleep 2
done
echo "report currency_totals=$totals"
[ "${totals:-0}" -gt 0 ] || fail "报表读模型没有消费到事件（Kafka 链路失败）"

# 测试 Pod 名带本次运行的 PID：脚本连续跑两次时，上一轮的 Pod 可能仍在
# Terminating（删除与 kubelet 的宽限期是异步的），固定名字会让 kubectl run
# 以 AlreadyExists 失败。带后缀既避开重名，也让失败时的现场可辨认。
suffix=$$
other="nettest-other-$suffix"
unlabeled="nettest-unlabeled-$suffix"
labeled="nettest-labeled-$suffix"

log "NetworkPolicy：跨命名空间访问必须被拒绝"
$KUBECTL -n default run "$other" --image=busybox:1.36 --restart=Never \
  --command -- sleep 300 >/dev/null
$KUBECTL -n default wait --for=condition=Ready "pod/$other" --timeout=120s >/dev/null
if $KUBECTL -n default exec "$other" -- \
  wget -T 3 -qO- http://expensio-identity."$NS".svc.cluster.local:8081/healthz >/dev/null 2>&1; then
  fail "default 命名空间本应被 NetworkPolicy 拒绝"
fi
echo "default -> identity: blocked"

log "NetworkPolicy：同命名空间但未打 release 标签的 Pod 也必须被拒绝"
$KUBECTL -n "$NS" run "$unlabeled" --image=busybox:1.36 --restart=Never \
  --command -- sleep 300 >/dev/null
$KUBECTL -n "$NS" wait --for=condition=Ready "pod/$unlabeled" --timeout=120s >/dev/null
if $KUBECTL -n "$NS" exec "$unlabeled" -- \
  wget -T 3 -qO- http://expensio-identity:8081/healthz >/dev/null 2>&1; then
  fail "未打标签的 Pod 本应被 NetworkPolicy 拒绝"
fi
echo "unlabeled -> identity: blocked"

log "NetworkPolicy：带同 release 标签的 Pod 放行（同一策略的另一半）"
$KUBECTL -n "$NS" run "$labeled" --image=busybox:1.36 --restart=Never \
  --labels 'app.kubernetes.io/name=expensio,app.kubernetes.io/instance=expensio,app.kubernetes.io/part-of=expensio' \
  --command -- sleep 300 >/dev/null
$KUBECTL -n "$NS" wait --for=condition=Ready "pod/$labeled" --timeout=120s >/dev/null
body=$($KUBECTL -n "$NS" exec "$labeled" -- \
  wget -T 3 -qO- http://expensio-identity:8081/healthz)
echo "labeled -> identity: $body"
echo "$body" | grep -q ok || fail "同 release Pod 应能访问 identity"

$KUBECTL -n default delete pod "$other" --wait=false >/dev/null
$KUBECTL -n "$NS" delete pod "$unlabeled" "$labeled" --wait=false >/dev/null

log "Helm 状态"
helm -n "$NS" status "$RELEASE" | sed -n '1,12p'

printf '\nSMOKE_RESULT: PASS\n'
