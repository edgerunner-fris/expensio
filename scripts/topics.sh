#!/usr/bin/env bash
# 创建业务事件主题。broker 的自动建主题只对旧客户端的 produce 请求生效，
# franz-go 先取元数据，不会触发 auto-create；缺主题时投递器会一直重试。
# 本脚本幂等：主题已存在时 --if-not-exists 不会报错。
set -euo pipefail

COMPOSE=${COMPOSE:-docker compose}

for topic in \
  expensio.expense.events.v1 \
  expensio.expense.events.v1.dlq \
  expensio.billing.events.v1 \
  expensio.billing.events.v1.dlq
do
  $COMPOSE exec -T kafka /opt/kafka/bin/kafka-topics.sh \
    --bootstrap-server localhost:9092 \
    --create --if-not-exists \
    --topic "$topic" \
    --partitions 6 \
    --replication-factor 1
done
