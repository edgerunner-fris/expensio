# 在 ACK 上接入托管依赖：逐步操作手册

- 编写日期：2026-10-29
- 适用集群：ACK 托管集群 `c772e4d3c99764bdf906ce05a63e559e0`（cn-hangzhou，
  命名空间 `expensio`，release `expensio`）
- 状态：**操作手册，尚未在真实托管实例上执行**。ACK 阶段 A/B 已实测的是
  集群平台本身（ACR 镜像、NLB Ingress、HPA、Terway NetworkPolicy、再平衡/
  排空/PDB 演练）；RDS / Tair / ApsaraMQ / OSS / ACME / 节点级扩容仍未验证。
  执行完后按第 9 节回填证据，再改状态。

本文只回答两件事：**你要在控制台做什么**，以及**做完交给我什么**。
后半段（第 9 节）是我拿到资源后会执行的验收命令。

---

## 0. 三条硬约束

### 0.1 网络：白名单只加 vSwitch 网段

Terway 的 Pod IP 直接从 vSwitch 分配，节点 IP 也在同一网段。RDS、Tair、
ApsaraMQ 的白名单必须覆盖**全部三个可用区的 vSwitch**，只加节点 IP 会在
Pod 漂移后随机断连。

| 可用区 | vSwitch ID |
| --- | --- |
| cn-hangzhou-i | `vsw-bp1e1ayk3b828zwq7en7d` |
| cn-hangzhou-j | `vsw-bp19b6bpzq1gs5876g6oi` |
| cn-hangzhou-k | `vsw-bp1gsjjq1mo1ldrt45zbv` |

网段在 VPC 控制台 → 交换机 → 对应 vSwitch 详情里看「IPv4 网段」。当前集群
观测到的节点与 Pod 都在 `10.75.255.0/24`，但**不要只加这一个**：另外两个
vSwitch 是 Pod 扩容到新可用区时的落点。

安全组 `sg-bp11n3094igg9rqohrca` 不参与 RDS/Tair 的访问控制（它们用自己的
白名单），不要为了"打通"去放宽安全组。

**不要**购买公网地址，**不要**在白名单里填 `0.0.0.0/0`。所有连接走 VPC 内网。

### 0.2 密钥：只进文件，不进对话、不进仓库

我需要的每个密钥，你写到 WSL 里的一个文件，**文件名就是 Secret 的键名**：

```bash
mkdir -p ~/expensio-ack/creds && chmod 700 ~/expensio-ack/creds

# 交互输入，值不落在 shell history 里；printf 不带换行，换行会成为口令的一部分
read -rsp 'RDS 应用账号口令: ' v; printf '%s' "$v" > ~/expensio-ack/creds/EXPENSIO_DB_APP_PASSWORD; unset v; echo
read -rsp 'RDS 迁移账号口令: ' v; printf '%s' "$v" > ~/expensio-ack/creds/EXPENSIO_DB_MIGRATOR_PASSWORD; unset v; echo
read -rsp 'Tair 密码: '        v; printf '%s' "$v" > ~/expensio-ack/creds/EXPENSIO_REDIS_PASSWORD;      unset v; echo
read -rsp 'ApsaraMQ 密码: '    v; printf '%s' "$v" > ~/expensio-ack/creds/EXPENSIO_KAFKA_SASL_PASSWORD; unset v; echo
read -rsp 'OSS AccessKey ID: ' v; printf '%s' "$v" > ~/expensio-ack/creds/EXPENSIO_MINIO_ACCESS_KEY;   unset v; echo
read -rsp 'OSS AccessKey Secret: ' v; printf '%s' "$v" > ~/expensio-ack/creds/EXPENSIO_MINIO_SECRET_KEY; unset v; echo

chmod 600 ~/expensio-ack/creds/*
```

CA 证书是文件，直接拷：

```bash
# RDS 控制台 → 数据安全性 → SSL 加密 → 下载 CA 证书（ApsaraDB-CA-Chain.pem）
cp ~/Downloads/ApsaraDB-CA-Chain.pem ~/expensio-ack/creds/EXPENSIO_DB_TLS_CA
```

然后合并进集群里已有的 Secret（**不重建**，否则会丢掉 JWT/SMTP 等键）：

```bash
cd ~/projects/expensio
bash deploy/cluster/ack/sync-secret.sh
```

