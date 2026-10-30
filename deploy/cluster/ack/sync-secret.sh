#!/usr/bin/env bash
# 把 ~/expensio-ack/creds/<KEY> 合并进命名空间里的 expensio-secrets。
#
# 为什么用 patch 而不是 kubectl create secret：
#   Secret 里已经有 JWT、SMTP、通知内部令牌等键，重建会静默丢掉它们；
#   --from-env-file 又承载不了多行 PEM（RDS 的 CA 链）。merge patch 只改
#   这里列出的键，其余原样保留。
#
# 不打印任何值：只回显键名与字节数。
#
# 用法：
#   bash deploy/cluster/ack/sync-secret.sh [凭据目录]
set -euo pipefail

NS=${NAMESPACE:-expensio}
SECRET=${SECRET:-expensio-secrets}
KUBECTL=${KUBECTL:-kubectl}
DIR=${1:-$HOME/expensio-ack/creds}

[ -d "$DIR" ] || { echo "缺少目录 $DIR" >&2; exit 2; }
chmod 700 "$DIR" 2>/dev/null || true

KEYS=(
  EXPENSIO_DB_APP_PASSWORD
  EXPENSIO_DB_MIGRATOR_PASSWORD
  EXPENSIO_DB_TLS_CA
  EXPENSIO_REDIS_PASSWORD
  EXPENSIO_KAFKA_SASL_PASSWORD
  # 阿里云 Kafka 的 SSL 接入点证书签自私有 CA（CN=AliKafka），必须单独挂载；
  # 少了这个键，chart 在 kafka.tlsCA=true 时会让 Pod 卡在 FailedMount。
  EXPENSIO_KAFKA_TLS_CA
  EXPENSIO_MINIO_ACCESS_KEY
  EXPENSIO_MINIO_SECRET_KEY
)

# 资源是分步开通的，密钥也就会分步到位：默认只同步已就绪的键，
# 缺失的列在末尾作为提醒。STRICT=1 用于"马上要 helm install"前的最终检查。
present=()
missing=()
for k in "${KEYS[@]}"; do
  if [ -s "$DIR/$k" ]; then
    present+=("$k")
  else
    missing+=("$k")
  fi
done

if [ "${#present[@]}" -eq 0 ]; then
  echo "没有任何可同步的键：$DIR 里没有非空文件" >&2
  exit 3
fi

for k in "${present[@]}"; do
  case "$k" in
    *TLS_CA) ;; # PEM 末尾的换行无害，x509 会忽略空白
    *)
      # 口令文件末尾的换行会被当作口令的一部分，是最常见的"明明填对了却认证失败"。
      if [ -z "$(tail -c1 "$DIR/$k")" ]; then
        echo "警告：$k 末尾有换行，会被当成口令的一部分" >&2
      fi
      ;;
  esac
done

if [ "${#missing[@]}" -gt 0 ]; then
  echo "未写入（文件缺失或为空）：${missing[*]}" >&2
  if [ "${STRICT:-0}" = "1" ]; then
    echo "STRICT=1：要求全部键就绪，已中止" >&2
    exit 3
  fi
fi

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
{
  printf '{"data":{'
  first=1
  for k in "${present[@]}"; do
    if [ "$first" -eq 1 ]; then first=0; else printf ','; fi
    printf '"%s":"%s"' "$k" "$(base64 -w0 <"$DIR/$k")"
  done
  printf '}}'
} >"$tmp"

$KUBECTL -n "$NS" get secret "$SECRET" >/dev/null 2>&1 \
  || $KUBECTL -n "$NS" create secret generic "$SECRET" >/dev/null

$KUBECTL -n "$NS" patch secret "$SECRET" --type=merge --patch-file="$tmp" >/dev/null

echo "已更新 $NS/$SECRET，本次写入的键与大小："
for k in "${present[@]}"; do
  printf '  %-32s %6s bytes\n' "$k" "$(wc -c <"$DIR/$k")"
done
echo "依赖这些键的 Pod 需要重启才会读到新值："
echo "  kubectl -n $NS rollout restart deploy"
