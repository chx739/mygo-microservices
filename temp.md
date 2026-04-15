可观测性：面试官最爱问的 Span 问题

核心心态

面试官问 span，不是想听你报菜名（比如“我埋了 `rabbitmq.publish`、`http.handle`”），而是想看：

1. 你埋的 span 能不能定位问题（定位性）
2. 关键属性（Attribute）有没有带全（可排查性）
3. 错误语义有没有正确传递（可靠性）

---

我这个项目埋了哪些 Span

```text
HTTP 入口                 handleTripStart / handleTripPreview / handleStripeWebhook
  ↓
RabbitMQ Publish          rabbitmq.publish (+ routing_key, owner_id)
  ↓  （AMQP Header 透传 traceparent）
RabbitMQ Consume          rabbitmq.consume (+ routing_key, owner_id)
  ↓
下游业务逻辑              service 层 span（按需）
  ↓
gRPC 调用                 自动 instrument
  ↓
MongoDB                   自动 instrument（mongo-driver otel 插件）
```

---

面试官常问：你在 span 上会特别注意什么？

这是经典开放题，按四个维度分层回答。

1. Span 命名：能不能在 Jaeger 一眼找到

> 命名要稳定、可搜索。我用“服务.动作”或“组件.动作”格式，比如 `rabbitmq.publish`、`handleStripeWebhook`。
>
> 不能把动态值放到 span name，比如不能写 `GET /trip/12345`。否则 Jaeger 里每个 tripID 都会变成独立 operation，聚合统计就废了。动态值应该放 Attribute。

2. Attribute：出问题时能不能快速定位

> Attribute 是 trace 的灵魂。我会必带几类：
>
> - 业务标识：`tripID`、`userID`、`driverID`、`orderID`
> - 调用对象：`messaging.routing_key`、`db.collection`、`rpc.method`
> - 结果分类：`hit` / `miss` / `rejected` / `deduped`
>
> 不带敏感字段（手机号、身份证、支付卡号），即使要带也先脱敏；不带大体积字段（完整 request body），避免后端拒收或采样丢弃。

3. Error 语义：错误要沿链路浮上去

> OpenTelemetry 规范里，`span.SetStatus(codes.Error, msg)` 才是“标红失败”，`span.RecordError(err)` 是写入错误详情事件。两个都要做。

```go
if err := publish(...); err != nil {
  span.RecordError(err)                    // 记录错误详情
  span.SetStatus(codes.Error, err.Error()) // 标记 span 为失败
  return err
}
```

> 另一个坑：业务错误和系统错误要区分。用户参数非法返回 4xx 一般不该标 span 错误；数据库连不上才是系统错误。如果 4xx 全标红，告警会变噪音。

4. 跨组件传播：链路要串得起来

> HTTP / gRPC 有现成 instrument，会自动传 `traceparent`。
>
> RabbitMQ 需要手动做：Publisher 把 `traceparent` 写入 AMQP Header，Consumer 用 `propagator.Extract` 还原父 context，才能把异步链路串成一条 trace。
>
> 不做这步时，Jaeger 常见两条孤立 trace：
> - “HTTP 请求 -> 发消息”
> - “消费消息 -> 调下游”

---

面试官的刁钻追问

Q1：你的 span 粒度怎么控制？每个函数都埋吗？

不是每个函数都埋，太细会导致 trace 巨大、Jaeger 加载慢、采样成本高。我的原则：

- 必埋：跨进程边界（HTTP / gRPC / MQ 收发）、外部依赖（DB、第三方 API、Redis 关键操作）
- 按需埋：重要业务决策点（如“选择哪个司机”这种分支逻辑）
- 不埋：纯 CPU 计算、小工具函数

判断标准：这个 span 如果消失了，线上问题会不会更难排查？不会就别埋。

Q2：生产环境全量采样吗？

不是。全量采样数据量太大，存储和查询成本都高。常见方案：

- 头部采样（Head-based）：入口决定采样率（如 1%）
- 尾部采样（Tail-based）：看完整条 trace 再决定，慢请求/错误 trace 必采

常见组合：开发环境全采；生产用尾部采样 + 关键错误 100% 采 + 慢请求 100% 采。

Q3：Span 数据量太大怎么办？

- 控制 Attribute 大小，不塞完整 payload
- 用 `span.AddEvent("cache miss")` 替代部分子 span
- 使用 Batch Exporter 并调优批大小与上报间隔
- 负载高时动态降采样率

Q4：`trace_id` 在日志里有吗？

必须有，否则 trace 和 log 割裂。做法是日志 formatter 从 context 自动提取 `trace_id`/`span_id` 注入每行日志。

你这边可坦诚说：目前项目里还没做，这正是下一步改进点。

Q5：OpenTelemetry 和 Jaeger 是什么关系？

- OpenTelemetry：标准 + SDK，负责采集和上报
- Jaeger：存储 + UI，负责查询和展示

好处是后端可插拔（Jaeger/Tempo/Datadog 等），通常只改 Exporter 配置，业务埋点代码基本不动。

---

最容易被抓包的三个地方（提前准备）

1. “你说链路打通了，RabbitMQ 的 context 具体怎么传？”
要能明确说出：`traceparent` Header + `propagator.Inject/Extract`。

2. “`span.End()` 漏调会怎样？”
span 可能不会被完整上报，链路展示不完整，严重时造成内存与缓冲压力。必须 `defer span.End()`。

3. “讲一个你用 trace 排过的真实问题。”
最好准备具体故事，例如：
“WS 推送延迟高，trace 里看到 `rabbitmq.consume` 到 MongoDB 写入之间有 800ms 空档，定位为 consumer 串行阻塞；改并发消费后降到 30ms。”

如果没有真实线上故事，也可以坦诚：当前主要是 demo 环境，线上故障案例还在积累。

---

一句话总结（口径）

我关注四件事：命名稳定（不塞动态值）、属性带足业务标识、错误双写 `SetStatus + RecordError`、跨组件手动传 context（尤其 MQ）。

目的只有一个：出问题时能从一条 trace 追到根因。可观测性本质是为排障服务，不是为了“看起来埋了很多点”。