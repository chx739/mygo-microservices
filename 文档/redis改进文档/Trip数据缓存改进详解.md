# Trip 数据缓存改进详解（面试导向）

## 1. 改进背景与问题

### 1.1 业务场景
在 Trip Service 的接单链路里，`handleTripAccepted` 会在一次请求中多次读取同一个 Trip：

1. 先查 Trip，确认存在并读取业务上下文。
2. 更新 Trip 状态后，再查一次 Trip 组装后续事件。

这类“短时间内重复读取同一条行程”的场景非常典型，天然适合缓存优化。

### 1.2 旧实现的痛点

- 每次都回源 MongoDB，重复 IO 明显。
- 读取高峰期会把压力放大到数据库层。
- 缺少统一缓存策略，后续想加监控和指标也困难。

### 1.3 本次改造目标

- 在 `service` 层实现 Cache Aside（旁路缓存）模式。
- `GetTripByID` 先查 Redis，未命中再查 Mongo 并回填缓存。
- `UpdateTrip` 成功后删除对应缓存，避免脏读。
- 保持 repository 与 handler 层接口不变，尽量低侵入。

---

## 2. 核心代码与改动点

### 2.1 service 层注入 Redis 客户端

文件：`services/trip-service/internal/service/service.go`

```go
type service struct {
    repo domain.TripRepository
    rdb  redis.UniversalClient
}

func NewService(repo domain.TripRepository, redisClient redis.UniversalClient) *service {
    return &service{
        repo: repo,
        rdb:  redisClient,
    }
}
```

说明：

- 缓存逻辑放在 service 层，避免污染 repository 的“纯数据访问职责”。
- `rdb` 可为空，方便降级和测试替身注入。

### 2.2 GetTripByID：缓存优先 + 回源回填

文件：`services/trip-service/internal/service/service.go`

```go
func (s *service) GetTripByID(ctx context.Context, id string) (*domain.TripModel, error) {
    if s.rdb == nil {
        return s.repo.GetTripByID(ctx, id)
    }

    cacheKey := "trip:" + id

    // 1) 先读缓存
    cached, err := s.rdb.Get(ctx, cacheKey).Bytes()
    if err == nil {
        var cachedTrip domain.TripModel
        if unmarshalErr := json.Unmarshal(cached, &cachedTrip); unmarshalErr == nil {
            return &cachedTrip, nil
        }

        // 缓存损坏时删坏 key，避免反复命中脏数据
        _ = s.rdb.Del(ctx, cacheKey).Err()
    }

    // 2) 回源 Mongo
    tripModel, repoErr := s.repo.GetTripByID(ctx, id)
    if repoErr != nil || tripModel == nil {
        return tripModel, repoErr
    }

    // 3) 回填缓存
    marshalled, _ := json.Marshal(tripModel)
    _ = s.rdb.Set(ctx, cacheKey, marshalled, tripCacheTTL).Err()

    return tripModel, nil
}
```

说明：

- 缓存命中立即返回，降低 Mongo 读压力。
- 缓存层异常不阻断主流程，保证业务可用性优先。
- 遇到损坏缓存主动清理，防止“脏数据常驻”。

### 2.3 UpdateTrip：写后失效

文件：`services/trip-service/internal/service/service.go`

```go
func (s *service) UpdateTrip(ctx context.Context, tripID string, status string, driver *pbd.Driver) error {
    if err := s.repo.UpdateTrip(ctx, tripID, status, driver); err != nil {
        return err
    }

    if s.rdb != nil {
        _ = s.rdb.Del(ctx, "trip:"+tripID).Err()
    }

    return nil
}
```

说明：

- 选择“删除缓存”而不是“直接覆盖缓存”，避免多个写路径导致缓存值分歧。
- 写后失效是 Cache Aside 的常见安全策略，容易保证一致性。

### 2.4 启动注入

文件：`services/trip-service/cmd/main.go`

```go
redisClient, err := NewRedisClient(ctx)
if err != nil {
    log.Fatalf("Failed to initialize Redis, err: %v", err)
}
defer redisClient.Close()

mongoDBRepo := repository.NewMongoRepository(mongoDb)
svc := service.NewService(mongoDBRepo, redisClient)
```

