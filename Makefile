# Expensio 根 Makefile —— 唯一的任务入口。
#
# Windows 说明：GNU make 在 Windows 上会忽略 `SHELL := bash`，静默回落到 cmd.exe，
# 表现为管道失效、$0 原样输出，但退出码仍是 0。必须给出 bash.exe 的完整路径。
# Linux/WSL 说明：必须显式指定 bash。Ubuntu 的 /bin/sh 是 dash，不支持
# `.SHELLFLAGS` 里的 `-o pipefail`，否则所有目标都会在启动时失败。
BASH_PATH ?= C:/PROGRA~1/Git/bin/bash.exe
ifeq ($(OS),Windows_NT)
  SHELL := $(BASH_PATH)
else
  SHELL := /bin/bash
endif
.SHELLFLAGS := -eu -o pipefail -c
.ONESHELL:
.DEFAULT_GOAL := help

GO      ?= go
COMPOSE ?= docker compose
VERSION ?= dev
LDFLAGS := -s -w -X github.com/edgerunner-fris/expensio/internal/platform/version.Version=$(VERSION)

# 本地开发统一从 .env 取配置：make run-<service> 与 make test 拿到的环境一致，
# 不会出现"脚本里生效、make 里不生效"的双轨。文件不存在时忽略。
-include .env
export

.PHONY: help preflight env tidy fmt vet lint test test-integration test-race build up down ps logs migrate migrate-version smoke clean

help: ## 列出所有可用目标
	@grep -hE '^[a-zA-Z_%:-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; !seen[$$1]++ {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' || true

ifeq ($(OS),Windows_NT)
preflight: ## 检查本机工具链（Windows）
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/preflight.ps1
else
preflight: ## 检查本机工具链（Linux/WSL）
	./scripts/preflight.sh
endif

env: ## 从模板生成 .env（已存在则不覆盖）
	test -f .env || cp .env.example .env

tidy: ## 整理依赖
	$(GO) mod tidy

fmt: ## 格式化
	$(GO) fmt ./...

vet: ## 静态检查
	$(GO) vet ./...

lint: ## golangci-lint
	golangci-lint run ./...

test: ## 单元 + 集成测试（本地无 Docker 时跳过集成用例）
	$(GO) test ./... -count=1

test-integration: ## 强制真实执行集成测试（Docker 不可用即失败，CI 使用）
	EXPENSIO_REQUIRE_DOCKER=1 $(GO) test ./... -count=1

test-race: ## 竞态检测
	$(GO) test -race ./... -count=1

build: ## 编译全部服务到 bin/
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

run-%: ## 本地运行单个服务，例如 make run-gateway
	EXPENSIO_DB_NAME=$*_db EXPENSIO_DB_USER=$*_app $(GO) run ./cmd/$*

up: env ## 启动依赖容器（core + obs + mail），阻塞到全部健康
	$(COMPOSE) --profile core --profile obs --profile mail up -d --wait --wait-timeout 180

down: ## 停止并移除容器
	$(COMPOSE) --profile core --profile obs --profile mail down

ps: ## 查看容器状态
	$(COMPOSE) ps

logs: ## 跟踪日志，可用 S=gateway 过滤
	$(COMPOSE) logs -f --tail=100 $(S)

migrate: ## 对 5 个库执行迁移
	./scripts/migrate.sh up

migrate-version: ## 查看迁移版本
	./scripts/migrate.sh version

smoke: ## 冒烟：打所有服务的 healthz/readyz
	./scripts/smoke.sh

clean: ## 清理构建产物
	rm -rf bin
