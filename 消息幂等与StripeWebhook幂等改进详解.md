# 消息幂等与 Stripe Webhook 幂等改进详解（面试导向）

## 1. 改进背景与问题

### 1.1 业务背景
当前系统使用 RabbitMQ，语义是 at-least-once。也就是说：

- 消费者处理失败后，消息可能重投。
- 网络抖动或消费者重启后，同一条消息可能再次到达。

如果没有幂等控制，会出现：

- Trip 状态被重复推进（例如重复 `payed`）。
- Payment Service 重复创建 Stripe Checkout Session。
- Stripe webhook 重试导致重复发布支付成功事件。

### 1.2 本次改造目标

- 发布端统一写入 `MessageId`。
- 关键消费者接入 Redis 幂等去重（声明 processing、成功标记 done、失败释放 processing）。
- API Gateway 的 Stripe webhook 按 `event.ID` 去重。
- 增加自动化测试，保证功能可验证。

---

## 2. 核心代码改动

## 2.1 发布端统一 MessageId

文件：`shared/messaging/rabbitmq.go`

```go
func newPersistentJSONMessage(body []byte) amqp.Publishing {
    return amqp.Publishing{
        MessageId:    uuid.NewString(),
        DeliveryMode: amqp.Persistent,
        ContentType:  "application/json",
        Body:         body,
    }
}
```

说明：

- MessageId 是消费者去重的主键。
- 所有 `PublishMessage` 统一走该构造函数，避免遗漏。

## 2.2 通用幂等工具（Redis）

文件：`shared/messaging/idempotency.go`

核心能力：

1. `ClaimMessageForProcessing`
- 首次消息：`SET NX` 写入 `processing` 状态。
- 已处理消息：检测到 `done` 后直接跳过。
- 正在处理冲突：返回错误触发重试，避免并发重复执行。

2. `MarkMessageProcessed`
- 业务成功后把状态写为 `done`。

3. `ReleaseMessageProcessing`
- 业务失败时删除 `processing` 占位，允许后续重试真正执行。

4. 兼容历史消息
- 若 `MessageId` 为空，回退为 `routingKey + body` 的 SHA256 摘要，避免历史消息无法去重。

## 2.3 Trip Service 消费者幂等

文件：`services/trip-service/internal/infrastructure/events/driver_consumer.go`
文件：`services/trip-service/internal/infrastructure/events/payment_consumer.go`

接入模式（简化）：

```go
claim, err := messaging.ClaimMessageForProcessing(ctx, c.redis, "trip-service:driver-consumer", msg, 10*time.Minute)
if err != nil {
    return err
}
if claim.AlreadyProcessed {
    return nil
}

// ...业务处理...

if err := messaging.MarkMessageProcessed(ctx, c.redis, claim.Key, 10*time.Minute); err != nil {
    return err
}
```

失败路径会调用 `ReleaseMessageProcessing`，确保 retry 不会被误判重复而直接跳过。

## 2.4 Payment Service 消费者幂等

文件：`services/payment-service/internal/events/trip_consumer.go`

- 对 `payment.cmd.create_session` 增加幂等声明。
- 避免重复创建 Stripe Session。

## 2.5 Stripe webhook 幂等

文件：`services/api-gateway/http.go`

```go
processed, err := markStripeWebhookEventProcessed(ctx, redisClient, event.ID)
if err != nil {
    http.Error(w, "Failed to process webhook idempotency", http.StatusInternalServerError)
    return
}
if !processed {
    w.WriteHeader(http.StatusOK)
    return
}
```

核心点：

- key: `stripe:event:{eventID}`
- TTL: 24h
- 重复事件直接 `200`，阻止 Stripe 持续重试。

## 2.6 启动注入与部署接线

新增 Redis 客户端初始化：

- `services/api-gateway/redis.go`
- `services/payment-service/cmd/redis.go`

启动接线：

- `services/api-gateway/main.go`
- `services/payment-service/cmd/main.go`
- `services/trip-service/cmd/main.go`（已有 Redis，复用）

本地部署接线：

- `infra/development/k8s/api-gateway-deployment.yaml` 增加 `REDIS_ADDR`
- `infra/development/k8s/payment-service-deployment.yaml` 增加 `REDIS_ADDR`
- `Tiltfile` 为 `api-gateway` 和 `payment-service` 增加 `redis` 依赖

---

## 3. 为什么要做（场景与困难）

### 3.1 场景

1. RabbitMQ 重试导致重复消息
- 消费者崩溃后，消息会重投。

2. Stripe webhook 重试
- Stripe 官方会对未成功响应或网络异常进行重试。

