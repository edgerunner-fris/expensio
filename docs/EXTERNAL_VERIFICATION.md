# 外部环境验证手册

本手册用于把 Expensio 从「本地/k3d 已验证」推进到「目标云环境已实测」。
它不重复 [DEPLOYMENT.md](DEPLOYMENT.md) 的依赖选型与 values 对照，而是给出
一套可照着执行、可留证据的顺序。

验证的边界要先说清楚：

- **算通过**：命令有真实输出、断言由脚本判定、证据能贴进
  `docs/PROGRESS.md`；失败也是结果，必须记录。
- **不算通过**：`helm template` 渲染成功、kubeconform 通过、本地 k3d 跑通。
  这些已经做过，不能替代云上验证。

## 0. 先决条件检查

在目标集群上逐条执行，任何一条不满足就先修环境：

```bash
kubectl version
kubectl get nodes -o wide
kubectl get ingressclass
kubectl get apiservice v1beta1.metrics.k8s.io \
  -o jsonpath='{.status.conditions[?(@.type=="Available")].status}{"\n"}'
kubectl top nodes
helm version
```

| 能力 | 需要什么 | 缺失时的表现 |
| --- | --- | --- |
| Ingress | 至少一个 IngressClass，且能分配外部地址 | Ingress 一直 `ADDRESS` 为空 |
| TLS 自动签发 | cert-manager CRD 与可用 Issuer（可选） | Certificate 不进入 `Ready=True` |
| HPA | metrics-server 的 `v1beta1.metrics.k8s.io` 为 `True` | HPA 显示 `<unknown>/70%` |
| 镜像拉取 | 节点能访问镜像仓库；私有仓库需 `imagePullSecrets` | `ImagePullBackOff` |
| NetworkPolicy | CNI 真正执行策略（Calico / VPC CNI / Azure NPM 等） | smoke 的拒绝断言会失败；ACK 的 Terway 默认关闭策略 |
| 托管依赖 | PostgreSQL / Redis / Kafka / 对象存储已创建并可连 | Pod 启动或 readiness 失败 |

阿里云 ACK 的 Terway 默认 `disable_network_policy=true`：NetworkPolicy 对象
能创建，但不会被 CNI 执行。先确认：

```bash
kubectl -n kube-system get cm eni-config \
  -o jsonpath='{.data.disable_network_policy}{"\n"}'
```

输出 `true` 时改成 `false` 并重启 `terway-eniip`。ACK 还有一层节点级粘性
开关：节点能力文件里的 `has_cilium_chainer`。如果节点创建时策略是关闭的，
这个值会被写成 `false` 并一直保留，仅改 ConfigMap 不会启动 cilium-agent
（策略容器只会打开 9099 健康检查端口）。确认：

```bash
kubectl -n kube-system exec <terway-pod> -c terway -- \
  cat /var/run/eni/node_capabilities
```

出现 `has_cilium_chainer = false` 时，需要删除每台节点的该文件并重启
`terway-eniip`；节点会切到 DataPath V2 并追加 `cilium-cni`。随后必须重建
业务 Pod 与 ingress-controller，让新 veth 挂上 eBPF 程序：旧 Pod 不在
`CiliumEndpoint` 里，会因默认拒绝而超时。完整实测见第 9 节。这是环境结论，
不是 chart 或脚本 bug。

EKS 的 metrics-server 需要单独安装 managed add-on；AKS/GKE 默认自带，但仍要
用上面的 `apiservice` 命令确认，而不是看控制台。

## 1. 准备 values 与 Secret

values 只放非敏感配置，按 [DEPLOYMENT.md](DEPLOYMENT.md) 的云厂商对照表填写。
需要重点确认的字段：

```yaml
database:
  host: pg.internal
  port: 5432
  sslmode: verify-full      # 公有 CA 用系统根证书；自签 CA 见下
  tlsCA: false              # 自签 CA 时 true，并准备 EXPENSIO_DB_TLS_CA

kafka:
  brokers: kafka.internal:9092
  # 主题 Hook 的 Kafka CLI 镜像。离线/私有仓库环境改成同版本的仓库副本；
  # CLI 必须与 broker 主版本兼容。默认 apache/kafka:3.9.2。
  cliImage: apache/kafka:3.9.2
  tls: true
  tlsCA: false
  saslMechanism: scram-sha-512   # MSK；Event Hubs / Confluent 用 plain
  saslUsername: expensio

http:
  trustedProxies: "10.100.0.0/16"   # 按集群 Pod CIDR 填，不能留空

ingress:
  host: expensio.example.com
  tls:
    enabled: true
    secretName: expensio-tls
  certManager:
    enabled: true
    clusterIssuer: letsencrypt-prod
```

