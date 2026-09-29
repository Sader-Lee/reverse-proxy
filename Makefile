BINARY := bin/proxy
CONFIG ?= configs/config.yaml

.PHONY: all build run test race cover vet fmt tidy clean

all: vet test build

## build: 编译代理二进制到 bin/proxy
build:
	go build -o $(BINARY) ./cmd/proxy

## run: 用指定配置启动代理（CONFIG=xxx 可覆盖）
run: build
	$(BINARY) -config $(CONFIG)

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
