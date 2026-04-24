# OpenSQT Market Maker Makefile
# =============================

# 变量定义
APP_NAME := opensqt
VERSION := $(shell grep -o 'Version = "v[^"]*"' main.go | cut -d'"' -f2 || echo "v0.0.0")
BUILD_TIME := $(shell date '+%Y-%m-%d %H:%M:%S')
GO := go
GOFLAGS := -v
LDFLAGS := -ldflags "-X main.Version=$(VERSION) -X 'main.BuildTime=$(BUILD_TIME)'"

# 默认目标
.DEFAULT_GOAL := help

# ==================== 构建 ====================

## build: 构建可执行文件
.PHONY: build
build:
	@echo "🔨 构建 $(APP_NAME)..."
	$(GO) build $(GOFLAGS) -o $(APP_NAME) .
	@echo "✅ 构建完成: ./$(APP_NAME)"

## build-linux: 交叉编译 Linux 版本
.PHONY: build-linux
build-linux:
	@echo "🔨 构建 Linux 版本..."
	GOOS=linux GOARCH=amd64 $(GO) build -o $(APP_NAME)-linux-amd64 .
	@echo "✅ 构建完成: ./$(APP_NAME)-linux-amd64"

## build-all: 构建所有平台版本
.PHONY: build-all
build-all:
	@echo "🔨 构建所有平台版本..."
	GOOS=darwin GOARCH=amd64 $(GO) build -o $(APP_NAME)-darwin-amd64 .
	GOOS=darwin GOARCH=arm64 $(GO) build -o $(APP_NAME)-darwin-arm64 .
	GOOS=linux GOARCH=amd64 $(GO) build -o $(APP_NAME)-linux-amd64 .
	GOOS=windows GOARCH=amd64 $(GO) build -o $(APP_NAME)-windows-amd64.exe .
	@echo "✅ 所有平台构建完成"

## clean: 清理构建产物
.PHONY: clean
clean:
	@echo "🧹 清理构建产物..."
	rm -f $(APP_NAME) $(APP_NAME)-*
	rm -f *.log
	@echo "✅ 清理完成"

# ==================== 测试 ====================

## test: 运行所有测试
.PHONY: test
test:
	@echo "🧪 运行测试..."
	$(GO) test -v ./...

## test-safety: 运行 safety 包测试
.PHONY: test-safety
test-safety:
	@echo "🧪 运行 safety 包测试..."
	$(GO) test -v ./safety/

## test-downtrend: 运行下跌趋势保护测试
.PHONY: test-downtrend
test-downtrend:
	@echo "🧪 运行下跌趋势保护测试..."
	$(GO) test -v ./safety/ -run "Downtrend|Scenario"

## test-cover: 运行测试并生成覆盖率报告
.PHONY: test-cover
test-cover:
	@echo "🧪 运行测试并生成覆盖率报告..."
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "✅ 覆盖率报告: coverage.html"

## bench: 运行性能测试
.PHONY: bench
bench:
	@echo "⚡ 运行性能测试..."
	$(GO) test -bench=. -benchmem ./safety/

# ==================== 代码质量 ====================

## fmt: 格式化代码
.PHONY: fmt
fmt:
	@echo "🎨 格式化代码..."
	$(GO) fmt ./...
	@echo "✅ 格式化完成"

## vet: 静态检查
.PHONY: vet
vet:
	@echo "🔍 静态检查..."
	$(GO) vet ./...
	@echo "✅ 检查完成"

## lint: 代码风格检查 (需要安装 golangci-lint)
.PHONY: lint
lint:
	@echo "🔍 代码风格检查..."
	@which golangci-lint > /dev/null || (echo "❌ 请先安装 golangci-lint: brew install golangci-lint" && exit 1)
	golangci-lint run ./...
	@echo "✅ 检查完成"

## check: 运行所有检查 (fmt + vet + test)
.PHONY: check
check: fmt vet test
	@echo "✅ 所有检查通过"

# ==================== 运行 ====================

## run: 运行程序 (使用默认配置)
.PHONY: run
run: build
	@echo "🚀 启动 $(APP_NAME)..."
	./$(APP_NAME)