Secret 的创建命令见 DEPLOYMENT 的「准备 Secret」小节。与本地不同，云上通常
还要多两个文件型键：

```bash
# 只在对应 tlsCA=true 时需要；公有 CA 的托管服务不要加
kubectl -n expensio create secret generic expensio-secrets \
  --from-file=EXPENSIO_DB_TLS_CA=/path/to/pg-ca.pem \
  --from-file=EXPENSIO_KAFKA_TLS_CA=/path/to/kafka-ca.pem \
  ...其余键同 DEPLOYMENT
```

两个容易犯的错：

- 把 CA 内容或口令写进 `my-values.yaml` 并提交；
- 使用 `database.tlsCA=true` 却没有 `EXPENSIO_DB_TLS_CA` 键。后者会让 Pod
  停在 `CreateContainerConfigError`，这是有意的 fail-fast。

安装前先用真实 values 做一次本地渲染，能提前发现模板与 values 的冲突：

```bash
helm template expensio deploy/helm/expensio -n expensio -f my-values.yaml \
  >/tmp/render.yaml
docker run --rm -v /tmp/render.yaml:/tmp/render.yaml \
  ghcr.io/yannh/kubeconform:v0.6.7 -strict -kubernetes-version 1.31.0 \
  -summary /tmp/render.yaml
```

## 2. 安装与观察

```bash
helm upgrade --install expensio deploy/helm/expensio \
  -n expensio --create-namespace -f my-values.yaml \
  --wait --timeout 20m

kubectl -n expensio get pods -o wide
kubectl -n expensio get events --sort-by=.lastTimestamp | tail -30
helm -n expensio status expensio
```

`--wait` 会等 Deployment、Service 与 Job 就绪。失败时不要反复重装，先看三类
对象：

```bash
kubectl -n expensio describe pod <pending-pod>
kubectl -n expensio logs job/expensio-migrate       # 若 Hook 还没被删除
kubectl -n expensio get certificate,order,challenge
```

注意：迁移与建主题 Hook 成功后会被 `hook-succeeded` 删除，`get jobs` 为空
是正常现象，不代表没执行。需要结构性证据时看第 4 节。

## 3. 功能验收

托管依赖没有 `deploy/postgres`、`deploy/kafka` 可 exec，必须加上
`MANAGED_DEPS=1`：

```bash
NAMESPACE=expensio \
INGRESS_HOST=expensio.example.com \
BASE_URL=https://expensio.example.com \
MANAGED_DEPS=1 \
bash deploy/cluster/acceptance/smoke.sh
```

它会验证：

- 7 个 Deployment 就绪，Pod 的安全上下文实际生效；
- ServiceAccount 没有 API 权限、Pod 不挂载令牌；
- Ingress `/healthz` 返回 200；
- 注册 → 邀请 → 接受 → 建单 → 提交 → 审批 → 报表的完整链路；
- 报表读模型能查到聚合（证明 outbox → Kafka → report 真的通了）；
- NetworkPolicy 的放行/拒绝三种情形（由目标 CNI 真实执行）。

最后一行必须是 `SMOKE_RESULT: PASS`。任何一条失败都会打印
`SMOKE FAIL: <原因>`。在托管依赖下，迁移与主题的存在由这条功能链路间接
证明：没有迁移就无法注册，没有主题就无法产生报表聚合。平台明确不执行
NetworkPolicy 时，按第 0 节先确认原因；在记录里写“部分通过”并列出未通过的
断言，不能把失败改写成通过。

如果 CNI 不支持 NetworkPolicy，脚本会在拒绝断言处失败。这是真实结论，
处理方式是给集群启用策略引擎，或在记录中明确标注该环境无法验证这一项。

## 4. 托管依赖的结构性证据（可选）

功能链路已经证明依赖可用；如果审计需要「迁移版本」与「主题列表」的直接
输出，可以单独渲染 chart 里的 Hook Job 并用 kubectl 执行一次。两个 Job 都是
幂等的（迁移 `up` 无变化时成功退出，建主题忽略已存在）。

```bash
# 迁移：应看到每个库的版本号，最后退出码 0
helm template expensio deploy/helm/expensio -n expensio -f my-values.yaml \
  --show-only templates/migration-job.yaml >/tmp/migrate-job.yaml
kubectl -n expensio delete job expensio-migrate --ignore-not-found
kubectl -n expensio apply -f /tmp/migrate-job.yaml
kubectl -n expensio wait --for=condition=complete job/expensio-migrate --timeout=10m
kubectl -n expensio logs job/expensio-migrate
kubectl -n expensio delete job expensio-migrate
```

