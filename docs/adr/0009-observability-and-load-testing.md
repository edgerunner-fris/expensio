# ADR-0009：可观测性链路与压测验收

- 状态：已接受（M7）
- 日期：2026-10-27

## 背景

M0–M6 只有 JSON 日志、健康探针和优雅关闭。服务之间（HTTP、Kafka、
outbox、邮件）一旦变慢或失败，排障只能靠 `docker compose logs` 拼时间线。
M7 需要一条能回答三个问题的链路：

1. 某个请求慢在哪一跳？
2. 异步链路（outbox → Kafka → 消费者）有没有断，断了多少？
3. 哪些故障应该在用户投诉前主动告警？

约束仍然是：不得把 token、密码、完整卡号写进日志或 span；遥测故障不能让
业务请求失败；本地无 Collector 时服务必须照常启动。

## 决策

### 1. 统一用 OpenTelemetry SDK，服务只导出 OTLP

六个 Go 服务和管理台都通过 `internal/platform/telemetry` 接入 OTel SDK，
用 OTLP/gRPC 发往 Collector，不直接依赖 Prometheus 客户端库，也不在各服务
里起 `/metrics`：

- trace 与 metric 共用一套资源属性（`service.name`、`service.version`、
  `deployment.environment`），Collector 的 `resource_to_telemetry_conversion`
  把它们变成 Prometheus 标签；
- trace 由 Collector 转 Tempo，指标由 Collector 的 Prometheus exporter
  在 `:8889` 暴露，Prometheus 主动抓取；
- 服务侧只需知道一个 `host:port`，后端换成 OTLP 兼容的 SaaS 时不用改代码。

`otel.enabled` 默认 `false`：本地裸跑、单元测试不需要先起 Collector。
开启后 endpoint 必须是 `host:port`（不带 scheme），启动期校验，避免把错误
留到第一次导出才暴露。

### 2. 指标名与标签保持低基数

指标统一 `expensio.` 前缀，关键指标：

| 指标 | 类型 | 关键标签 |
| --- | --- | --- |
| `expensio.http.server.requests` | counter | `http.route`、`http.request.method`、`http.response.status_code` |
| `expensio.http.server.duration` | histogram | 同上 |
| `expensio.http.server.active_requests` | up/down counter | `http.request.method` |
| `expensio.kafka.producer.records` / `consumer.records` | counter | `messaging.destination.name`、`outcome` |
| `expensio.kafka.producer.duration` / `consumer.duration` | histogram | 同上 |
| `expensio.outbox.events.claimed` / `delivered` | counter | `event_type`、`outcome` |
| `expensio.notification.emails` | counter | `outcome`、`final` |
| `expensio.gateway.rate_limit.decisions` | counter | `scope`、`outcome`（allowed/denied/error） |
| `expensio.db.pool.*` | gauge/counter | 服务级 |

`http.route` 用 Gin 的 `FullPath()`（如 `/api/v1/expenses/:id`），不是原始
URL；管理台按前缀归并为 `api` / `index` / `spa`。健康检查不产生 span 与
指标，避免探针把请求数和延迟稀释掉。

HTTP 延迟直方图显式设置 `5ms–10s` 边界。OTel 默认边界从 5s 起跳，对毫秒级
接口的 P95 没有任何分辨率；Kafka 生产/消费延迟同理，使用 `1ms–10s` 边界。
修改边界会让同一指标在滚动发布期间短暂出现两组 `le` 序列，Prometheus 的
`rate` 窗口滑过旧序列后自然收敛。

### 3. 传播：HTTP 用 W3C，Kafka 用记录头

- 入站 HTTP 从 `traceparent` 提取上下文；gateway 与管理台的反代传输也包了
  `otelhttp.Transport`，因此 `web → gateway → 下游` 是一条 trace。
- Kafka 生产时把 `traceparent` 写进记录头，消费时提取；重试循环整体处于
  同一个 consumer span 内，重试次数不会制造新的 trace。
- **已知断点：outbox。** 业务事务只把事件写进数据库，trace 上下文没有持久化；
  投递器是在另一个 goroutine 里重新开始 span。因此
  `HTTP 请求 → outbox → 投递器` 这一段不连续，但 `投递器 → Kafka → 消费者`
  是连续的（producer span 是 consumer span 的父级）。

### 4. 采样与保留

本地 Collector 使用 `ParentBased(AlwaysSample)`：样本量小，优先保证联调时
每次都能查到 trace。生产环境应在 Collector 侧改为尾部采样或按比例采样，
不在服务里写死采样率；同时把 Tempo 的本地文件后端替换为对象存储，并设置
与查询需求匹配的保留期。本地 Tempo 只保留 24 小时，Prometheus 使用默认
保留策略，两者都不适合当长期审计日志。

### 5. 告警只覆盖"能行动"的故障

`deploy/local/prometheus/rules.yml` 定义了 8 条规则：服务缺失、5xx 比例、
P95 延迟、Kafka 生产失败、outbox 投递失败、邮件失败、限流器 fail-open、
连接池饱和。全部使用持续窗口（5–15 分钟）而不是瞬时尖峰，避免一次重试触发
告警。限流器单独计数是因为 429 只能说明"拒绝过"，无法区分 Redis 故障时的
fail-open——那会让全站配额保护静默失效。

告警表达式依赖 OTel exporter 的命名转换（点变下划线、counter 加 `_total`、
单位 `s` 变 `_seconds`）。改指标名必须同步规则与看板，否则会得到"永远不
触发"的规则——这类错误不会被单元测试发现，只能靠联调时实际查询验证。

### 6. 压测用 k6，结果必须来自实跑

`load/k6/api.js` 在 setup 阶段注册租户并创建预算，正文按
创建费用 → 提交 → 取消 → 列表 → 报表汇总 → 通知列表循环，阈值：

- `http_req_failed rate < 1%`
- `http_req_duration p(95) < 500ms`
- `checks rate > 99%`

每次迭代 sleep 1.5s，避免打满租户级 1200/min 限流后把 429 误判成性能问题。
压测结论只记录 k6 与 Prometheus 的实际输出，不引用外部基准或估算值。

## 后果

- Collector 是单点：它挂掉时业务不受影响（SDK 异步导出、失败丢弃），但会
  丢遥测。生产应至少两副本，并用节点级 agent 或 DaemonSet 收敛出口。
- Collector 的 Prometheus exporter 是拉取端点，Prometheus 必须能访问它；
  不能跨网络时改用 remote write。
- 日志仍未进入 Loki：trace 与日志目前靠 `trace_id` 手工关联，M8 未评估，M9 再评估。
- outbox 断点意味着跨异步边界的端到端耗时无法用一条 trace 直接计算，需要
  用 outbox 的 `created_at` / `delivered_at` 指标补足。
- 采样率、Tempo 保留期、告警阈值都是本地演示值，不是生产 SLO。

## 后续

1. 把 trace 上下文随 outbox 事件持久化（新增列），投递时恢复父级；
2. 生产 Collector 双副本 + 尾部采样，Tempo 接对象存储；
3. 为告警补 runbook 链接与 `for` 窗口的容量依据；
4. 评估 Loki 与 trace 的关联，以及前端 RUM。
