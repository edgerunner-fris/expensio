# 不写 `# syntax=` 指令：构建环境拉取 docker/dockerfile 前端镜像经常超时，
# 而本文件只用到经典 Dockerfile 语法，去掉后构建不依赖额外镜像。
#
# 六个 Go 服务共用一份多阶段构建：SERVICE 选择 cmd/<name>。
# 运行镜像只含静态二进制与 CA 证书，以非 root 用户运行；
# 服务本身不写本地磁盘，因此可以在 K8s 里配 readOnlyRootFilesystem。
# 固定到补丁版本：1.27.0 命中多个标准库漏洞（net/http、crypto/tls 等），
# 1.27.3 修复。构建镜像与 go.mod 的 go 指令保持一致，避免"本地修好了、
# 镜像还在用旧补丁"。
ARG GO_VERSION=1.27.3
FROM golang:${GO_VERSION}-alpine AS build

# GOPROXY 必须声明在 go mod download 之前；SERVICE / VERSION 则故意放到后面：
# Docker 会把每个 ARG 值算进后续 RUN 的缓存键，如果 SERVICE 在下载依赖之前
# 声明，六个服务会各自重新下载一遍模块，构建时间成倍增加。
ARG GOPROXY=https://proxy.golang.org,direct
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOPROXY=${GOPROXY} \
    GOTOOLCHAIN=local

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG SERVICE
ARG VERSION=dev

# 空 SERVICE 或拼错目录会在这里失败，而不是产出一个没有入口的镜像。
RUN test -n "${SERVICE}" && test -d "cmd/${SERVICE}"
RUN go build -trimpath \
      -ldflags "-s -w -X github.com/edgerunner-fris/expensio/internal/platform/version.Version=${VERSION}" \
      -o /out/expensio "./cmd/${SERVICE}"

FROM alpine:3.22

# CA 证书用于 SMTP STARTTLS 与将来任何出站 HTTPS；运行期不安装包管理器之外的东西。
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 expensio \
    && adduser -S -u 10001 -G expensio -h /nonexistent -s /sbin/nologin expensio

COPY --from=build /out/expensio /usr/local/bin/expensio

USER 10001:10001
ENTRYPOINT ["/usr/local/bin/expensio"]
