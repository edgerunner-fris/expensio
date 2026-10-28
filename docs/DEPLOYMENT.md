# 部署

本文只描述当前真实可执行的路径。K8s 部分除 `helm lint`、`helm template`
与 kubeconform（K8s 1.31 schema，24/24 valid）外，已在真实 k3d 集群
（k3s v1.31.5，双节点）完成 `helm upgrade --install --wait` 与行为验收：
迁移/建主题 Hook 实际执行、Ingress 可达、NetworkPolicy 放行与拒绝符合预期、
ServiceAccount 无 API 权限、事件链路在集群内跑通。验收清单与 smoke 脚本
见 [`deploy/cluster/acceptance/`](../deploy/cluster/acceptance/)，实测输出见
`docs/PROGRESS.md` 的 M14。托管云集群与 Ingress TLS 终止仍未验证，见文末
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
```

必需键见 `values.yaml` 的 `secretKeys`。JWT 只支持 EdDSA：identity 需要
`EXPENSIO_JWT_PRIVATE_KEY`，其余服务需要 `EXPENSIO_JWT_PUBLIC_KEYS`；
不存在 `EXPENSIO_JWT_SECRET` 或 HS256 开关（M12 已删除）。
SMTP 密码是可选键：没有认证的 SMTP 可以不提供，但
`notification.emailEnabled=true` 时必须保证 SMTP 可用。

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
  NetworkPolicy 实现、Ingress TLS 终止与 cert-manager、PDB 驱逐演练均未验证；
  `deploy/cluster/acceptance/` 提供了可复用的断言，迁移到新环境时应先跑一遍。
- **出站未做白名单**：NetworkPolicy 只限制入站。数据库/Kafka/SMTP 地址由
  平台提供，写死 CIDR 会让 chart 无法跨环境复用；需要时由集群级策略补足。
- **HSTS**：web 在 Ingress 终止 TLS 时看不到 `r.TLS`，因此 HSTS 应由
  Ingress 配置添加（例如 nginx 的 `server-snippet`）。
- **单地域**：没有多集群、跨区复制与故障转移设计。
- **观测后端不在 Chart 内**：Collector、Prometheus、Grafana、Tempo 均由平台
  提供；Chart 只负责把 OTLP 地址注入服务。没有这些后端时 trace/指标会丢失，
  但业务请求不受影响。
