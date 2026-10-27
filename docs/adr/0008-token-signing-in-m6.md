# ADR-0008：令牌签名——M6–M8 用 HS256，M9 迁移到 EdDSA（Ed25519）

- 状态：已接受（M6）；迁移已在 M9 落地（2026-10-27）
- 日期：2026-10-27

## 背景

六个 Go 服务用同一把 HS256 密钥校验 access token。任何持有密钥的服务都能
签发任意租户、任意角色的令牌——这是共享对称密钥的固有限制，不是实现漏洞。
M1 记录该风险时约定"M6 上 K8s 前评估改为 EdDSA/RS256"，本 ADR 给出结论。

## 决策

M6 维持 HS256，不引入非对称签名；当时计划 M7 再迁移到 Ed25519，
M7、M8 均未执行，顺延到 M9（见文末"M7/M8 实际执行情况"）。

理由：

- 当前六个服务同属一个信任域：同一集群、同一 namespace、同一运维方，
  之间只有网关→下游与内部 HTTP 调用。没有需要隔离密钥的第三方验签方。
- 密钥只从环境变量 / K8s Secret 读取，由 `scripts/env.sh` 生成 32 字节随机值，
  不入库、不入仓、日志不输出。Helm chart 只引用 `existingSecret`。
- access token 已限制短 TTL，refresh token 条件轮换并对重放整族撤销；
  令牌泄露窗口有限。
- 仓促加入 EdDSA 会同时引入 JWKS 分发、缓存、kid 轮换与多密钥并存，
  在没有真实"服务间互不信任"需求前，新增的攻击面大于收益。

## 补偿控制

- gateway 是唯一公网入口，业务服务只在集群网络内可达；Helm 的
  NetworkPolicy 默认拒绝入站，只放行 ingress-controller 与同 release 调用。
- 本地 compose 只把 gateway 与 web 发布到 `127.0.0.1`。
- 服务间内部接口另用独立 HMAC token（TTL 2 分钟），不复用用户 JWT。
- `tenant_id` 只从校验过的 JWT 提取，禁止从 body/query/path 读取（ADR-0001）。

## 后果

- 任一服务被攻陷即可伪造任意租户的 access token；NetworkPolicy 与最小镜像
  只能缩小到达面，不能消除该风险。
- 轮换签名密钥需要同时重启全部服务；当前没有 kid，无法双密钥并行。
- 不能把令牌验签外包给不共享密钥的第三方（如 ingress、API 网关）。

## M7 实际执行情况（2026-10-27）

M7 的可交付范围定为可观测性与压测，没有执行 Ed25519/JWKS 迁移。
决策依据没有变化：六个服务仍同处一个信任域、没有第三方验签方；
在 M7 内加入签名算法迁移会同时改动全部服务的验签路径与密钥分发，
与"先把链路观测起来"的目标冲突，且难以在同一次验收里区分两类回归。

当前状态仍然是纯 HS256：`grep -r "EdDSA\|jwks" internal/` 无实现。
风险 17 因此保持"已解决（决策已记录）"，但迁移时点改为 **M8 或出现
真实第三方验签需求时**，而不是默认在 M7 完成。

## M8 实际执行情况（2026-10-27）

M8 的范围是前端包体优化与 Playwright E2E，同样没有执行签名迁移。
信任域前提未变：仍是六个服务共享一把 HS256 密钥，`grep -r "EdDSA\|jwks"
internal/` 依旧无实现。迁移时点顺延到 **M9 或出现真实第三方验签需求时**。

## M9 实际执行情况（2026-10-27）

M9 完成了迁移，实现位于 `internal/platform/auth`：

- `jwt.go`：`TokenIssuer` 同时支持 EdDSA 签名与验证。签名时 header 带
  `kid`（公钥的 RFC 7638 thumbprint）；验证时按 `kid` 从 `verifyKeys` 选钥，
  未知 `kid` 直接拒绝。解析用 `jwt.WithValidMethods` 锁定允许的算法族，
  `alg=none` 与 RS/HS 混淆都会被拒。
- `jwtkeys.go`：JWKS 输出（`kty=OKP`、`crv=Ed25519`、`use=sig`、`alg=EdDSA`），
  以及私钥种子 / 公钥 list 的 base64 解析。
- identity 暴露 `/.well-known/jwks.json`，带 `Cache-Control: public, max-age=300`；
  路由由 `RegisterRoutes` 接收 `TokenSource`，签发票据与公布公钥用同一份配置。
- 配置：`EXPENSIO_JWT_PRIVATE_KEY`（仅 identity）、`EXPENSIO_JWT_PUBLIC_KEYS`
  （逗号分隔多公钥，支持轮换）、`EXPENSIO_JWT_ALLOW_HS256`。只配置公钥时
  服务可以验签但不能签发；identity 缺私钥直接启动失败。
- 分发：Compose 与 Helm 都只给 identity 注入私钥；其余五个服务只挂公钥，
  且 `ALLOW_HS256=false`，不再持有 HMAC 密钥（本地 compose 里 identity 仍保留
  旧 `EXPENSIO_JWT_SECRET` 以在迁移期接受旧令牌）。

迁移期的实际约束：旧 HS256 令牌只对 identity 有效的时间窗等于 access TTL；
其余服务从本次部署起只接受 EdDSA。等一个 TTL 之后可以把
`EXPENSIO_JWT_ALLOW_HS256` 改为 false 并删除 `EXPENSIO_JWT_SECRET`，
届时 HS256 代码路径一并删除。

运行期证据（构建、JWKS 响应、登录、E2E、全量回归）记录在
`docs/PROGRESS.md` 的 M9 章节。

## M12 收尾（2026-10-27）

迁移窗口结束后删除了 HS256 兼容路径，代码只保留 EdDSA：

- `auth.IssuerConfig` 去掉 `HS256Secret`；`TokenIssuer` 不再持有对称密钥，
  `IssueAccess` 没有私钥直接返回错误，`ParseAccess` 的
  `WithValidMethods` 只允许 `EdDSA`，HS256 令牌一律校验失败。
- 配置去掉 `jwt.secret` 与 `jwt.allow_hs256`；验签方必须配置
  `EXPENSIO_JWT_PUBLIC_KEYS`（否则拒绝启动），签发方必须配置
  `EXPENSIO_JWT_PRIVATE_KEY`。
- Compose、Helm、`.env.example` 与 `scripts/env.sh` 同步移除
  `EXPENSIO_JWT_SECRET` / `EXPENSIO_JWT_ALLOW_HS256`；Helm Secret 不再需要
  `secretKeys.jwt`。
- 测试：`TestHS256TokensAreRejected` 用真实 HS256 签名令牌验证被拒；
  配置测试验证「只给共享密钥」不再能启动；各集成测试 harness 改用
  临时 Ed25519 私钥签发。

## 遗留

1. 多公钥并存的轮换演练尚未在真实集群执行（单元测试覆盖新旧 `kid` 并存）；
2. 尚未有第三方验签方接入（ingress/API 网关仍不验签）；
3. 公钥分发走 Secret 挂载，没有实现 JWKS 拉取与缓存（服务间已经用同一把
   公钥，静态注入比运行时拉取少一个故障点）。
