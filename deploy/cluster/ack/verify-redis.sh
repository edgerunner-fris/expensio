#!/usr/bin/env bash
# 从集群内部验证 Tair（兼容 Redis）：网络、认证、读写、以及"认证确实生效"。
#
# 为什么单独一个脚本：Redis 的失败模式与 PostgreSQL 不同——白名单不通表现为
# 连接超时，开了 SSL 表现为握手失败，口令错误表现为 NOAUTH/WRONGPASS，而
# 这三者在应用侧都只是"启动或请求失败"。这里把三种原因分开断言。
#
# 口令只以文件形式挂进临时 Pod，并通过 REDISCLI_AUTH 环境变量传给 redis-cli，
# 不出现在进程参数（kubectl exec 的 argv 会留在审计日志里）。
#
# 用法：
#   HOST=r-xxxx.redis.rds.aliyuncs.com bash deploy/cluster/ack/verify-redis.sh
#   HOST=... PORT=6379 NAMESPACE=expensio bash deploy/cluster/ack/verify-redis.sh
#
# 前置：expensio-secrets 里已有 EXPENSIO_REDIS_PASSWORD。
set -uo pipefail

NS=${NAMESPACE:-expensio}
SECRET=${SECRET:-expensio-secrets}
HOST=${HOST:?请设置 HOST=<Tair 内网地址>}
PORT=${PORT:-6379}
IMAGE=${IMAGE:-redis:7.4.11-alpine}
POD=${POD:-redis-verify}
KUBECTL=${KUBECTL:-kubectl}

fail=0
say() { printf '%s\n' "$*"; }
bad() { printf 'FAIL: %s\n' "$*" >&2; fail=1; }

# 在临时 Pod 里执行一段 shell；口令从挂载的文件读入 REDISCLI_AUTH。
# rc() 统一加 timeout：白名单没放行时 TCP 会挂到内核超时，不加会让脚本
# 看起来"卡死"，而真实原因只是连接被丢包。
rexec() {
  $KUBECTL -n "$NS" exec -i "$POD" -- sh -s 2>&1 <<EOF
export REDISCLI_AUTH="\$(cat /secrets/redis)"
rc() { timeout 15 redis-cli -h '$HOST' -p $PORT --no-auth-warning "\$@"; }
$1
EOF
}

cleanup() {
  $KUBECTL -n "$NS" delete pod "$POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

$KUBECTL -n "$NS" delete pod "$POD" --ignore-not-found >/dev/null 2>&1 || true

say "== 起临时 Pod $POD（镜像 $IMAGE）=="
cat <<EOF | $KUBECTL -n "$NS" apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $POD
  labels:
    app.kubernetes.io/name: redis-verify
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 999
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: redis-cli
      image: $IMAGE
      command: ["sleep", "1800"]
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: creds
          mountPath: /secrets
          readOnly: true
        - name: tmp
          mountPath: /tmp
  volumes:
    - name: creds
      secret:
        secretName: $SECRET
        items:
          - key: EXPENSIO_REDIS_PASSWORD
            path: redis
    - name: tmp
      emptyDir: {}
EOF

if ! $KUBECTL -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=180s >/dev/null; then
  $KUBECTL -n "$NS" describe pod "$POD" | tail -20
  bad "临时 Pod 未就绪（镜像拉取失败？换 IMAGE= 指向 ACR 的同版本镜像）"
  exit 1
fi
say "Pod 就绪"

say
say "== 1. DNS 与 TCP 连通（白名单是否放行这个网段）=="
out=$(rexec "getent hosts '$HOST' | head -1")
if [ -n "$out" ] && ! printf '%s' "$out" | grep -q '^FAIL'; then
  say "  DNS：$out"
else
  bad "Pod 内无法解析 $HOST"
fi

say
say "== 2. 认证与 PING =="
out=$(rexec "rc PING")
ping_out=$(printf '%s' "$out" | tr -d '[:space:]')
if [ "$ping_out" = "PONG" ]; then
  say "  PING -> PONG"
else
  # timeout(1) 杀掉 redis-cli 时只剩 "Terminated"，拿不到它自己的报错；
  # 这也是"TCP 被丢弃"的典型表现。
  if printf '%s' "$out" | grep -qiE 'timed out|could not connect|connection refused|terminated'; then
    bad "连接失败（TCP 无响应）：$out"
    say "  常见原因，按可能性排序："
    say "   1) Tair 白名单未放行集群网段——把 10.75.0.0/16 加入实例白名单；"
    say "   2) 实例开启了 SSL——本项目的 Redis 客户端不支持 TLS，必须关闭；"
    say "   3) 实例与集群不在同一 VPC（需要云企业网/对等连接）。"
  else
    bad "PING 失败（认证或协议层）：$out"
    say "  口令来自 Secret 的 EXPENSIO_REDIS_PASSWORD；确认末尾没有换行、"
    say "  与 Tair 控制台设置的密码一致。"
  fi
  # 连不上或认证不过，后面的诊断都无意义，且会打印误导性的 FAIL。
  say "跳过 3–5：实例不可用或口令错误"
  exit 1
fi

say
say "== 3. 实例信息（版本、角色）=="
out=$(rexec "rc INFO server | grep -E '^redis_version:|^redis_mode:' | tr -d '\r'")
say "  $out"
out=$(rexec "rc INFO replication | grep -E '^role:' | tr -d '\r'")
say "  $out"

say
say "== 4. 负例：不认证必须被拒（证明口令真的生效）=="
out=$($KUBECTL -n "$NS" exec -i "$POD" -- sh -s 2>&1 <<EOF
unset REDISCLI_AUTH
timeout 15 redis-cli -h '$HOST' -p $PORT PING
EOF
)
if printf '%s' "$out" | grep -qE 'NOAUTH|Authentication required'; then
  say "  无口令 -> $(printf '%s' "$out" | head -1)"
else
  bad "实例允许无口令访问（或报错不是 NOAUTH）：$out"
fi

out=$($KUBECTL -n "$NS" exec -i "$POD" -- sh -s 2>&1 <<EOF
export REDISCLI_AUTH=expensio-definitely-wrong
timeout 15 redis-cli -h '$HOST' -p $PORT --no-auth-warning PING
EOF
)
if printf '%s' "$out" | grep -qiE 'WRONGPASS|invalid password|AUTH failed'; then
  say "  错误口令 -> $(printf '%s' "$out" | head -1)"
else
  bad "错误口令未被拒（或报错不是认证失败）：$out"
fi

say
say "== 5. 读写与 TTL =="
key="expensio:verify:$(date +%s)-$$"
set_out=$(rexec "rc SET '$key' ok EX 60" | tr -d '[:space:]')
get_out=$(rexec "rc GET '$key'" | tr -d '[:space:]')
ttl=$(rexec "rc TTL '$key'" | tr -d '[:space:]')
rexec "rc DEL '$key'" >/dev/null
if [ "$set_out" = "OK" ] && [ "$get_out" = "ok" ] && [ "$ttl" -gt 0 ] 2>/dev/null; then
  say "  SET/GET/TTL -> OK/ok/ttl=${ttl}s，key 已删除"
else
  bad "读写失败：SET=$set_out GET=$get_out TTL=$ttl"
fi

say
if [ "$fail" -eq 0 ]; then
  say "REDIS_VERIFY: PASS"
else
  say "REDIS_VERIFY: FAIL"
fi
exit "$fail"
