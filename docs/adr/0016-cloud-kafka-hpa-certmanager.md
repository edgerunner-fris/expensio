# ADR-0016：托管 Kafka/PostgreSQL 认证、HPA 与 Ingress 证书

- 状态：已接受（M20）
- 日期：2026-10-28

## 背景

M19 收口后，"在本机跑得起来"与"能放到托管云"之间还有四处硬缺口：

1. Kafka 客户端只支持 PLAINTEXT。AWS MSK、Azure Event Hubs、Confluent Cloud
   都要求 TLS 加 SASL，现有 chart 即使填了地址也连不上；
2. PostgreSQL 只能到 `sslmode=require`：加密但不校验证书，私有 CA 的托管
   实例无法做 `verify-full`，中间人可以换掉服务端证书；
3. chart 只有固定副本数，流量上来只能手工改 values 再滚动发布；
4. Ingress 只声明 `tls.secretName`，证书要么手工创建，要么永远不生效。

这些都可以在本地完成代码与模板层面；真实托管依赖、ACME 签发与节点扩容
需要外部环境，不能假装验证过。2026-10-29 的 ACK 阶段 A 已补上集群部署、
ACR 镜像、NLB Ingress 与 Pod 级 HPA 实测，但没有补上托管 RDS/Tair/
ApsaraMQ/OSS、Terway NetworkPolicy 与 ACME。

## 决策

### 1. Kafka：显式的 TLS + SASL，拒绝 SASL_PLAINTEXT

配置面新增 `kafka.tls`、`kafka.tls_ca_file`、`kafka.sasl_mechanism`、
`kafka.sasl_username`、`kafka.sasl_password`；支持 `plain`、`scram-sha-256`、
`scram-sha-512`。口令只从 Secret 注入，且只挂给
`services.*.kafka=true` 的四个服务（expense / billing / report / notification）
——identity 与网关不消费事件，给它们注入认证参数只会在缺键时无故起不来。

`tls_ca_file` 只用于私有 CA（自建集群、企业 PKI）：为空时用系统根证书，
托管服务的证书都签自公有 CA。Chart 对应 `kafka.tlsCA=true`，从 Secret 挂载
`ca.pem`；未开启 TLS 却配置 CA 会在配置层与模板层分别失败。

校验放两层：

- 配置层：机制必须属于白名单；用户名与口令必须成对出现；**SASL 必须同时
  开启 TLS**；
- 客户端层（`internal/platform/kafka/security.go`）：同样的规则再校验一次，
  因为集成测试与工具可能绕过配置层直接构造客户端。

拒绝 SASL_PLAINTEXT 是有意的：SASL 只保护认证，不保护数据；允许它会让人
以为"加了认证就安全"，实际口令与事件都在明文网上。本地 compose 不使用
SASL，默认路径不变。

`NewProducer` 用可变参数接收 `Security`，`ConsumerOptions` 增加同名字段；
这样既有调用方与测试不需要改签名，托管配置只在 `cmd/*` 装配处传入。

### 2. PostgreSQL：`verify-full` 与可选的私有 CA

配置面新增 `db.ssl_root_cert`，非空时写入 DSN 的 `sslrootcert`。它只在
`db.sslmode` 为 `verify-ca` 或 `verify-full` 时允许配置，且不能与 `db.dsn`
同时设置——后者会整体覆盖构造出的连接串，静默忽略 CA 是最危险的组合。

Chart 对应 `database.tlsCA=true`：从 Secret 挂载 `ca.pem` 到五个数据库服务
与迁移 Job，ConfigMap 注入 `EXPENSIO_DB_SSL_ROOT_CERT`，迁移 DSN 追加
`&sslrootcert=...`。`tlsCA` 与 `sslmode` 不是校验模式时模板直接失败。

公有 CA（RDS/Aurora、Cloud SQL、Flexible Server）不需要 `tlsCA`：pgx 与
lib/pq 默认使用系统根证书。这样既支持托管服务的默认证书，也支持企业 PKI
自签 CA，而不用把 CA 内容塞进 values。

### 3. HPA：开启后由 HPA 独占副本数

`autoscaling.enabled=true` 时，Deployment 模板**不渲染 `replicas`**。这是
HPA 与 Helm 共存的关键：如果两边都写副本数，每次 `helm upgrade` 都会把
HPA 扩出来的副本改回 values 值，表现为"扩容后过一会儿又缩回去"。

- 全局 `minReplicas` / `maxReplicas` / CPU 目标，按服务覆盖（`services.<name>`，
  管理台用 `autoscaling.web`）；
- 缺少 CPU 目标、`minReplicas > maxReplicas` 在模板阶段 `fail`；
- 内存指标默认关闭：内存利用率抖动大，除非服务有明确堆上限，否则容易
  反复扩缩；
- `behavior` 原样透传，用于限制缩容速率。

没有把 PDB 一起改：`minAvailable: 1` 在副本数大于 1 时仍成立；HPA 缩到
`minReplicas: 1` 时 PDB 会让驱逐阻塞，这是需要按环境调 `minReplicas` 的
运维决策，不在 chart 里替用户猜。

### 4. cert-manager：只写注解，不创建 Issuer

Chart 在 `ingress.certManager.enabled=true` 时给 Ingress 加
`cert-manager.io/cluster-issuer` 或 `cert-manager.io/issuer` 注解，证书写入
`ingress.tls.secretName`。

为什么不在 chart 里创建 Issuer：ACME 账号私钥、邮箱、DNS solver 凭据都是
平台级配置，属于集群管理员而不是某个 release；把 ClusterIssuer 塞进业务
chart 会导致两个 release 抢同一个名字。Chart 只声明"我需要哪张证书"。

