# reverse-proxy

基于 **Go + Gin** 实现的简易 HTTP 反向代理与负载均衡器。

支持多种负载均衡策略、后端健康检查与自动熔断恢复、请求超时与重试、访问日志，
并提供 YAML 配置文件驱动的启动方式。

> 技术栈：Go 1.25+ · Gin · `net/http/httputil.ReverseProxy`
> 详细设计见 [`docs/design.md`](docs/design.md)，实施计划见 [`docs/plan.md`](docs/plan.md)。

## 功能特性

| #   | 特性                                     | 状态        |
| --- | ---------------------------------------- | ----------- |
| 1   | 监听指定端口，接收客户端 HTTP 请求       | ✅ 阶段 1   |
| 2   | 按配置将请求转发到多个后端实例           | ✅ 阶段 2   |
| 3   | 负载均衡策略：轮询 / 随机 / 平滑加权轮询 | ✅ 阶段 2   |
| 4   | 后端健康检查，自动剔除与恢复             | 🚧 阶段 3   |
| 5   | 请求超时、失败重试、访问日志             | 🚧 阶段 1/4 |
| 6   | CLI + YAML 配置文件启动                  | ✅ 阶段 0   |
| 7   | 单元测试覆盖核心逻辑                     | 🚧 阶段 6   |
| 8   | 项目设计文档                             | 🚧 阶段 7   |
| 9   | 令牌桶限流（可选）                       | 🚧 阶段 8   |
| 10  | Docker / docker-compose 部署（可选）     | 🚧 阶段 8   |

## 快速开始

```bash
# 1. 准备配置
cp configs/config.example.yaml configs/config.yaml

# 2. 启动两个演示后端（仓库自带，用于本地验证；各开一个终端）
go run ./hack/demo-backend -addr :9001 -name backend-a
go run ./hack/demo-backend -addr :9002 -name backend-b

# 3. 启动代理
go run ./cmd/proxy -config configs/config.yaml

# 4. 发起请求：响应体会回显是哪个实例处理的
curl -s "http://127.0.0.1:8080/api/users?page=2"
```

> 请求会按配置的 `strategy` 分发到全部后端实例：
> `round-robin` 轮询、`random` 随机、`weighted-round-robin` 平滑加权轮询（按 `weight` 分配）。
> 某个实例被标记为不可用时会被自动跳过；全不可用时返回 `503`。
>
> 也可以编译成二进制：`go build -o bin/proxy ./cmd/proxy`。注意 Windows 下产物是 `bin\proxy.exe`
> （建议显式写成 `-o bin/proxy.exe`），否则 PowerShell 无法直接执行；`make run` 同理可用
> `make run BINARY=bin/proxy.exe` 覆盖输出名。

也可以用 Makefile：

```bash
make build      # 编译到 bin/proxy
make run        # 编译并启动
make race       # 开启竞态检测运行测试
make cover      # 生成覆盖率报告
```

## 配置文件

完整示例见 [`configs/config.example.yaml`](configs/config.example.yaml)。

```yaml
listen: ":8080"
strategy: "weighted-round-robin" # round-robin | random | weighted-round-robin

backends:
  - url: "http://127.0.0.1:9001"
    weight: 3
  - url: "http://127.0.0.1:9002"
    weight: 1

health_check:
  enabled: true
  path: "/healthz"
  interval: "5s"
  timeout: "2s"
  failure_threshold: 3 # 连续失败 N 次标记 DOWN
  success_threshold: 2 # 连续成功 M 次标记 UP

retry:
  max_attempts: 3 # 含首次请求的总尝试次数
  per_try_timeout: "3s"
  retry_on_status: [502, 503, 504]
  retry_non_idempotent: false

rate_limit:
  enabled: false
  rps: 1000
  burst: 200
  by: "ip" # ip | global

logging:
  level: "info" # debug | info | warn | error
  format: "json" # json | text
  access_log: true

timeouts:
  read_header: "10s"
  write: "0s" # 0 = 不限制，支持流式 / SSE
  idle: "90s"
```

配置加载规则：

- 未填写的字段自动沿用内置默认值；
- 时长支持 `300ms` / `3s` / `1m` 写法，纯数字按**秒**解释，空值视为 `0`；
- 端口、策略、日志级别等字符串会自动 trim 并转小写；
- `weight <= 0` 自动归一为 `1`，后端 URL 尾部斜杠自动去除；
- YAML 语法/类型错误会直接报出首个错误；语义校验不通过时**一次性列出全部问题**并以 `exit code 1` 拒绝启动。

## 目录结构

```
.
├── cmd/proxy/            # 程序入口
├── internal/
│   ├── config/           # 配置加载、默认值、校验
│   ├── backend/          # 后端实例、存活状态、统计计数与注册表
│   ├── balancer/         # 负载均衡策略：轮询 / 随机 / 平滑加权轮询
│   ├── health/           # 健康检查（阶段 3）
│   ├── proxy/            # 转发、超时、重试（阶段 1、4）
│   └── middleware/       # 访问日志、限流（阶段 1、8）
├── hack/demo-backend/    # 本地验证用的演示后端
├── configs/              # 配置示例
├── docs/                 # 计划与设计文档
└── test/                 # 端到端测试
```

## 开发

```bash
go build ./...          # 编译检查
go vet ./...            # 静态检查
go test ./... -race     # 单元测试（含竞态检测）
go test ./... -cover    # 覆盖率
```

## 开发进度

- [x] **阶段 0** 项目骨架、配置加载与校验
- [x] **阶段 1** 最小可用转发（Gin + ReverseProxy 透传、X-Forwarded-\*、访问日志、优雅退出、502 兜底）
- [x] **阶段 2** 负载均衡策略（轮询 / 随机 / 平滑加权轮询，自动跳过不可用实例）
- [ ] **阶段 3** 健康检查
- [ ] **阶段 4** 超时与重试
- [ ] **阶段 5** CLI 与配置完善
- [ ] **阶段 6** 单元测试补全
- [ ] **阶段 7** 设计文档
- [ ] **阶段 8** 限流与 Docker 部署
