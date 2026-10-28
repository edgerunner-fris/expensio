# 部署

本文只描述当前真实可执行的路径。K8s 部分除 `helm lint`、`helm template`
与 kubeconform（K8s 1.31 schema，31/31 valid）外，已在真实 k3d 集群
（k3s v1.31.5，双节点）完成 `helm upgrade --install --wait` 与行为验收：
迁移/建主题 Hook 实际执行、Ingress 可达、NetworkPolicy 放行与拒绝符合预期、
ServiceAccount 无 API 权限、事件链路在集群内跑通。验收清单与 smoke 脚本
见 [`deploy/cluster/acceptance/`](../deploy/cluster/acceptance/)，实测输出见
`docs/PROGRESS.md` 的 M14。HPA 与 cert-manager 只做了模板渲染与 schema
校验，托管云集群、ACME 签发与真实负载下的扩缩容仍未验证，见文末
「已知限制」。这些状态也记录在 `docs/PROGRESS.md` 的风险清单里，不在这里
含糊带过。

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
| Grafana | <http://127.0.0.1:3000/> | 概览看板 `Expensio Overview`，数据源 Prometheus + Tempo + Loki |
| Prometheus | <http://127.0.0.1:9090/> | 指标查询与 7 条告警规则 |
| Tempo | <http://127.0.0.1:3200/> | trace 查询（本地文件后端，保留 24h） |
| Loki | <http://127.0.0.1:3100/> | 日志查询（单进程 + 文件系统，保留 7 天） |
| Alloy | <http://127.0.0.1:12345/> | 日志采集器 UI（Docker socket 发现容器） |
| OTLP gRPC | `127.0.0.1:4317` | 服务导出口；Collector 指标端点 `:8889` 只在 compose 网络内 |

Grafana 管理员口令来自 `.env` 的 `GRAFANA_ADMIN_PASSWORD`，不要提交或写进文档。
Grafana 已预置 Prometheus（UID `expensio-prometheus`）、Tempo（UID
`expensio-tempo`）与 Loki（UID `expensio-loki`）数据源；看板 UID 为
`expensio-overview`。

访问日志是 JSON，带 `request_id` 与 `trace_id`。Grafana 的 Loki 数据源配置了
derived field：日志里的 `"trace_id":"<32 位十六进制>"` 可直接跳到 Tempo 的
同一链路；Tempo 侧也配置了 `tracesToLogsV2` 反向跳转。