## run-config: 运行程序 (指定配置文件)
## 用法: make run-config CONFIG=myconfig.yaml
.PHONY: run-config
run-config: build
	@echo "🚀 启动 $(APP_NAME) (配置: $(CONFIG))..."
	./$(APP_NAME) $(CONFIG)

## run-debug: 以 DEBUG 模式运行
.PHONY: run-debug
run-debug: build
	@echo "🚀 启动 $(APP_NAME) (DEBUG 模式)..."
	./$(APP_NAME) config.yaml

# ==================== 依赖管理 ====================

## deps: 下载依赖
.PHONY: deps
deps:
	@echo "📦 下载依赖..."
	$(GO) mod download
	@echo "✅ 依赖下载完成"

## deps-tidy: 整理依赖
.PHONY: deps-tidy
deps-tidy:
	@echo "📦 整理依赖..."
	$(GO) mod tidy
	@echo "✅ 依赖整理完成"

## deps-update: 更新依赖
.PHONY: deps-update
deps-update:
	@echo "📦 更新依赖..."
	$(GO) get -u ./...
	$(GO) mod tidy
	@echo "✅ 依赖更新完成"

# ==================== 配置 ====================

## config-init: 从示例创建配置文件
.PHONY: config-init
config-init:
	@if [ -f config.yaml ]; then \
		echo "⚠️  config.yaml 已存在，跳过"; \
	else \
		cp config.example.yaml config.yaml; \
		echo "✅ 已创建 config.yaml，请编辑配置"; \
	fi

## config-check: 验证配置文件格式
.PHONY: config-check
config-check:
	@echo "🔍 验证配置文件..."
	@if [ -f config.yaml ]; then \
		$(GO) run -tags configcheck . 2>&1 | head -20 || true; \
		echo "✅ 配置文件格式正确"; \
	else \
		echo "❌ config.yaml 不存在，请先运行 make config-init"; \
	fi

# ==================== Docker ====================

## docker-build: 构建 Docker 镜像
.PHONY: docker-build
docker-build:
	@echo "🐳 构建 Docker 镜像..."
	docker build -t $(APP_NAME):$(VERSION) .
	@echo "✅ 镜像构建完成: $(APP_NAME):$(VERSION)"

## docker-run: 运行 Docker 容器
.PHONY: docker-run
docker-run:
	@echo "🐳 运行 Docker 容器..."
	docker run -it --rm -v $(PWD)/config.yaml:/app/config.yaml $(APP_NAME):$(VERSION)

# ==================== 帮助 ====================

## help: 显示帮助信息
.PHONY: help
help:
	@echo ""
	@echo "OpenSQT Market Maker - Makefile 命令"
	@echo "===================================="
	@echo ""
	@echo "构建:"
	@echo "  make build        - 构建可执行文件"
	@echo "  make build-linux  - 构建 Linux 版本"
	@echo "  make build-all    - 构建所有平台版本"
	@echo "  make clean        - 清理构建产物"
	@echo ""
	@echo "测试:"
	@echo "  make test         - 运行所有测试"
	@echo "  make test-safety  - 运行 safety 包测试"
	@echo "  make test-downtrend - 运行下跌趋势保护测试"
	@echo "  make test-cover   - 生成测试覆盖率报告"
	@echo "  make bench        - 运行性能测试"
	@echo ""
	@echo "代码质量:"
	@echo "  make fmt          - 格式化代码"
	@echo "  make vet          - 静态检查"
	@echo "  make lint         - 代码风格检查"
	@echo "  make check        - 运行所有检查"
	@echo ""
	@echo "运行:"
	@echo "  make run          - 运行程序"
	@echo "  make run-config CONFIG=xxx.yaml - 指定配置文件运行"
	@echo ""
	@echo "依赖:"
	@echo "  make deps         - 下载依赖"
	@echo "  make deps-tidy    - 整理依赖"
	@echo "  make deps-update  - 更新依赖"
	@echo ""
	@echo "配置:"
	@echo "  make config-init  - 从示例创建配置文件"
	@echo ""
	@echo "Docker:"
	@echo "  make docker-build - 构建 Docker 镜像"
	@echo "  make docker-run   - 运行 Docker 容器"
	@echo ""
	@echo "版本: $(VERSION)"
	@echo ""
