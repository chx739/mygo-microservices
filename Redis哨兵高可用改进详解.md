# Redis 哨兵高可用改进详解（Sentinel Failover）

## 1. 改进目标

本次改进目标是让项目从“单节点 Redis 直连”升级为“可切换 Sentinel 高可用模式”，并保持业务代码零侵入：

1. 支持 Sentinel 模式自动故障转移（master 挂掉后自动切换）。
2. 支持开发环境继续走单节点（不引入额外部署负担）。
3. 统一四个服务的 Redis 初始化逻辑，避免重复实现和配置分叉。

---

## 2. 核心代码

## 2.1 共享客户端工厂（哨兵优先，单节点回退）

文件：`shared/cache/redis_client.go`

```go
// NewRedisClientFromEnv 根据环境变量初始化 Redis 客户端。
// 约定：
// 1) 如果 REDIS_SENTINEL_ADDRS 非空，则优先启用 Sentinel 模式。
// 2) 否则回退到 REDIS_ADDR 单节点直连模式。
func NewRedisClientFromEnv() RedisClientInitResult {
    password := env.GetString("REDIS_PASSWORD", "")
    db := env.GetInt("REDIS_DB", 0)

    sentinelAddrs := parseSentinelAddrs(env.GetString("REDIS_SENTINEL_ADDRS", ""))
    if len(sentinelAddrs) > 0 {
        masterName := env.GetString("REDIS_MASTER_NAME", "mymaster")
        client := redis.NewFailoverClient(&redis.FailoverOptions{
            MasterName:    masterName,
            SentinelAddrs: sentinelAddrs,
            Password:      password,
            DB:            db,
        })
        return RedisClientInitResult{Client: client, Mode: RedisClientModeSentinel}
    }

    addr := env.GetString("REDIS_ADDR", "redis:6379")
    client := redis.NewClient(&redis.Options{
        Addr:     addr,
        Password: password,
        DB:       db,
    })
    return RedisClientInitResult{Client: client, Mode: RedisClientModeStandalone}
}
```

## 2.2 各服务复用共享工厂

已接入文件：

- `services/api-gateway/redis.go`
- `services/trip-service/cmd/redis.go`
- `services/driver-service/redis.go`
- `services/payment-service/cmd/redis.go`

核心思路：

1. 服务层仍保留启动时 `Ping` 校验（3 秒超时）。
2. 客户端创建交给共享工厂。
3. 失败日志增加 mode + endpoint，便于定位是 sentinel 还是 standalone 配置问题。

## 2.3 生产配置下发

文件：`infra/production/k8s/app-config.yaml`

```yaml
REDIS_ADDR: "redis:6379"
REDIS_DB: "0"
REDIS_MASTER_NAME: "mymaster"
REDIS_SENTINEL_ADDRS: "redis-headless:26379,redis-headless:26380,redis-headless:26381"
```

文件：

- `infra/production/k8s/api-gateway-deployment.yaml`
- `infra/production/k8s/trip-service-deployment.yaml`

两者已注入上述变量，保证容器可直接启用哨兵模式。

---

## 3. 为什么要做（场景与困难）

## 3.1 场景

1. 单节点 Redis 是单点故障
- 一旦 Pod 重启/迁移，依赖 Redis 的能力（锁、缓存、幂等、GEO）会同时抖动。

2. 服务规模扩大后稳定性要求上升
- 需要在 Redis 主节点故障时自动切换，避免业务长时间不可用。

3. 多服务共用 Redis
- 不能让每个服务各自实现一套连接策略，否则维护成本和风险都高。

## 3.2 困难

1. 兼容双模式
- 生产要支持哨兵，开发仍需单节点简单可跑。

2. 最小改动
- 不希望修改大量业务逻辑，只应改客户端初始化层。

3. 可观测性
- 连接失败日志要能快速区分模式和目标端点，便于排查。

---

## 4. 怎么实现（技术原理）

## 4.1 Sentinel 工作机制

Sentinel 负责：

1. 监控 master 健康。
2. 发生故障时选举新的 master。
3. 客户端通过 Sentinel 获取当前 master 地址并自动切换。

## 4.2 go-redis FailoverClient

`redis.NewFailoverClient` 会在内部通过 Sentinel 发现主节点并维护连接。

只要配置以下关键参数即可：

- `MasterName`
- `SentinelAddrs`
- `Password`
- `DB`

## 4.3 配置优先级策略

当前采用策略：

1. `REDIS_SENTINEL_ADDRS` 非空 → Sentinel 模式。
2. 否则 → `REDIS_ADDR` 单节点模式。

这样开发环境可零成本复用，生产只需加配置即可切换高可用。

---

## 5. 实现后提升（量化）

## 5.1 本次验证结果

- `go test ./shared/cache/... -v` 通过
- `go test ./services/api-gateway/... -v` 通过
- `go test ./services/trip-service/... -v` 通过
- `go test ./services/driver-service/... -v` 通过
- `go test ./services/payment-service/... -v` 通过
- 四个服务构建全部通过

## 5.2 建议线上监控指标

1. `redis_failover_detected_total`
- Sentinel 主从切换次数。

2. `redis_client_reconnect_total`
- 客户端重连次数。

3. `redis_ping_latency_ms_p95`
- Redis 健康延迟变化。

4. `critical_operation_error_rate`
- 锁、幂等、缓存相关错误率（切换前后对比）。

---