本地 Alloy 通过挂载 `docker.sock` 发现容器，仅适用于开发环境（等价于
root 级宿主权限）。生产使用 DaemonSet + `loki.source.kubernetes`，见
[`deploy/cluster/logging/README.md`](../deploy/cluster/logging/README.md)。

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
# 使用托管 Kafka 的 SASL 认证时再加一个键（本地/无认证 Kafka 不需要）：
#   --from-literal=EXPENSIO_KAFKA_SASL_PASSWORD='...'
# broker 使用私有 CA 时还要提供 PEM（公有 CA 的托管服务不需要）：
#   --from-file=EXPENSIO_KAFKA_TLS_CA=/path/to/ca.pem
```

必需键见 `values.yaml` 的 `secretKeys`。JWT 只支持 EdDSA：identity 需要
`EXPENSIO_JWT_PRIVATE_KEY`，其余服务需要 `EXPENSIO_JWT_PUBLIC_KEYS`；
不存在 `EXPENSIO_JWT_SECRET` 或 HS256 开关（M12 已删除）。
SMTP 密码是可选键：没有认证的 SMTP 可以不提供，但
`notification.emailEnabled=true` 时必须保证 SMTP 可用。
Kafka SASL 口令只在 `kafka.saslMechanism` 非空时被挂载。

轮换公钥按「验签端先行、签发端后动、旧钥最后退役」的顺序做，`kid` 是公钥的
RFC 7638 指纹，验签端按 `kid` 选钥，不依赖顺序：

1. 生成新密钥对，把**新公钥追加**到所有服务的 `EXPENSIO_JWT_PUBLIC_KEYS`
   （identity 也要，它用同一份列表生成 JWKS），滚动重启除 identity 外的五个
   服务。此时旧令牌仍全部可用，`/.well-known/jwks.json` 不变。
2. 把 identity 的 `EXPENSIO_JWT_PRIVATE_KEY` 换成新私钥，同时保留
   `PUBLIC_KEYS=A,B`，重启 identity。此后新签发的令牌用新 `kid`，JWKS 同时
   公布新旧公钥，**新旧令牌并存**。
3. 观察至少一个 access token TTL（默认 15 分钟）后，把 `PUBLIC_KEYS` 收窄为
   只剩新公钥，滚动重启全部六个服务。旧令牌立即 401。
4. 回退方案：任一步出问题，把 `PUBLIC_KEYS` 改回包含旧公钥、私钥改回旧值并
   重启即可；旧私钥在步骤 3 完成前不要销毁。

M14 在 Compose 真实栈上按上述四阶段实测（含回退），见 `docs/PROGRESS.md`。
集群里同样的步骤用 `kubectl rollout status` 代替 `docker compose up`。
注意 Helm 安装时 `EXPENSIO_JWT_PUBLIC_KEYS` 来自 Secret，轮换要改 Secret 并
触发一次滚动（`kubectl rollout restart deploy/...`）。

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
  # 托管 Kafka：TLS 必开；SASL 机制按供应商选择
  # （MSK 常用 scram-sha-512，Event Hubs / Confluent Cloud 用 plain）。
  tls: true
  # 自建集群 / 企业 PKI 才设 true，并从 Secret 提供 EXPENSIO_KAFKA_TLS_CA。
  tlsCA: false
  saslMechanism: scram-sha-512
  saslUsername: expensio
  # 口令来自 Secret 键 EXPENSIO_KAFKA_SASL_PASSWORD，不在 values 里。

# 水平扩缩容：需要集群有 metrics-server（EKS 需安装 managed add-on，
# AKS/GKE 默认自带）。开启后 Deployment 不再写死 replicas。
autoscaling:
  enabled: true
  minReplicas: 2
  maxReplicas: 10
  targetCPUUtilizationPercentage: 70
  services:
    gateway:
      maxReplicas: 20
      targetCPUUtilizationPercentage: 60

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

http:
  # 必须改成集群 Pod CIDR（或 ingress controller 的地址段），
  # 下面只是 k3s 默认值示例；EKS 常见 10.100.0.0/16，AKS 常见 10.0.0.0/8。
  # 流量是 ingress → web → gateway，留空会让网关只看到 web Pod IP：
  # 所有用户共享一个按 IP 的限流桶，审计日志也记不到真实来源。
  trustedProxies: "10.42.0.0/16"

ingress:
  host: expensio.example.com
  tls:
    secretName: expensio-tls
  certManager:
    enabled: true
    clusterIssuer: letsencrypt-prod
```

写入 `my-values.yaml`，不要提交真实地址与密钥。

`http.trustedProxies` 只信任列出的地址发来的 `X-Forwarded-For`，不要填
`0.0.0.0/0`（启动时会拒绝）。网关在未配置该值时会打一条 WARN：此时按 IP
的认证限流与邀请枚举防护仍会生效，但粒度为整个 web Pod，而不是单个客户端。

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

## 自动扩缩容（HPA）

`autoscaling.enabled=true` 后，chart 为六个服务与管理台各创建一个
`autoscaling/v2` HPA，并按 CPU（可选内存）利用率扩缩容。开启时 Deployment
**不再渲染 `replicas`**——否则 Helm 每次同步都会把副本数改回 values 里的值，
与 HPA 互相覆盖。

前置条件：集群必须有 metrics-server（EKS 需安装 managed add-on，AKS/GKE
默认自带）；没有指标源时 HPA 的 Targets 显示 `<unknown>`，不会扩容。

```yaml
autoscaling:
  enabled: true
  minReplicas: 2
  maxReplicas: 10
  targetCPUUtilizationPercentage: 70
  # 可选：HPA v2 behavior，限制扩容/缩容速率
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 300
  # 按服务覆盖；键名与 services 相同，web 用 autoscaling.web
  services:
    gateway:
      maxReplicas: 20
      targetCPUUtilizationPercentage: 60
```

模板会拒绝 `minReplicas > maxReplicas` 以及缺少 CPU 目标的配置。按内存扩容
默认关闭：内存指标抖动大，通常只适合有明确堆上限的服务。

```bash
kubectl -n expensio get hpa
kubectl -n expensio describe hpa expensio-gateway
```

> 待验证：HPA 只做过 `helm template` 与 kubeconform 校验，没有在真实集群
> 上跑过"加压 → 扩容 → 减压 → 缩容"的完整过程。

## Ingress TLS 与 cert-manager

Chart 只渲染 Ingress 注解，Issuer / ClusterIssuer 由平台安装：

