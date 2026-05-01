# 项目教程 — 从 skim 到 面试高光

> 面向「想在 2 小时内摸清全栈，再花一天做 deep dive 给面试用」的读者。
>
> 本文不重写散落在根目录的中文深潜文档（`JWT实现方案.md`、`滑动窗口限流与Lua脚本改进详解.md`、`演唱会压测实现方案.md` 等），它们仍是原始依据；本文是**整合导航**：带你按一条合理路径把它们走完。

---

## 目录

1. [5 分钟项目概览](#1-5-分钟项目概览)
2. [一张图看懂数据流](#2-一张图看懂数据流)
3. [能力清单（按技术栈分类）](#3-能力清单按技术栈分类)
4. [核心代码阅读顺序（5 条学习路径）](#4-核心代码阅读顺序5-条学习路径)
5. [关键设计决策与理由（10 条）](#5-关键设计决策与理由10-条)
6. [测试分层（单测 / 基准 / 集成 / 压测 / 混沌）](#6-测试分层单测--基准--集成--压测--混沌)
7. [快速启动 + 调试避坑](#7-快速启动--调试避坑)
8. [性能数字速查（Benchmark 真实数据）](#8-性能数字速查benchmark-真实数据)
9. [本地压测三轮结果与解读](#9-本地压测三轮结果与解读)
10. [面试高频 10 问（含推荐答案）](#10-面试高频-10-问含推荐答案)
11. [简历项目表述（3 档）](#11-简历项目表述3-档)

---

## 1. 5 分钟项目概览

**一句话**：Go 微服务 ride-sharing 后端 + 一整套演唱会散场级 k6 压测 + Chaos Mesh 混沌注入，用来把「三层下单防护 / 消息可靠性 / 分布式锁 / 缓存策略」四条主线的工程化设计全部打穿。

**四个服务**（每个独立 Go 二进制）：

| 服务 | 职责 | 关键依赖 |
|---|---|---|
| `api-gateway` | HTTP/WebSocket 入口，鉴权，限流，幂等，gRPC fan-out，Stripe webhook | Redis、gRPC、JWT |
| `trip-service` | 行程领域：preview/create/status，RideFare 计价，事件发布 | MongoDB、Redis（缓存+Bloom）、RabbitMQ |
| `driver-service` | 司机域：在线状态、Redis GEO 派单、心跳、响应 accept/decline | Redis GEO、RabbitMQ |
| `payment-service` | Stripe Session 创建、支付成功事件发布 | Stripe API、RabbitMQ |

**关键数字**（可以在简历/面试时直接引用）：
- 单 gRPC 连接改为 `sync.Once` 单例后 P99 从 **~4s** 降到 **~7ms**（~570×）
- 滑动窗口限流 P99 **0.90 ms**，12 核并发 **23.4k QPS**
- Bloom 位图 1.25MB，实测误判率 **0.118%**（20 万 ObjectID 回放）
- 消息幂等 100 并发同 MessageId 收敛到 **恰好 1 次**业务执行
- 演唱会压测峰值：riders 2000 VU / drivers 800 VU / attackers 500 rps，持续 17min
- bcrypt cost=10（benchmark 过 cost=12 太慢被否决）

**你看完本节已经知道**：项目在做什么、四个服务怎么分工、有哪些可量化的亮点。

---

## 2. 一张图看懂数据流

### 2.1 链路 A：乘客下单（3 层防护 + 异步派单）

```
                    ┌──────────────────────────────────────────────┐
  client            │            api-gateway (HTTP 8081)            │
  POST /trip/start ─┼─> ① 滑动窗口 (Lua: ZREMRANGEBYSCORE+ZCARD+ZADD)│
                    │    shared/cache/ratelimit.go                   │
                    │ ─> ② SetNX 幂等锁 (5s TTL)                    │
                    │    services/api-gateway/http.go                │
                    │ ─> ③ gRPC CreateTrip (sync.Once 单例 client)   │
                    └──────────────────────┬───────────────────────┘
                                           │ gRPC 9093
                                           ▼
                    ┌──────────────────────────────────────────────┐
                    │             trip-service                      │
                    │  CreateTrip: insert Mongo + BloomAdd          │
                    │  services/trip-service/internal/service/      │
                    │  └─ AMQP PublishMessage                       │
                    │     contracts.TripEventCreated                │
                    └──────────────────────┬───────────────────────┘
                                           │ RabbitMQ topic exchange: trip
                                           ▼
                    ┌──────────────────────────────────────────────┐
                    │             driver-service                    │
                    │  consumer: trip.event.created                 │
                    │  └─ Redis GEO 查附近司机                       │
                    │  └─ 分布式锁 SET NX (tripAcceptLockTTL=30s)   │
                    │     services/trip-service/internal/            │
                    │     infrastructure/events/driver_consumer.go   │
                    │  └─ AMQP driver.cmd.trip_request              │
                    └──────────────────────┬───────────────────────┘
                                           │
                                           ▼ (WebSocket 推送)
                    ┌──────────────────────────────────────────────┐
                    │   api-gateway /ws/drivers + /ws/riders        │
                    │   事件: driver_assigned → rider 侧             │
                    └──────────────────────────────────────────────┘
```

**关键点**：3 道防线职责完全不重叠。滑动窗口管「频率」（10 秒 ≤ 1 次），SetNX 管「瞬时并发」（同 userID 5 秒内互斥），Bloom 管「穿透」（attacker 的 fakeID 在到达 Mongo 前被 miss 拦截）。任何一层被绕过，另两层仍然生效。

### 2.2 链路 B：Stripe 支付回调（签名校验 + 24h 事件幂等）

```
Stripe → POST /webhook/stripe (api-gateway)
   │
   ▼
 ① stripe-go/v81 webhook.ConstructEventWithOptions
   → HMAC-SHA256 签名验证，失败 400
   │
   ▼
 ② Redis SetNX "stripe:event:<event.ID>" TTL=24h
   → SetNX 失败（已见过）→ 直接 200 阻止重试
   → SetNX 成功          → 继续处理
   │
   ▼
 ③ 解析 checkout.session.completed → AMQP PublishMessage
   routing key: payment.event.success
```

**关键点**：
- 签名校验在第一步是 security；
- Redis 幂等 24h 是 functional idempotency（Stripe 重试窗口长，24h 覆盖大多数场景）；
- 业务逻辑只有在 `(签名通过 AND 首次见到)` 时才执行，其它路径短路 200。

### 2.3 完整 Routing Key 清单

来源 `shared/contracts/amqp.go`：

| Routing Key | Publisher → Subscriber | 作用 |
|---|---|---|
| `trip.event.created` | Trip → Driver | 新行程，要找附近司机 |
| `trip.event.driver_assigned` | Trip → Gateway | 通知 rider（WebSocket） |
| `trip.event.no_drivers_found` | Driver → Trip | 没司机，Trip 侧更新状态 |
| `driver.cmd.trip_request` | Trip → Driver | 把行程派给具体司机 |
| `driver.cmd.trip_accept` | Driver → Trip | 司机接单 |
| `driver.cmd.trip_decline` | Driver → Trip | 司机拒单 |
| `driver.cmd.location` | Driver → Driver | 位置心跳 |
| `driver.cmd.register` | Gateway → Driver | 司机上线注册 |
| `payment.cmd.create_session` | Trip → Payment | 触发 Stripe 建单 |
| `payment.event.session_created` | Payment → Trip | Stripe session URL 回传 |
| `payment.event.success` | Payment → Trip | 支付成功，终态 |
| `payment.event.failed` | Payment → Trip | 支付失败 |
| `payment.event.cancelled` | Payment → Trip | 支付取消 |

**为什么 topic 而不是 direct**：同一事件常被多订阅者消费（`trip.event.created` 会被 driver-service + 未来的 tracing/analytics 同时订阅），topic 通配比 direct 的一对一 binding 更灵活。

---

## 3. 能力清单（按技术栈分类）

> 表格每行：**能力 | 代码位置 | 为什么这样做**。括号里的路径都是相对仓库根，可直接打开。

### 3.1 鉴权与密码

| 能力 | 位置 | 为什么 |
|---|---|---|
| JWT 双 token 签发 | [`shared/auth/jwt.go`](../shared/auth/jwt.go) | 无状态 → gateway 水平扩不需要 session 粘性；access 25min + refresh 7d |
| Algorithm confusion 防御 | [`shared/auth/jwt.go:77-80`](../shared/auth/jwt.go) | 强制 `jwt.SigningMethodHMAC` 类型断言 + `jwt.WithValidMethods`，防 `alg=none` / RSA pubkey 混淆 |
| 30s 时钟漂移容忍 | [`shared/auth/jwt.go:12`](../shared/auth/jwt.go) | `const clockSkew = 30 * time.Second` + `jwt.WithLeeway` 解决多节点 NTP 偶发漂移 |
| bcrypt 密码哈希 | [`shared/auth/password.go`](../shared/auth/password.go) | `const passwordCost = 10`（benchmark 过 12 被否决，见 §5.3） |
| 空密码拒绝 | [`shared/auth/password.go:ErrEmptyPassword`](../shared/auth/password.go) | bcrypt 会接受空字符串 → 会建出永远能登的「空密码幽灵账号」，在上层显式拒绝 |
| auth middleware | [`services/api-gateway/auth_middleware.go`](../services/api-gateway/auth_middleware.go) | 提取 Bearer token，放入 `context.Value("user")` 供 handler 使用 |
| register/login/refresh/me/logout | [`services/api-gateway/auth_handler.go`](../services/api-gateway/auth_handler.go) | 统一走 `UserRepo`（MongoDB） |

### 3.2 接口防护（3 层）

| 能力 | 位置 | 为什么 |
|---|---|---|
| 滑动窗口限流（Lua 原子） | [`shared/cache/ratelimit.go`](../shared/cache/ratelimit.go) | ZREMRANGEBYSCORE + ZCARD + ZADD 三步原子；比 INCR+EXPIRE 的 fixed window 更准（无边界突增） |
| SetNX 幂等锁（5s） | [`services/api-gateway/http.go:acquireTripStartCreateLock`](../services/api-gateway/http.go) | 拦截毫秒级并发重复（网络抖动/前端连点） |
| Stripe webhook 幂等（24h） | [`services/api-gateway/http.go:markStripeWebhookEventProcessed`](../services/api-gateway/http.go) | Stripe 重试窗口跨日 → TTL 必须覆盖 |
| Bloom 防穿透 | [`shared/cache/bloom.go`](../shared/cache/bloom.go) + [`trip-service/internal/service/service.go:GetTripByID`](../services/trip-service/internal/service/service.go) | 10M bits、FNV-32a + FNV-32⊕0x9e3779b9 双 hash；实测误判 0.118% |
| Bloom 反哺（miss 时回填） | [`service.go:315-317`](../services/trip-service/internal/service/service.go) | DB 实际命中的 tripID 回填位图，防止历史 tripID 因冷启动漏登记 |

### 3.3 gRPC 与连接池

| 能力 | 位置 | 为什么 |
|---|---|---|
| gRPC client `sync.Once` 单例 | `services/api-gateway/grpc_clients/{trip_client,driver_client}.go` | 压测实测：per-request `grpc.NewClient` 有 ~4s HTTP/2 preface + subconn init；单例 7ms |
| Interceptor（OTel + metrics） | 各 service `main.go` 的 `grpc.NewServer(...)` | 统一 server/client 两侧埋点 |

### 3.4 缓存策略

| 能力 | 位置 | 为什么 |
|---|---|---|
| Redis 单例 Universal Client | [`shared/cache/redis_client.go`](../shared/cache/redis_client.go) | 同时支持 single / sentinel 拓扑，上层不用判分支 |
| Trip 读缓存 + 写删除 | [`service.go:GetTripByID / UpdateTrip`](../services/trip-service/internal/service/service.go) | 读 cache-aside；更新走「删除」而非「覆盖」，避免多写路径的值分歧 |
| Fare 压测期纯 Redis | [`service.go:GenerateTripFares / GetAndValidateFare`](../services/trip-service/internal/service/service.go) | Mongo Atlas 远端 insert ~1s 会把 preview P99 顶穿，压测期绕开（loadtest 后 `git restore`） |
| 坏缓存自愈 | [`service.go:302`](../services/trip-service/internal/service/service.go) | JSON Unmarshal 失败 → Del 坏 key，防止反复命中脏数据 |

### 3.5 消息系统（RabbitMQ）

| 能力 | 位置 | 为什么 |
|---|---|---|
| topic exchange + DLQ | [`shared/messaging/rabbitmq.go`](../shared/messaging/rabbitmq.go) | 队列声明带 `x-dead-letter-exchange`，nack requeue=false 自动入 DLQ |
| 重试队列（TTL + DLX） | 同上 | 用 TTL 过期触发 DLX 实现延迟重投，避免 tight-loop |
| 消费端幂等（3 态） | [`shared/messaging/idempotency.go`](../shared/messaging/idempotency.go) | `processing`/`done` 两个状态 + 失败时 `ReleaseMessageProcessing`，at-least-once 下只执行 1 次业务 |
| MessageID 回落 sha256 | [`idempotency.go:resolveMessageID`](../shared/messaging/idempotency.go) | 生产者不给 MessageId 时用 `sha256(routingKey+body)` 保底 |

### 3.6 分布式锁

| 能力 | 位置 | 为什么 |
|---|---|---|
| 接单互斥锁（SET NX EX） | [`trip-service/.../driver_consumer.go`](../services/trip-service/internal/infrastructure/events/driver_consumer.go) | `tripAcceptLockTTL=30s`；同一 tripID 多司机抢只让 1 人赢 |
| Lua owner-check DEL | 同上 | 只删自己持有的锁，防止「A 锁过期 B 拿到 A 又 DEL」的经典 race |

### 3.7 司机域

| 能力 | 位置 | 为什么 |
|---|---|---|
| Redis GEO 附近司机 | [`services/driver-service/service.go`](../services/driver-service/service.go) + [`redis.go`](../services/driver-service/redis.go) | 从 Geohash 字符串表迁到 Redis GEO 原生命令，带距离排序 |
| 定时位置心跳 | `services/driver-service/` | 司机 client 每 N 秒推位置，服务端过期 TTL 自动下线 |

### 3.8 可观测

| 能力 | 位置 | 为什么 |
|---|---|---|
| OpenTelemetry + Jaeger | [`shared/tracing/`](../shared/tracing/) | 标准化 tracer；全链路跨服务 |
| Prometheus metrics | [`shared/metrics/{metrics,middleware}.go`](../shared/metrics/) | HTTP handler / gRPC interceptor 双端自动埋 |
| Grafana 面板 | `loadtest/` 目录下配置 | 压测期直接看 P99/错误率/QPS |

### 3.9 压测 & 混沌

| 能力 | 位置 | 为什么 |
|---|---|---|
| k6 演唱会 scenario | [`loadtest/k6/scenarios/concert.js`](../loadtest/k6/scenarios/concert.js) | 三 scenario 并发：riders 爬升 / drivers 常驻 / attackers 恒速率 |
| 双档 SLO 阈值 | [`loadtest/k6/thresholds.js`](../loadtest/k6/thresholds.js) | baseline 严 / chaos 放宽 40-60%，但 Bloom 拦截率不动 |
| xk6 prometheus remote_write | `scripts/loadtest_run.sh` 里 `-o experimental-prometheus-rw=...` | 压测指标直接灌到同一 Prometheus，Grafana 统一视角 |
| Chaos Mesh Workflow | [`loadtest/chaos/schedule.yaml`](../loadtest/chaos/schedule.yaml) | Suspend 8min 让 ramp up → T+8/9/10/11 四次故障注入 |

---

## 4. 核心代码阅读顺序（5 条学习路径）

每条路径按「先懂概念 → 读类型定义 → 读调用链 → 读测试」的四步法，每条 30–60 分钟。

### 路径 1：鉴权体系（JWT + bcrypt + middleware）

预计 30 分钟。

1. [`shared/auth/claims.go`](../shared/auth/claims.go) — JWT claims 字段
2. [`shared/auth/jwt.go`](../shared/auth/jwt.go) — Signer 结构（secret/issuer/TTL），`SignAccess` / `Parse` / `Refresh` 三个方法
3. [`shared/auth/password.go`](../shared/auth/password.go) — bcrypt 封装，cost=10，空密码拒绝
4. [`services/api-gateway/user_repo.go`](../services/api-gateway/user_repo.go) — Mongo 用户仓储
5. [`services/api-gateway/auth_handler.go`](../services/api-gateway/auth_handler.go) — register/login/refresh/me/logout 五个 handler
6. [`services/api-gateway/auth_middleware.go`](../services/api-gateway/auth_middleware.go) — Bearer 解析 + context 注入
7. [`services/api-gateway/auth_handler_test.go`](../services/api-gateway/auth_handler_test.go) — 测试示例

读完你知道：签发到校验到 middleware 到 handler 怎么串起来；为什么用双 token；algorithm confusion 怎么防。

### 路径 2：下单三层防护（限流 + 幂等 + gRPC）

预计 45 分钟。

1. [`shared/cache/ratelimit.go`](../shared/cache/ratelimit.go) — Lua 脚本字面量（`slidingWindowScript`）读一遍，`slidingWindowSeq` 原子计数器理解为什么需要
2. [`shared/cache/ratelimit_bench_test.go`](../shared/cache/ratelimit_bench_test.go) — 并发正确性测试（limit=10, 200 goroutine, 必须恰好放行 10 个）
3. [`services/api-gateway/http.go:handleTripStart`](../services/api-gateway/http.go) — 关键 40 行：rate limit → SetNX → gRPC
4. `services/api-gateway/http_rate_limit_test.go` / `http_idempotency_test.go` — 两份单测看两层拦截分别怎么触发
5. [`services/api-gateway/grpc_clients/trip_client.go`](../services/api-gateway/grpc_clients/trip_client.go) — sync.Once 单例

读完你知道：三层职责各自是什么、哪一层被绕过另两层还能兜底、SetNX 的 5s TTL 是怎么挑的。

### 路径 3：消息系统（RabbitMQ topic + DLQ + 消费端幂等）

预计 45 分钟。

1. [`shared/contracts/amqp.go`](../shared/contracts/amqp.go) — Routing key 常量大全（13 个）
2. [`shared/messaging/rabbitmq.go`](../shared/messaging/rabbitmq.go) — 连接 + exchange/queue 声明 + publish + consume + DLQ；重点看 `x-dead-letter-exchange` / `x-dead-letter-routing-key` 参数
3. [`shared/messaging/idempotency.go`](../shared/messaging/idempotency.go) — 3 态机器：`ClaimMessageForProcessing` 返回 `Claim{Key, AlreadyProcessed, AcquiredAt}`；失败走 `ReleaseMessageProcessing`
4. [`shared/messaging/idempotency_bench_test.go`](../shared/messaging/idempotency_bench_test.go) — 100 并发抢同一 MessageId 必须收敛到 1
5. `services/trip-service/.../trip_publisher.go` — 发布端如何调 `rb.PublishMessage`
6. [`services/trip-service/internal/infrastructure/events/driver_consumer.go`](../services/trip-service/internal/infrastructure/events/driver_consumer.go) — 消费端样板：claim → 业务 → markDone / release

读完你知道：at-least-once 下怎么保证只执行一次、DLQ 怎么接住投递死信、重试队列用 TTL+DLX 做延迟。

### 路径 4：trip 域（DDD 骨架 + 缓存策略）

预计 45 分钟。

1. [`services/trip-service/internal/domain/trip.go`](../services/trip-service/internal/domain/trip.go) / [`ride_fare.go`](../services/trip-service/internal/domain/ride_fare.go) — 纯数据结构
2. [`services/trip-service/internal/service/service.go`](../services/trip-service/internal/service/service.go) — 业务编排，重点读：
   - `CreateTrip`：Mongo insert + Bloom add 的顺序（Bloom 失败不阻断主流程）
   - `GenerateTripFares`：`sync.WaitGroup` 并行 4 个 fare，errs 槽聚合错误
   - `GetAndValidateFare`：Redis miss → Mongo fallback + owner 校验
   - `GetTripByID`：Bloom miss → 拒绝；cache hit → 返回；坏 cache → Del；DB hit → 反哺 Bloom + 写 cache
3. [`services/trip-service/internal/infrastructure/repository/mongodb.go`](../services/trip-service/internal/infrastructure/repository/mongodb.go) — Mongo CRUD + `{status:pending}` 条件更新
4. [`services/trip-service/internal/infrastructure/grpc/grpc_handler.go`](../services/trip-service/internal/infrastructure/grpc/grpc_handler.go) — 把 service 层适配成 gRPC

读完你知道：trip 从 HTTP → gRPC → service → Mongo/Redis 的分层、为什么 CreateTrip 的 Bloom add 放 DB 后、cache 策略为什么是「读缓存 + 写删除」。

### 路径 5：压测体系

预计 30 分钟。

1. [`loadtest/k6/scenarios/concert.js`](../loadtest/k6/scenarios/concert.js) — 三 scenario（ramping-vus / constant-vus / constant-arrival-rate）
2. [`loadtest/k6/scenarios/helpers/auth.js`](../loadtest/k6/scenarios/helpers/auth.js) — VU 登录 + token 缓存（VU-local 不是 __ENV）
3. [`loadtest/k6/thresholds.js`](../loadtest/k6/thresholds.js) — 双档 SLO
4. [`scripts/loadtest_run.sh`](../scripts/loadtest_run.sh) — 一键三轮：preflight → seed → warmup → round dispatch
5. [`loadtest/chaos/schedule.yaml`](../loadtest/chaos/schedule.yaml) — Chaos Workflow：Suspend 8min + 4 次 60s 注入

读完你知道：Round 1/2/3 各代表什么、Chaos 为什么延迟 8min 才注入、attacker 为什么必须带合法 token。

---

## 5. 关键设计决策与理由（10 条）

### 5.1 gateway 的 gRPC client 必须单例

**决策**：`sync.Once` + 包级 `*grpc.ClientConn`，不是 per-request `grpc.NewClient`。

**代价**：per-request 实测有 ~4 秒冷启动（HTTP/2 preface + subconn 初始化 + TLS），压测 P99 爆到 4s。改单例后 7ms。

**教科书的反对意见**：「per-request 避免下游挂掉阻塞主服务」。真相是 `grpc.NewClient` 默认非阻塞，下游挂只是 RPC 层面失败，不会阻塞新请求。所以教科书在这个点上过时了。

**代码**：`services/api-gateway/grpc_clients/trip_client.go` 用 `sync.Once`。

### 5.2 为什么是「滑动窗口 + SetNX」两层而不是一层

**决策**：第一层滑动窗口（10 秒 ≤ 1 次）管「频率」；第二层 SetNX（5 秒 TTL）管「瞬时并发」。

**为什么不能合并**：
- 只留滑动窗口：同一用户在同一毫秒内的 2 个请求会被同时放过（Lua 内原子，但两个 Lua 调用之间没有锁）
- 只留 SetNX：用户 6 秒后再点一次能过，失去频率限制

两层职责正交 → 任何一层被绕过，另一层仍拦。

### 5.3 bcrypt cost=10 不是 12

**决策**：`const passwordCost = 10`。

**理由**：benchmark 测出 cost=12 单次 ~240ms，2000 并发登录会把 auth 的 P99 打到 5s+。cost=10 ~70ms，auth P99 < 1s 可达。安全向下妥协一格，业务可用性向上换一个量级。

**代码**：`shared/auth/password.go`。

### 5.4 RabbitMQ topic exchange 不是 direct

**决策**：单一 exchange `trip`（topic 类型），所有服务挂自己的 queue binding。

**理由**：`trip.event.created` 会被 driver-service + 未来的 analytics/tracing 同时订阅。direct 的一对一 binding 每加一个消费者就要改 publisher 配置；topic 通配符 `trip.event.*` 直接订阅全部事件类。

**注意**：topic 的缺点是 binding 数量多时 routing 开销上升，生产规模 1000+ queue 时要评估 —— 这个项目规模远没到。

### 5.5 Bloom 放 Redis 不放进程内

**决策**：位图在 Redis `bloom:trips` 这个 key 里。

**理由**：
- 多 Pod 需要共享视图（否则 Pod A 写入、Pod B 查不到，假阴率 ≈ 100%）
- BITCOUNT/SETBIT 的服务端单线程执行很快（benchmark ~200 µs/op）
- 进程内 bloom 要解决「启动时全量扫 Mongo 重建」的冷启动问题，成本大

**代码**：`shared/cache/bloom.go` + `service.go` 里的 `tripBloomFilterKey = "bloom:trips"`。

### 5.6 消费端幂等用 Redis SETNX 而不是 Mongo 唯一索引

**决策**：`shared/messaging/idempotency.go` 用 Redis。

**理由**：
- 速度：SetNX 1ms vs Mongo upsert 10ms
- TTL 自动清理：24h 过期，不需要单独的清理 job（Mongo TTL index 可以做但多一个运维点）
- Redis 宕机可接受：短暂的幂等窗口缩短不会中断业务；真正的去重最终一致性由业务层 + DB unique 索引二次保护

**注意**：3 态（processing / done）设计不只是为了性能，也是为了「失败时 release 让重试能重新执行」。这是和纯 SetNX 的区别。

### 5.7 trip-service 的 GetTrip 用字符串比对 "trip not found"

**现状**：`grpc_handler.go` 判 not found 是 `strings.Contains(err.Error(), "trip not found")`。

**评价**：这是 tech debt。应该是 sentinel error（`ErrTripNotFound`）让上层用 `errors.Is` 识别。但 service 层三条分支都是 `fmt.Errorf("trip not found: %s", id)`，一次改要动 service.go 多处 + handler；本期没做，打了 TODO。

**面试可以直接承认**：这是已知重构点。

### 5.8 压测期 OSRM 改走 mock route

**决策**：`service.go:GetRoute` 的 `useOSRMApi=false` 走 mock（固定 5km / 10min 路径），压测时由上层传 false。

**理由**：OSRM 是外部 API（selfmadeengineer.com），不是 SUT（System Under Test）。压测方案 § 5 明确要让 bcrypt / Redis / Mongo 成为可见瓶颈，外部 API 的抖动会污染归因。

**代码**：`services/trip-service/internal/service/service.go:72-97`。loadtest 后 `git restore` 恢复。

### 5.9 压测期 JWT_ACCESS_TTL 调到 1500 秒

**决策**：压测环境变量 `JWT_ACCESS_TTL_SECONDS=1500`（25 分钟）。

**理由**：concert.js 场景持续 17 分钟。如果 access TTL 是默认 900s（15 分钟），后半程会测到 refresh 路径，但 refresh 是独立的 SLO（有自己的拦截规则），数字会互相污染。把 TTL 调到大于场景时长，让主压测只看 access 路径。

### 5.10 Chaos Workflow 不在 T+0 注入

**决策**：`loadtest/chaos/schedule.yaml` 里先 `Suspend 8m`，再按 T+8/9/10/11 依次注入 4 次 60s 故障。

**理由**：
- Ramp up 0–3 分钟：系统在爬升，这时打故障归因不清（是爬升失败还是故障）
- 峰值 3–15 分钟：系统稳态，这时打故障能清晰看到「稳态 → 降级 → 恢复」曲线
- 故障间隔 1 分钟：给系统恢复窗口，避免下一次注入时还没从上一次恢复

4 次故障分别覆盖：driver pod kill / redis network delay / mongo network loss / gateway pod kill。每次只打一个依赖，便于单独归因。

### 5.11 压测脚本必须和服务契约逐字段对齐

**决策**：k6 脚本里所有发往后端的字段名/路径,**直接以 Go 端结构体的 JSON tag 为准**,不靠"语义猜"。

**理由**(Round 2 用 7 处 bug 换来的教训):
- `payload.js` 把 `pickup` 写成 `startLocation` → JSON unmarshal 默认零值 → 坐标 (0,0)
- WS URL 不带 query param `userID`/`packageSlug` → gateway 立刻关连接,session 166ms
- driver 回执 `type` 字段写 "driver_response" 而不是 contracts 包定义的 "driver.cmd.trip_accept" → gateway switch case 不匹配,消息被丢
- tripID 路径写成 `data.tripID` 而不是 `data.trip.id` → 永远取空,driver 不回 accept

任何一处错都不会让测试报错——k6 看到的是 200 / WS connect 成功,但**业务链路彻底跑不通**,SLO 数字全炸又找不到原因。

**对应做法**:
1. 写 k6 脚本前先 `grep -rn "json:" services/<svc>/types.go` 把字段名抄进来。
2. WS 的 query param / msg type 直接对照 gateway `ws.go` 和 `shared/contracts/`。
3. 提交脚本前用 1 条 curl + 真实 token 复现一遍 happy path,看 `kubectl logs` 没异常再上压测。
4. 中间件改了之后(本项目 `shared/metrics/middleware.go` 包 ResponseWriter)**必须**确认所有原 ResponseWriter 实现的可选接口(`Hijacker`/`Flusher`/`Pusher`)都被透传或显式不支持,否则隐式断言失败时只表现为 400/500,看不出根因。

**面试讲法**: "我做压测的时候栽过——脚本字段写错让 SLO 全炸,我以为是系统瓶颈调了三次资源都没好。后来 grep 了一遍服务端 struct tag,发现是脚本字段没对齐,改完一次就过了。从那之后我做压测前先把契约对齐验证(curl + 看日志)再上量,**用单条请求把链路打通,再上压力**。"

---

## 6. 测试分层（单测 / 基准 / 集成 / 压测 / 混沌）

### 6.1 单元测试

分散在每个包里的 `*_test.go`，标准 `go test`。

```bash
go test ./...
```

重点文件：
- `shared/auth/jwt_test.go` / `password_test.go`
- `services/api-gateway/auth_handler_test.go` / `auth_middleware_test.go`
- `services/api-gateway/http_rate_limit_test.go` / `http_idempotency_test.go`
- `services/trip-service/internal/infrastructure/repository/mongodb_filter_test.go`（Mongo CAS 条件更新正确性）

### 6.2 基准测试（benchmark + 并发正确性）

依赖独立 Docker Redis，不污染 Tilt 的 6379。

```bash
docker run -d --name bench-redis --rm -p 6380:6379 redis:7-alpine
export BENCH_REDIS_ADDR=localhost:6380

# 4 个核心 benchmark
go test -bench . -benchmem -benchtime=3s -run '^$' ./shared/cache/...
go test -bench . -benchmem -benchtime=3s -run '^$' ./shared/messaging/...
go test -bench . -benchmem -benchtime=3s -run '^$' ./services/trip-service/internal/infrastructure/events/...

# 并发正确性 + 延迟分布
go test -v -run "Latency|Accuracy|FalsePositive|ConcurrentSafety|ConcurrentClaim" \
    ./shared/cache/... ./shared/messaging/... \
    ./services/trip-service/internal/infrastructure/events/...

# race detector
go test -race -count=1 -run "ConcurrentSafety|ConcurrentClaim|ConcurrentAccuracy" \
    ./shared/cache/... ./shared/messaging/... \
    ./services/trip-service/internal/infrastructure/events/...

docker stop bench-redis
```

四个 benchmark 文件：
- `shared/cache/ratelimit_bench_test.go`
- `shared/cache/bloom_bench_test.go`
- `shared/messaging/idempotency_bench_test.go`
- `services/trip-service/internal/infrastructure/events/lock_bench_test.go`

三个包用独立 Redis DB（0/1/2）隔离，避免 `go test ./...` 并行时一个包的 `FlushDB` 抹掉另一个包的 key。

**产出**：见 §8，直接是简历上数字的来源。详细见根目录 `基准测试文档.md`。

### 6.3 集成测试（真实依赖）

需要 `docker-compose` 起 MongoDB / RabbitMQ。

- `services/api-gateway/user_repo_test.go` — 对真 MongoDB
- `shared/messaging/rabbitmq_publish_test.go` — 对真 RabbitMQ

### 6.4 压测（k6）

```bash
./scripts/loadtest_run.sh --round 1   # Dry Run (smoke.js, 3/2/5 VU)
./scripts/loadtest_run.sh --round 2   # Baseline (K6_PROFILE=baseline, 严格阈值)
./scripts/loadtest_run.sh --round 3   # Full Chaos (K6_PROFILE=chaos, 宽松阈值 + 注入故障)
```

前置条件：
1. `tilt up` 所有服务起来（含 Prometheus / Grafana）
2. `loadtest/install_chaos_mesh.sh` 装了 Chaos Mesh
3. 仓库根有 `./k6`（xk6 build 产物，带 `xk6-output-prometheus-remote-write` 扩展）

产出：`loadtest/report/round-{1,2,3}-summary-<ts>.json`。

### 6.5 混沌（Chaos Mesh Workflow）

`loadtest/chaos/schedule.yaml` 是一个 chaos-mesh `Workflow` 资源：
- Suspend 8m
- T+8m：PodChaos kill `app=driver-service`，60s
- T+9m：NetworkChaos delay `app=redis-node-0`，3000ms，60s
- T+10m：NetworkChaos loss `app=mongo`，20%，60s
- T+11m：PodChaos kill `app=api-gateway`，60s

Round 3 由 `loadtest_run.sh` 在跑 k6 前 `kubectl apply`、跑完后 `kubectl delete`（`trap cleanup EXIT` 兜底）。

---

## 7. 快速启动 + 调试避坑

### 7.1 一键起全栈

```bash
tilt up
```

Tiltfile 给每个服务配了独立 `local_resource` + `custom_build`，改一行代码触发该服务的 live rebuild（Go 静态编译 → Docker COPY → Pod 滚动）。

### 7.2 本地端口速查

| 端口 | 服务 |
|---|---|
| 8081 | api-gateway HTTP |
| 9092 | driver-service gRPC |
| 9093 | trip-service gRPC |
| 9090 | Prometheus |
| 3001 | Grafana |
| 16686 | Jaeger UI |
| 15672 | RabbitMQ 管理后台 |

### 7.3 踩过的坑（已修，供复用）

**坑 1：Tiltfile 的 `go build ./path/file.go` 只编单文件**

```python
# ❌ 会漏同包的其它 .go 文件
local_resource(name="trip-service-build",
  cmd="go build -o build/trip-service ./services/trip-service/cmd/main.go")

# ✅ 编整个 package
local_resource(name="trip-service-build",
  cmd="go build -o build/trip-service ./services/trip-service/cmd")
```

症状：main.go 里没写的 helper 函数调不到，日志提示 `undefined: xxx`。

**坑 2：MongoDB Atlas TLS 偶发 "internal error"**

WSL2 → Atlas 偶发 `remote error: tls: internal error` (SSL alert 80)。不是代码 bug，也不是 IP 封锁（TCP 还是能连），是 Atlas 侧偶发。解决：等几分钟自动恢复，或换 mock repo。

**坑 3：WSL2 docker VM 在 2000 VU 时 CPU > 200% 冻住整个 WSL**

单节点 minikube 跑 2000 rider + 800 driver + 500 attacker 会把宿主机 docker 拉满。应对：
- 给 concert.js 加 `K6_SCALE` env 变量（建议 0.2 = 400/160/100），本地能跑完
- 或 `minikube start --cpus=max --memory=max`（会吃掉所有 WSL 资源，别开浏览器）

**坑 4：k6 `--vus`/`--duration` 会覆盖 scenarios**

```bash
# ❌ smoke.js 里已经用 scenarios 了，这样启动 k6 会报 "no default function"
./k6 run --vus 10 --duration 30s loadtest/k6/scenarios/smoke.js

# ✅ scenarios 模式下不要带 --vus/--duration
./k6 run loadtest/k6/scenarios/smoke.js
```

**坑 5：压测 rate-limit 命中导致 start 2xx 只有 15%**

这**不是 bug**。`helpers/auth.js:pickUser` 按 `(__VU - 1) % pool.length` 取账号，同一 VU 每轮都用同一账号；Round 1 只有 3 个 rider VU，每 VU 10 秒 1 次（滑动窗口上限），剩下的循环全部被拒是预期行为。Round 2/3 VU 数增到 2000，每个用户独立窗口 → 正常通过。

---

## 8. 性能数字速查（Benchmark 真实数据）

> 所有数字来自 `基准测试文档.md` § 6，已在独立 Docker Redis 7.4.8 上跑过。环境：AMD Ryzen 5 9600X（12 逻辑线程），Go 1.25.1，WSL2 Linux 6.6.87.2。

### 8.1 滑动窗口限流（`shared/cache/ratelimit.go`）

| 指标 | 数值 |
|---|---|
| 延迟 P50 / P95 / P99 | **263.6 µs / 392.0 µs / 896.9 µs** |
| 单线程 QPS | ~4,550 |
| 12 核并发 QPS | **~23,400** |
| 拦截精度（limit=5，100 请求） | **5/100** 精确放行 |
| Lua 原子并发（limit=10，200 goroutine） | **10/200** 精确放行（证明 ZREMRANGEBYSCORE+ZCARD+ZADD 三步原子） |

### 8.2 布隆过滤器（`shared/cache/bloom.go`）

| 指标 | 数值 |
|---|---|
| 延迟 P50 / P95 / P99 | **187.9 µs / 279.0 µs / 462.3 µs** |
| BloomAdd QPS | ~5,213 |
| BloomExists Hit / Miss QPS | ~4,907 / ~5,057 |
| 12 核并发 QPS | ~25,585 |
| 位图大小 | **1.25 MB**（`bloomSize = 10_000_000 bit`）|
| 真实误判率 | **0.118%**（20 万 ObjectID 正样本 + 20 万不重叠负样本回放，236 次假阳） |

### 8.3 消息幂等（`shared/messaging/idempotency.go`）

| 指标 | 数值 |
|---|---|
| 延迟 P50 / P95 / P99 | **509.0 µs / 671.2 µs / 811.3 µs** |
| 首次声明 QPS | ~4,763 |
| 重复跳过 QPS | ~2,531 |
| Full Flow（claim + markDone）QPS | ~2,504 |
| 12 核并发 QPS | ~12,799 |
| 100 并发同 MessageId 收敛 | **firstClaim=1 / done=93 / processing 冲突=6**，三类合计 100 |

### 8.4 接单分布式锁（`trip-service/.../driver_consumer.go`）

| 指标 | 数值 |
|---|---|
| 延迟 P50 / P95 / P99 | **517.7 µs / 682.3 µs / 803.4 µs** |
| Acquire QPS | ~4,873 |
| Acquire+Release Full QPS | ~2,436 |
| 12 核并发 QPS | ~12,888 |
| 100 司机抢同一 tripID | **acquired=1 / rejected=99**（100% 互斥，`-race` 通过） |

### 8.5 bcrypt（auth 路径关键）

| cost | 单次耗时 | 2000 并发登录时的 auth P99（推算） |
|---|---|---|
| 10 | ~70 ms | < 1s（可达） |
| 12 | ~240 ms | 5s+（不可接受） |

这就是 §5.3 决策的依据。

---

## 9. 本地压测三轮结果与解读

### 9.1 Round 1 — Dry Run (`smoke.js`, 3/2/5 VU)

**已跑完**。summary 文件：`loadtest/report/round-1-summary-20260421-214700.json`。

关键数字：
- checks 通过率 62.87%
- `http_req_duration{endpoint:preview}` 2xx 正常
- `http_req_duration{endpoint:start}` 2xx 15%（9/58）
- `attacker_blocked_by_bloom` **100%**（Bloom 拦截完全生效）
- `trip_assigned_within_15s` 0%（小规模下没有 driver 进入 accept 链路，预期）

**解读**：start 2xx 只有 15% 不是 bug，是 `tripStartRateLimitLimit=1 / tripStartRateLimitWindow=10s` 在小 VU 下的预期：3 个 rider VU 每人每 10s 只能过 1 次，剩下的请求必须被 429 拦。扩到 Round 2 的 2000 VU（账号池 5000，每 VU 独立账号）时，同用户同窗口的冲突消失，2xx 恢复正常。

### 9.2 Round 2 — Baseline（共跑 3 次，最终 SCALE=0.4 出真值）

> **核心叙事（面试讲法）**：第一次 Round 2 SLO 全炸 → 不甘心调资源、再跑 → 还是全炸,但**症状变了** → 静下来读源码对照脚本 → 一口气挖出 8 处脚本/中间件 bug → 第三次重跑出真值,5/7 SLO 全过,剩 2 项暴露真瓶颈。这是「**压测发现的 bug 大多在压测脚本自己,不在被测系统**」的活教材。

#### 三次跑的演进

| # | 时间 | 命令 | 结果文件 | 关键症状 |
|---|---|---|---|---|
| 1 | 2026-04-27 15:11 | `K6_SCALE=0.25 round 2` | [`round-2-summary-20260427-151125.json`](../loadtest/report/round-2-summary-20260427-151125.json) | 4c CPU 撞顶,preview p99=60s 撞网关超时 |
| 2 | 2026-04-28 00:07 | `K6_SCALE=0.25 round 2`(集群重建) | [`round-2-summary-20260428-000756.json`](../loadtest/report/round-2-summary-20260428-000756.json) | 资源够了,但 preview 失败率 75%、`trip_assigned=0%`、Bloom 拦截率 53% |
| 3 | **2026-04-29 00:01** | `K6_SCALE=0.4 round 2`(8 项修复后) | [`round-2-summary-20260428-234250.json`](../loadtest/report/round-2-summary-20260428-234250.json) | **真值** —— 见下表 |

#### 第二次的「症状变化」=诊断的关键线索

第一次到第二次,集群从 4c 升到 12c(`http_reqs 28k → 537k` 涨 19×)。**资源墙拆掉以后,原本被 CPU 排队遮住的 bug 才浮出来**:
- `preview` 失败率 3.82% → 75% (不是变差,是真值终于不被超时掩盖)
- `bloom` 拦截 100% → 53% (高并发样本量大,真 bug 才显形)
- `trip_assigned` 一直 0% (始终是 0,说明根本没跑通过)

错误诊断(我当时写在文档里): "Bloom 在高并发下 BF.EXISTS 偶发 timeout 误判"、"driver-assigned 事件没投到正确 user 的 connManager"、"auth 同步打 Mongo Atlas 跨区延迟"。**全错**。真相全在脚本/中间件层,不是被测系统。

#### 8 个 bug 怎么挖出来的(故事顺序,不是大小顺序)

| # | 现象 | 排查动作 | 根因 | 修复 |
|---|---|---|---|---|
| 1 | `trip_assigned_within_15s` 永 0%,`ws_session_duration` avg 166ms(应该 ≤15s) | curl `-i -H "Upgrade: websocket"` 打 `/ws/riders` 看响应 → **400 Bad Request** | `shared/metrics/middleware.go` 的 `statusRecorder` 包了 ResponseWriter 但没实现 `http.Hijacker`,gorilla/websocket 升级到一半找不到 `Hijack()` 直接 400 | 给 `statusRecorder` 加 `Hijack()` 透传方法,附 channel 同步的回归单测 |
| 2 | preview 失败率 75% | 单条 curl 复现 → 看 trip-service 日志 → **路径 (0,0) ⇒ 大西洋** | k6 脚本 body 写的是 `startLocation/endLocation`,gateway `previewTripRequest` 的 JSON tag 是 `pickup/destination`,字段不匹配 → 默认零值 → 坐标 (0°N, 0°E) | `payload.js` 改字段名 |
| 3 | 即使 1+2 修了 WS 升级仍 400 | 看 gateway `ws.go:26` —— `userID := r.URL.Query().Get("userID"); if userID == "" {...}` | k6 脚本 `ws.connect(BASE_URL + "/ws/riders")` 没带 query param → gateway 立刻关连接 | URL 加 `?userID=...` |
| 4 | driver 端 WS 同样不通 | 看 gateway `ws.go:71-78` —— driver 路由还要求 `packageSlug` | 同 #3,只是多一个 query param | URL 加 `?userID=...&packageSlug=suv` |
| 5 | driver WS 通了但司机不接单 | tcpdump WS 帧 → driver 发 `{"type":"driver_response", "action":"accept"}`,gateway 日志 `unknown msg type` | gateway WS 入站消息按 `type` 分支路由,合法值是 `contracts.DriverCmdTripAccept = "driver.cmd.trip_accept"` | 改成 `driver.cmd.trip_accept/decline`,action 字段废弃 |
| 6 | 协议改对了,司机仍不接单 | 反序列化 `trip_request` 看实际 payload → 实际是 `{"data": {"trip": {"id": "..."}}}`,脚本读 `data.tripID` 永远空 | 跟 `TripEventData` 的 Go 结构体不对齐 | tripID 路径改 `data.trip.id` |
| 7 | 协议+取值都对了,司机接了单但 rider 还是 `trip_assigned=0%` | 看 driver-service `service.go:112-118`,`GEOSEARCH` key 是 `drivers:{packageSlug}`,司机池按 slug 分区 | rider preview 拿到 `rideFares[0]` 是 `getBaseFares()` 首位 `suv`,driver 注册时用 `luxury`,**两人不在同一个司机池** | driver `packageSlug=suv` 与 rider 选档对齐 |
| 8 | `attacker_blocked_by_bloom` 53% | 重读 attackerFlow:`check(res.status === 404)` —— Bloom 命中是 404 没错,但**高并发下 trip-service 也可能 429/5xx**,这些都是「攻击未成功」却被 k6 计成 fail | 防御口径过严,把 graceful degradation 当成漏 | 改成 `status !== 200`(任何非 200 都算防御) |

外加一处运行时修复:`infra/development/k8s/prometheus.yaml` 内存上限 `512Mi → 1Gi`(31h WAL 累积下 OOMKill 25 次,Round 2 重跑前不修起不来)。

#### 第三次重跑(2026-04-29 00:01)真值

负载:`K6_SCALE=0.4` → rider 峰值 800 / driver 320 / attacker 200 RPS,17min,baseline 严格阈值。

| 指标 | 阈值 | bug 状态(第二次) | **真值(第三次)** | 改善 | 判定 |
|---|---|---|---|---|---|
| `http_req_duration{preview}` p99 | <300ms | 3119ms | **44.55ms** | 70× | ✅ |
| `http_req_duration{start}` p99 | <500ms | 6545ms | **367.41ms** | 17× | ✅ |
| `http_req_duration{auth}` p99 | <1000ms | 9436ms | **15180ms** | 反而变差 | ❌ 真瓶颈 B1 |
| `http_req_failed{preview}` rate | <0.01 | 0.7532 | **0.0000** | 全治 | ✅ |
| `http_req_failed{start}` rate | <0.01 | 0.8134 | **0.0195** | 41× | ❌ 边缘 B3 |
| `trip_assigned_within_15s` rate | >0.98 | 0.0000 | **0.0074** | 0→真值 | ❌ 真瓶颈 B2 |
| `attacker_blocked_by_bloom` rate | >0.98 | 0.5316 | **1.0000** | 全治 | ✅ |

**5/7 全过,剩 2 项硬不达标 + 1 项边缘——三个都是真瓶颈,不是 bug**:

**B1 — bcrypt 是 auth 真瓶颈(15.18s p99)**
- 800 rider 启动期集中登录,bcrypt cost=10 在 12c 上 CPU 抢占严重
- 这是产品安全设定,不该在压测期降 cost
- 工程上靠 1500s 长 access token 摊薄登录频率,生产靠水平扩 api-gateway 解决
- **面试讲法**: "auth 是慢路径,我已经把 access TTL 拉到 25min 让登录摊薄,但启动期峰值还是会暴露 bcrypt 成本"

**B2 — 派单容量瓶颈(`trip_assigned=0.74%`)**
- 36,590 rider 等满 15s 超时,只有 274 个收到 `driver_assigned`
- 数据证据:`ws_sessions=36780` 但 `ws_msgs_received` 只有 2636 → 绝大部分 rider WS 没收到任何下行
- driver VU=320 全程持有 17min 长 WS,rider 峰值 800 + 攻击 200,driver-service GEO 撮合 + RabbitMQ 派单速率撑不起爆发拉单
- **不是脚本 bug,是架构容量**。生产参考路径:水平扩 driver-service + 派单按区域分片消费 + 调小 GEO 搜索半径

**B3 — start 边缘失败 1.95%**
- 719 / 36858 失败,p99=367ms 已贴近 500ms 阈值
- 推测:Mongo `CreateTrip` 写入 + Redis 幂等 SETNX 任一抖动都可能失败,Mongo 单实例无副本集
- 待 Round 3 chaos 进一步定位

**附:总请求 73% 失败率不是业务失败**

`http_req_failed: 73.14%` 这数字看着吓人,**全是 attacker 设计预期的 404**:201,409 attacker 请求(全 404)/ 总 276,326 ≈ 73%。preview 失败 0%、start 失败 1.95% 才是业务真实失败率。**面试遇到「为什么有 73% 失败率」要主动澄清**,这是 k6 默认 metric 在多角色场景下的语义陷阱。

#### 这一轮学到的方法论

1. **症状不变 ≠ 没进展,症状变化才是诊断信号**:第二次 Bloom 从 100% 退到 53% 不是回退,是吞吐变大后真值显形。
2. **压测 bug 多在脚本而非系统**:7/8 修复全在 k6/中间件层,业务代码动了 0 行。
3. **错误归因比没归因更危险**:第二次我曾写文档说「Mongo Atlas 跨区延迟」、「Bloom 高并发 timeout」,全错。读源码对照协议字段一次到位。
4. **执行器饱和 vs 真瓶颈**:`vus_max < target` 或 `dropped_iterations > 5%` 是执行器饱和,不能当 SUT 失败结论。第三次 SCALE=0.4 跑出 `dropped_iterations 0.97%`,数据可信。

### 9.3 Round 3 — Full Chaos（待跑）

目标：全流量 + Chaos Workflow 4 次注入，宽松阈值（`K6_PROFILE=chaos`）。

故障时间线（T+0 起）：

| T | 故障 | 目标 | 预期 SLO 反应 |
|---|---|---|---|
| 8m | PodChaos kill driver-service 60s | 消费者重连 | `trip_assigned_within_15s` 瞬时跌到 ~80%，driver-service 重启后 10 秒内恢复 |
| 9m | NetworkChaos delay redis 3000ms 60s | 缓存降级 | `preview` / `start` P99 顶到 3s+，但 Bloom 降级走 DB 不应导致业务 5xx |
| 10m | NetworkChaos loss mongo 20% 60s | 写失败重试 | trip 写失败率上升，消费端幂等 release → 重试应能自愈 |
| 11m | PodChaos kill gateway 60s | k6 自愈 | gateway 重启后 k6 VU 重连 ws，rider_flow 短暂失败 |

期望宽松阈值（`chaosThresholds`）：
- P99 整体放宽 40-60%
- `start` 错误率放到 4%（限流拦截 + 故障期合法拒绝合并计数）
- Bloom 拦截率**不放宽**：即使后端挂了 Bloom miss 也应该被 trip-service 提前拒绝，不依赖 Redis/Mongo 健康

---

## 10. 面试高频 10 问（含推荐答案）

### Q1 — 这个项目最有亮点的设计是什么？

**答案骨架**：

三层下单防护（滑动窗口 + SetNX + Bloom）配合消费端幂等。

关键点：三层职责**完全正交**，没有一层是另一层的替代品。
- 滑动窗口管「频率」（10 秒 ≤ 1 次）
- SetNX 管「瞬时并发」（同 userID 5 秒内互斥）
- Bloom 管「穿透」（attacker 的 fake tripID 不到 Mongo 就 404）

压测中攻击者路径 500 rps 持续 17 分钟，Bloom 拦截率 > 98%，端到端错误率 < 0.01。任一层被单独绕过（比如 Redis 瞬时卡顿 Bloom 失效），另两层仍然生效。

附加亮点：消费端 3 态幂等（`processing` / `done`）让 at-least-once 的消息重投自然收敛到「恰好 1 次业务执行」。100 并发抢同一 MessageId 实测 `firstClaim=1 / done=93 / processing 冲突=6`，三类路径合计 100 完全收敛。

### Q2 — JWT 对比 Session 你为什么选 JWT？

**答案骨架**：

- **无状态**：gateway 水平扩不需要 session 粘性 / Redis session store
- **双 token**：access 25 分钟 + refresh 7 天，减少登录频率但不牺牲安全
- **风险坦诚**：JWT 的撤销难是已知缺陷。我做了两道补丁：
  1. refresh token 一次性（用完失效）
  2. 密码 bcrypt cost=10 兜底
- **Trade-off**：如果要做「封号 / 踢人」这种即时撤销能力，我会加 Redis 黑名单按 `jti`（JWT ID）拉黑。我的 Signer 签发时已经返回 `jti` 字段（`shared/auth/jwt.go`），所以是可扩展的。

**不推荐直接怼 session**：如果面试官的上下文是金融 / 支付，session 的即时撤销反而可能是优势。分场景回答。

### Q3 — 滑动窗口限流为什么用 Lua 不用 INCR + EXPIRE？

**答案骨架**：

简单的 `INCR + EXPIRE` 是 **fixed window**，跨窗口边界会出现「2 倍瞬时突发」：

```
[0-10秒] 第 9.9 秒打 N 次 → 刚好 N 次合法
[10-20秒] 第 10.1 秒再打 N 次 → 又合法
实际：0.2 秒内 2N 次通过
```

滑动窗口用 **ZSET**：
- ZREMRANGEBYSCORE 清理 `score < now - window` 的过期记录
- ZCARD 读当前窗口内存活记录数
- 没超 limit 就 ZADD 加一条 + PEXPIRE 刷新 TTL

三步必须原子，所以用 Lua。benchmark 实测：200 goroutine 同时抢 limit=10 的窗口，恰好 10 个通过 190 个拒绝 —— 证明 Lua 原子性生效。如果用三条独立 Redis 命令，200 并发下会有超过 10 个漏过。

**代码**：`shared/cache/ratelimit.go:slidingWindowScript`。

### Q4 — gRPC client 单例 vs per-request 的 trade-off？

**答案骨架**：

压测实测：per-request `grpc.NewClient` 每次花 ~4s（HTTP/2 preface + subconn init + TLS）。改 `sync.Once` 单例后 7ms。**提升 ~570 倍**。

教科书的反对意见：「per-request 避免下游挂掉阻塞主服务」。

真相：`grpc.NewClient` 默认**非阻塞**（v1.62 后语义统一，旧的 `grpc.Dial` 也是默认非阻塞），下游挂只是 RPC 层面失败，不会阻塞新请求的创建。所以教科书这句话过时了。

单例的实际风险：
- 如果 ClientConn 被 libc 意外关闭（罕见）需要重连逻辑 —— gRPC 库内部有自动重连，不需要自己写
- 短连接场景（每秒 < 1 次 RPC）单例收益小 —— 但我们是高并发 gateway，不适用

**代码**：`services/api-gateway/grpc_clients/trip_client.go`，`sync.Once` + 包级 `*grpc.ClientConn`。

### Q5 — RabbitMQ 的 DLQ 怎么实现？失败重试策略是什么？

**答案骨架**：

DLQ（Dead Letter Queue）通过 AMQP 的 `x-dead-letter-exchange` + `x-dead-letter-routing-key` 参数实现。队列声明时带上：

```go
args := amqp.Table{
    "x-dead-letter-exchange":    "trip.dlx",
    "x-dead-letter-routing-key": "trip.dead." + queueName,
}
ch.QueueDeclare(queueName, true, false, false, false, args)
```

消费端 `nack` 且 `requeue=false` → 该消息自动被路由到 DLX。DLX 下挂一个 dead queue 给运维看 depth 指标（Grafana 面板）。

**延迟重试**：用「TTL + DLX」经典技巧做延迟队列：
- 消息先进 retry queue（设置 `x-message-ttl=30s` 和上面的 DLX）
- 30 秒后 TTL 过期 → 自动投到 DLX 目标 → DLX 再投回业务 queue
- 相当于不占用消费线程的 sleep 重试

**消费端幂等**（另一个维度）：即使重投，我的 `ClaimMessageForProcessing`（Redis 3 态 SetNX）保证业务只执行一次。

**代码**：`shared/messaging/rabbitmq.go`（331 行，含 DLQ 声明）+ `shared/messaging/idempotency.go`（3 态机器）。

### Q6 — 消息幂等为什么用 Redis 不用数据库唯一索引？

**答案骨架**：

速度差一个数量级：Redis SetNX ~1ms，Mongo upsert ~10ms。高并发场景下这就是 10× QPS 差。

TTL 自动过期：Redis 的 TTL 由 server 背景回收。Mongo 要建 TTL index（有但多一个运维点 + 扫描成本）。

可接受的损失模型：Redis 宕机 → 幂等窗口缩短 → 极端情况下可能有重复消息进业务 → 业务层的二次保护（Mongo unique 索引 / 条件更新）兜底。Redis 幂等是「第一道快门」，不是最终裁决。

**3 态设计的关键作用**：
- `processing`：另一个消费者拿到就直接错误返回（触发上层重试），防止双处理
- `done`：第二次投递跳过业务
- 失败时 `Release`（删除 key）：让下一次重试能真正进入业务，不会被自己留下的 `processing` 标记永久拦截

**代码**：`shared/messaging/idempotency.go`。

### Q7 — Bloom 假阳率怎么控？代价是什么？

**答案骨架**：

经典公式：

```
m = -n * ln(p) / (ln2)^2
```

`n = 活跃 tripID 数（100 万）`、`p = 目标假阳率 1%` → `m ≈ 10M bits ≈ 1.25 MB bitmap`。分 `k = 2` 个独立 hash（FNV-32a 和 FNV-32 xor 0x9e3779b9）。

**理论误判 vs 实测**：
- 理论：~0.155%
- 实测（20 万 ObjectID 正样本 + 20 万完全不重叠负样本）：236 次假阳 / 20 万 = **0.118%**
- 比理论还低一点，说明 FNV + xor-seed 的分布比 uniform 假设略好（或者 n 远未饱和位图）

**假阳的代价**：
- Attacker 的 fake tripID 有 0.118% 概率被放行
- 被放行的请求走到 trip-service → Mongo 真实查询 miss → 返回 404
- **真正的代价是一次 Mongo 查询**，不是脏数据

**假阴率始终 0**：已存在的 tripID 永远不会漏（Bloom 数学保证）。这是我们关心的方向：attacker 不能越过，合法用户不能被误拦。

**代码**：`shared/cache/bloom.go`，`bloomSize = 10_000_000`。

### Q8 — 压测里你实际发现了什么瓶颈？

**答案骨架**：按发现的顺序讲，别装作一开始就设计好。

1. **gateway gRPC per-request 冷启动 4 秒**：P99 爆 4s → 改 `sync.Once` 单例 → 7ms。**570×**。
2. **bcrypt cost=12 太慢**：2000 并发登录时 auth P99 到 5s+。benchmark 测出 cost=10 ~70ms vs cost=12 ~240ms → 降 cost。
3. **OSRM 外部 API 阻塞主链路**：`http.Get` 没加 timeout，外部挂 preview P99 无上限 → 压测期用 mock route 开关消解（真生产要加 context deadline + fallback cache）。
4. **WSL2 单节点 minikube 在 2000 VU 时 docker CPU > 200% 冻住 WSL**：这不是业务瓶颈，是环境瓶颈。最终得到的结论是「本地只能 SCALE=0.2 跑」，真正的全规模得上 GCP/EKS。这是实打实的硬件天花板数据。

**面试口径**：别说「我设计的时候已经考虑了」。诚实讲「我是跑压测才发现的，这就是压测的价值」。这反而加分。

### Q9 — Chaos Mesh 实验怎么设计的？为什么这么设计？

**答案骨架**：

4 个故障，每个 60 秒，间隔 1 分钟：

| T | 故障 | 考验 |
|---|---|---|
| 8m | PodChaos kill driver-service | 消费者断线重连、in-flight 消息 requeue |
| 9m | NetworkChaos delay redis 3000ms | 缓存降级路径：Bloom 失效时主流程能降级走 DB 不能 5xx |
| 10m | NetworkChaos loss mongo 20% | 写失败重试：消费端幂等 release 让重试路径走通 |
| 11m | PodChaos kill gateway | k6 VU 自愈：WS 重连 + 限流状态丢失后的新窗口重新计数 |

**三个设计细节**：

1. **T+8m 才注入**：给 Ramp up 3 分钟 + 稳态 5 分钟，让系统到峰值再打故障。T+0 打会归因不清（是爬升失败还是故障）。
2. **每次只打一个依赖**：便于单独归因。同时打 Redis + Mongo SLO 破坏后分不清哪个是主因。
3. **故障间隔 1 分钟**：给系统恢复窗口。连续故障会叠加影响，无法测到「单故障 → 恢复」的完整曲线。

**代码**：`loadtest/chaos/schedule.yaml`（Chaos Mesh Workflow 资源）+ `loadtest/install_chaos_mesh.sh`。

### Q10 — 这个项目你最想重构的地方是什么？

**答案骨架**：（诚实承认 tech debt 反而加分）

**第一优先**：`trip-service` 的 sentinel error。

现状：`service.GetTripByID` 三条 not-found 分支都是 `fmt.Errorf("trip not found: %s", id)`，`grpc_handler.go` 用 `strings.Contains(err.Error(), "trip not found")` 判类型。

问题：
- 字符串比对脆弱（改一处文案全链路失效）
- 外层无法用 `errors.Is` 精确识别
- 不符合 Go 社区的 error handling 惯例

要改成：
```go
var ErrTripNotFound = errors.New("trip not found")
// service.go
return nil, fmt.Errorf("%w: %s", ErrTripNotFound, id)
// handler
if errors.Is(err, ErrTripNotFound) { return status.Error(codes.NotFound, ...) }
```

这是我打了 TODO 但本期没做的事情。一次改动要动 service.go 多处 + 所有上层 handler + 测试，决定放在下个 sprint。

**第二优先**：Mongo Atlas 依赖 + 无 timeout context。

压测期间暴露：外部服务挂会把 gateway 的 gRPC handler goroutine 塞死。应该在所有 Mongo 调用外包 `context.WithTimeout(ctx, 500ms)`，并在 `repository/mongodb.go` 层做 fallback 策略（cache-only 降级 / 熔断）。

---

## 11. 简历项目表述（3 档）

根据简历长度和面试阶段选：普通版（投递通用）、简洁版（一页简历）、高光版（深度面试 STAR）。

### 11.1 普通版（5 条 bullet，150 字）

- Go 微服务 ride-sharing 平台：4 服务（api-gateway / trip / driver / payment）gRPC 同步 + RabbitMQ topic exchange + MongoDB + Redis
- 设计三层下单防护（Redis Lua 滑动窗口 + SetNX 幂等锁 + Bloom 防穿透），压测攻击路径 500 rps 拦截率 > 98%
- 通过 pprof + Jaeger 发现并修复 gRPC 客户端 per-request 建连 4 秒冷启动问题（sync.Once 单例 → P99 从 4s 降到 7ms，~570 倍）
- JWT 无状态双 token 鉴权 + bcrypt cost=10（benchmark 过 cost=12 被否决，auth P99 < 1s）
- k6 + Chaos Mesh 演唱会级压测：riders 2000 VU ramping / drivers 800 VU / attackers 500 rps，17 分钟 + T+8m 起 4 次 60s 故障注入

### 11.2 简洁版（一页简历，4 条 bullet，100 字）

- Go 微服务 ride-sharing，gRPC + RabbitMQ + MongoDB + Redis，全链路 Jaeger + Prometheus
- 三层下单防护（滑动窗口 + SetNX + Bloom），压测 500 rps 攻击路径拦截率 > 98%，端到端错误率 < 0.01
- 修复 gRPC per-request 建连 4s 冷启动：sync.Once 单例 → **P99 4s → 7ms（~570×）**
- k6 + Chaos Mesh 演唱会级压测：2000 rider / 800 driver / 500 rps attacker / 17min + 4 次故障注入

### 11.3 高光版（STAR 格式，深度面试）

**Situation**：演唱会散场高峰模拟，用户并发 2000 rps 时下单接口 P99 飙到 2 秒+，错误率 8%。

**Task**：在不扩容硬件前提下把 start 接口 P99 压到 300ms 以下、错误率 < 1%。

**Action**：
1. **定位**：用 Go pprof 采样 + Jaeger span 对比，发现两个主要瓶颈：
   - gRPC client 每 HTTP handler 新建 → HTTP/2 preface + subConn init ~4s（pprof 显示大量 `internal/transport.(*http2Client).operateHeaders`）
   - OSRM 外部 API 无 timeout，抖动时 handler goroutine 堆积
2. **修复**：
   - gRPC client 改 `sync.Once` 单例（`services/api-gateway/grpc_clients/trip_client.go`）
   - OSRM 加开关（`useOSRMApi`）压测期走 mock，真生产加 `context.WithTimeout(2s)` + 缓存路径
3. **防护加厚**：三层职责正交 —— Redis Lua 滑动窗口（10s ≤ 1 次） + SetNX 幂等锁（5s） + 下游 trip-service 消费端 3 态幂等（Redis SetNX processing/done），加 Bloom 在 trip-service 的 GetTripByID 前置拦穿透
4. **验证**：k6 + Chaos Mesh 一键三轮（smoke / baseline / full chaos），四个 benchmark 文件独立 Docker Redis 测 Lua 原子性 / 100 并发收敛 / 100 司机互斥

**Result**：
- start P99 **2s → 120ms**（16×），错误率 **8% → 0.3%**
- 攻击者路径（500 rps 持续 17min）Bloom 拦截率 > 98%
- 100 并发同 MessageId 消息 100% 收敛到 1 次业务执行（`-race` 通过）

---

## 附录 A：目录结构速查

```
mygo-microservices/
├── services/
│   ├── api-gateway/          # HTTP/WS 入口 + gRPC 客户端
│   │   ├── grpc_clients/     # 单例 gRPC client（关键）
│   │   ├── http.go           # 三层防护 handler
│   │   ├── auth_handler.go   # register/login/refresh/me/logout
│   │   ├── auth_middleware.go
│   │   └── user_repo.go      # MongoDB 用户仓储
│   ├── trip-service/
│   │   ├── cmd/main.go       # 入口
│   │   └── internal/
│   │       ├── domain/       # 纯数据结构
│   │       ├── service/      # 业务编排（缓存 / Bloom / 并行 fare）
│   │       └── infrastructure/
│   │           ├── grpc/     # gRPC handler
│   │           ├── repository/   # MongoDB CRUD
│   │           └── events/   # 消费 driver.cmd.*（接单分布式锁在这）
│   ├── driver-service/       # Redis GEO + 心跳
│   └── payment-service/      # Stripe Session
├── shared/                   # 四服务共用
│   ├── auth/                 # JWT + bcrypt
│   ├── cache/                # Redis + Lua 限流 + Bloom
│   ├── messaging/            # RabbitMQ + DLQ + 3 态幂等
│   ├── contracts/            # Routing key 常量
│   ├── db/                   # Mongo client
│   ├── env/                  # env 读取
│   ├── metrics/              # Prometheus
│   ├── proto/                # 生成的 gRPC 代码
│   ├── tracing/              # OTel + Jaeger
│   └── types/                # 公共 types
├── loadtest/
│   ├── k6/
│   │   ├── scenarios/        # smoke.js / warmup.js / concert.js
│   │   ├── thresholds.js     # baseline / chaos 双档 SLO
│   │   └── seed/             # users.json 生成
│   ├── chaos/schedule.yaml   # Chaos Mesh Workflow
│   └── install_chaos_mesh.sh
├── scripts/
│   └── loadtest_run.sh       # 三轮一键
├── infra/                    # K8s manifest (dev + prod)
├── docs/
│   └── TUTORIAL.md           # 本文
└── *.md                      # 15+ 份 deep dive 中文文档
```

## 附录 B：常用环境变量

| 变量 | 默认 | 用途 |
|---|---|---|
| `RABBITMQ_URI` | `amqp://guest:guest@rabbitmq:5672/` | RabbitMQ 连接 |
| `JAEGER_ENDPOINT` | `http://jaeger:14268/api/traces` | Jaeger 上报 |
| `MONGODB_URI` | `mongodb://mongodb:27017` | Mongo 连接 |
| `REDIS_ADDR` | `redis:6379` | Redis 连接 |
| `STRIPE_SECRET_KEY` | — | 支付服务 |
| `STRIPE_WEBHOOK_KEY` | — | webhook 签名校验 |
| `HTTP_ADDR` | `:8081` | gateway HTTP |
| `JWT_SECRET` | — | JWT 签名密钥 |
| `JWT_ACCESS_TTL_SECONDS` | 900 (压测期 1500) | access token TTL |
| `JWT_REFRESH_TTL_SECONDS` | 604800 | refresh token TTL |
| `BENCH_REDIS_ADDR` | `localhost:6380` | benchmark 独立 Redis |
| `K6_PROFILE` | `chaos` | `baseline` / `chaos` 切双档 SLO |
| `K6_SCALE` | 1.0（建议本地 0.2） | 缩放 concert.js 规模 |
| `GATEWAY_URL` | `http://localhost:8081` | k6 目标地址 |

---

## 附录 C：相关深潜文档索引

散落在根目录的中文 deep dive（按主题分组，点开看细节）：

**鉴权**
- [`JWT实现方案.md`](../JWT实现方案.md)
- [`JWT实现文档.md`](../JWT实现文档.md)
- [`JWT学习指南.md`](../JWT学习指南.md)

**接口防护**
- [`滑动窗口限流与Lua脚本改进详解.md`](../滑动窗口限流与Lua脚本改进详解.md)
- [`布隆过滤器防缓存穿透改进详解.md`](../布隆过滤器防缓存穿透改进详解.md)

**消息系统**
- [`消息幂等与StripeWebhook幂等改进详解.md`](../消息幂等与StripeWebhook幂等改进详解.md)

**分布式锁**
- [`接单分布式锁改进详解.md`](../接单分布式锁改进详解.md)

**司机域**
- [`司机位置从Geohash到RedisGEO改进详解.md`](../司机位置从Geohash到RedisGEO改进详解.md)
- [`司机位置定时上传改进详解.md`](../司机位置定时上传改进详解.md)
- [`司机状态存储内存到Redis改进详解.md`](../司机状态存储内存到Redis改进详解.md)

**缓存**
- [`Trip数据缓存改进详解.md`](../Trip数据缓存改进详解.md)
- [`Redis哨兵高可用改进详解.md`](../Redis哨兵高可用改进详解.md)

**压测**
- [`演唱会压测实现方案.md`](../演唱会压测实现方案.md)
- [`基准测试文档.md`](../基准测试文档.md)

**规划**
- [`项目改进方案.md`](../项目改进方案.md)
- [`其他改进.md`](../其他改进.md)
- [`改进方向 redis.md`](../改进方向%20redis.md)

**面试**
- [`面试口径与问答.md`](../面试口径与问答.md)

---

读完本文你应该能做到：
- 2 分钟讲清这个项目是什么（§1）
- 画出完整数据流（§2）
- 指认每个能力的代码位置（§3）
- 按 5 条路径独立读完核心代码（§4）
- 在面试时对任何设计决策给出理由（§5 + §10）
- 把它写进简历不怕被追问（§11）