```bash
# 主题：应看到四个主题的创建/已存在结果
helm template expensio deploy/helm/expensio -n expensio -f my-values.yaml \
  --show-only templates/topics-job.yaml >/tmp/topics-job.yaml
kubectl -n expensio delete job expensio-topics --ignore-not-found
kubectl -n expensio apply -f /tmp/topics-job.yaml
kubectl -n expensio wait --for=condition=complete job/expensio-topics --timeout=10m
kubectl -n expensio logs job/expensio-topics
kubectl -n expensio delete job expensio-topics
```

把 `logs` 的输出保存到证据文件；不要只写「已执行」。

## 5. 逐项验证

### 5.1 TLS 与 ACME

```bash
kubectl -n expensio get certificate
kubectl -n expensio describe certificate expensio-tls
echo | openssl s_client -connect expensio.example.com:443 \
  -servername expensio.example.com 2>/dev/null \
  | openssl x509 -noout -issuer -subject -dates
curl -sS -o /dev/null -w '%{http_code}\n' https://expensio.example.com/healthz
```

判据：Certificate 为 `Ready=True`；证书 issuer 是预期 CA；`/healthz` 为 200。
续期验证要等到接近到期（或临时把 `renewBefore` 调大），并在 cert-manager
日志里看到一次成功的续期。

### 5.2 HPA

```bash
kubectl -n expensio get hpa
kubectl -n expensio top pods
BASE_URL=https://expensio.example.com k6 run load/k6/api.js
# 另一个终端持续观察 3–5 分钟
kubectl -n expensio get hpa,pods -w
```

判据：CPU 超过目标后 `REPLICAS` 上升，负载停止后回落到 `minReplicas`；
扩容/缩容期间没有 5xx 尖峰。若 HPA 一直显示 `<unknown>`，先回到第 0 节确认
metrics-server。

k3d 与阿里云 ACK 已用这组参数实测 identity 的 1→3→1（见
`docs/PROGRESS.md` M20）；云上还需要额外关注节点级扩容延迟。扩缩容期间的
消费者再平衡、连接排空与 PDB 驱逐用下面三条演练覆盖。

```bash
# HPA 开启时脚本会 patch HPA 的 minReplicas，关闭时直接 scale Deployment
for d in rebalance drain pdb; do
  NAMESPACE=expensio INGRESS_HOST=expensio.test BASE_URL=https://expensio.example.com \
    bash "deploy/cluster/acceptance/drills/$d.sh"
done
```

判据：三条都以 `DRILL_RESULT: PASS` 结尾。`rebalance` 要求位点前移量等于
日志增量、LAG 归零且报表金额正确；`drain` 与 `pdb` 要求探针期间没有非 200。
阿里云 ACK 已实测通过（见第 9 节）。

### 5.3 Kafka 供应商差异

先确认第 4 节的主题 Job 在目标 broker 上成功；再用一次真实事件链路确认
生产与消费：

```bash
# 复用 smoke 的建单/审批/报表，或手工触发一次
NAMESPACE=expensio INGRESS_HOST=... BASE_URL=... MANAGED_DEPS=1 \
  bash deploy/cluster/acceptance/smoke.sh
```

MSK 用 SCRAM-SHA-512、Event Hubs / Confluent 用 PLAIN 加连接串；这些差异
只体现在 values 与 Secret，不在代码里。首次接入必须至少验证一次
`Ping` 等价行为（服务启动无认证错误）和一条端到端事件。

### 5.4 PostgreSQL `verify-full`

托管实例的证书主机名必须与 `database.host` 完全一致，代理端点会改变这一点。

在集群内用一次性 Pod 验证，不把数据库暴露到公网。下面的 ConfigMap 名按
release 实际值替换（release 名为 `expensio` 时就是 `expensio-config`）：

```yaml
# /tmp/pg-check.yaml
apiVersion: v1
kind: Pod
metadata:
  name: pg-check
  namespace: expensio
spec:
  restartPolicy: Never
  containers:
    - name: psql
      image: postgres:16-alpine
      command: ["sh", "-c"]
      args:
        - >-
          psql "host=$EXPENSIO_DB_HOST port=$EXPENSIO_DB_PORT
          dbname=expense_db user=expense_app sslmode=verify-full
          sslrootcert=/etc/expensio/postgres/ca.pem"
          -c "select current_setting('ssl')"
      env:
        - name: PGPASSWORD
          valueFrom:
            secretKeyRef:
              name: expensio-secrets
              key: EXPENSIO_DB_APP_PASSWORD
      envFrom:
        - configMapRef:
            name: expensio-config
      volumeMounts:
        - name: postgres-ca
          mountPath: /etc/expensio/postgres
          readOnly: true
  volumes:
    - name: postgres-ca
      secret:
        secretName: expensio-secrets
        items:
          - key: EXPENSIO_DB_TLS_CA
            path: ca.pem
```