```bash
helm repo add jetstack https://charts.jetstack.io
helm upgrade --install cert-manager jetstack/cert-manager \
  -n cert-manager --create-namespace --set crds.enabled=true --wait

# ClusterIssuer 示例（邮箱与 solver 按环境改）
cat <<'EOF' | kubectl apply -f -
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-prod
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: ops@example.com
    privateKeySecretRef:
      name: letsencrypt-prod-account-key
    solvers:
      - http01:
          ingress:
            class: nginx
EOF
```

values：

```yaml
ingress:
  enabled: true
  host: expensio.example.com
  tls:
    enabled: true
    # 必须与 cert-manager 写入的 secretName 一致
    secretName: expensio-tls
  certManager:
    enabled: true
    # clusterIssuer 与 issuer 二选一，前者优先
    clusterIssuer: letsencrypt-prod
```

模板在 `certManager.enabled=true` 但 `tls.enabled=false`、或两个 issuer 都
为空时直接 `fail`，避免 Ingress 起来后证书永远不签发。HTTP-01 要求域名解析
到集群入口且公网可达；内网或通配符证书改用 DNS-01。

```bash
kubectl -n expensio get certificate,secret expensio-tls
curl -fsS https://expensio.example.com/healthz
```

> 待验证：ACME 签发需要真实域名与公网入口，本仓库的 CI 与 k3d 验收都没有
> 执行过完整签发/续期流程。

## 托管云（EKS / AKS / GKE）

Chart 本身与云厂商无关，差异集中在依赖服务、身份与网络三处。下面是一张
对照表，不是"一键部署"脚本。

| 能力 | AWS | Azure | GCP |
| --- | --- | --- | --- |
| 集群 | EKS | AKS | GKE |
| PostgreSQL | RDS / Aurora PostgreSQL | Database for PostgreSQL Flexible Server | Cloud SQL for PostgreSQL |
| Redis | ElastiCache（开启 AUTH + TLS） | Azure Cache for Redis | Memorystore |
| Kafka | MSK（SCRAM-SHA-512 + TLS） | Event Hubs Kafka 端点（SASL/PLAIN + TLS） | 无托管 Kafka：Confluent Cloud / Redpanda |
| 对象存储 | S3 | Blob（需 S3 兼容网关） | GCS（需 S3 互操作端点） |
| 密钥 | Secrets Manager + External Secrets / CSI（IRSA） | Key Vault + Workload Identity | Secret Manager + Workload Identity |
| 网络策略 | VPC CNI network policy 或 Calico | Azure NPM 或 Calico | Dataplane V2 或 Calico |
| 入口 | AWS Load Balancer Controller + ingress-nginx | Application Gateway 或 ingress-nginx | GCE Ingress 或 ingress-nginx |

### 1. 数据库

托管 PostgreSQL 需要先用管理员账号执行
[`deploy/local/postgres/init/01-bootstrap.sh`](../deploy/local/postgres/init/01-bootstrap.sh)：
它创建五个业务库、`*_migrator` / `*_app` 角色与权限。迁移 Job 随后以
`{service}_migrator` 连接各库。

`database.sslmode` 托管环境至少用 `require`。`verify-full` 需要 CA 文件与
自定义 DSN，chart 目前不挂载 CA——这是已知限制，不在 values 里假装支持。

### 2. Kafka 认证

M20 之前客户端只支持明文 PLAINTEXT，托管 Kafka 接不上。现在
`internal/platform/kafka/security.go` 支持：

| 配置 | 说明 |
| --- | --- |
| `kafka.tls` | 对 broker 连接启用 TLS 1.2+ |
| `kafka.tlsCA` | 私有 CA：把 Secret 里的 PEM 挂到 `/etc/expensio/kafka/ca.pem`；公有 CA 的托管服务保持 `false` |
| `kafka.saslMechanism` | `plain` / `scram-sha-256` / `scram-sha-512` |
| `kafka.saslUsername` | 用户名（非敏感） |
| Secret `EXPENSIO_KAFKA_SASL_PASSWORD` | 口令，只注入需要 Kafka 的四个服务 |
| Secret `EXPENSIO_KAFKA_TLS_CA` | 私有 CA 的 PEM，只在 `kafka.tlsCA=true` 时挂载 |

配置层与客户端都会拒绝"SASL 但不启用 TLS"：SASL 只保护认证，不保护数据；
托管服务的 SASL 端点也都要求 TLS。