脚本只回显键名与字节数，不打印值；末尾会提示需要 `rollout restart`。
资源是分步开通的，所以脚本默认只同步已就绪的键，把缺失的列在 stderr；
`STRICT=1 bash ...` 才会要求七个键全部齐全（正式部署前用它做最终检查）。
**不要把任何密钥贴进对话或提交到仓库**——我不需要看到值，只需要你确认
`sync-secret.sh` 成功。

### 0.3 回执：只要地址与 ID，不要密码

每完成一项，回我一条类似这样的消息即可：

```text
RDS 已完成：pgm-xxxx.pg.rds.aliyuncs.com:5432，白名单已加 3 个 vSwitch
```

---

## 1. RDS PostgreSQL

### 1.1 创建实例

控制台路径：**云数据库 RDS → 实例列表 → 创建实例**

| 项 | 值 | 说明 |
| --- | --- | --- |
| 数据库类型 | PostgreSQL | 与本地 compose 的 16 对齐 |
| 版本 | 16 | 迁移只用 16 支持的语法 |
| 系列 | 高可用版（或基础版） | 基础版单节点只适合验收，别长期用 |
| 地域 / 可用区 | 华东1（杭州），单可用区即可 | 与 ACK 同地域 |
| 网络类型 | **专有网络** | 与 ACK 同一个 VPC |
| vSwitch | `vsw-bp1e1ayk3b828zwq7en7d`（i 区） | 任选其一 |
| 规格 | 2 核 4 GB 起 | 五个库、低流量足够 |
| 存储 | ESSD PL1 20 GB 起 | 最低档即可 |
| 公网地址 | **不开通** | 全程 VPC 内网 |

创建后等实例状态变成「运行中」，记下**内网地址**（`pgm-xxxx.pg.rds.aliyuncs.com`）。

### 1.2 白名单

**数据安全性 → 白名单设置 → 修改**，模式选「专有网络」，把 0.1 节三个
vSwitch 的网段加进去。

如果实例是「高安全模式」，还要新建白名单组并把网段放进组里；验收阶段用
普通模式即可。

### 1.3 账号

**账号管理 → 创建账号**，建一个高权限账号：

- 账号名：`expensio_admin`
- 账号类型：高权限账号
- 密码：自定（这个密码不需要给我，只用于执行下面的初始化 SQL）

高权限账号（`pg_rds_superuser`）可以 `CREATE ROLE`，但**不能** `CREATE USER`
之后直接拥有超级权限——这正是我们需要的：应用账号全程不是超级用户。

### 1.4 建库与角色

仓库里已经准备好幂等脚本 [bootstrap-rds.sql](../deploy/cluster/ack/bootstrap-rds.sql)：

```bash
cd ~/projects/expensio
read -rsp '应用账号口令（与 0.2 节一致）: ' APP_PW; echo
read -rsp '迁移账号口令（与 0.2 节一致）: ' MIGRATOR_PW; echo

psql "host=pgm-<id>.pg.rds.aliyuncs.com port=5432 dbname=postgres \
      user=expensio_admin sslmode=verify-full \
      sslrootcert=$HOME/expensio-ack/creds/EXPENSIO_DB_TLS_CA" \
     -v app_pw="$APP_PW" -v migrator_pw="$MIGRATOR_PW" \
     -f deploy/cluster/ack/bootstrap-rds.sql

unset APP_PW MIGRATOR_PW
```

`sslmode=verify-full` 会校验主机名。若报 `x509: certificate is valid for ...`，
说明该实例的证书 SAN 不覆盖这个内网域名，改用 `verify-ca` 并把实际报错
发我——这是需要记录的环境差异，不是可以静默跳过的。

脚本只做三件事：建 5 个 `_migrator`、5 个 `_app`（`NOBYPASSRLS`）、
5 个库（属主是 migrator）。**它不建 schema，也不设 `search_path`**——见下一节，
这一步必须发生在第一次迁移之前。

预期回执（最后两条 SELECT 的输出）：

```text
identity_app / identity_migrator / expense_app / ... （共 10 行）
identity_db | identity_migrator
expense_db  | expense_migrator
...
```

#### 1.4.1 第一次迁移之前：建 schema 与库级 `search_path`

