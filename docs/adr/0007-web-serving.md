# ADR-0007：用 Go 服务提供管理台静态资源与网关反代

- 状态：已接受
- 日期：2026-10-27
- 相关：ADR-0006、`internal/web`、`Dockerfile.web`

## 背景

管理台是 Vue 3 + Vite 的单页应用。生产环境需要：

1. 提供 `frontend/dist` 下的静态文件；
2. 把前端路由（如 `/expenses/42`）回退到 `index.html`；
3. 让浏览器只访问一个源，`/api/*` 转发到 gateway，避免 CORS；
4. 以非 root、只读根文件系统运行。

常见做法是 nginx 镜像加一份 `nginx.conf`。本项目在构建环境里无法稳定拉取
nginx 镜像（Docker Hub 认证端点多次超时），但这只是触发因素，不是唯一理由：
引入 nginx 会多一套配置语法、一套日志格式和一套生命周期管理，而这里需要的
功能用标准库不到 300 行即可完成。

## 决策

新增 `cmd/web` + `internal/web`：

- `/` 提供静态文件；不存在的无扩展名路径回退 `index.html`；
- 缺失的 `/assets/*` 或带扩展名的文件返回 404，不返回 HTML，避免浏览器把
  HTML 当 JS 解析后报 MIME 错误；
- `/api/` 原样转发到 `EXPENSIO_GATEWAY_URL`（`httputil.ReverseProxy`），
  方法、查询串、请求体、`Authorization` 与 `X-Request-ID` 都不改写；
- `/healthz`、`/readyz` 返回服务名与版本；
- 统一安全响应头：`nosniff`、`DENY`、`no-referrer`、CSP、Permissions-Policy；
  TLS 直连时加 HSTS；
- `assets/` 带内容哈希，缓存一年；`index.html` 与其余文件 `no-cache`；
- 优雅关闭、请求头/读写/空闲超时与后端服务一致。

镜像 `Dockerfile.web` 三段式：Node 构建前端 → Go 构建 web 二进制 →
alpine 运行，UID 10001，`readOnlyRootFilesystem` 可直接启用。

## 后果

正面：

- 少一个运行时组件，镜像 28MB，启动与关闭行为和其余服务一致；
- 静态资源与 API 同源，前端不需要任何 CORS 配置；
- 缓存策略、安全头、404 行为都有 Go 测试覆盖。

负面与边界：

- 没有内置 gzip/brotli、HTTP/2、证书管理；这些交给 Ingress（TLS、压缩）
  或 CDN，web 只监听集群内明文 HTTP；
- 前端发版必须重建 web 镜像（前端本来就在镜像里，未增加额外步骤）；
- HSTS 在 Ingress 终止 TLS 时不会由 web 设置，需要 Ingress 配置补上；
- 只服务单实例目录，不支持多版本静态资源并存。

## 被否方案

- **nginx 镜像**：配置与日志是第二套体系，且构建环境无法稳定拉取；功能上
  没有不可替代之处。
- **gateway 直接托管静态文件**：会让入口网关承担静态资源缓存与文件 IO，
  扩大网关职责；网关保持无状态、只做治理。
- **`go:embed` 前端产物**：每次前端改动都要重新编译 Go 二进制，且二进制
  体积随前端增长；分离镜像让两者独立构建。
- **对象存储 + CDN 托管前端**：生产可行，但本地与 CI 无法验证，且需要额外
  的 bucket、回源与缓存失效配置，超出当前里程碑范围。
