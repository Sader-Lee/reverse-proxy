# reverse-proxy

基于 **Go + Gin** 实现的简易 HTTP 反向代理与负载均衡器。

支持多种负载均衡策略、后端健康检查与自动熔断恢复、请求超时与重试、访问日志，
并提供 YAML 配置文件驱动的启动方式。

> 技术栈：Go 1.25+ · Gin · `net/http/httputil.ReverseProxy`
> 详细设计见 [`docs/design.md`](docs/design.md)（架构、配置格式、转发流程、健康检查机制），
> 实施计划见 [`docs/plan.md`](docs/plan.md)。

## 功能特性

| #   | 特性                                     | 状态        |
| --- | ---------------------------------------- | ----------- |
| 1   | 监听指定端口，接收客户端 HTTP 请求       | ✅ 阶段 1   |
| 2   | 按配置将请求转发到多个后端实例           | ✅ 阶段 2   |
| 3   | 负载均衡策略：轮询 / 随机 / 平滑加权轮询 | ✅ 阶段 2   |
| 4   | 后端健康检查，自动剔除与恢复             | ✅ 阶段 3   |
| 5   | 请求超时、失败重试、访问日志             | ✅ 阶段 1/4 |
| 6   | CLI + YAML 配置文件启动                  | ✅ 阶段 0   |
| 7   | 单元测试覆盖核心逻辑                     | ✅ 阶段 6   |
| 8   | 项目设计文档                             | ✅ 阶段 7   |
| 9   | 令牌桶限流（可选）                       | ✅ 阶段 8   |

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
make build      # 编译到 bin/proxy（自动注入版本、commit、构建时间）
make run        # 编译并启动
make check      # 只校验配置文件，不启动服务
make version    # 打印版本信息
make race       # 开启竞态检测运行测试
make cover      # 生成覆盖率报告
```

## 命令行参数

命令行只提供**配置文件不具备**的能力，不重复配置字段：

| 参数             | 说明                                                             |
| ---------------- | ---------------------------------------------------------------- |
| `-config <path>` | 配置文件路径，默认 `configs/config.yaml`                         |
| `-check`         | 只加载并校验配置，打印生效配置摘要后退出（适合 CI / 上线前自检） |
| `-version`       | 打印版本、commit、构建时间后退出                                 |
| `-listen <addr>` | 覆盖配置中的监听地址，例如 `-listen :9090`（留空则不覆盖）       |

```bash
go run ./cmd/proxy -check -config configs/config.yaml
go run ./cmd/proxy -config configs/config.yaml -listen :9090
```

退出码：`0` 正常（含 `-h`）、`1` 配置或启动失败、`2` 命令行参数错误。

版本信息有两个来源：`go build` 时 Go 会自动写入 git 信息（含工作区是否有未提交改动）；
也可以通过 `-ldflags` 注入，`make build` 已接好。

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

## 健康检查

两条通道共同维护实例状态，而且只有**连续**达到阈值才会切换，避免网络抖动造成实例反复剔除/恢复：

| 通道 | 触发方式                                                | 说明                                          |
| ---- | ------------------------------------------------------- | --------------------------------------------- |
| 主动 | 每 `health_check.interval` 请求一次 `health_check.path` | 仅 `2xx` 视为健康；不跟随重定向；超时按失败计 |
| 被动 | 转发失败（连接失败、超时）时由转发链路上报              | 立即计入连续失败，无需等待下一个探测周期      |

- 连续失败 ≥ `failure_threshold` → 标记为不可用，负载均衡自动跳过该实例；
- 连续成功 ≥ `success_threshold` → 标记为恢复，重新参与负载均衡；
- 探测成功会清零被动累计的连续失败计数；客户端主动断开**不**计入失败；
- 实例被剔除期间仍会被继续探测，因此恢复后能自动重新加入；全部实例不可用时返回 `503`；
- `health_check.enabled: false` 时主动探测与被动剔除**同时**关闭。

## 超时与重试

单次尝试由 `retry.per_try_timeout` 控制超时（回写 `504`）；失败后是否换实例重试，
由「是否还有剩余尝试次数」与请求方法共同决定：

| 失败类型               | 幂等方法（GET/HEAD/PUT/DELETE/OPTIONS/TRACE） | 非幂等方法（POST/PATCH）     |
| ---------------------- | --------------------------------------------- | ---------------------------- |
| 连接建立失败           | 重试                                          | 重试（请求肯定没被后端处理） |
| 超时 / 连接中断        | 重试                                          | 不重试                       |
| 命中 `retry_on_status` | 重试                                          | 不重试                       |

- `retry.retry_non_idempotent: true` 后，非幂等方法也按上表重试；
- 重试会**排除已尝试过的实例**，不会反复打同一个坏实例；因此只配置一个后端时不会重试
  （与 nginx 的 `proxy_next_upstream` 一致）；
- 重试决策发生在响应写回客户端**之前**（`ModifyResponse` 阶段）：失败尝试的响应被整体丢弃，
  客户端只会收到一次响应，成功响应仍然流式透传（不缓冲、不影响 SSE）；
- 请求体会被缓冲（上限 1 MiB）以便重放；超过上限的请求体原样流式转发，但不再重试；
- `max_attempts` 含首次请求；全部尝试失败时，超时回写 `504`，其余回写 `502`
  （命中状态码重试耗尽时额外返回 `last_status`）。

## 限流

`rate_limit` 启用后按令牌桶（`golang.org/x/time/rate`）限流：

| 字段      | 说明                                                  |
| --------- | ----------------------------------------------------- |
| `enabled` | 是否启用                                              |
| `rps`     | 每秒补充的令牌数，即平均速率上限                      |
| `burst`   | 桶容量，决定允许的瞬时突发量                          |
| `by`      | `ip`（每个客户端 IP 一个桶）或 `global`（全局一个桶） |

- 超出速率返回 `429`，响应体 `{"error":"too many requests","scope":"ip","retry_after":1}`，
  并带标准 `Retry-After` 头（秒）；
- 按 IP 限流时**不信任 `X-Forwarded-For`**：客户端 IP 取真实 TCP 来源
  （`SetTrustedProxies(nil)`），否则伪造头部就能换个桶绕过限流；
- 每 IP 的桶会按空闲时长回收（10 分钟），且总数有上限（10000），避免大量来源 IP 撑爆内存；
- 配置非法（`rps <= 0` 或 `burst < 1`）时跳过限流并打警告，而不是把请求全部拒绝。

## 目录结构

```
.
├── cmd/proxy/            # 程序入口：CLI、依赖装配、优雅退出
├── internal/
│   ├── config/           # 配置加载、默认值、规范化与聚合校验
│   ├── backend/          # 后端实例、存活状态、统计计数与注册表
│   ├── balancer/         # 负载均衡策略：轮询 / 随机 / 平滑加权轮询
│   ├── health/           # 健康检查：主动探测 + 转发失败被动上报
│   ├── proxy/            # 转发、实例选择、超时与重试
│   └── middleware/       # 访问日志、令牌桶限流
├── hack/demo-backend/    # 本地验证用的演示后端
├── configs/              # 配置示例（config.example.yaml）
└── docs/                 # 实施计划与设计文档
```

## 开发

```bash
go build ./...          # 编译检查
go vet ./...            # 静态检查
go test ./... -race     # 单元测试（含竞态检测）
go test ./... -cover    # 覆盖率
```

单元测试聚焦题面要求的**核心逻辑**（负载均衡、健康检查、超时重试），不为覆盖率给
装配代码或演示工具造测试。实测覆盖率：

| 包                    | 覆盖率 | 覆盖内容                                                       |
| --------------------- | ------ | -------------------------------------------------------------- |
| `internal/backend`    | 100.0% | 实例模型、权重归一化、存活状态、统计、并发                     |
| `internal/balancer`   | 98.3%  | 三种策略的分配序列、权重、下线/排除、并发与状态切换            |
| `internal/health`     | 95.8%  | 阈值剔除与恢复、探测路径/状态码/重定向、主动被动共用计数、启停 |
| `internal/proxy`      | 94.8%  | 转发、单次超时、重试策略与排除、幂等性、请求体重放             |
| `internal/middleware` | 90.9%  | 访问日志字段、日志级别、单行 JSON                              |
| `internal/config`     | 87.0%  | 默认值继承、规范化、时长解析、聚合校验                         |
| `cmd/proxy`           | 40.3%  | 仅参数解析与动作分发（监听/装配路径由手工端到端演练覆盖）      |
| `hack/demo-backend`   | -      | 本地验证用演示工具，不纳入单测                                 |

## 开发进度

- [x] **阶段 0** 项目骨架、配置加载与校验
- [x] **阶段 1** 最小可用转发（Gin + ReverseProxy 透传、X-Forwarded-\*、访问日志、优雅退出、502 兜底）
- [x] **阶段 2** 负载均衡策略（轮询 / 随机 / 平滑加权轮询，自动跳过不可用实例）
- [x] **阶段 3** 健康检查（主动探测 + 被动失败剔除，阈值防抖，自动恢复）
- [x] **阶段 4** 超时与重试（单次尝试超时 504、按状态码/连接失败换实例重试、请求体重放）
- [x] **阶段 5** 最小 CLI（`-config` / `-check` / `-version` / `-listen`，退出码约定，版本信息注入）
- [x] **阶段 6** 单元测试补全（核心逻辑覆盖率：负载均衡 98.3% / 健康检查 95.8% / 超时重试 94.8%）
- [x] **阶段 7** 设计文档（[`docs/design.md`](docs/design.md)：架构、配置格式、转发流程、健康检查机制）
- [x] **阶段 8** 令牌桶限流（按 IP / 全局，超限 429 并返回 `Retry-After`）