说明：

- 与接单分布式锁复用同一个 Redis 客户端，减少额外连接复杂度。

---

## 3. 为什么要做（场景与困难）

### 3.1 为什么需要

- 行程读取是高频路径，且存在短时间重复读。
- 纯数据库回源在高并发下成本高，扩展性弱。
- 该数据是“读多写少”的典型缓存场景。

### 3.2 实现难点

- 既要提速，又不能破坏接单状态一致性。
- 需要兼容 Redis 异常场景，不能“缓存挂了业务也挂”。
- 必须定义清晰失效策略，避免旧状态长期驻留。

---

## 4. 怎么实现（技术原理）

### 4.1 Cache Aside（旁路缓存）

读流程：

1. 读 Redis。
2. 命中则直接返回。
3. 未命中则回源 Mongo。
4. 回源成功后写回 Redis。

写流程：

1. 先写 Mongo（权威数据源）。
2. 写成功后删除 Redis 对应 key。

这个模式的优点是实现简单、风险可控，适合当前微服务结构。

### 4.2 为什么不用写穿（Write Through）

当前系统已有多条状态更新路径（driver accept、payment success 等）。
直接写穿会把缓存写逻辑散落在多处，增加维护成本和一致性风险。写后失效更稳。

### 4.3 TTL 选择思路

本次默认 `10m`：

- 太短：命中率不高，达不到减压效果。
- 太长：若失效链路遗漏，脏数据影响时间变大。

配合“写后删除”时，TTL 主要承担兜底清理作用。

---

## 5. 测试与验证结果

新增测试文件：

- `services/trip-service/internal/service/service_cache_test.go`

关键测试用例：

- `TestGetTripByID_CacheMissThenHit`
: 验证第一次回源 + 第二次命中缓存，repo 调用次数保持 1。

- `TestUpdateTrip_InvalidatesTripCache`
: 验证更新 Trip 后缓存 key 被删除。

执行命令：

```bash
go test ./services/trip-service/... -v
go build ./services/trip-service/...
```

结果：全部通过。

---

## 6. 实现后提升（量化指标）

### 6.1 本次可直接验证的结果

- Trip 缓存核心路径单测通过率：`2/2 = 100%`
- trip-service 全量测试通过。
- trip-service 全量构建通过。

### 6.2 建议上线后观测指标

- `trip_cache_hit_ratio`：目标 > 70%
- `trip_get_mongo_qps`：目标下降 40%+
- `trip_get_p95_latency`：目标下降 20%+
- `trip_cache_invalidation_fail_total`：目标接近 0

---

## 7. STAR 项目表述

### S（Situation）
Trip Service 在接单链路中对同一行程存在重复读取，所有请求都直接回源 Mongo，数据库压力偏高。

### T（Task）
在不破坏既有业务接口和状态一致性的前提下，降低行程读取路径的数据库压力并提升响应速度。

### A（Action）
我在 service 层实现了 Redis Cache Aside：`GetTripByID` 先查缓存，未命中回源并回填；`UpdateTrip` 成功后删除缓存确保一致性；并通过依赖注入把 Redis 客户端统一接入 Trip Service。补充了缓存命中和写后失效单测，覆盖核心行为。

### R（Result）
Trip 读取从“全量回源”升级为“缓存优先”，关键缓存逻辑测试 100% 通过，服务全量测试与构建通过；为后续缓存命中率和读延迟优化建立了稳定基线。

---

## 8. 替代技术方案与取舍

### 8.1 可以用本地内存缓存吗（例如 sync.Map/LRU）
可以，但不优先。

原因：

- 多实例下缓存不共享，命中率与一致性都不稳定。
- 扩容后每个 Pod 各自维护一份热点数据，整体收益下降。

### 8.2 可以用 Mongo 本身做热点优化吗
可以做索引/连接池优化，但这不等于缓存。

原因：

- 索引优化只能提升查询效率，不能减少重复请求总量。
- 高频重复读场景里，Redis 命中通常延迟更低。

### 8.3 可以用写穿缓存吗
可以，但当前不推荐。