```bash
kubectl apply -f /tmp/pg-check.yaml
kubectl -n expensio wait --for=condition=Ready pod/pg-check --timeout=2m
kubectl -n expensio logs pg-check
kubectl -n expensio delete pod pg-check
```

判据是输出 `on`。公有 CA 不需要 `sslrootcert` 与这个卷，直接用
`sslmode=verify-full` 连接系统根证书即可；把 `sslrootcert` 换成错误的 CA
必须连接失败。

### 5.5 对象存储与附件

smoke 没有覆盖附件。单独验证一次：

1. 用 owner 登录，创建费用单并上传一个附件；
2. 断言返回的下载链接是预期的对象存储域名（`minio.publicEndpoint`）；
3. 用该链接下载，内容与上传一致；
4. 删除费用单后，确认对象存储的生命周期规则会清理孤儿对象（若未配置，
   记录为未解决）。

### 5.6 可观测性

```bash
kubectl -n expensio logs deploy/expensio-gateway --tail=50
```

在 Grafana 里确认：

- tempo 能按服务名查到 `expensio-*` 的 trace；
- Loki 能查到 smoke 租户的请求日志；
- Prometheus 告警规则处于 loaded 状态。
采集后端由平台提供，chart 只注入地址；后端不可用时业务不受影响，但这项
验证会失败。

### 5.7 备份与恢复

按 `scripts/backup-drill.sh` 的逻辑在云上重做一次：对象存储版本/跨区复制、
PostgreSQL PITR 到一个新实例、恢复后跑 smoke 的只读断言。参考
[DEPLOYMENT.md](DEPLOYMENT.md) 的备份小节。

## 6. 记录与收口

每一项都按下面的格式留证据（命令 + 关键输出 + 日期）：

```text
[2026-11-xx] EKS 1.31 / managed PG 16 / MSK
$ NAMESPACE=expensio ... MANAGED_DEPS=1 bash deploy/cluster/acceptance/smoke.sh
...
SMOKE_RESULT: PASS

$ kubectl -n expensio get hpa
NAME               REFERENCE             TARGETS   MINPODS MAXPODS REPLICAS
expensio-gateway   Deployment/gateway    63%/60%   2       20      4
```

完成后：

1. 把结果写进 `docs/PROGRESS.md`：新增一个「外部环境验证」小节，逐项标注
   实测/失败/未执行；
2. 更新风险表：把对应行从「待验证」改为「已解决」或写明失败原因，不要写
   「应该没问题」；
3. 如果发现供应商差异（证书主机名、SASL 机制、代理端点），把实际值补进
   `docs/DEPLOYMENT.md` 的对照表；
4. 删除临时验证 Pod、Job 与测试租户；`helm uninstall` 后确认云资源按计费
   策略清理。

## 7. 常见失败

| 现象 | 先查什么 |
| --- | --- |
| Pod 停在 `CreateContainerConfigError` | Secret 缺键；对比 `values.yaml` 的 `secretKeys` |
| `ImagePullBackOff` | 仓库地址、tag、`imagePullSecrets` |
| readiness 一直失败 | 托管依赖地址/安全组、`verify-full` 主机名、Kafka TLS/SASL |
| 迁移 Job 失败 | 数据库账号权限、`sslrootcert` 路径、库名 |
| 主题 Job 失败 | broker 地址、SASL 机制与口令、CA |
| smoke 在 NetworkPolicy 处失败 | CNI 是否真的执行策略；这是环境结论，不是脚本 bug |
| HPA `<unknown>` | metrics-server 的 APIService 是否 Available |

## 8. 不要做的事

- 不要 apply `deploy/cluster/acceptance/deps.yaml`：它是本地验收用的单节点
  依赖，没有备份、TLS 与高可用；
- 不要把 `my-values.yaml`、CA 文件、口令提交到仓库；
- 不要在验证环境使用生产域名与生产 Secret；
- 不要把「渲染通过」写成「云上验证通过」。

## 9. 已执行记录：阿里云 ACK 阶段 A/B（2026-10-29）

