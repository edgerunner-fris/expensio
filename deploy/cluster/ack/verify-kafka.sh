#!/usr/bin/env bash
# 从集群内部验证 ApsaraMQ for Kafka：VPC 接入点、SASL 认证、主题与消费组。
#
# 为什么不在宿主机上验：接入点是 VPC 地址，白名单/分组是按 vSwitch 网段放的；
# 而且 SASL 口令必须以文件形式进 Pod，用 --command-config 传给 CLI，不能出现
# 在命令行参数里（kubectl exec 的 argv 会进审计日志）。
#
# 这里不往业务主题写探测消息：消费端会把任何消息当成事件解析，一条脏消息
# 会进 DLQ 甚至阻塞。生产/消费的真实证明交给 smoke（提交费用 → 事件 →
# report 读模型）。本脚本只在能临时建主题时做一次自环，否则跳过。
#
# 阿里云 Kafka 的接入点有三种，选错了表现都不同：
#   9092 明文            —— 能连，但事件与口令都裸奔；
#   9094 SASL_PLAINTEXT  —— 认证能过，但口令明文过网，项目配置层拒绝；
#   9095 SSL             —— 服务器证书签自私有 CA（CN=AliKafka），
#                           必须把控制台下载的 CA 挂进来，否则 PKIX 校验失败。
# 默认按 SASL_SSL 验；TLS_CA=1 时挂 EXPENSIO_KAFKA_TLS_CA 作为信任根。
#
# 用法：
#   BROKERS=a:9095,b:9095 SASL_USERNAME=expensio_test TLS_CA=1 \
#     bash deploy/cluster/ack/verify-kafka.sh
#   # 只想看认证是否通（会公开口令，仅限排查）：
#   PROTOCOL=SASL_PLAINTEXT ... bash deploy/cluster/ack/verify-kafka.sh
#
# 前置：expensio-secrets 里已有 EXPENSIO_KAFKA_SASL_PASSWORD；
#       TLS_CA=1 时还要有 EXPENSIO_KAFKA_TLS_CA。
set -uo pipefail

NS=${NAMESPACE:-expensio}
SECRET=${SECRET:-expensio-secrets}
BROKERS=${BROKERS:?请设置 BROKERS=<a:9095,b:9095,...>}
SASL_USERNAME=${SASL_USERNAME:?请设置 SASL_USERNAME=<用户名>}
MECHANISM=${MECHANISM:-PLAIN}
PROTOCOL=${PROTOCOL:-SASL_SSL}
TLS_CA=${TLS_CA:-0}
TLS_CA_KEY=${TLS_CA_KEY:-EXPENSIO_KAFKA_TLS_CA}
IMAGE=${IMAGE:-apache/kafka:3.9.2}
POD=${POD:-kafka-verify}
KUBECTL=${KUBECTL:-kubectl}
TOPICS=${TOPICS:-"expensio.expense.events.v1 expensio.expense.events.v1.dlq expensio.billing.events.v1 expensio.billing.events.v1.dlq"}
# 不要用变量名 GROUPS：它是 bash 的只读内置变量（当前用户组 ID），赋值会被
# 忽略，循环会变成 "1000"。
CONSUMER_GROUPS=${CONSUMER_GROUPS:-"report-expense-v1 notification-expense-v1 notification-billing-v1"}

fail=0
say() { printf '%s\n' "$*"; }
bad() { printf 'FAIL: %s\n' "$*" >&2; fail=1; }

cleanup() {
  $KUBECTL -n "$NS" delete pod "$POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

$KUBECTL -n "$NS" delete pod "$POD" --ignore-not-found >/dev/null 2>&1 || true

# 私有 CA 才需要额外挂载；公有 CA 由 JDK cacerts 覆盖。
if [ "$TLS_CA" = "1" ]; then
  ca_mount='        - name: ca
          mountPath: /certs
          readOnly: true'
  ca_volume="    - name: ca
      secret:
        secretName: $SECRET
        items:
          - key: $TLS_CA_KEY
            path: ca.pem"
else
  ca_mount=""
  ca_volume=""
fi

say "== 起临时 Pod $POD（镜像 $IMAGE，PROTOCOL=$PROTOCOL，TLS_CA=$TLS_CA）=="
cat <<EOF | $KUBECTL -n "$NS" apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $POD
  labels:
    app.kubernetes.io/name: kafka-verify
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: kafka
      image: $IMAGE
      command: ["sleep", "1800"]
      env:
        - name: KAFKA_SASL_PASSWORD
          valueFrom:
            secretKeyRef:
              name: $SECRET
              key: EXPENSIO_KAFKA_SASL_PASSWORD
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: tmp
          mountPath: /tmp
$ca_mount
  volumes:
    - name: tmp
      emptyDir: {}
$ca_volume
EOF

if ! $KUBECTL -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=180s >/dev/null; then
  $KUBECTL -n "$NS" describe pod "$POD" | tail -20
  bad "临时 Pod 未就绪（镜像拉取失败？换 IMAGE= 指向 ACR 的同版本镜像）"
  exit 1
fi
say "Pod 就绪"

# 在 Pod 里执行一段 shell；口令只在环境变量与 /tmp/client.properties 里。
kexec() {
  $KUBECTL -n "$NS" exec -i "$POD" -- sh -s 2>&1 <<EOF
export KAFKA_SASL_PASSWORD="\$(printenv KAFKA_SASL_PASSWORD)"
export BOOTSTRAP='$BROKERS'
export SASL_USERNAME='$SASL_USERNAME'
export MECH='$MECHANISM'
export PROTO='$PROTOCOL'
# apache/kafka 镜像的 PATH 里没有 /opt/kafka/bin（topics-job 也是写绝对路径）。
export PATH="/opt/kafka/bin:\$PATH"
case "\$MECH" in
  PLAIN|plain) CLASS=org.apache.kafka.common.security.plain.PlainLoginModule ;;
  *)           CLASS=org.apache.kafka.common.security.scram.ScramLoginModule ;;