```bash
HOST=pgm-<id>.pg.rds.aliyuncs.com \
IMAGE=crpi-<id>.cn-hangzhou.personal.cr.aliyuncs.com/expensio/postgres:16.15-alpine \
  bash deploy/cluster/ack/init-rds-schema.sh
```

这一步不能省，原因是一个真实的踩坑：golang-migrate 在应用**第一个**迁移之前
就建版本表 `schema_migrations`，用的是连接当时的 `search_path`。`search_path`
若由 `000001_baseline` 迁移自己设置，版本表已经先落在 `public`，而业务表落
在 `app`；下一次 migrate 在 `app` 里找不到版本表，会把所有迁移重跑一遍，
多数在 `CREATE TABLE` 处失败并把库标成 `dirty`。本地 compose 没暴露这个
问题，因为它的初始化脚本先设了 `search_path`。

脚本以 `*_migrator` 身份执行（库属主，有权 `CREATE SCHEMA` 与
`ALTER DATABASE`），幂等：建 `app` schema、把库级 `search_path` 设为
`app, public`，并把**已经跑歪**的 `public.schema_migrations` 移回 `app`。
最后一行 `RDS_SCHEMA_INIT: PASS` 才算就绪。

脚本必须在集群里跑，因为 RDS 白名单只放行 vSwitch 网段，跳板机上执行
`CREATE SCHEMA` 会连不上；口令同样只以文件形式挂进临时 Pod。

### 1.5 开启 SSL 并下载 CA

**数据安全性 → SSL 加密 → 开启**，然后下载 CA 证书，按 0.2 节放到
`~/expensio-ack/creds/EXPENSIO_DB_TLS_CA`。

> RDS 的证书签自阿里云自建 CA，不在容器的系统根证书里，所以
> `database.tlsCA` 必须是 `true`。这一点与 AWS RDS（公有 CA）不同。

### 1.6 交给我什么

- 内网地址与端口
- 确认白名单已加三个 vSwitch 网段
- 确认 `sync-secret.sh` 成功、SSL 已开启

---

## 2. Tair（兼容 Redis）

控制台：**云数据库 Tair（兼容 Redis）→ 创建实例**

| 项 | 值 |
| --- | --- |
| 版本 | 社区版（兼容 Redis 7） |
| 架构 | 标准版 / 主从 |
| 规格 | 1 GB 起 |
| 地域 | 华东1（杭州），与 ACK 同 VPC |
| vSwitch | `vsw-bp1e1ayk3b828zwq7en7d` |
| 密码 | **必须设置**，不要用免密 |
| 白名单 | 0.1 节的三个 vSwitch 网段 |

连接地址用**内网连接地址**（形如 `r-xxxx.redis.rds.aliyuncs.com:6379`），
不要用「直连地址」——直连地址绕过代理，扩容后客户端会连到错误的节点。

> **已知缺口**：`internal/platform/redis` 目前只支持「明文 + 密码」，
> chart 没有 `redis.tls` 开关。因此**不要开启 SSL 加密**，否则应用连不上。
> 这条已列在 `docs/PROGRESS.md` 的风险清单里；要补 TLS 需要改客户端与
> chart，属于后续工作，不在本轮。

交给我的：内网地址 + 密码文件已就绪。

### 2.1 验收（2026-10-30 实测）

```bash
HOST=r-bp1vv55oefnvdvpvci.redis.rds.aliyuncs.com \
IMAGE=crpi-0z2i4q13886a2ief.cn-hangzhou.personal.cr.aliyuncs.com/expensio/redis:7.4.11-alpine \
  bash deploy/cluster/ack/verify-redis.sh
```

脚本从集群内起临时 Pod，口令经 `REDISCLI_AUTH` 传入，断言 DNS、PING、
版本/角色、无口令与错误口令两个负例、SET/GET/TTL。最后一行
`REDIS_VERIFY: PASS` 才算通过。

白名单没放行时现象是 **TCP 超时**（`Terminated`），不是认证失败：
`redis-cli` 连内核超时都等不到。这时把 `10.75.0.0/16` 加入实例白名单。
口令错误才是 `NOAUTH` / `WRONGPASS`。开了 SSL 则是握手失败——本项目的
客户端不支持 TLS。

---

## 3. ApsaraMQ for Kafka

控制台：**云消息队列 Kafka 版 → 创建实例**。Serverless 版最省（按量），
标准版/专业版按规格计费；两者都支持 SASL_SSL。