集成测试会在 testcontainers 里启动真实的 `apache/kafka:3.9.2`
（SASL/PLAIN + TLS + 自签 CA），跑通握手、生产、消费与位点提交，并验证错误
口令和明文客户端都会被拒绝：
`EXPENSIO_REQUIRE_DOCKER=1 go test ./internal/platform/kafka/ -run TestSASL -race`。
未验证的是各云厂商的具体端点（MSK 的 SCRAM、Event Hubs 的连接串形态）；
首次接入时先跑一次生产者的 `Ping` 与一条端到端事件。

### 3. 身份与网络

- Pod 不挂 ServiceAccount 令牌（`automountServiceAccountToken=false`），
  云身份只能用节点实例角色或工作负载身份；如果引入 External Secrets，
  需要单独给它的 ServiceAccount 绑定 IRSA / Workload Identity；
- NetworkPolicy 只限制入站；各 CNI 的实现差异见上表，迁移前先跑
  `deploy/cluster/acceptance/smoke.sh` 的三种放行/拒绝断言；
- `http.trustedProxies` 必须按集群 Pod CIDR 填写（EKS 常见
  `10.100.0.0/16`，AKS 常见 `10.0.0.0/8`），否则按 IP 的限流与审计来源
  都会退化成整个 web Pod；
- 负载均衡器注解（如 `service.beta.kubernetes.io/aws-load-balancer-type`）
  属于 ingress controller 的 values，不写进本 chart。

### 4. 迁移与验收顺序

```bash
# 1) 平台依赖就绪：数据库/Redis/Kafka/对象存储、Secret、metrics-server
# 2) 安装 ingress controller 与 cert-manager
# 3) 用托管依赖的地址覆盖 values（不要提交到仓库）
helm upgrade --install expensio deploy/helm/expensio \
  -n expensio --create-namespace -f my-values.yaml --wait --timeout 15m
# 4) 复用验收脚本的断言（把 deps 相关的检查跳过）
NAMESPACE=expensio INGRESS_HOST=expensio.example.com \
  BASE_URL=https://expensio.example.com bash deploy/cluster/acceptance/smoke.sh
```

> 待验证：以上流程没有在真实 EKS/AKS/GKE 上执行过；本仓库没有云账号与
> 集群凭据。已完成的验证是 chart 渲染、kubeconform schema 校验与 k3d 上的
> chart 行为验收，不能替代云环境验证。

## 发布流程（tag → 镜像 → Helm）

`.github/workflows/cd.yml` 在推送 `v*` tag 时触发，分三段：

1. **verify**：在 tag 指向的提交上重跑 gofmt / vet / build、真实依赖集成测试、
   前端构建与 OpenAPI lint。不假设"它曾经在 main 上绿过"。
2. **images**：构建六个服务、`web` 与 `migrate` 共八个镜像，同时推
   `vX.Y.Z` 与 commit sha 两个 tag，并为每个镜像生成 SPDX SBOM（构建产物，
   不落仓库）。
3. **deploy**：`environment: production`（可配 required reviewers）下执行
   `helm upgrade --install --wait`。集群凭据从 `KUBE_CONFIG` Secret 读取；
   未配置时跳过部署并打印 notice，不影响镜像发布。

依赖（PostgreSQL / Redis / Kafka / MinIO）、命名空间与 `expensio-secrets`
仍由平台预先提供，见本文前面的 Secret 小节。

回滚：

```bash
helm -n expensio history expensio
helm -n expensio rollback expensio <REVISION>
```

镜像 tag 是不可变的 commit sha 或语义化版本，回滚不需要重建镜像；
迁移镜像随 release 一起回退，但**已执行的迁移不会自动回退**，
破坏性变更必须写成向前兼容的两步迁移。

> 待验证：deploy 段在真实托管集群上没有执行过（本仓库没有目标集群与
> `KUBE_CONFIG`）；命令形态与 CI 里 k3d 验收的 `helm upgrade --install`
> 相同。首次接入真实集群时按本文"集群验收"小节先跑一遍。

## 集群验收（可复现）

`deploy/cluster/acceptance/` 是一套在真实集群上验证 chart 行为的清单与脚本，
CI 的 `cluster` job 与本地演练共用：

- `deps.yaml`：本地验收用的 PostgreSQL / Redis / Kafka（单节点，仅用于演练，
  生产必须用托管服务或你的 Operator）；
- `values.yaml`：指向本地依赖与 `expensio.test` 的覆盖值；
- `smoke.sh`：断言 7 个 Deployment 就绪与安全上下文、ServiceAccount 无权限、
  五个库有迁移记录、四个主题已创建、Hook Job 已清理、Ingress `/healthz` 200、
  注册→邀请→接受→费用→审批→报表的完整链路，以及 NetworkPolicy 的
  放行/拒绝三种情形；
