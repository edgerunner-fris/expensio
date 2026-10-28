# ADR-0016：托管 Kafka 认证、HPA 与 Ingress 证书

- 状态：已接受（M20）
- 日期：2026-10-28

## 背景

M19 收口后，"在本机跑得起来"与"能放到托管云"之间还有三处硬缺口：

1. Kafka 客户端只支持 PLAINTEXT。AWS MSK、Azure Event Hubs、Confluent Cloud
   都要求 TLS 加 SASL，现有 chart 即使填了地址也连不上；
2. chart 只有固定副本数，流量上来只能手工改 values 再滚动发布；
3. Ingress 只声明 `tls.secretName`，证书要么手工创建，要么永远不生效。

这三项都可以在本地完成代码与模板层面，但真实云集群、ACME 签发与扩缩容
行为需要外部环境，不能假装验证过。

## 决策

### 1. Kafka：显式的 TLS + SASL，拒绝 SASL_PLAINTEXT

配置面新增 `kafka.tls`、`kafka.sasl_mechanism`、`kafka.sasl_username`、
`kafka.sasl_password`；支持 `plain`、`scram-sha-256`、`scram-sha-512`。
口令只从 Secret 注入，且只挂给 `services.*.kafka=true` 的四个服务
（expense / billing / report / notification）——identity 与网关不消费事件，
给它们注入认证参数只会在缺键时无故起不来。

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

### 2. HPA：开启后由 HPA 独占副本数

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

### 3. cert-manager：只写注解，不创建 Issuer

Chart 在 `ingress.certManager.enabled=true` 时给 Ingress 加
`cert-manager.io/cluster-issuer` 或 `cert-manager.io/issuer` 注解，证书写入
`ingress.tls.secretName`。

为什么不在 chart 里创建 Issuer：ACME 账号私钥、邮箱、DNS solver 凭据都是
平台级配置，属于集群管理员而不是某个 release；把 ClusterIssuer 塞进业务
chart 会导致两个 release 抢同一个名字。Chart 只声明"我需要哪张证书"。

模板在 `certManager.enabled` 但 `tls.enabled=false`，或两个 issuer 都为空时
直接失败：否则 Ingress 能起来，但证书永远不会被签发，问题要等到用户访问
时才暴露。

### 4. 托管云：provider-neutral chart + 文档，不引入 Terraform

没有为三家云各写一套 manifest。Chart 只依赖标准 K8s API，差异通过 values
注入（依赖地址、Kafka 认证、TLS、Pod CIDR）；`docs/DEPLOYMENT.md` 给出
EKS/AKS/GKE 的依赖与网络对照表。

不引入 Terraform / Crossplane 的原因：当前没有云账号与集群凭据，写了也
无法 apply；一份未验证的 IaC 比一份明确标注"待验证"的运维文档更容易误导。
首次接入真实集群时，按文档顺序跑 `deploy/cluster/acceptance/smoke.sh` 的
断言，再把实际差异补回文档。

## 风险

- Kafka SASL/TLS 只做了选项构造与配置校验，没有连接过真实 SASL broker；
  首次接入 MSK / Event Hubs 时必须验证 `Ping` 与一条端到端事件。
- HPA 没有在 metrics-server 下做过加压测试；扩缩容期间消费者再平衡与
  网关连接排空（`terminationGracePeriodSeconds=30`）也没有实测。
- cert-manager 的 ACME 流程依赖真实域名与公网入口，CI 无法覆盖。
- 托管 PostgreSQL 只支持到 `sslmode=require`；`verify-full` 需要 CA 挂载与
  自定义 DSN，chart 没有实现，文档已注明。
- 云厂商的 NetworkPolicy 实现差异真实存在；`smoke.sh` 的三种断言需要在
  目标 CNI 上重跑。

## 测试

- `internal/platform/config`：SASL 必须有 TLS、机制白名单、凭据成对；
- `internal/platform/kafka`：`Security.options()` 的 TLS/SASL 组合、非法
  机制、缺少凭据、`NewProducer` 在拨号前拒绝非法安全参数；
- `helm lint` + `helm template`：默认、开启 HPA、开启 cert-manager、开启
  Kafka SASL 四种渲染；负例（min > max、cert-manager 无 TLS）必须失败；
- kubeconform（K8s 1.31 schema）：31 个资源全部 valid；
- 未执行：真实云集群、ACME 签发、HPA 实际扩缩容、真实 SASL broker。