这一阶段验证的是**平台适配**，不是托管依赖：集群是阿里云 ACK 托管集群
（ID `c772e4d3c99764bdf906ce05a63e559e0`，cn-hangzhou，4 个 Lifsea 节点，
K8s `v1.37.1-aliyun.1`，Terway ENIMultiIP，ingress-nginx v1.14.5 公网 NLB，
metrics-server v0.3.16）。PostgreSQL / Redis / Kafka / MinIO 仍是
`deploy/cluster/acceptance/deps.yaml` 渲染出的临时单副本 Pod；因此这一节
不能替代第 1–5 节对 RDS / Tair / ApsaraMQ / OSS、TLS 与备份的验证。第 8 节
“不要 apply deps.yaml”仍适用于生产验证；这里是有意为之的阶段 A 例外，
复核后应删除临时依赖。

执行方式（完整输出见 `docs/PROGRESS.md` 的「阿里云 ACK 阶段 A」）：

```bash
# 1) ACR 镜像 + 命名空间级 pull secret（凭据不写入仓库）
# 2) 临时依赖
kubectl apply -f rendered-deps.yaml
# 3) chart 0.3.2；ACK values 把 kafka.cliImage 指向 ACR 的同版本镜像
helm upgrade --install expensio deploy/helm/expensio -n expensio \
  -f ack-values.yaml --wait --timeout 20m
# 4) NLB 地址 + Host 头，不依赖域名解析
NAMESPACE=expensio INGRESS_HOST=expensio.test \
  BASE_URL=http://<nlb-hostname> \
  BUSYBOX_IMAGE=registry.cn-hangzhou.aliyuncs.com/google_containers/busybox:1.27 \
  bash deploy/cluster/acceptance/smoke.sh
```

结果：

```text
helm STATUS: deployed（7 个 Deployment；gateway/web 首装各 2 副本，启用 HPA 后 1/1）
五个库 schema_migrations 版本数=1；四个业务主题已创建；成功 Hook Job 已清理
GET /healthz -> 200
注册→邀请→接受→费用→审批→Kafka→报表：通过
HPA identity：1→3（约 60s，CPU 908%/50%）→1（停压后约 2m10s）
NetworkPolicy：default -> identity 被拒；未打 release 标签的 Pod 被拒；同 release 放行
SMOKE_RESULT: PASS
```

NetworkPolicy 最初失败，根因有两层：`disable_network_policy=true`；改成
`false` 后仍不生效，因为节点能力 `has_cilium_chainer=false` 是粘性的。
重置节点能力并重启 Terway 后，节点切到 DataPath V2 + cilium-cni；再重建
7 个应用 Deployment 与 ingress-controller，三条断言全部通过。完整步骤与
证据见 `docs/PROGRESS.md` 的「阿里云 ACK 阶段 A」。

阶段 A 还暴露并修复了一个 chart 缺陷：主题 Hook 的 Kafka CLI 镜像原先硬编码
`apache/kafka:3.9.2`，节点访问 Docker Hub 超时导致 `pre-install` Hook
`ImagePullBackOff`、Helm 停在 `pending-install`。0.3.2 起可用
`kafka.cliImage` 指向 ACR 镜像，默认值不变。

阶段 B 在同一集群补做滚动与中断演练，脚本是仓库里的
`deploy/cluster/acceptance/drills/`（本机 Docker 不可用，无法用 k3d 彩排）：

```text
消费者再平衡（rebalance.sh）
  baseline parts=3 consumers=1 current=10 end=10 lag=0
  扩到 2 副本 -> consumers=2，分区 2/1 分配，min per consumer=1
  造 4 条真实事件 -> produced=4 consumed=4 lag=0
  报表读模型 CNY amount_minor=24690（2 × 12345）
  缩回 1 副本 -> consumers=1 current=14 end=14 lag=0
  DRILL_RESULT: PASS rebalance

连接排空（drain.sh）
  滚动重启 gateway，100ms 间隔经 NLB 请求 /api/v1/me
  probe requests=78 ok=78 bad=0；Pod UID 前后不同
  DRILL_RESULT: PASS drain

PDB 驱逐（pdb.sh）
  1 副本：disruptionsAllowed=0，Eviction 429（DisruptionBudget）
  2 副本：disruptionsAllowed=1；并发驱逐 1×201 + 1×429
  驱逐期间 probe requests=43 ok=43 bad=0；恢复后 allowed=0
  DRILL_RESULT: PASS pdb
```

## 10. 已执行记录：RDS PostgreSQL 接管（2026-10-29）

第一个真正接入的托管依赖。实例在 cn-hangzhou，内网地址
`pgm-bp122vf7jr65sz8y.pg.rds.aliyuncs.com`（解析到 10.225.76.4，
与节点同 VPC），SSL 已开启，库与角色由 `bootstrap-rds.sql` 建立。