配置要点：

1. **VPC 接入点**：实例详情 → 接入点信息，取「VPC 网络」+ **SASL_SSL** 的
   那一行，不要用公网接入点。端口随实例类型不同，必须按控制台列出的来：

   | 端口 | 协议 | 实测 |
   | --- | --- | --- |
   | 9092 | PLAINTEXT | 能连，但事件与口令裸奔；项目配置层不接受 |
   | 9093 | — | 本实例（Serverless）**没有监听** |
   | 9094 | SASL_PLAINTEXT | 认证能过，口令明文过网；配置层拒绝（SASL 必须配 TLS） |
   | 9095 | SASL_SSL | 可用；证书签自私有 CA `CN=AliKafka`，必须挂 CA |

   因此 values 用 `brokers: ...:9095`、`tls: true`、`tlsCA: true`。
   CA 从控制台「接入点信息 → 下载 CA 证书」取，写到
   `~/expensio-ack/creds/EXPENSIO_KAFKA_TLS_CA`，由 `sync-secret.sh` 同步。
   不挂 CA 的现象是 `PKIX path building failed`，不是认证失败。
2. **SASL 用户**：实例 → 用户管理 → 创建用户，类型选 **PLAIN**
   （对应 chart 的 `saslMechanism: plain`；SCRAM-SHA-256 也支持，两者必须一致）。
3. **Topic**（4 个，分区 6；实例支持副本 3 就用 3，否则 1）：
   - `expensio.expense.events.v1`
   - `expensio.expense.events.v1.dlq`
   - `expensio.billing.events.v1`
   - `expensio.billing.events.v1.dlq`
4. **消费组**（3 个）：
   - `report-expense-v1`
   - `notification-expense-v1`
   - `notification-billing-v1`
5. **授权**：SASL 用户对 4 个 Topic 有读写、对 3 个 Group 有读。

为什么 Topic 在控制台建、而不是让 chart 的 Hook 建：托管实例的 SASL 用户
通常没有 `CreateTopics` 权限，`kafka-topics.sh --create` 会被 ACL 拒绝。
因此 values 里 `topics.enabled=false`，Hook 不执行。

> 如果实例开了 IP 白名单，同样加 0.1 节的三个 vSwitch 网段。

交给我的：VPC 接入点（含端口）、SASL 用户名与机制、确认 4 Topic + 3 Group
已建并授权、密码文件与 CA 文件已就绪。

### 3.1 验收（2026-10-30 实测）

```bash
BROKERS='alikafka-serverless-...-1000-vpc...:9095,...-2000-vpc...:9095,...-3000-vpc...:9095' \
SASL_USERNAME=expensio_test TLS_CA=1 \
IMAGE=crpi-0z2i4q13886a2ief.cn-hangzhou.personal.cr.aliyuncs.com/expensio/kafka:3.9.2 \
  bash deploy/cluster/ack/verify-kafka.sh
```

脚本从集群内起临时 Pod（口令经 `--command-config` 传，不进 argv），断言
接入点 DNS、SASL_SSL 认证、错误口令负例、四个业务 Topic、三个消费组，
最后一行 `KAFKA_VERIFY: PASS` 才算通过。

`IMAGE` 必须指向集群能拉到的镜像：ACK 节点直连 Docker Hub 会超时，
默认的 `apache/kafka:3.9.2` 在这种环境下拉不下来。

Serverless 实例禁止客户端建 Topic，脚本第 6 步（临时主题自环）会跳过；
生产/消费的真实证明是 smoke 里的「提交费用 → 事件 → report 读模型」。

---

## 4. OSS（附件存储）

### 4.1 Bucket

控制台：**对象存储 OSS → Bucket 列表 → 创建 Bucket**

| 项 | 值 |
| --- | --- |
| 名称 | `expensio-test` |
| 地域 | 华东1（杭州） |
| 存储类型 | 标准存储 |
| 读写权限 | **私有** |
| 版本控制 | 关闭（验收阶段） |

### 4.2 RAM 用户与最小权限

**RAM 访问控制 → 用户 → 创建用户**：勾选「使用永久 AccessKey 访问」，
记下 AccessKey ID / Secret（只写进 0.2 节的文件）。