原因：

- 写路径多，写穿容易让缓存写逻辑分散，维护成本高。
- Cache Aside（写后删）更容易先安全落地。

---

## 9. 面试表达

### 9.1 简历项目表述（可直接粘贴）
在 Trip Service 落地 Redis Cache Aside（Trip 维度）：读取走“缓存优先+回源回填”，状态更新后执行缓存失效，降低 Mongo 重复读压力并提升读取性能；补齐缓存命中/失效单测并通过全量构建验证。

### 9.2 口头表述（1-2 分钟）
Trip 的读取在接单流程里会短时间重复发生，之前每次都打 Mongo，属于典型的读多写少场景。我做法是把缓存加在 service 层而不是 repository 层，这样数据访问职责不变、侵入更小。具体上，GetTripByID 先查 Redis，命中直接返回；未命中回源 Mongo 后回填缓存。UpdateTrip 成功后删除 `trip:{id}`，保证后续读取不会拿到旧状态。缓存层我做了降级处理，Redis 异常不阻断主流程，避免把缓存变成单点风险。最后我加了两类单测：缓存命中行为和写后失效行为，并跑通 trip-service 全量测试和构建。

---

## 10. 高频面试问答（10 组）

### Q1：为什么把缓存放 service 层，不放 repository 层？
A：repository 保持“纯存储访问”更清晰，缓存属于业务编排层策略，放 service 层更符合职责分离。

### Q2：为什么更新后是删缓存，不是改缓存？
A：删缓存能避免多写路径带来的缓存值分歧，且实现简单稳健，是 Cache Aside 的常见实践。

### Q3：Redis 挂了会怎样？
A：缓存层异常会降级回源 Mongo，不影响主业务可用性。

### Q4：缓存雪崩/击穿怎么考虑？
A：当前先用 TTL + 写后失效，后续可加热点 key 单飞（singleflight）和 TTL 抖动。

### Q5：为什么缓存 key 用 `trip:{id}`？
A：简单且可读，便于排障和按前缀统计命中率。

### Q6：为什么 TTL 设 10 分钟？
A：配合写后删除时，TTL 主要是兜底回收；10 分钟在命中率与风险之间更平衡。

### Q7：如何保证缓存里不是脏 JSON？
A：反序列化失败时立即删除坏 key，防止后续请求持续命中脏值。

### Q8：这个方案会不会导致短暂不一致？
A：会有很短窗口，但通过“先写库再删缓存”可把窗口降到最小，且业务可接受。

### Q9：为什么不用消息队列异步删缓存？
A：当前路径同步删除即可满足时效，异步方案复杂度更高，适合后续规模化再引入。

### Q10：你如何证明优化有效？
A：用命中率、Mongo QPS、P95 延迟三组指标对比优化前后，并结合压测验证。

---

## 11. 代码阅读学习顺序教程

建议按“注入 -> 读路径 -> 写路径 -> 测试 -> 启动链路”顺序学习：

1. 先看 `services/trip-service/internal/service/service.go`
- 重点读 `service` 结构体里的 `rdb` 注入。
- 重点读 `tripCacheTTL` 和 `tripCacheKey`。

2. 再看读路径
- `GetTripByID`：缓存命中、回源、回填三段逻辑。
- 注意缓存损坏时删除坏 key 的处理。

3. 再看写路径
- `UpdateTrip`：先写库后删缓存。
- 理解为什么选择删缓存而不是覆盖缓存。

4. 对照测试反推设计意图
- `services/trip-service/internal/service/service_cache_test.go`
- 先看 `TestGetTripByID_CacheMissThenHit`，理解命中链路。
- 再看 `TestUpdateTrip_InvalidatesTripCache`，理解失效链路。

5. 最后看启动接线
- `services/trip-service/cmd/main.go`
- 关注 Redis 客户端如何注入到 `service.NewService(...)`。

推荐学习节奏：

- 第一遍 20 分钟：只看调用关系。
- 第二遍 40 分钟：逐行理解注释和异常分支。
- 第三遍 20 分钟：脱稿讲一遍“为什么 service 层做缓存 + 为什么写后删”。