执行顺序（与 `docs/ACK_MANAGED_RESOURCES.md` 1.4–1.4.1 一致）：

```bash
# 1) 口令与 CA 写进 Secret（只同步已就绪的键，不打印值）
bash deploy/cluster/ack/sync-secret.sh

# 2) 第一次迁移之前：建 app schema 与库级 search_path
HOST=pgm-bp122vf7jr65sz8y.pg.rds.aliyuncs.com \
IMAGE=crpi-0z2i4q13886a2ief.cn-hangzhou.personal.cr.aliyuncs.com/expensio/postgres:16.15-alpine \
  bash deploy/cluster/ack/init-rds-schema.sh

# 3) 连通性、TLS、权限边界、版本表位置
HOST=pgm-bp122vf7jr65sz8y.pg.rds.aliyuncs.com \
IMAGE=crpi-0z2i4q13886a2ief.cn-hangzhou.personal.cr.aliyuncs.com/expensio/postgres:16.15-alpine \
  bash deploy/cluster/ack/verify-rds.sh

# 4) 切换 release 到 RDS（沿用原 values，只改连接三件套）
helm -n expensio upgrade expensio deploy/helm/expensio --reuse-values \
  --set database.host=pgm-bp122vf7jr65sz8y.pg.rds.aliyuncs.com \
  --set database.sslmode=verify-full --set database.tlsCA=true \
  --wait --timeout 20m
```

结果：

```text
DNS 解析：通过（Pod 内 getent hosts 解析到 10.225.76.4）
五个库 *_app 登录：sslmode=verify-full + 阿里云 CA，会话 ssl=on
五个库 *_migrator 登录：成功，库属主即 migrator
identity_app: create table 被拒；rolbypassrls=f
schema_migrations 位置：identity=5 expense=5 billing=3 report=2 notification=2，
  全部在 app schema，dirty=false
RDS_VERIFY: PASS
```

迁移 Hook 的幂等性单独取过证据：把 chart 渲染出的 migrate Job 去掉 Hook 注解
重跑一次，五个库的日志全部是 `no change`，版本号不变——这是"版本表位置正确、
下一次升级不会重跑迁移"的直接证据。

切换后完整链路（托管依赖模式）在 NLB 入口上跑通：

```text
MANAGED_DEPS=1 NAMESPACE=expensio INGRESS_HOST=expensio.test \
  BASE_URL=http://nlb-jhmprt2f1ov8ufn74r.cn-hangzhou.nlb.aliyuncsslb.com \
  BUSYBOX_IMAGE=registry.cn-hangzhou.aliyuncs.com/google_containers/busybox:1.27 \
  bash deploy/cluster/acceptance/smoke.sh

7 个 Deployment 1/1，runAsNonRoot/seccomp/只读根/不允许提权 全部生效
SA 无 API 权限、不挂载令牌
GET /healthz -> 200
注册→邀请→接受→费用→审批→Kafka→报表：通过（report currency_totals=1）
NetworkPolicy：default -> identity 被拒；未打标签 Pod 被拒；同 release 放行
SMOKE_RESULT: PASS
```

这一步暴露并修复了一个真实缺陷：`bootstrap-rds.sql` 原先把 `search_path` 留给
`000001_baseline` 迁移设置，但 golang-migrate 在第一个迁移之前就建
`schema_migrations`，导致版本表落在 `public`、业务表落在 `app`，下一次 migrate
会重跑全部迁移。修复是新增 `init-rds-schema.sh`（建 schema、设库级
`search_path`、把已跑歪的版本表移回 `app`），并把"必须在首次迁移之前执行"
写进文档与脚本头部。

**安全提示（待处理）**：实例白名单当前是「全部开放」（0.0.0.0/0）。实例虽有
SSL 与强口令，但公网可路由意味着口令泄露即可直连。建议收窄到三个 vSwitch
网段 `10.75.0.0/16`（本集群的 Pod/节点网段），或至少移除 0.0.0.0/0。

ApsaraMQ Kafka 的接管记录见 §12，OSS 见 §13（`OSS_VERIFY: PASS`，待切到
release）。仍未执行：
ACME 签发（需要已备案域名）、云上节点级扩容（cluster-autoscaler）与多可用区。

## 11. 已执行记录：Tair Redis 接管（2026-10-30）

实例 `r-bp1vv55oefnvdvpvci.redis.rds.aliyuncs.com:6379`（Redis 7.0.15，
standalone / master），口令在 `expensio-secrets` 的
`EXPENSIO_REDIS_PASSWORD`，release revision 6 起 `redis.addr` 指向它。
SSL 关闭——`internal/platform/redis` 只支持明文 + 口令。

