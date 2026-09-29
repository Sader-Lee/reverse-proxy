BINARY := bin/proxy
CONFIG ?= configs/config.yaml

# 版本信息：优先取最近的 tag，退化为短 commit；可在命令行覆盖
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null)
BUILT_AT ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.builtAt=$(BUILT_AT)

.PHONY: all build run check version test race cover vet fmt tidy clean

all: vet test build

## build: 编译代理二进制到 bin/proxy
build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/proxy

## run: 用指定配置启动代理（CONFIG=xxx 可覆盖）
run: build
	$(BINARY) -config $(CONFIG)

## check: 只校验配置文件，不启动服务（适合 CI / 上线前自检）
check: build
	$(BINARY) -check -config $(CONFIG)

## version: 打印版本信息
version: build
	$(BINARY) -version

## test: 运行全部单元测试
test:
	go test ./...

## race: 开启竞态检测运行测试
race:
	go test ./... -race

## cover: 生成覆盖率报告
cover:
	go test ./... -coverprofile=coverage.out
	go tool cover -func=coverage.out

## vet: 静态检查
vet:
	go vet ./...

## fmt: 格式化代码
fmt:
	gofmt -l -w .

## tidy: 整理依赖
tidy:
	go mod tidy

## clean: 清理构建产物
clean:
	rm -rf bin coverage.out coverage.html
