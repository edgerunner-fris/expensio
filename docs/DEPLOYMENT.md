# 部署

本文只描述当前真实可执行的路径。K8s 部分在本地做过 `helm lint` 与
`helm template` 验证，但**尚未在真实集群上 apply**；这项状态记录在
`docs/PROGRESS.md` 的风险清单里，不在这里含糊带过。

## 本地整栈（Docker Compose）

```bash
make env       # 首次：从 .env.example 生成 .env 并填入随机密钥
make app-up    # 构建六个服务 + 管理台 + 迁移/建主题任务并启动
```

启动后：

- 管理台 <http://127.0.0.1:5173/>
- 网关   <http://127.0.0.1:8080/>
- Mailpit 收件箱 <http://127.0.0.1:8025/>

`make app-up` 会执行：

1. `migrate` 一次性 Job：用 `{service}_migrator` 对五个业务库执行迁移；
2. `topics` 一次性 Job：创建 expense / billing 主题与 DLQ；
3. 六个服务按依赖顺序启动，全部通过 `/healthz` 健康检查；
4. `web` 最后启动，把 `/api` 反代到 gateway。

所有应用容器以 UID 10001 运行，根文件系统只读，丢弃全部 capability，并设置
`no-new-privileges`（`docker-compose.yml` 的 `x-app-service`）。服务不写本地
磁盘；需要排查时用 `docker compose logs`，不要在容器内落文件。

停止：`make app-down`。只看依赖不跑容器：`make up` + `make run-<service>`。

### 密钥约束

- `.env` 由 `scripts/env.sh` 生成，权限 600，已被 `.gitignore` 忽略。
- 迁移口令会拼进 `postgres://` 连接串，只允许 `[0-9a-f]`；脚本生成的值满足。
  生产 Secret 里的迁移口令同样必须 URL 安全，否则请改用 `EXPENSIO_DB_DSN`。
- 重新生成 `.env` 不会修改已经初始化的数据库角色。轮换口令需要
  `ALTER ROLE`，或删除 `pgdata` 卷后重建。

### app profile 的端口

只有 gateway 与 web 发布到宿主，且绑定 `127.0.0.1`：

| 服务 | 宿主端口 | 容器端口 |
| --- | --- | --- |
| gateway | 8080 | 8080 |
| web | 5173 | 8086 |

其余服务只在 compose 网络内可达。

### 可观测性（本地）

`app` profile 会同时启动 OTel Collector、Tempo、Prometheus 与 Grafana：

| 组件 | 地址 | 用途 |
| --- | --- | --- |
| Grafana | <http://127.0.0.1:3000/> | 概览看板 `Expensio Overview`，数据源 Prometheus + Tempo |
| Prometheus | <http://127.0.0.1:9090/> | 指标查询与 7 条告警规则 |
| Tempo | <http://127.0.0.1:3200/> | trace 查询（本地文件后端，保留 24h） |
| OTLP gRPC | `127.0.0.1:4317` | 服务导出口；Collector 指标端点 `:8889` 只在 compose 网络内 |

Grafana 管理员口令来自 `.env` 的 `GRAFANA_ADMIN_PASSWORD`，不要提交或写进文档。
Grafana 已预置 Prometheus（UID `expensio-prometheus`）与 Tempo（UID
`expensio-tempo`）数据源；看板 UID 为 `expensio-overview`。

压测对已启动的整栈执行：

```bash
make load K6_DURATION=30s
```

k6 会注册临时租户、创建预算并循环执行业务接口；结果只以实际输出为准。
Collector / Tempo / Prometheus / Grafana 的配置在 `deploy/local/` 下，
修改后需 `docker compose --profile app up -d --force-recreate <服务>`。

## Kubernetes（Helm）

Chart 位于 `deploy/helm/expensio`。它**不部署** PostgreSQL / Redis / Kafka，
也**不创建 Secret**：这些由平台提供。

### 1. 准备 Secret

```bash
# Ed25519 密钥对：私钥给 identity 签发，公钥给其余五个服务验签。
# DER 的末 32 字节分别是种子与公钥，前面是固定前缀。
openssl genpkey -algorithm ED25519 -out jwt.pem
JWT_PRIVATE_KEY=$(openssl pkey -in jwt.pem -outform DER | tail -c 32 | base64 -w0)
JWT_PUBLIC_KEY=$(openssl pkey -in jwt.pem -pubout -outform DER | tail -c 32 | base64 -w0)
rm -f jwt.pem   # 私钥只应进入 Secret，不要留在工作目录

kubectl -n expensio create secret generic expensio-secrets \
  --from-literal=EXPENSIO_JWT_PRIVATE_KEY="$JWT_PRIVATE_KEY" \
  --from-literal=EXPENSIO_JWT_PUBLIC_KEYS="$JWT_PUBLIC_KEY" \
  --from-literal=EXPENSIO_DB_APP_PASSWORD='...' \
  --from-literal=EXPENSIO_DB_MIGRATOR_PASSWORD='...' \
  --from-literal=EXPENSIO_BILLING_WEBHOOK_SECRET="$(openssl rand -hex 32)" \
  --from-literal=EXPENSIO_NOTIFICATION_INTERNAL_TOKEN="$(openssl rand -hex 32)" \
  --from-literal=EXPENSIO_NOTIFICATION_SMTP_PASSWORD='...'
```