```bash
HOST=r-bp1vv55oefnvdvpvci.redis.rds.aliyuncs.com \
IMAGE=crpi-0z2i4q13886a2ief.cn-hangzhou.personal.cr.aliyuncs.com/expensio/redis:7.4.11-alpine \
  bash deploy/cluster/ack/verify-redis.sh

helm -n expensio upgrade expensio deploy/helm/expensio --reuse-values \
  --set redis.addr=r-bp1vv55oefnvdvpvci.redis.rds.aliyuncs.com:6379 \
  --set services.report.redis=true --wait
```

结果：

```text
DNS：10.0.0.49  r-bp1vv55oefnvdvpvci.redis.rds.aliyuncs.com
PING -> PONG
redis_version:7.0.15  redis_mode:standalone  role:master
无口令 -> NOAUTH Authentication required.
错误口令 -> WRONGPASS invalid username-password pair or user is disabled.
SET/GET/TTL -> OK/ok/ttl=58s，key 已删除
REDIS_VERIFY: PASS
```

应用真的在用这个实例，不只是"进程起来了"：跑完 smoke 后扫描实例，
`expensio:rl:ip:<client-ip>`（gateway 的令牌桶）与
`report:version:<tenant-id>`（report 的仪表盘缓存版本）都存在。

第一次接入时连接超时，原因是实例白名单没放行集群网段；加入 `10.75.0.0/16`
后通过。这一步还暴露了一个 chart 缺陷：`services.report.redis` 默认没开，
report 拿不到 `EXPENSIO_REDIS_PASSWORD`，托管 Redis 上启动即
`NOAUTH`，仪表盘缓存被静默禁用（功能不坏，但永远不命中）。0.3.3 起
`values.yaml` 为 report 打开 `redis: true`；自建/无口令 Redis 不受影响。

## 12. 已执行记录：ApsaraMQ Kafka 接管（2026-10-30）

实例为 Serverless 版，VPC 接入点三台 broker。端口先测后选：

```text
9093  —— 本实例没有监听
9094  —— SASL_PLAINTEXT：认证能过，但口令明文过网，配置层拒绝（SASL 必须配 TLS）
9095  —— SASL_SSL：可用；证书签自私有 CA CN=AliKafka，需挂控制台下载的 CA
```

release revision 13 起 `kafka.brokers` 指向三台 broker 的 9095，`tls=true`、
`tlsCA=true`、`saslMechanism=plain`、`saslUsername=expensio_test`，
CA 来自 `expensio-secrets` 的 `EXPENSIO_KAFKA_TLS_CA`。

```bash
BROKERS='alikafka-serverless-...-1000-vpc...:9095,...-2000-vpc...:9095,...-3000-vpc...:9095' \
SASL_USERNAME=expensio_test TLS_CA=1 \
IMAGE=crpi-0z2i4q13886a2ief.cn-hangzhou.personal.cr.aliyuncs.com/expensio/kafka:3.9.2 \
  bash deploy/cluster/ack/verify-kafka.sh
```

结果：

```text
SASL_SSL 认证通过；kafka-broker-api-versions 只返回 1 个 broker（Serverless
的引导节点），Produce/Metadata 等 API 均可用
错误口令 -> Authentication failed: Invalid username or password
四个主题：expensio.expense.events.v1(.dlq)、expensio.billing.events.v1(.dlq)
三个消费组：report-expense-v1、notification-expense-v1、notification-billing-v1
            （应用启动后自行 join，不是控制台预建）
KAFKA_VERIFY: PASS
```

Serverless 禁止客户端建 Topic，自环步骤跳过；生产/消费由 smoke 证明。切换后
`MANAGED_DEPS=1` smoke 通过，`report currency_totals=1`，链路为
提交费用 → outbox → Kafka → report 读模型。

这一次暴露并修复了一个**只在托管 Kafka 上才会出现**的代码缺陷：四个服务的
`cmd/*/main.go` 各自手拼 `kafka.Security`，只填了 CA/机制/用户名/口令，漏了
`TLS`。本地 compose 的明文 Kafka 恰好 `tls=false`，单测与本地联调全绿；切到
SASL_SSL 后四个服务全部 CrashLoop，报
`kafka: 配置了 tls_ca_file 但没有开启 tls`。修复是统一的
`kafka.SecurityFromConfig`（`internal/platform/kafka/security.go`）加回归测试，
四个调用点改为一行。