3. 支付链路副作用敏感
- 一旦重复，会产生重复状态推进、重复会话创建等问题。

### 3.2 困难

- 不能让去重破坏原有重试机制。
- 要兼容历史消息缺失 MessageId 的情况。
- 需要在多个服务中保持幂等策略一致，避免各写各的。

---

## 4. 技术原理（怎么实现）

## 4.1 RabbitMQ at-least-once 与幂等

at-least-once 的本质是“可能至少处理一次”，所以消费者必须幂等。

本次方案把幂等状态放在 Redis：

- `processing`：已抢到处理权，正在执行。
- `done`：已成功处理，后续重复消息直接跳过。

## 4.2 为什么不是只用 SetNX 一步

如果只在开头 `SET NX` 一步，会遇到失败重试时“被自己锁死”的问题。

本次采用三段式：

1. Claim（processing）
2. 业务成功后 MarkDone（done）
3. 业务失败时 Release（删除 processing）

这样既保留重试能力，又避免重复执行。

## 4.3 Stripe webhook 去重原理

`event.ID` 在 Stripe 事件中是天然幂等键：

- 首次处理：`SET NX` 成功，执行后续发布。
- 重复回调：`SET NX` 失败，直接 200。

---

## 5. 测试与验证

### 5.1 新增测试

- `shared/messaging/idempotency_test.go`
- `shared/messaging/rabbitmq_publish_test.go`
- `services/api-gateway/http_idempotency_test.go`

### 5.2 关键测试点

1. MessageId 自动生成
- 验证发布消息含非空 `MessageId`。

2. 幂等状态机
- 首次 claim 成功。
- 标记 done 后重复 claim 返回 `AlreadyProcessed=true`。
- processing 冲突返回错误。
- MessageId 缺失时 fallback 摘要仍可去重。

3. Stripe webhook
- 首次事件通过。
- 重复事件被识别为已处理。
- 空事件 ID / nil redis 参数校验。

### 5.3 本次执行命令与结果

```bash
go test ./shared/messaging/... -v
go test ./services/trip-service/... -v
go test ./services/payment-service/... -v
go test ./services/api-gateway/... -v
go build ./services/trip-service/...
go build ./services/payment-service/...
go build ./services/api-gateway/...
```

结果：全部通过。

---

## 6. 实现后提升（量化指标）

### 6.1 本次可直接验证指标

- 幂等基础能力测试通过率：100%
- API webhook 去重测试通过率：100%
- 关键服务构建成功率：100%

### 6.2 上线后建议观测指标

- `mq_duplicate_skip_total`：重复消息被跳过次数（应上升，表示去重生效）
- `payment_session_duplicate_total`：重复创建会话次数（目标接近 0）
- `stripe_webhook_duplicate_total`：重复 webhook 命中次数（可观测重试规模）
- `trip_status_duplicate_update_total`：重复状态推进次数（目标接近 0）

---

## 7. STAR（面试可直接用）

### S（Situation）
系统基于 RabbitMQ at-least-once 语义，消费者在故障重试和网络抖动下会收到重复消息；Stripe webhook 也会重试，导致支付链路存在重复副作用风险。

### T（Task）
在不破坏现有消息重试机制的前提下，建立统一的幂等处理方案，覆盖消息消费者和 webhook 入口。

### A（Action）
我先在发布端统一注入 MessageId，再抽象 Redis 幂等工具（processing/done 状态机），并接入 Trip Service 与 Payment Service 关键消费者；对于 Stripe webhook，按 event.ID 做 SetNX 去重并重复返回 200。最后补齐单测并完成全服务构建验证。

### R（Result）
建立了跨服务一致的幂等基础能力，显著降低重复支付状态推进和重复创建会话风险；测试与构建全部通过，方案具备可上线条件。

---

## 8. 其他技术可行性与取舍

## 8.1 用数据库唯一索引实现幂等可以吗
可以，但不优先。

原因：

- 每次消费都需要打数据库，延迟更高。
- 会把高频消息压力转移到关系库/文档库。
- Redis 更适合短周期幂等窗口和高并发写入。

## 8.2 用 Kafka Exactly-Once 可以吗
理论可行，但不适合当前架构。

原因：

- 当前系统基于 RabbitMQ，迁移成本和学习成本高。
- EOS 对全链路约束更重，不是“低改动落地”方案。

## 8.3 用内存 Map 去重可以吗
单实例可行，多实例不行。

原因：

- 幂等状态无法跨实例共享。
- 服务重启后状态丢失，无法覆盖重投场景。