必需键见 `values.yaml` 的 `secretKeys`。`EXPENSIO_JWT_SECRET`（HS256）在
迁移期是可选的：提供它时 identity 会同时接受旧的 HS256 令牌，其余服务
只接受 EdDSA 且不持有 HMAC 密钥；一个 access TTL 之后可以移除该键。
SMTP 密码同样是可选键：没有认证的 SMTP 可以不提供，但
`notification.emailEnabled=true` 时必须保证 SMTP 可用。

轮换公钥时把新旧两个公钥用逗号拼进 `EXPENSIO_JWT_PUBLIC_KEYS`，等所有
Pod 重启完成后再切私钥，最后移除旧公钥；`kid` 是公钥的 RFC 7638 指纹，
验签端按 `kid` 选钥，不依赖顺序。

### 2. 覆盖 values

至少覆盖以下与环境相关的值：

```yaml
images:
  registry: ghcr.io/<org>/expensio
  tag: "<git-sha>"
  migrate: ghcr.io/<org>/expensio-migrate

database:
  host: pg.internal
  port: 5432
  sslmode: require

redis:
  addr: redis.internal:6379
kafka:
  brokers: kafka.internal:9092

otel:
  # Chart 不部署 Collector；地址必须由平台提供且集群内可达。
  enabled: true
  endpoint: otel-collector.observability.svc.cluster.local:4317
  insecure: true

notification:
  smtpAddr: smtp.internal:587
  smtpFrom: no-reply@example.com
  smtpUseTLS: true

invite:
  # 邀请邮件里链接的公开管理台地址，必须是浏览器可达的 https 地址；
  # 不要填集群内 Service 名。ttl 允许 1h–720h。
  baseURL: https://expensio.example.com
  ttl: 168h

ingress:
  host: expensio.example.com
  tls:
    secretName: expensio-tls
```

写入 `my-values.yaml`，不要提交真实地址与密钥。

启用 `otel.enabled` 前，集群里必须已有一套接收 OTLP/gRPC 的 Collector，
并且满足以下条件（Chart 不会替你创建）：

- 六个服务 Deployment 与管理台能出站访问 `otel.endpoint`；
- Collector 的指标端点（Prometheus exporter）能被集群 Prometheus 抓取，
  否则 trace 有数据但指标为空；
- trace 后端（Tempo/Jaeger）与指标后端（Prometheus）已配置保留期与采样；
- Collector 内存限制与采样率按生产流量设置，本地配置的 `256 MiB` 只适用于演示。

关闭时把 `otel.enabled` 设为 `false`，服务使用 noop provider，不会阻塞启动。

### 3. 安装

```bash
helm upgrade --install expensio deploy/helm/expensio \
  -n expensio --create-namespace \
  -f my-values.yaml \
  --wait --timeout 10m
```

`--wait` 会等待 hook Job（迁移、建主题）与 Deployment 就绪。升级时 hook
先于新 Pod 执行；迁移失败会阻止滚动发布。

### 4. 验证

```bash
kubectl -n expensio get jobs,pods
kubectl -n expensio logs job/expensio-migrate
kubectl -n expensio port-forward svc/expensio-web 8086:8086
curl -fsS http://127.0.0.1:8086/healthz
```

## Chart 的安全默认值

- 全部容器 `runAsNonRoot`、UID 10001、`readOnlyRootFilesystem`、
  `allowPrivilegeEscalation=false`、drop ALL capabilities；
- `automountServiceAccountToken=false`；
- 默认拒绝入站，仅 ingress controller 可访问 gateway/web，同 release 的
  Pod 之间可互访（`networkPolicy.enabled`）；
- gateway 与 web 各 2 副本并配置 `minAvailable: 1` 的 PDB；
- 探针区分 `/healthz`（liveness，不碰下游）与 `/readyz`（readiness）。

## 已知限制

- **未在真实集群验证**：本机没有 kind/k3s 或可用集群，`helm template` 只能
  证明模板可渲染，不能证明 RBAC、NetworkPolicy、Ingress Controller 的实际
  行为。上集群前必须按上面的验证步骤跑一遍。
- **出站未做白名单**：NetworkPolicy 只限制入站。数据库/Kafka/SMTP 地址由
  平台提供，写死 CIDR 会让 chart 无法跨环境复用；需要时由集群级策略补足。
- **HSTS**：web 在 Ingress 终止 TLS 时看不到 `r.TLS`，因此 HSTS 应由
  Ingress 配置添加（例如 nginx 的 `server-snippet`）。
- **单地域**：没有多集群、跨区复制与故障转移设计。
- **观测后端不在 Chart 内**：Collector、Prometheus、Grafana、Tempo 均由平台
  提供；Chart 只负责把 OTLP 地址注入服务。没有这些后端时 trace/指标会丢失，
  但业务请求不受影响。