给它一条**只作用于这个 Bucket** 的自定义策略：

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "oss:ListObjects",
        "oss:PutObject",
        "oss:GetObject",
        "oss:DeleteObject",
        "oss:AbortMultipartUpload",
        "oss:ListParts"
      ],
      "Resource": [
        "acs:oss:*:*:expensio-test",
        "acs:oss:*:*:expensio-test/*"
      ]
    }
  ]
}
```

不要给 `AliyunOSSFullAccess`，也不要用主账号 AccessKey。

### 4.3 跨域（浏览器直传必需）

**Bucket → 数据安全 → 跨域设置 → 创建规则**：

- 来源：`https://<你的前端域名>`（验收阶段可临时用 `*`，上线前收紧）
- 允许 Methods：`PUT`、`GET`、`HEAD`、`POST`
- 允许 Headers：`*`
- 暴露 Headers：`ETag`
- 缓存时间：600 秒

附件是浏览器拿 presigned URL 直传的；没有 CORS，上传会以 403 失败，
而服务端日志里看不到任何错误——这是最容易漏的一步。

### 4.4 应用侧配置

| values | 值 |
| --- | --- |
| `minio.endpoint` | `s3.oss-cn-hangzhou-internal.aliyuncs.com`（VPC 内网接入点，不带 scheme） |
| `minio.publicEndpoint` | `s3.oss-cn-hangzhou.aliyuncs.com`（浏览器可达；presigned URL 按这个 Host 签名） |
| `minio.bucket` | `expensio-test` |
| `minio.useSSL` | `true` |

集群内用内网接入点可以完全不依赖节点出公网；浏览器直传/下载必须走公网
接入点，所以两个字段不能写成同一个值。OSS 只接受**虚拟主机风格**
（`<bucket>.<endpoint>`），路径风格返回 `0003-00001401 Please use virtual
hosted style to access`。minio-go v7.0.99 的 `s3utils.IsVirtualHostSupported`
对 `aliyuncs.com` 端点直接返回 true，因此应用不需要额外配置 lookup 方式；
2026-10-30 在 `expensio-test.s3.oss-cn-hangzhou.aliyuncs.com` 上实测：ListObjects、
PutObject、StatObject、GetObject、分片上传四步、预签名 GET/PUT 均通过。

两点实测出来的权限细节：

- `BucketExists`（HeadBucket）在 OSS 上**不可用**：即使策略已授予
  `oss:GetBucketInfo`，仍返回 `403 AccessDenied`（Message 为
  `The bucket you access does not belong to you`，`x-oss-ec=0003-00000001`）。
  同一凭据下 `GET /?bucketInfo` 与 `ListObjects` 都成功，说明这不是策略
  缺项，而是 OSS 对 S3 HeadBucket 走了另一条权限检查。应用启动探测因此
  改为 `MaxKeys=1` 的 `ListObjects`（`internal/platform/objectstore.ensureBucket`），
  只要求已有的 `oss:ListObjects`。
- 删除附件需要 `oss:DeleteObject`；缺它时返回
  `You have no right to access this object because of bucket acl`，
  上传/下载都正常，只有"删除附件"这一个功能静默失败。

服务端只做 `ListObjects` 探测、`StatObject`、`RemoveObject` 与**签发** URL，
不把对象字节经由 API 进程；因此不涉及 OSS 不支持的 `aws-chunked` 上传。

> 若实测不兼容，退路是在集群内部署 MinIO（ACK 云盘 StorageClass
> `alicloud-disk-essd`）+ 一个 Ingress 暴露 `files.<域名>`，把
> `minio.endpoint` 指到 `minio.expensio.svc:9000`、`publicEndpoint` 指到
> 该域名。这条路会多一个需要备份的有状态组件，只在 OSS 实测失败时启用。

交给我的：Bucket 名、确认 CORS 已配、AccessKey 文件已就绪，并确认
AccessKey 所属账号与 Bucket 所属账号**是同一个**。注意
`The bucket you access does not belong to you` 并不是跨账号的专属提示：
同一账号下缺少某个 bucket 级动作时也会返回它（HeadBucket 就是这样），
要区分得靠"同一组的 `GetBucketInfo` / `ListObjects` 是否成功"。

### 4.5 验收

