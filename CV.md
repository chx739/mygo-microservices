分布式共享出行微服务后端系统

技术架构：Go、gRPC、Redis、RabbitMQ、MongoDB、Kubernetes、OpenTelemetry、Jaeger、Stripe

项目简介：基于分布式微服务架构的共享出行调度平台，实现了行程预览、实时订单、司机调度与支付链路全流程。

– 微服务与 gRPC 通信：负责网关、行程、司机调度、支付等微服务的设计，基于 gRPC + Protobuf 实现服务高效通信。
– 实现基于 JWT 的用户认证：采用 Access Token + Refresh Token 双令牌机制，支持令牌签发、过期自动刷新与黑/白名单控制，保障 API 接口安全。
– 设计事件驱动和消息可靠性：使用 RabbitMQ 设计并实现订单、司机调度、支付等异步事件处理流程，增加分级重试与死信队列（DLQ）隔离策略，实现核心功能消息幂等去重，避免状态重复推进。
– 并发接单防护：对多司机同时接受同一行程导致的并发问题，落地 Redis 分布式锁 + MongoDB CAS 条件更新双保险，入口用 SET NX EX 抢锁做前置限流，Lua 脚本原子校验 owner 释放，持久层用状态更新实现最终一致性。
– 多级缓存与接口防护：设计 Ristretto 本地缓存（L1）+ Redis（L2）多级缓存架构，热点数据查询时间降至 µs 级，降低 Redis 与 DB 压力；针对缓存三大问题分别落地防护——(1) 引入 Redis Bitmap 自实现布隆过滤器（双 FNV Hash，20w ObjectID 误判率 0.118%）防穿透，(2) singleflight 合并并发请求防击穿，(3) 过期时间加随机抖动防雪崩。网关层实现 Redis ZSET + Lua 滑动窗口限流做流量防护。


分布式出行平台 - 测试开发与质量保障体系
技术架构：Go test、k6、Prometheus、xk6、Chaos Mesh、OpenTelemetry、Jaeger、Kubernetes、GitHub Actions
项目简介：基于分布式共享出行微服务架构，搭建测试金字塔分层覆盖、并发安全专项验证、端到端性能压测、混沌故障演练与 CI 质量门禁的质量保障体系。
– 测试金字塔分层与覆盖：搭建单测、集成测试、并发安全验证与 k6 端到端压测四层结构，构成完整的测试体系。
– 并发正确性与缓存对照测试：针对分布式锁、消息幂等、缓存击穿、限流四类并发风险设计断言型测试，并搭建多级缓存三模式对照测试（NoCache / L2-only / L1+L2），定量验证减压效果。
– 端到端性能压测体系：用 k6 设计三阶段场景（ramp-up→steady→ramp-down），通过 xk6-output-prometheus-remote 推 k6 指标至 Prometheus，配合自定义指标定位 bcrypt 慢哈希、派单容量等多个瓶颈。
– 跨服务可观测性与故障定位：基于 OpenTelemetry + Jaeger 打通 HTTP / gRPC / RabbitMQ 跨服务链路追踪，快速定位跨服务故障、缩短排查时间。
– 混沌测试与故障演练：基于 Chaos Mesh 模拟节点异常与网络故障，验证系统容错与恢复能力。
– 质量门禁与 CI 自动化：基于 GitHub Actions 配置 PR 级卡口，lint job（gofmt + go vet）+ test job（servicecontainer 启动真实 mongodb，并发安全测试与覆盖率检测并上传报告）。
