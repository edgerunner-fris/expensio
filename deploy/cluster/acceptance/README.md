# 集群验收环境

这组文件用于在**真实 Kubernetes 集群**里验证 `deploy/helm/expensio` 的实际
行为，而不是只渲染模板。CI 的 `cluster` job（`.github/workflows/ci.yml`）
在 k3d 上执行它；本地也可以照着跑。

它验证的是 schema 与 `helm template` 证明不了的东西：

- `helm upgrade --install --wait` 与 `pre-install,pre-upgrade` Hook 的真实执行；
- 迁移 Hook 是否真的建出了五套 `app.schema_migrations`；
- 主题 Hook 是否真的在 Kafka 里创建了四个主题；
- Ingress → web → gateway → 五个后端服务的请求链路；
- 提交 → 审批 → Kafka → report 读模型的端到端事件链路；
- NetworkPolicy：跨命名空间、同命名空间未打 release 标签的 Pod 被拒绝，
  同 release 的 Pod 与 ingress controller 被放行（k3s 由 kube-router 真正执行策略；
  ACK 的 Terway 默认关闭策略，且节点能力 `has_cilium_chainer=false` 是粘性的，
  需重置节点能力、重启 `terway-eniip` 并重建 Pod 后再跑，见
  [docs/PROGRESS.md](../../../docs/PROGRESS.md)）；
- RBAC：ServiceAccount 没有 API 权限、Pod 不挂载令牌；
- 容器安全上下文实际生效（非 root、只读根文件系统、禁止提权）。

## 文件

| 文件 | 用途 |
| --- | --- |
| `deps.yaml` | 单节点 PostgreSQL / Redis / Kafka / MinIO（**仅验收用**，emptyDir，无备份/TLS） |
| `values.yaml` | 指向集群内依赖、Traefik、关闭 TLS 与邮件、主题单副本 |
| `smoke.sh` | 安装后的验收断言，失败即非零退出；托管依赖加 `MANAGED_DEPS=1` 跳过两条直连检查 |
| `drills/lib.sh` | 三个演练共用的环境变量与辅助函数（HPA/Deployment 两种扩缩方式） |
| `drills/rebalance.sh` | 消费者再平衡：扩/缩副本期间 4 条真实事件不丢、LAG 归零 |
| `drills/drain.sh` | 连接排空：滚动重启 gateway 时经 Ingress 的请求不中断 |
| `drills/pdb.sh` | PDB 驱逐：1 副本拒绝驱逐，2 副本并发驱逐恰好 1 个成功 |

## 前提

- 集群节点能访问镜像仓库；chart 通过 `imagePullSecrets` 拉取私有包；
  拉不到 Docker Hub 时用 `BUSYBOX_IMAGE` 指向可达仓库里的同版本 busybox
  （例如 `registry.cn-hangzhou.aliyuncs.com/google_containers/busybox:1.27`）；
- 命名空间已存在，且预先创建：
  - ConfigMap `postgres-init`（key 为 `01-bootstrap.sh`，内容取
    `deploy/local/postgres/init/01-bootstrap.sh`）；
  - Secret `expensio-secrets`（键见 chart `values.yaml` 的 `secretKeys`，
    `EXPENSIO_NOTIFICATION_SMTP_PASSWORD` 可省略；
    MinIO 的 `EXPENSIO_MINIO_ACCESS_KEY` / `EXPENSIO_MINIO_SECRET_KEY` 必填）；
  - Secret `deps-secrets`（`POSTGRES_PASSWORD`、`MINIO_ROOT_USER`、`MINIO_ROOT_PASSWORD`）。
- `kubectl`、`helm`、`curl`、`jq` 可用。

## 本地演练

```bash
# 1. 起一个双节点 k3d（宿主 8080 → 集群 80）
k3d cluster create expensio --servers 1 --agents 1 --port 8080:80@loadbalancer --wait

# 2. 构建并导入镜像（tag 与 values 的 images.registry/tag 对应）
#    七个服务镜像：expensio/{gateway,identity,expense,billing,report,notification,web}:dev
#    本地若已用 make 构建，可直接 k3d image import 到集群；
docker build -f Dockerfile.migrate -t expensio/migrate:dev .
k3d image import expensio/migrate:dev -c expensio

# 3. 创建命名空间、ConfigMap 与 Secret（示例：复用本地 .env 的密钥）
kubectl create namespace expensio
kubectl -n expensio create configmap postgres-init \
  --from-file=01-bootstrap.sh=deploy/local/postgres/init/01-bootstrap.sh
# …… 创建 expensio-secrets 与 deps-secrets，见上

# 4. 依赖与 chart
kubectl apply -f deploy/cluster/acceptance/deps.yaml
helm upgrade --install expensio deploy/helm/expensio -n expensio --wait --timeout 15m \
  -f deploy/cluster/acceptance/values.yaml \
  --set images.registry=expensio --set images.tag=dev \
  --set images.migrate=expensio/migrate \
  --set-json 'imagePullSecrets=[]'

# 5. 验收
bash deploy/cluster/acceptance/smoke.sh
```

本地与 CI 的差异只有镜像来源：CI 用本次提交推送到 GHCR 的镜像，
本地用导入到 containerd 的 `expensio/*:dev`。

## 滚动与中断演练

`smoke.sh` 只证明“装起来能用”，不覆盖副本变化时的行为。三个演练补上这部分：

```bash
for d in rebalance drain pdb; do
  NAMESPACE=expensio INGRESS_HOST=expensio.test BASE_URL=http://127.0.0.1:8080 \
    bash "deploy/cluster/acceptance/drills/$d.sh"
done
```

- `rebalance.sh`：把 report 从 1 副本扩到 2、再造 4 条真实事件（2 张费用单的
  提交与审批），断言分区重新分配、`produced == consumed`、LAG 归零、报表
  金额为 2 × 12345 CNY；最后缩回 1 副本再验证一次分配。
- `drain.sh`：滚动重启 gateway，以 100ms 间隔经 Ingress 请求 `/api/v1/me`
  （gateway → identity），断言没有非 200 且 Pod 确实被替换。
- `pdb.sh`：1 副本时 Eviction 必须返回 429；扩到 2 副本后并发驱逐两个 Pod，
  断言恰好 1 个 201 + 1 个 429，期间探针全部 200。

三者都通过 HPA 状态自动选择扩缩方式：HPA 存在时 patch `minReplicas`，
不存在时直接 `kubectl scale`。`pdb.sh` 用 `kubectl proxy` 调 Eviction 子资源
（该子资源不在 discovery 里，`kubectl create -f` 无法映射），端口可用
`PROXY_PORT` 覆盖。端点 URL 需要能被 `curl` 从执行脚本的主机访问；
在 ACK 上用了公网 NLB 地址。

阿里云 ACK 的实测结果见
[docs/EXTERNAL_VERIFICATION.md](../../../docs/EXTERNAL_VERIFICATION.md) 第 9 节：
三条演练均以 `DRILL_RESULT: PASS` 结束。

## 不要用于生产

`deps.yaml` 是刻意做小的：单副本、emptyDir、无认证，Kafka 单 broker 只能
承载 1 副本主题。生产依赖（数据库、缓存、消息队列）由平台提供，
chart 及其默认值不会替你创建它们。
