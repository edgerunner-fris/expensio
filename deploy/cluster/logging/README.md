# 生产日志聚合（Loki + Alloy）

本目录是**集群级**日志采集的部署值，不属于 `deploy/helm/expensio`：
应用 chart 只负责业务服务，日志采集是平台能力，遵循同样的边界——
chart 不部署有状态依赖，也不创建 Secret。

| 文件 | 对应 Helm chart | 作用 |
| --- | --- | --- |
| `loki-values.yaml` | `grafana/loki`（≥ 6.x） | 单二进制 Loki，S3 兼容对象存储，30 天保留 |
| `alloy-values.yaml` | `grafana/alloy`（≥ 0.5） | DaemonSet，采集全部命名空间的 Pod 日志 |

## 部署顺序

```bash
# 1. 命名空间与 S3 凭据（键名固定为 AWS SDK 默认变量）
kubectl create namespace observability
kubectl -n observability create secret generic loki-s3-credentials \
  --from-literal=AWS_ACCESS_KEY_ID=... \
  --from-literal=AWS_SECRET_ACCESS_KEY=...

# 2. 先在对象存储里建桶：expensio-loki-chunks / expensio-loki-ruler
#    （S3 兼容实现通常不会因写入自动建桶，缺失时 Loki 启动后第一次写入才报错）

# 3. Loki
helm upgrade --install loki grafana/loki -n observability \
  -f deploy/cluster/logging/loki-values.yaml --wait --timeout 10m

# 4. Alloy（DaemonSet 必须在 Loki 就绪后安装，否则首批日志会丢）
helm upgrade --install alloy grafana/alloy -n observability \
  -f deploy/cluster/logging/alloy-values.yaml --wait --timeout 5m
```

## 验证

```bash
# Loki 就绪且能接受写入
kubectl -n observability port-forward svc/loki-gateway 3100:80 &
curl -s http://127.0.0.1:3100/ready          # 期望 ready
curl -s http://127.0.0.1:3100/loki/api/v1/labels

# 采集到业务 Pod 后应出现 service 标签
curl -s 'http://127.0.0.1:3100/loki/api/v1/label/service/values'
# {"status":"success","data":["billing","expense","gateway","identity","notification","report","web"]}
```

在 Grafana 里添加 Loki 数据源（UID `expensio-loki`），`derivedFields` 配置与本地
`deploy/local/grafana/provisioning/datasources/datasources.yaml` 相同：
匹配访问日志里的 `"trace_id":"<32 位十六进制>"` 并跳转 Tempo。

## 为什么采集走 API 而不是读宿主机文件

`alloy-values.yaml` 使用 `loki.source.kubernetes`，通过 kubelet API 读取
Pod 日志，只授予 `pods/log` 的 get/list/watch 权限：

- 不需要 hostPath 挂载 `/var/log/pods`，节点上不暴露可写目录；
- 不受容器运行时（containerd / CRI-O）日志文件布局变化影响；
- 每个流天然带 namespace / pod / container 标签，不需要按节点拼路径。

代价是日志读取经 API Server/kubelet 转发，规模很大时要评估 kubelet 压力；
本项目的日志量（7 个服务、单副本）远低于该阈值。

## 与告警的关系

日志**不参与**告警判定。`deploy/local/prometheus/rules.yml` 与生产告警
全部基于指标；日志用于事后定位。这条边界是有意的：基于日志的告警在
采集链路故障时会静默失效，而指标链路有 `up` 与采集延迟可以自证。