其它两个运维事实：`topics.enabled=true` 时 Hook 在 Serverless 上会 30×5s
重试建 Topic，把 `helm upgrade` 卡成 `pending-upgrade`，托管环境必须关掉；
镜像 tag 必须完整推送——`images.tag` 同时作用于七个服务与 migrate，少推
migrate 或 web 会分别卡在迁移 Hook 与 `ImagePullBackOff`。

## 13. 已执行记录：OSS S3 兼容接入点（2026-10-30，验收通过）

Bucket `expensio-test`（华东1），endpoint `s3.oss-cn-hangzhou.aliyuncs.com`，
`useSSL=true`。minio-go v7.0.99 对 `aliyuncs.com` 端点自动使用虚拟主机风格
（`s3utils.IsVirtualHostSupported`），应用侧不需要额外配置。

实测通过的项（真实请求）：

```text
ListObjects             -> PASS（前 5 个对象）
PutObject               -> PASS（acceptance/<nano>.txt）
StatObject              -> PASS（size/contentType 与上传一致）
GetObject               -> PASS（内容逐字节一致）
NewMultipartUpload      -> PASS
PutObjectPart           -> PASS（5 MiB，ETag 返回）
ListObjectParts         -> PASS（1 个分片）
AbortMultipartUpload    -> PASS
PresignedGetObject      -> 签发成功；curl 回 404 是因为对象随后已被删除
PresignedPutObject      -> 200（curl 模拟浏览器直传）
PresignHeader(PUT, CT)  -> 不带 Content-Type 时 403，带上时 200
                           （应用实际上传走的就是这条路径）
RemoveObject            -> PASS（补 oss:DeleteObject 之后）
CORS 预检 OPTIONS       -> 200，Access-Control-Allow-Origin: *，
                           Allow-Methods 含 PUT，Expose-Headers: ETag
实际 PUT 响应           -> 带 Origin 时同样回 CORS 头并暴露 ETag
```

### 13.1 HeadBucket 在 OSS 上不可用（代码已适配）

`BucketExists`（HEAD Bucket）在策略**已授予** `oss:GetBucketInfo` 之后仍
返回：

```text
status 403
x-amz-bucket-region: oss-cn-hangzhou
x-oss-ec: 0003-00000001
<Code>AccessDenied</Code>
<Message>The bucket you access does not belong to you.</Message>
```

同一凭据下 `GET /?bucketInfo` 返回 200，`ListObjects` 也正常，说明不是
策略缺项。继续用原生签名做对照：

```text
GET  /?bucketInfo   -> 200
GET  /?acl          -> 403 AccessDenied
GET  /?location     -> 403 AccessDenied
GET  /（ListBuckets）-> 403 You are forbidden to oss:ListBuckets
HEAD /（原生）       -> 403
HEAD /（S3 虚拟主机） -> 403 ec=0003-00000001
HEAD /（S3 路径风格） -> 403 ec=0003-00001401 Please use virtual hosted style
```

结论：OSS 的 HeadBucket 走的是与 `GetBucketInfo` 不同的权限检查，补齐策略
这条路走不通（两次策略修改后错误不变）。应用原本在 `objectstore.New`
第一步就调 `BucketExists`，会让 expense 在 OSS 上启动即失败。

修复：启动探测改为 `MaxKeys=1` 的 `ListObjects`（`bucketReachable`），
桶不存在时仍返回 `NoSuchBucket` 并走 `MakeBucket`，本地 MinIO 行为不变；
`ossverify` 把 HeadBucket 降为 `[INFO]`，不计入失败。提交 `aafcc98`。
应用本来就需要 `oss:ListObjects` 做附件核对，没有新增权限要求。

修复后：

```text
[PASS] ListObjects（前 5 个对象，实际返回 3）
[INFO] HeadBucket 不可用（应用不依赖）: code=AccessDenied ...
...（其余 10 项 PASS）
OSS_VERIFY: PASS
```

### 13.2 排查留痕

最初对象级动作也返回 `The bucket you access does not belong to you`，
确实与"桶在另一个账号下"有关；但**这个提示并非跨账号专属**——同账号下
缺少某个 bucket 级动作时（例如 HeadBucket）也会原样返回它。判断依据应换成
"同组的 `GetBucketInfo` / `ListObjects` 是否成功"，而不是看错误文案。

状态：**OSS 验收通过**。剩余动作是把 release 的
`minio.endpoint/publicEndpoint/bucket/useSSL` 切到 OSS 并跑完整 smoke；
该步骤依赖集群 API 可达，2026-10-30 重试时 `121.43.157.247:6443`
返回 `connection refused`（NLB 主机名 NXDOMAIN），与 §11 记录的
环境级中断一致，待集群恢复后执行。