模板在 `certManager.enabled` 但 `tls.enabled=false`，或两个 issuer 都为空时
直接失败：否则 Ingress 能起来，但证书永远不会被签发，问题要等到用户访问
时才暴露。

### 5. 托管云：provider-neutral chart + 文档，不引入 Terraform

没有为三家云各写一套 manifest。Chart 只依赖标准 K8s API，差异通过 values
注入（依赖地址、Kafka 认证、TLS、Pod CIDR）；`docs/DEPLOYMENT.md` 给出
AWS/Azure/GCP 与阿里云 ACK 的依赖与网络对照。主题 Hook 的 Kafka CLI 镜像也
做成 values（`kafka.cliImage`，0.3.2 起），否则离线/ACR 环境会因拉不到
Docker Hub 的 `apache/kafka` 卡在 `pending-install`。

2026-10-29 的 ACK 阶段 A 实测证明“provider-neutral chart + values”这条路
可用：ACR 镜像、公网 NLB、Terway CNI 与 metrics-server 都没有要求改模板；
唯一的结构性缺口是 ACK 默认关闭 NetworkPolicy（需要重置节点能力并切到
DataPath V2 才能开启），以及临时依赖不能替代托管 RDS/Tair/ApsaraMQ/OSS。

不引入 Terraform / Crossplane 的原因：本轮目标是可复用的 chart 与可执行手册，
而不是为单一云维护 IaC；ACK 阶段 A 也只用 values 与验收脚本完成平台适配，
没有把云资源生命周期带进仓库。一份未验证的 IaC 比一份明确标注"待验证"的
运维文档更容易误导。首次接入真实集群时，按文档顺序跑
`deploy/cluster/acceptance/smoke.sh` 的断言，再把实际差异补回文档。

## 风险

- Kafka SASL/TLS 已对 testcontainers 启动的真实 broker 验证；但 MSK 的
  SCRAM、Event Hubs 的连接串形态属于供应商差异，首次接入时必须再验证
  `Ping` 与一条端到端事件。
- HPA 已在 k3d 与阿里云 ACK 的 metrics-server 下实测 1→3→1（ACK identity，
  CPU 908%/50%，扩容约 60s、停压后约 2m10s 回落），并据此把 chart 默认内存
  上限从 256Mi 提到 512Mi；扩缩容期间消费者再平衡、网关连接排空
  （`terminationGracePeriodSeconds=30`）与云上节点扩容仍未实测。
- cert-manager 的 ACME 流程依赖真实域名与公网入口，CI 无法覆盖。
- PostgreSQL `verify-full` 已对 testcontainers 启动的真实 TLS 实例验证，
  包括换错 CA 必须失败；云厂商的证书主机名、代理端点与 CA 轮换仍需在目标
  环境实测。
- 云厂商的 NetworkPolicy 实现差异真实存在；`smoke.sh` 的三种断言需要在
  目标 CNI 上重跑。ACK 的 Terway 默认 `disable_network_policy=true`，且节点
  能力 `has_cilium_chainer=false` 具有粘性：只改 ConfigMap 不会启动
  cilium-agent。实测需重置节点能力、重启 Terway 切到 DataPath V2，并重建
  业务 Pod 与 ingress-controller 后断言才通过。

## 测试

- `internal/platform/config`：SASL 必须有 TLS、机制白名单、凭据成对、
  CA 文件必须配合 TLS；
- `internal/platform/kafka`：`Security.options()` 的 TLS/SASL 组合、非法
  机制、缺少凭据、CA 文件缺失/内容非法、`NewProducer` 在拨号前拒绝非法
  安全参数；
- `internal/platform/kafka` 集成测试：testcontainers 启动
  `apache/kafka:3.9.2`（SASL/PLAIN + TLS + 自签 CA），完成真实握手、生产、
  消费与位点提交；错误口令与明文客户端必须被拒绝；
- `internal/platform/config`：`db.ssl_root_cert` 只允许配合 `verify-ca` /
  `verify-full`，不能与 `db.dsn` 同时设置；DSN 带 `sslrootcert`；
- `internal/platform/db` 集成测试：testcontainers 启动带 TLS 的 PostgreSQL，
  `verify-full` + 自签 CA 建连并提交事务；换一张 CA 必须握手失败；
- `helm lint` + `helm template`：默认、开启 HPA、开启 cert-manager、开启
  Kafka SASL、开启私有 CA、开启 PostgreSQL CA 六种渲染；负例（min > max、
  cert-manager 无 TLS、Kafka tlsCA 无 TLS、database tlsCA 配 require）必须失败；
- kubeconform（K8s 1.31 schema）：渲染结果全部 valid；
- HPA 实际扩缩容：k3d 与 ACK 的 metrics-server 下 identity 均 1→3→1，
  扩缩容事件与 CPU 指标均已记录（见 `docs/PROGRESS.md` M20）；
- ACK 阶段 A：ACR 镜像、公网 NLB Ingress、临时依赖下的安装、事件链路、
  HPA 与 Terway NetworkPolicy 均已执行（NetworkPolicy 在节点切到 DataPath V2
  并重建工作负载后，三条断言通过，`SMOKE_RESULT: PASS`）；
- 未执行：真实托管 RDS/Tair/ApsaraMQ/OSS、ACME 签发、云上 HPA 节点扩容与
  事件消费再平衡、各云厂商 Kafka 与 PostgreSQL 端点。
