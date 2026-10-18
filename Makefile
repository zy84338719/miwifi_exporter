PROJECT   = miwifi-exporter
MAIN_PATH = .
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -X main.version=$(VERSION) -X main.buildDate=$(DATE)
BIN_DIR   = bin

.DEFAULT_GOAL := build

.PHONY: help
help: ## 显示可用目标
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## 构建当前平台的二进制到 ./bin
	@echo "version=$(VERSION) date=$(DATE)"
	@mkdir -p $(BIN_DIR)
	@go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(PROJECT) $(MAIN_PATH)

.PHONY: build-linux
build-linux: ## 交叉编译 linux/amd64
	@mkdir -p $(BIN_DIR)
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -trimpath -ldflags "$(LDFLAGS) -s -w" -o $(BIN_DIR)/$(PROJECT)-linux-amd64 $(MAIN_PATH)

.PHONY: build-all
build-all: build-linux ## 构建 linux 各架构
	@for arch in arm64 armv7; do \
		echo "==> linux/$$arch"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch \
			go build -trimpath -ldflags "$(LDFLAGS) -s -w" \
			-o $(BIN_DIR)/$(PROJECT)-linux-$$arch $(MAIN_PATH); \
	done

.PHONY: run
run: build ## 构建并本地运行
	@$(BIN_DIR)/$(PROJECT)

.PHONY: test
test: ## 跑全部测试（含 -race）
	@go test -race ./...

.PHONY: test-short
test-short: ## 跳过 race 的快速测试
	@go test ./...

.PHONY: cover
cover: ## 生成覆盖率报告 coverage.html
	@go test -coverprofile=coverage.out -covermode=atomic ./...
	@go tool cover -html=coverage.out -o coverage.html
	@go tool cover -func=coverage.out | tail -1

.PHONY: fmt
fmt: ## 格式化代码
	@gofmt -s -w $$(git ls-files '*.go' 2>/dev/null || find . -name '*.go' -not -path './vendor/*')

.PHONY: lint
lint: ## 静态检查（gofmt + go vet）
	@test -z "$$(gofmt -s -l . | grep -v '^$$')" || { echo "以下文件未格式化，请执行 make fmt:"; gofmt -s -l .; exit 1; }
	@go vet ./...

.PHONY: tidy
tidy: ## 整理依赖
	@go mod tidy

.PHONY: verify-config
verify-config: build ## 校验本地配置但不启动服务
	@$(BIN_DIR)/$(PROJECT) -validate-config

.PHONY: docker
docker: ## 构建 docker 镜像
	@docker build -t $(PROJECT):$(VERSION) .

.PHONY: clean
clean: ## 清理构建产物
	@rm -rf $(BIN_DIR) coverage.out coverage.html

.PHONY: version
version: ## 打印版本号
	@echo $(VERSION)