```bash
OSS_ENDPOINT=s3.oss-cn-hangzhou.aliyuncs.com OSS_BUCKET=expensio-test \
OSS_AK="$(cat ~/expensio-ack/creds/EXPENSIO_MINIO_ACCESS_KEY)" \
OSS_SK="$(cat ~/expensio-ack/creds/EXPENSIO_MINIO_SECRET_KEY)" \
  go run ./cmd/ossverify
```

逐条打印 `[PASS]/[FAIL]`，最后一行 `OSS_VERIFY: PASS` 才算通过。它覆盖
ListObjects（同时就是应用的桶可达性探测）、PutObject、StatObject、
GetObject、分片上传四步、RemoveObject、两种预签名 PUT（不带 Content-Type
与把 Content-Type 签进 URL），并把预签名 GET/PUT 的 URL 写到 `/tmp`
（不打印含签名的 URL）。HeadBucket 只打印 `[INFO]`、不计失败，
因为它在 OSS 上恒为 403（见 4.4）。
预签名链接用 `curl` 再验一次：

```bash
curl -s -o /tmp/oss-get -w '%{http_code}\n' "$(cat /tmp/oss-presign-get.txt)"
curl -s -o /dev/null -w '%{http_code}\n' -X PUT -H 'Content-Type: text/plain' \
  --data-binary 'x' "$(cat /tmp/oss-presign-put.txt)"
# 带 Content-Type 签名的 URL：带头必须 200，不带头必须 403
curl -s -o /dev/null -w '%{http_code}\n' -X PUT -H 'Content-Type: text/plain' \
  --data-binary 'x' "$(cat /tmp/oss-presign-put-ct.txt)"
curl -s -o /dev/null -w '%{http_code}\n' -X PUT \
  --data-binary 'x' "$(cat /tmp/oss-presign-put-ct.txt)"
curl -s -D - -o /dev/null -X OPTIONS \
  -H 'Origin: http://expensio.test' -H 'Access-Control-Request-Method: PUT' \
  https://expensio-test.s3.oss-cn-hangzhou.aliyuncs.com/acceptance/probe.txt \
  | grep -i access-control
```

`OSS_LIST_BUCKETS=1` 会额外尝试 `ListBuckets`（策略没授权时打印
`oss:ListBuckets` 被拒，属正常）。

---

## 5. cert-manager 与 ACME

前置条件（缺一不可）：

1. 一个**真实域名**，A 记录指向 `nginx-ingress-lb` 的公网 IP；
2. 大陆地域的 80/443 站点需要 **ICP 备案**。没有备案，NLB 上的 HTTP
   会被拦截，ACME 的 HTTP-01 校验无法通过；域名在境外注册也一样。
   没有备案域名时，本项只能停留在"chart 已支持、未签发"。

### 5.1 安装 cert-manager

```bash
helm repo add jetstack https://charts.jetstack.io
helm repo update
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --set crds.enabled=true --version <当前稳定版>
```

版本以 `helm search repo jetstack/cert-manager --versions` 为准，装完记录
实际版本——验收证据里要写版本号。

### 5.2 HTTP-01（推荐，少一个 webhook）

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-prod
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: <你的邮箱>
    privateKeySecretRef:
      name: letsencrypt-prod-account-key
    solvers:
      - http01:
          ingress:
            ingressClassName: nginx
```

```bash
kubectl apply -f clusterissuer.yaml
kubectl get clusterissuer letsencrypt-prod -o jsonpath='{.status.conditions}'
```

DNS-01 也可以：需要再加 `alidns-webhook` 与一个只授权该 zone 的 RAM 用户。
步骤比 HTTP-01 多两步，且 webhook 版本要与 cert-manager 对齐；验收阶段
优先 HTTP-01。

### 5.3 让 chart 使用它

```yaml
ingress:
  host: <真实域名>
  tls:
    enabled: true
    secretName: expensio-tls
  certManager:
    enabled: true
    clusterIssuer: letsencrypt-prod