---

## 9. 面试表达

### 9.1 简历项目表述（可直接粘贴）
在消息系统中落地跨服务幂等方案：发布端统一注入 MessageId，消费者基于 Redis processing/done 状态机实现消息去重，Stripe webhook 基于 event.ID 去重并重复回调直接返回 200，显著降低重复支付副作用风险并通过全链路测试验证。

### 9.2 口头表述（1-2 分钟）
我们系统用 RabbitMQ，天然是 at-least-once，所以重复消息是必然现象。单靠业务代码里 if 判断不够稳，我做了三层改造：第一层，发布端统一补 MessageId，保证每条消息可唯一标识；第二层，消费者接入 Redis 幂等状态机，先 Claim 成 processing，成功后写 done，失败则释放 processing，这样既能防重，又不会破坏重试；第三层，Stripe webhook 按 event.ID 做 SetNX 去重，重复回调直接返回 200，避免重复推进支付状态。改造后我补了幂等工具和 webhook 的单测，并跑通 trip、payment、gateway 的测试和构建。

---

## 10. 高频面试问答（10 组）

### Q1：为什么 RabbitMQ 下必须做幂等？
A：因为 at-least-once 语义保证“至少一次”而非“恰好一次”，重复投递是正常行为。

### Q2：为什么不用简单 SetNX 就结束？
A：简单 SetNX 会在失败重试时把自己锁住，必须区分 processing/done 并在失败时释放 processing。

### Q3：为什么 MessageId 为空时要做 fallback 摘要？
A：兼容历史消息或异常发布路径，避免因字段缺失导致幂等完全失效。

### Q4：为什么 Stripe webhook 重复要返回 200？
A：告知 Stripe 该事件已被成功处理，防止它继续重试放大流量。

### Q5：TTL 应该怎么定？
A：消息幂等窗口通常取“业务重试窗口 + 安全余量”；webhook 去重可取 24h 覆盖重试周期。

### Q6：幂等键放 Redis 会不会丢？
A：会有 Redis 故障边界，所以启动阶段做 Ping，生产可配哨兵/持久化；同时通过监控观察去重命中。

### Q7：为什么不把幂等写进业务表唯一约束？
A：可以，但会把高频流量打到数据库，Redis 在性能和解耦上更合适。

### Q8：幂等状态机怎么避免并发误判？
A：同一 key 使用原子 SetNX 声明 processing，done 状态用于快速跳过已完成消息。

### Q9：如果 MarkDone 失败会怎样？
A：当前实现返回错误触发重试，避免“业务成功但幂等状态缺失”长期放大。

### Q10：如何证明改造有效？
A：通过 `duplicate_skip_total`、`stripe_webhook_duplicate_total`、重复副作用次数等指标做优化前后对比。

---

## 11. 代码阅读学习顺序教程

推荐按“发布端 -> 通用幂等工具 -> 消费端 -> webhook -> 测试 -> 部署”顺序阅读：

1. 发布端 MessageId
- `shared/messaging/rabbitmq.go`
- 重点看 `newPersistentJSONMessage`。

2. 通用幂等工具
- `shared/messaging/idempotency.go`
- 重点看 `ClaimMessageForProcessing`、`MarkMessageProcessed`、`ReleaseMessageProcessing`。

3. Trip Service 消费端接入
- `services/trip-service/internal/infrastructure/events/driver_consumer.go`
- `services/trip-service/internal/infrastructure/events/payment_consumer.go`
- 重点看“claim -> 业务处理 -> mark/release”流程。

4. Payment Service 消费端接入
- `services/payment-service/internal/events/trip_consumer.go`
- 重点看支付会话创建前后的幂等处理。

5. Stripe webhook 幂等
- `services/api-gateway/http.go`
- 重点看 `markStripeWebhookEventProcessed` 与重复返回 200。

6. 测试验证
- `shared/messaging/idempotency_test.go`
- `shared/messaging/rabbitmq_publish_test.go`
- `services/api-gateway/http_idempotency_test.go`

7. 启动与部署接线
- `services/api-gateway/main.go`
- `services/payment-service/cmd/main.go`
- `services/api-gateway/redis.go`
- `services/payment-service/cmd/redis.go`
- `infra/development/k8s/api-gateway-deployment.yaml`
- `infra/development/k8s/payment-service-deployment.yaml`
- `Tiltfile`

建议学习节奏：

- 第一遍 20 分钟：看整体调用链。
- 第二遍 45 分钟：逐行理解幂等状态机和失败路径。
- 第三遍 20 分钟：按 STAR 脱稿讲一遍。