- `README.md`：前置条件与本地 k3d 的步骤。

在已有集群上执行：

```bash
bash deploy/cluster/acceptance/smoke.sh
# 或指定地址：NAMESPACE=expensio INGRESS_HOST=expensio.test \
#             BASE_URL=http://127.0.0.1:8080 bash deploy/cluster/acceptance/smoke.sh
```

脚本末尾打印 `SMOKE_RESULT: PASS` 才算通过；任何一条断言失败都会以
`SMOKE FAIL: <原因>` 退出。M14 的实测输出见 `docs/PROGRESS.md`。

## 集群日志（Loki + Alloy）

日志聚合属于平台能力，不进业务 chart。`deploy/cluster/logging/` 提供
`grafana/loki` 与 `grafana/alloy` 的部署值：

```bash
kubectl create namespace observability
kubectl -n observability create secret generic loki-s3-credentials \
  --from-literal=AWS_ACCESS_KEY_ID=... --from-literal=AWS_SECRET_ACCESS_KEY=...
# 先创建桶 expensio-loki-chunks / expensio-loki-ruler

helm upgrade --install loki grafana/loki -n observability \
  -f deploy/cluster/logging/loki-values.yaml --wait
helm upgrade --install alloy grafana/alloy -n observability \
  -f deploy/cluster/logging/alloy-values.yaml --wait
```

Alloy 用 DaemonSet + `loki.source.kubernetes`，只采集
`app.kubernetes.io/part-of=expensio` 的 Pod；RBAC 仅授予
`pods` / `pods/log` / `namespaces` / `nodes` 的只读权限，不挂 hostPath，
也不给 Pod 挂 `docker.sock`。验证方法与取舍见该目录的 README。

## Chart 的安全默认值

- 全部容器 `runAsNonRoot`、UID 10001、`readOnlyRootFilesystem`、
  `allowPrivilegeEscalation=false`、drop ALL capabilities；
- `automountServiceAccountToken=false`；
- 默认拒绝入站，仅 ingress controller 可访问 gateway/web，同 release 的
  Pod 之间可互访（`networkPolicy.enabled`）；
- gateway 与 web 各 2 副本并配置 `minAvailable: 1` 的 PDB；
- 探针区分 `/healthz`（liveness，不碰下游）与 `/readyz`（readiness）。

## 已知限制

- **只在 k3d 上验证过**：验收集群是 k3d v5.8.3（k3s v1.31.5，双节点）加
  单节点 PostgreSQL/Redis/Kafka。托管云（EKS/AKS/GKE）、各 CNI 的
  NetworkPolicy 实现、PDB 驱逐演练均未验证；`deploy/cluster/acceptance/`
  提供了可复用的断言，迁移到新环境时应先跑一遍。
- **HPA 未做真实扩缩容**：模板与 schema 校验通过，但没有 metrics-server
  下的加压测试；也没有验证扩缩容期间的事件消费再平衡。
- **cert-manager 未走完整 ACME**：注解与 Issuer 二选一、缺少 TLS 时模板
  直接失败；但签发与续期依赖真实域名，未在 CI 中执行。
- **Kafka SASL 只对本地真实 broker 验证过**：testcontainers 启的
  `apache/kafka`（SASL/PLAIN + TLS）已经跑通生产/消费/位点提交，错误口令与
  明文连接会被拒绝；但 MSK 的 SCRAM、Event Hubs 的连接串形态属于供应商差异，
  首次接入时仍需验证。
- **`verify-full` 不可直接配置**：chart 只透传 `sslmode`，没有挂载 CA 与
  `sslrootcert`，托管数据库目前止步于 `require`。
- **出站未做白名单**：NetworkPolicy 只限制入站。数据库/Kafka/SMTP 地址由
  平台提供，写死 CIDR 会让 chart 无法跨环境复用；需要时由集群级策略补足。
- **HSTS**：web 在 Ingress 终止 TLS 时看不到 `r.TLS`，因此 HSTS 应由
  Ingress 配置添加（例如 nginx 的 `server-snippet`）。
- **单地域**：没有多集群、跨区复制与故障转移设计。
- **观测后端不在 Chart 内**：Collector、Prometheus、Grafana、Tempo 均由平台
  提供；Chart 只负责把 OTLP 地址注入服务。没有这些后端时 trace/指标会丢失，
  但业务请求不受影响。