```

`certManager.enabled=true` 时模板会拒绝 `tls.enabled=false`，避免"注解加了、
证书没地方放"。

### 5.4 验收

```bash
kubectl -n expensio get certificate,order,challenge
kubectl -n expensio describe certificate expensio-tls
curl -vI https://<真实域名>/healthz
```

Certificate 必须是 `Ready=True`，`curl` 的证书链必须是 Let's Encrypt 而不是
ingress 默认的自签证书。

---

## 6. 节点级自动伸缩

ACK 的 HPA 只扩 Pod；节点不够时 Pod 会一直 Pending。节点级扩容需要在
控制台开启集群自动伸缩（会在 `kube-system` 部署 cluster-autoscaler）。

1. **集群 → 基本信息 → 集群自动伸缩 → 开启**；
2. **节点管理 → 节点池 → 创建节点池**：
   - 付费方式：按量付费（验收完成后好释放）
   - 实例规格：与现有节点同族（避免镜像与内核差异）
   - 可用区：至少选两个（见第 7 节）
   - **自动伸缩**：开启，设 min / max（例如 min=0、max=8）
   - 系统盘与标签沿用现有节点池
3. 只有「开启自动伸缩」的节点池会被扩缩；已有的固定节点池不会。

验收脚本（会真的触发扩容，几分钟后回收）：

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: autoscale-probe
  namespace: default
spec:
  replicas: 6
  selector:
    matchLabels: { app: autoscale-probe }
  template:
    metadata:
      labels: { app: autoscale-probe }
    spec:
      containers:
        - name: pause
          image: registry.cn-hangzhou.aliyuncs.com/expensio/pause:3.9
          resources:
            requests: { cpu: "1", memory: 512Mi }
EOF
kubectl get pods -w          # 先看到 Pending
kubectl get nodes -w         # 2–5 分钟后新节点加入，Pod 转为 Running
kubectl delete deploy autoscale-probe -n default
```

`pause` 镜像已在 ACR（或任一同 registry 的空闲镜像），避免再拉 Docker Hub。

> 注意：自动缩容（节点回收）默认有约 10 分钟的冷却，属于正常现象。
> 验收时记录"从 Pending 到 Running 的耗时"，这比截图更有说服力。

---

## 7. 多可用区

当前 4 个节点都在同一个网段/可用区，单区故障会整站不可用。多可用区是
**可用性**需求，不影响功能验收：

1. 节点池的可用区选 `cn-hangzhou-i/j/k`，三个 vSwitch 都要在池里；
2. chart 支持全局 `affinity`，用软反亲和把同一服务的副本分散到不同可用区：

```yaml
affinity:
  podAntiAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          topologyKey: topology.kubernetes.io/zone
          labelSelector:
            matchLabels:
              app.kubernetes.io/part-of: expensio
```

用 `preferred` 而不是 `required`：副本数少于可用区数时，`required` 会让
Pod 永久 Pending，比"挤在一个区"更糟。

验收：`kubectl -n expensio get pods -o wide`，两个以上副本的 Deployment
应分布在不同节点，节点的 `topology.kubernetes.io/zone` 标签不同。

---

## 8. 一次安装的命令形态

```bash
cd ~/projects/expensio
helm -n expensio upgrade --install expensio deploy/helm/expensio \
  -f ~/expensio-ack/values-managed.yaml \
  --set images.tag=$(git rev-parse --short HEAD) \
  --wait --timeout 15m
```

注意：

- 迁移 Hook 会对**真实 RDS** 执行；第一次跑之前确认五个库已存在且属主是
  `*_migrator`，否则会在 `CREATE SCHEMA` 处权限失败；
- 旧的临时依赖（`deps.yaml` 里的 postgres/redis/kafka/minio）不要删，
  先保留到验收通过——回滚时还要用；
- `--wait` 会等 rollout；HPA 开启时 Deployment 不渲染 replicas，这是预期行为。

---

## 9. 拿到资源后我会执行的验收

### 9.1 依赖连通性

```bash
HOST=pgm-<id>.pg.rds.aliyuncs.com bash deploy/cluster/ack/verify-rds.sh
```

脚本起一个临时 Pod（口令从 Secret 以文件挂载，不进命令行参数），断言：

1. Pod 内能解析并连上 RDS——白名单按 vSwitch 网段放行，跳板机能连不代表
   Pod 能连；
2. 五个库的 `*_app` 账号都能用 `sslmode=verify-full` 建连，且会话里 `ssl=on`；
3. 五个库的 `*_migrator` 账号能连，且库属主是它自己；
4. `identity_app` 不能建表、`rolbypassrls=f`；
5. 五个库的 `schema_migrations` 都必须在 `app` schema，并打印
   `version|dirty=false`。落在 `public` 直接判失败——那正是 1.4.1 描述的
   "下一次升级重跑迁移"故障。

