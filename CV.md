陈 瀚翔
求职意向：后端开发工程师 Ɵ 1377954361@qq.com × 13456397284
ŵ 教育背景
Ġ 中山大学 • 计算机科学与技术 • 本科 2020.09 – 2024.07
Ġ 中山大学 • 计算机技术 • 硕士 2024.09 – 至今
Ð 专业技能
Ġ 熟练掌握 Golang 语言，理解 goroutine、channel 等并发编程模型与 GMP 调度机制。
Ġ 熟悉 gRPC、Protocol Buffers 与微服务通信机制，熟悉 Docker 容器化与 Kubernetes 部署。
Ġ 熟悉 MySQL 数据库，Redis、MongoDB 等 NoSQL 与缓存策略。
Ġ 熟悉 RabbitMQ 等消息队列，理解其在系统解耦、通信、治理中的作用。
Ġ 熟悉 Eino 等 Agent 开发框架，熟悉 RAG 架构，熟悉 LLM 应用工程化。
Ġ 熟悉网络协议与编程：熟悉应用层 HTTP/HTTPS 等协议，熟悉网络编程及 Linux 环境开发。
Ġ 熟悉 Git 版本管理与团队协作流程，具备良好的系统设计、问题分析与团队沟通能力。
Ĳ 项目经历
Ġ 分布式共享出行微服务后端系统
技术架构：Go、gRPC、Protobuf、RabbitMQ、MongoDB、Docker、Kubernetes、OpenTelemetry、Jaeger
项目简介：基于分布式微服务架构的共享出行调度平台，实现了行程预览、实时订单、司机调度与支付链路全流程
– 微服务与 gRPC 通信：基于 gRPC + Protobuf 实现网关、行程、司机调度、支付等服务的接口设计与高效通信。
– 设计事件驱动和消息可靠性：使用 RabbitMQ 设计并实现订单、司机调度、支付等异步事件处理流程，增加分级
重试与死信队列（DLQ）隔离策略。实现@@消息幂等去重（MessageId + Redis processing/done 状态机），基准实
测单实例完整流程吞吐 ∼2.5k QPS，100 个 goroutine 抢同一 MessageId 100% 收敛到 1 次业务执行。
– 并发接单防护：对多司机同时接受同一行程导致的重复支付问题，落地 Redis 分布式锁 + MongoDB 条件更新双保
险，入口用 SET NX EX 抢锁做前置限流，Lua 脚本原子校验 owner 释放，持久层用状态更新作为最终一致性闸
门。基准实测 100 司机抢同一 tripID 100% 互斥正确（go test -race 通过）。
– 接口防护: 在网关接口实现三层防护——（1）基于 Redis ZSET + Lua 脚本的滑动窗口限流，原子执行” 删过期 →
计数 → 写入”，P99 ≈ 0.90 ms、单实例 ∼4.5k QPS，200 并发 limit=10 精确放行 10 个；（2）SET NX EX 幂等锁
拦截网络抖动引发的毫秒级重复请求，与限流覆盖不同时间粒度；（3）引入 Redis Bitmap 自实现布隆过滤器（双
FNV Hash），20w ObjectID 实测误判率 0.118%，防止恶意枚举 ObjectID 的缓存穿透。
– 可观测性与云原生工程化：基于 OpenTelemetry + Jaeger 打通 HTTP/gRPC/RabbitMQ 跨服务链路追踪；使用
Kubernetes 进行环境部署。
Ġ 智能 OnCall Agent 系统
技术架构：Goframe、Eino、RAG、ReAct、Plan-Executor、Multi-Agent、MCP
项目简介：智能 OnCall 系统通过 AI Agent 解决团队真实痛点，整合知识库、对话、运维三大核心能力，实现问题自动
应答和故障智能排查的一体化服务。致力于降低团队 OnCall 的人力成本，提升团队效率。
– AI Agent 架构设计：基于 Eino 图编排构建了 KnowledgeIndex / ChatReAct / Plan-Execute-Replan 三类 Agent。
通过 MCP 协议封装了 Prometheus 告警查询、日志检索等真实运维工具，解决大模型无法直接触达生产环境数据
的痛点。
– RAG 知识库系统设计：设计了完整的文档向量化存储和检索方案，支持内部文档的智能检索增强生成。使 Agent
能够基于内部知识库提供准确的业务咨询和技术支持。
– 负责对话功能开发：基于 ReAct 模式实现了对话 Agent。支持多轮对话上下文记忆，并通过容错处理优化体验。同
时基于 SSE 技术实现 AI 对话流式输出，解决大模型响应延迟问题，前端呈现实时对话效果。
– 负责 AIOps 功能开发：基于 Plan-Execute 模式实现了智能运维 Agent。实现了根据告警信息 → 检索知识库 → 规
划执行步骤 → 调用工具查询 → 分析结果 → 生成建议的完整业务流程。
Ô 荣誉与证书
Ġ 奖学金类别：教育部港澳台奖学金三等奖
Ġ CET 4：580 CET 6：560