esac
printf 'security.protocol=%s\nsasl.mechanism=%s\nsasl.jaas.config=%s required username="%s" password="%s";\n' \\
  "\$PROTO" "\$MECH" "\$CLASS" "\$SASL_USERNAME" "\$KAFKA_SASL_PASSWORD" > /tmp/client.properties
if [ -f /certs/ca.pem ]; then
  {
    printf 'ssl.truststore.type=PEM\n'
    printf 'ssl.truststore.location=/certs/ca.pem\n'
  } >> /tmp/client.properties
fi
$1
EOF
}

say
say "== 1. 接入点 DNS 解析 =="
for b in ${BROKERS//,/ }; do
  host=${b%:*}
  out=$(kexec "getent hosts $host | head -1")
  if [ -n "$out" ]; then
    say "  $out"
  else
    bad "无法解析 $host"
  fi
done

say
say "== 2. $PROTOCOL 认证（kafka-broker-api-versions）=="
out=$(kexec "timeout 90 kafka-broker-api-versions.sh --bootstrap-server \"\$BOOTSTRAP\" --command-config /tmp/client.properties 2>&1 | head -8")
if printf '%s' "$out" | grep -qE 'id: [0-9]+'; then
  say "$out"
  say "  认证通过，可用 broker 数：$(printf '%s' "$out" | grep -cE 'id: [0-9]+')"
else
  bad "连接或认证失败：$out"
  say "  按报错区分原因："
  say "   - PKIX path building / SSL handshake：服务器证书不被信任，"
  say "     需要控制台下载 CA 并用 TLS_CA=1 挂载（阿里云证书签自 CN=AliKafka 私有 CA）；"
  say "   - terminated during authentication：端口不是 TLS 端口（9094 是 SASL_PLAINTEXT）；"
  say "   - SaslAuthenticationException / Authentication failed：用户名/口令/机制不对。"
  say "跳过后续检查"
  exit 1
fi

say
say "== 3. 负例：错误口令必须被拒 =="
out=$(kexec "sed 's/password=\".*\";/password=\"expensio-definitely-wrong\";/' /tmp/client.properties > /tmp/bad.properties
timeout 60 kafka-broker-api-versions.sh --bootstrap-server \"\$BOOTSTRAP\" --command-config /tmp/bad.properties 2>&1 | head -4")
if printf '%s' "$out" | grep -qiE 'authentication|sasl|failed'; then
  say "  错误口令 -> $(printf '%s' "$out" | head -1)"
else
  bad "错误口令未被拒：$out"
fi

say
say "== 4. 主题（控制台里预建，chart 不自动建）=="
out=$(kexec "timeout 60 kafka-topics.sh --bootstrap-server \"\$BOOTSTRAP\" --command-config /tmp/client.properties --list 2>&1")
printf '%s\n' "$out" | sed 's/^/  /'
for t in $TOPICS; do
  if printf '%s\n' "$out" | grep -qx "$t"; then
    say "  $t: 存在"
  else
    bad "$t: 不存在"
  fi
done

say
say "== 5. 消费组 =="
out=$(kexec "timeout 60 kafka-consumer-groups.sh --bootstrap-server \"\$BOOTSTRAP\" --command-config /tmp/client.properties --list 2>&1")
printf '%s\n' "$out" | sed 's/^/  /'
for g in $CONSUMER_GROUPS; do
  if printf '%s\n' "$out" | grep -qx "$g"; then
    say "  $g: 存在"
  else
    # 消费组首次 join 才会出现；这里只提示，不判失败。
    say "  $g: 尚未创建（消费者启动后自动创建，无需失败）"
  fi
done

say
say "== 6. 自环（仅当允许临时建主题时）=="
ts=$(date +%s)
topic="expensio.verify.$ts"
out=$(kexec "timeout 60 kafka-topics.sh --bootstrap-server \"\$BOOTSTRAP\" --command-config /tmp/client.properties --create --topic $topic --partitions 1 --replication-factor 1 2>&1")
if printf '%s' "$out" | grep -qi 'created topic'; then
  producer=$(kexec "printf 'ping-$ts\n' | timeout 60 kafka-console-producer.sh --bootstrap-server \"\$BOOTSTRAP\" --command-config /tmp/client.properties --topic $topic 2>&1")
  consumer=$(kexec "timeout 60 kafka-console-consumer.sh --bootstrap-server \"\$BOOTSTRAP\" --command-config /tmp/client.properties --topic $topic --from-beginning --max-messages 1 --timeout-ms 30000 2>&1")
  kexec "timeout 60 kafka-topics.sh --bootstrap-server \"\$BOOTSTRAP\" --command-config /tmp/client.properties --delete --topic $topic" >/dev/null
  if printf '%s' "$consumer" | grep -q "ping-$ts"; then
    say "  临时主题 $topic：produce/consume 通过，已删除"
  else
    bad "临时主题自环失败：producer=$producer consumer=$consumer"
  fi
else
  say "  实例不允许客户端建主题，跳过（生产/消费由 smoke 证明）：$(printf '%s' "$out" | head -1)"
fi

say
if [ "$fail" -eq 0 ]; then
  say "KAFKA_VERIFY: PASS"
else
  say "KAFKA_VERIFY: FAIL"
fi
exit "$fail"