最后一行是 `RDS_VERIFY: PASS` 才算通过。集群访问不到 Docker Hub 时用
`IMAGE=crpi-<id>.cn-hangzhou.personal.cr.aliyuncs.com/expensio/postgres:16.15-alpine`
指向 ACR 的同版本镜像。

顺序是：`bootstrap-rds.sql` → `sync-secret.sh` → `init-rds-schema.sh` →
`verify-rds.sh` → `helm upgrade --wait`。迁移 Hook 跑完后再执行一次
`verify-rds.sh`，状态应为 `version=5/5/3/2/2`（identity/expense/billing/
report/notification）、`dirty=false`；再 `helm upgrade` 一次，migrate Job 的
日志应当五个库全部 `no change`——这是"版本表位置正确"的最终证据。

### 9.2 完整链路（托管依赖模式）

```bash
MANAGED_DEPS=1 NAMESPACE=expensio INGRESS_HOST=expensio.test \
  BASE_URL=http://<NLB> bash deploy/cluster/acceptance/smoke.sh
```

`MANAGED_DEPS=1` 会跳过"命名空间内有没有 postgres/kafka Pod"这类检查，
功能链路（注册 → 邀请 → 费用 → 审批 → 报表 → 事件）仍会真实执行。

### 9.3 OSS 端到端（不只是"进程起来了"）

1. 调用 `POST /api/v1/expenses/{id}/attachments/presign` 拿 presigned URL；
2. 用 `curl -X PUT --data-binary @file` 把字节发到 OSS（**不经过 API**）；
3. 调 `POST .../attachments` 确认，再 `GET .../attachments` 拿下载链接；
4. 下载后比对 `sha256`。

第 2 步失败通常就是 CORS/虚拟主机风格问题，见 4.3 与 4.4。

### 9.4 滚动与中断演练（仍适用）

```bash
NAMESPACE=expensio INGRESS_HOST=expensio.test BASE_URL=http://<NLB> \
  bash deploy/cluster/acceptance/drills/drain.sh
NAMESPACE=expensio INGRESS_HOST=expensio.test BASE_URL=http://<NLB> \
  bash deploy/cluster/acceptance/drills/pdb.sh
```

`drills/rebalance.sh` **不适用**：它依赖命名空间内的 Kafka Pod 执行
`kafka-consumer-groups.sh`。托管 Kafka 的再平衡要改造成用外部 CLI +
SASL 配置，本轮不做，已在风险清单里标注。

### 9.5 回填

验收结果写进 `docs/PROGRESS.md`（带真实命令输出）与本文件开头的状态行，
再把 `docs/DEPLOYMENT.md`、`docs/EXTERNAL_VERIFICATION.md` 中对应的
"未验证"改成实测。

---

## 10. 已知限制（执行前必须知道）

| 项 | 状态 | 影响 |
| --- | --- | --- |
| Redis 只支持明文 + 密码 | 未解决 | 不能在 Tair 上开 SSL；VPC 内明文 |
| ApsaraMQ Topic/Group 由控制台维护 | 设计取舍 | chart 的 `topics` Hook 关闭，主题漂移靠人工 |
| OSS S3 兼容性 | 待实测 | 失败则退到集群内 MinIO + 云盘 |
| 大陆地域域名备案 | 外部前置 | 无备案则 ACME 无法签发 |
| 节点级扩容 | 未实测 | 需要先在控制台开启自动伸缩 |
| 多可用区 | 未实测 | 需要新建多可用区节点池 |

## 参考

- 阿里云 OSS S3 兼容性与虚拟主机风格：
  <https://help.aliyun.com/zh/oss/developer-reference/compatibility-with-amazon-s3>
- 使用 AWS SDK 访问 OSS（S3 接入点）：
  <https://help.aliyun.com/zh/oss/developer-reference/use-aws-sdks-to-access-oss>
- RDS PostgreSQL 账号与高权限账号权限：
  <https://help.aliyun.com/zh/rds/apsaradb-rds-for-postgresql/create-an-account-on-an-apsaradb-rds-for-postgresql-instance>
- ApsaraMQ for Kafka 创建 SASL 用户：
  <https://help.aliyun.com/zh/apsaramq-for-kafka/>
