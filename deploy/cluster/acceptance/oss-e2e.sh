#!/usr/bin/env bash
# 对象存储接管后的附件闭环验收：注册租户 → 建费用单 → presign → 浏览器式
# PUT（带 Content-Type）→ 确认 → 列表取下载 URL → 下载比对 → 删除 → 列表归零。
#
# 为什么单独一个脚本：smoke.sh 只证明服务活着、链路通，不碰对象存储；
# 而附件是唯一横跨"应用签名 / 浏览器直连对象存储 / 应用核对元数据"的动作，
# 换了对象存储供应商（MinIO → OSS）必须单独跑一遍。
#
# presigned URL 由服务的 `publicEndpoint` 签名，所以本脚本可以在集群外运行；
# 走 NLB 时用 `Host: expensio.test`，不依赖 DNS 解析。
# 依赖：kubectl、curl、jq；凭据只用于取 NLB 地址，不打印任何 token。
set -euo pipefail
export KUBECONFIG=${KUBECONFIG:-$HOME/.kube/ack-expensio.yaml}

LB=$(kubectl -n kube-system get svc nginx-ingress-lb -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')
[ -n "$LB" ] || LB=$(kubectl -n kube-system get svc nginx-ingress-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
BASE=${BASE_URL:-http://$LB}
slug="oss$(date +%s)"
email="$slug@example.com"
pass='Oss-pass-12345'

# 附件白名单只收 JPEG/PNG/WebP/PDF，所以用一张真实的 1x1 PNG，而不是随便一段文本。
payload=/tmp/oss-e2e.png
printf '%s' 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==' | base64 -d > "$payload"
size=$(wc -c < "$payload")

api() {
  local method=$1 path=$2 token=${3:-} body=${4:-}
  local args=(-sS -X "$method" -H 'Host: expensio.test' -H 'Content-Type: application/json')
  [ -n "$token" ] && args+=(-H "Authorization: Bearer $token")
  [ -n "$body" ] && args+=(-d "$body")
  curl "${args[@]}" "$BASE$path"
}

reg=$(api POST /api/v1/auth/register "" \
  "{\"tenant_slug\":\"$slug\",\"tenant_name\":\"OSS $slug\",\"email\":\"$email\",\"password\":\"$pass\",\"display_name\":\"OSS Owner\"}")
token=$(printf '%s' "$reg" | jq -r '.data.access_token // empty')
[ -n "$token" ] || { echo "REGISTER_FAILED: $reg"; exit 1; }
echo "tenant=$slug registered"

created=$(api POST /api/v1/expenses "$token" \
  "{\"category\":\"travel\",\"currency\":\"CNY\",\"amount_minor\":999,\"merchant\":\"OSS Air\",\"description\":\"oss e2e\",\"occurred_at\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}")
expense=$(printf '%s' "$created" | jq -r '.data.id // empty')
[ -n "$expense" ] || { echo "EXPENSE_FAILED: $created"; exit 1; }
echo "expense=$expense"

pre=$(api POST "/api/v1/expenses/$expense/attachments/presign" "$token" \
  "{\"file_name\":\"e2e.png\",\"content_type\":\"image/png\",\"size_bytes\":$size}")
upload=$(printf '%s' "$pre" | jq -r '.data.upload_url // empty')
key=$(printf '%s' "$pre" | jq -r '.data.object_key // empty')
[ -n "$upload" ] || { echo "PRESIGN_FAILED: $pre"; exit 1; }
echo "presign ok object_key=$key"

code=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT \
  -H 'Content-Type: image/png' --data-binary @"$payload" "$upload")
[ "$code" = "200" ] || { echo "UPLOAD_FAILED http=$code"; exit 1; }
echo "browser-style PUT -> $code"

conf=$(api POST "/api/v1/expenses/$expense/attachments" "$token" \
  "{\"object_key\":\"$key\",\"file_name\":\"e2e.png\",\"content_type\":\"image/png\",\"size_bytes\":$size}")
att=$(printf '%s' "$conf" | jq -r '.data.id // empty')
[ -n "$att" ] || { echo "CONFIRM_FAILED: $conf"; exit 1; }
echo "confirmed attachment=$att"

list=$(api GET "/api/v1/expenses/$expense/attachments" "$token")
dl=$(printf '%s' "$list" | jq -r '.data.items[0].download_url // empty')
[ -n "$dl" ] || { echo "LIST_FAILED: $list"; exit 1; }
got=/tmp/oss-e2e-download.png
curl -sS -o "$got" "$dl"
cmp -s "$payload" "$got" || { echo "DOWNLOAD_MISMATCH"; exit 1; }
echo "download verified ($size bytes)"

api DELETE "/api/v1/expenses/$expense/attachments/$att" "$token" >/dev/null
left=$(api GET "/api/v1/expenses/$expense/attachments" "$token" | jq -r '.data.items | length')
[ "$left" = "0" ] || { echo "DELETE_FAILED left=$left"; exit 1; }
echo "ATTACHMENT_E2E: PASS"
