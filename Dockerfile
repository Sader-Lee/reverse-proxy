# syntax=docker/dockerfile:1

# ---------- 构建阶段：静态编译代理与演示后端 ----------
FROM golang:1.25-alpine AS build

WORKDIR /src

# 先只复制依赖清单，让 go mod download 这一层能被缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 版本信息由构建参数注入，默认值保证不传参也能构建
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILT_AT=unknown

ENV CGO_ENABLED=0
RUN go build -trimpath \
        -ldflags "-s -w \
            -X main.version=${VERSION} \
            -X main.commit=${COMMIT} \
            -X main.builtAt=${BUILT_AT}" \
        -o /out/proxy ./cmd/proxy \
 && go build -trimpath -ldflags "-s -w" -o /out/demo-backend ./hack/demo-backend

# ---------- 运行阶段（默认目标）：只含代理 ----------
FROM alpine:3.20 AS proxy

RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 proxy

COPY --from=build /out/proxy /usr/local/bin/proxy
# 内置一份示例配置作为兜底，实际部署通常挂载自己的配置到同一路径
COPY configs/config.example.yaml /etc/proxy/config.yaml

USER proxy
EXPOSE 8080

# 检查进程可正常执行且配置合法（代理无独立健康端点，上游状态见 JSON 日志）
HEALTHCHECK --interval=15s --timeout=3s --start-period=3s --retries=3 \
    CMD ["/usr/local/bin/proxy", "-check", "-config", "/etc/proxy/config.yaml"]

ENTRYPOINT ["/usr/local/bin/proxy"]
CMD ["-config", "/etc/proxy/config.yaml"]

# ---------- 运行阶段：演示后端（供 docker compose 起多实例）----------
FROM alpine:3.20 AS demo-backend

RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10002 backend

COPY --from=build /out/demo-backend /usr/local/bin/demo-backend

USER backend
EXPOSE 9001

ENTRYPOINT ["/usr/local/bin/demo-backend"]
CMD ["-addr", ":9001", "-name", "backend"]
