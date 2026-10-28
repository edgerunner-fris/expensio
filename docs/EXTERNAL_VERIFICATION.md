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
| NetworkPolicy | CNI 真正执行策略（Calico / VPC CNI / Azure NPM 等） | smoke 的拒绝断言会失败 |
| 托管依赖 | PostgreSQL / Redis / Kafka / 对象存储已创建并可连 | Pod 启动或 readiness 失败 |

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
证明：没有迁移就无法注册，没有主题就无法产生报表聚合。

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