## 6. STAR 面试叙述

## S（Situation）
项目 Redis 采用单节点直连，存在明显单点故障风险；Redis 一旦异常，分布式锁、缓存、幂等、GEO 都会受影响。

## T（Task）
在不改业务代码的前提下，提供可切换的 Redis 高可用能力，并同时兼容开发环境单节点。

## A（Action）
我抽象了共享 Redis 客户端工厂，按环境变量自动选择 Sentinel 或单节点模式；然后把 api-gateway、trip-service、driver-service、payment-service 的初始化全部切到共享工厂，并保留启动 Ping 校验；最后补充生产 k8s 配置注入哨兵参数并完成测试构建回归。

## R（Result）
实现了 Redis 高可用能力可配置切换，业务层零侵入；故障场景下具备自动主节点切换基础，服务稳定性显著增强。

---

## 7. 其他技术方案与取舍

## 7.1 为什么不用 Redis Cluster
可行，但本次不选。

原因：

1. Cluster 更偏向横向分片扩展，运维和键路由约束更复杂。
2. 当前诉求核心是高可用故障切换，Sentinel 更轻量。
3. 现有 key 设计无需立即引入分片复杂度。

## 7.2 为什么不用应用层重试 + 多地址兜底
不选。

原因：

1. 应用层很难完整覆盖主从角色变化。
2. Sentinel 已提供成熟主节点发现机制。
3. 统一工厂 + Sentinel 可以减少各服务重复实现。

## 7.3 为什么不用 sidecar/proxy（如 twemproxy）
可行，但当前不需要。

原因：

1. 引入额外组件与运维面。
2. 现阶段 FailoverClient 已满足需求，链路更直接。

---

## 8. 面试表达

## 8.1 简历项目表述
完成 Redis 高可用改造：抽象共享客户端工厂并支持 Sentinel/单节点双模式切换，统一接入四个微服务并补充生产环境哨兵配置下发，实现业务代码零侵入的故障切换能力。

## 8.2 口头表述（1-2 分钟）
我们原来是单节点 Redis，单点风险很高，Redis 挂了会影响缓存、幂等和分布式锁。我做的改造是把 Redis 初始化抽成一个共享工厂，优先读取 `REDIS_SENTINEL_ADDRS` 走 `FailoverClient`，没有哨兵配置就回退单节点，这样开发环境不受影响。然后把四个服务全部接到这套工厂，启动时仍做 Ping 校验，并把模式和端点打到错误日志里方便排障。最后我在生产 k8s 配置里补了哨兵参数注入，跑了全量相关测试和构建，确保兼容性没回归。

---

## 9. 面试高频 10 问 10 答

1. 问：Sentinel 和 Cluster 的核心区别？
答：Sentinel 主要解决高可用故障切换；Cluster 主要解决分片扩展并附带高可用。

2. 问：为什么这次选 Sentinel？
答：当前瓶颈是单点可用性，不是容量分片，Sentinel 更轻量且改造成本低。

3. 问：如何做到业务代码零侵入？
答：只改 Redis 客户端初始化层，业务读写接口保持 `redis.UniversalClient` 不变。

4. 问：为什么要保留单节点回退模式？
答：便于本地开发和测试，不要求每个环境都部署 Sentinel。

5. 问：主节点切换时客户端需要重启吗？
答：不需要，FailoverClient 会通过 Sentinel 动态发现新主节点。

6. 问：为什么要在启动时 Ping？
答：把配置错误前置，避免服务带病启动。

7. 问：REDIS_SENTINEL_ADDRS 写空字符串会怎样？
答：会自动回退到 standalone 模式。

8. 问：如果 Sentinel 配错会怎样？
答：启动 Ping 会失败，错误日志包含 mode 和 endpoint，便于快速定位。

9. 问：这次改造最大的工程收益是什么？
答：统一配置和初始化逻辑，降低多服务配置漂移和故障排查复杂度。

10. 问：后续还能怎么增强？
答：补充 Sentinel 部署自动化、切换演练脚本、以及 Redis 关键指标告警闭环。

---

## 10. 代码阅读学习顺序教程

建议按以下顺序学习：

1. 共享工厂入口
- `shared/cache/redis_client.go`
- 重点理解：模式选择、地址解析、返回结果结构。

2. 共享工厂单测
- `shared/cache/redis_client_test.go`
- 重点理解：哨兵优先、单节点回退、地址解析边界。

3. 服务接入实现
- `services/api-gateway/redis.go`
- `services/trip-service/cmd/redis.go`
- `services/driver-service/redis.go`
- `services/payment-service/cmd/redis.go`
- 重点理解：如何复用共享工厂并保留启动 Ping 校验。

4. 生产配置注入
- `infra/production/k8s/app-config.yaml`
- `infra/production/k8s/api-gateway-deployment.yaml`
- `infra/production/k8s/trip-service-deployment.yaml`
- 重点理解：配置如何进入容器环境变量。

5. 本地验证命令
```bash
go test ./shared/cache/... -v
go test ./services/api-gateway/... -v
go test ./services/trip-service/... -v
go test ./services/driver-service/... -v
go test ./services/payment-service/... -v
go build ./services/api-gateway/...
go build ./services/trip-service/...
go build ./services/driver-service/...
go build ./services/payment-service/...
```

按这个顺序学习，可以快速建立“原理 -> 代码 -> 部署 -> 验证”的完整认知链路。