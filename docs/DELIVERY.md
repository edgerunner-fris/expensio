# 交付说明与启动指南

本文是"拿到仓库后从零跑起来"的唯一入口。设计取舍看
[ARCHITECTURE.md](ARCHITECTURE.md)，云上验证顺序看
[EXTERNAL_VERIFICATION.md](EXTERNAL_VERIFICATION.md)，逐里程碑的实测输出
与未关闭风险看 [PROGRESS.md](PROGRESS.md)。

## 1. 交付物

| 类别 | 位置 | 说明 |
| --- | --- | --- |
| 六个 Go 服务 | `cmd/{gateway,identity,expense,billing,report,notification}` | 各带迁移、健康探针、集成测试 |
| 管理台 | `frontend/` | Vue 3 + Vite，产物由 `internal/web` 静态托管 |
| 本地整栈 | `docker-compose.yml` | `core` / `obs` / `mail` / `app` / `load` / `e2e` 六档 profile |
| 容器镜像 | `Dockerfile`、`Dockerfile.migrate`、`Dockerfile.web` | 多阶段构建，非 root，健康检查 |
| Helm chart | `deploy/helm/expensio` | 0.3.3；迁移与建主题为 hook Job |
| 集群验收 | `deploy/cluster/acceptance/` | `smoke.sh`、`oss-e2e.sh`、`drills/` |
| 接口契约 | `api/openapi.yaml` | redocly 校验，且与真实路由双向比对 |
| 文档 | `docs/` | 架构、部署、外部验证、ACK 资源、进度、ADR、面试讲解 |

## 2. 三档启动

### A. 本机整栈（不需要任何云资源，推荐）

前置：Docker Desktop（WSL2 后端，开启 WSL 集成）、Go 1.27.3、Node 24、
GNU Make、bash。Windows 先跑 `scripts/preflight.ps1`，WSL/Linux 跑
`./scripts/preflight.sh`。

```bash
cd ~/projects/expensio
make env        # 生成 .env：随机本地口令，权限 600，不入仓
make app-up     # 构建并启动：依赖 + 六个服务 + 迁移 + 建主题 + 管理台
```

`make app-up` 会等到全部容器 healthy 才返回（`--wait --wait-timeout 420`）。
整个过程不需要手工创建数据库、主题或账号。

| 入口 | 地址 | 说明 |
| --- | --- | --- |
| 管理台 | <http://127.0.0.1:5173/> | 直接注册新租户，第一个用户是 owner |
| 网关 | <http://127.0.0.1:8080/> | `/healthz` 不碰下游 |
| Grafana | <http://127.0.0.1:3000/> | `Expensio Overview` |
| Prometheus / Tempo | <http://127.0.0.1:9090/> / <http://127.0.0.1:3200/> | |
| Mailpit | <http://127.0.0.1:8025/> | 邀请与通知邮件的本地收件箱 |

停止与清理：

```bash
make app-down       # 停掉整栈（含 app profile）
make down           # 连依赖一起停
```

只想起依赖、服务用宿主进程跑：`make up`（依赖 + 建主题），然后
`make run-expense` 之类，配 `.env` 里的数据库连接。

### B. 演示数据

```bash
export EXPENSIO_SEED_PASSWORD='<至少 10 字节的口令>'   # 只从环境变量读
go run ./cmd/seed
```

`cmd/seed` 只走公开 HTTP API：注册 `demo-seed` 租户、邀请并提升审批人、
建四条预算、把四张费用单驱动到已报销/已批准/待审批/已驳回，再建订阅与账单。
可重复执行，第二次输出"本次新建 0"。

### C. Kubernetes（k3d 演练）

```bash
k3d cluster create expensio --agents 1
kubectl apply -f deploy/cluster/acceptance/deps.yaml
helm upgrade --install expensio deploy/helm/expensio \
  -n expensio --create-namespace \
  -f deploy/cluster/acceptance/values.yaml \
  --wait --timeout 10m
bash deploy/cluster/acceptance/smoke.sh
```

细节（Secret 准备、端口转发、NetworkPolicy 断言）见
[deploy/cluster/acceptance/README.md](../deploy/cluster/acceptance/README.md)
与 [DEPLOYMENT.md](DEPLOYMENT.md)。

### D. 阿里云 ACK

**2026-10-30 起该集群四台节点处于 `Stopped`，集群不可用**，因此这条路径当前
跑不通；恢复后按 [EXTERNAL_VERIFICATION.md](EXTERNAL_VERIFICATION.md) §14 的
顺序执行（先 `MANAGED_DEPS=1 smoke.sh`，再 `oss-e2e.sh`）。集群侧资源清单、
接入步骤与已执行记录在 [ACK_MANAGED_RESOURCES.md](ACK_MANAGED_RESOURCES.md)。

## 3. 起来之后，一分钟自检

```bash
docker compose --profile app ps      # 期望：全部 healthy，migrate/topics 为Exited(0)
curl -s -o /dev/null -w 'gateway %{http_code}\n' http://127.0.0.1:8080/healthz
curl -s -o /dev/null -w 'web     %{http_code}\n' http://127.0.0.1:5173/healthz
```

只有 gateway（8080）与 web（5173）发布到宿主；identity/expense/billing/report/
notification 走容器网络，不对外暴露端口，这是 compose 的刻意设计（小程序
只能经网关访问）。两个探针都应返回 `200`。管理台打开后注册一个租户即可走
完整流程（建预算 → 建费用单 → 提交 → 审批 → 报表）。

## 4. 出问题先看

| 现象 | 处理 |
| --- | --- |
| `./scripts/env.sh: Permission denied` | `chmod +x scripts/*.sh deploy/cluster/acceptance/*.sh deploy/cluster/ack/*.sh` |
| WSL 里 `docker: command not found` | Docker Desktop → Settings → Resources → WSL integration 打开 `Ubuntu-24.04` |
| `accessing specified distro mount service … ubuntu-24-04.sock` | 同上；WSL 集成没生效，容器挂不上配置文件 |
| `ImagePullBackOff`（集群） | 镜像 tag、`imagePullSecrets`、ACR 地址 |
| `CreateContainerConfigError` | Secret 缺键，对照 `values.yaml` 的 `secretKeys` |
| HPA 显示 `<unknown>` | metrics-server 的 APIService 是否 Available |

## 5. 验收状态（一句话版）

- 本机整栈、单元/集成测试、k6、Playwright E2E、k3d 集群验收：**已实测通过**。
- 阿里云 ACK 平台适配 + HPA + NetworkPolicy + 滚动/驱逐演练：**已实测通过**。
- 托管 RDS / Tair / ApsaraMQ / OSS：**已接管并端到端验收通过**（OSS 切换后
  `MANAGED_DEPS=1` smoke 与附件 E2E 均 PASS，MinIO 下线仍 PASS）。
- 未完成：ACME 签发、云上节点级自动扩容、多可用区、OSS 生命周期与跨区域
  复制、托管 PostgreSQL 备份/PITR、云端可观测性后端与全量压测、其他云
  Kafka 端点。逐项证据与原因见 [PROGRESS.md](PROGRESS.md) 的风险表。
