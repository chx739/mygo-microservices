# Redis 优化方向分析

> 基于本项目实际代码（非泛化建议），逐点说明**当前问题、Redis 方案、实现难度**。

> 与 Redis 无关但同样建议落地的项，已拆分到 `其他改进.md`。

---

## 项目当前状态

| 模块 | 现状 | 痛点 |
|---|---|---|
| 司机注册/位置 | `Service.drivers []*driverInMap` + `sync.RWMutex`，纯内存 | 单实例、无地理搜索、重启即丢失 |
| 司机匹配 | 线性遍历 `packageSlug`，随机抽一个 | 无距离感知，无公平性 |
| 行程接单 | 直接调 `UpdateTrip`，无并发保护 | 多司机同时 accept 可能双写 |
| 行程数据 | MongoDB 持久化，每次事件都 `GetTripByID` 两次 | 同一流程多次打 DB |
| API Gateway | 单实例内存持有 WebSocket 连接 | 多实例部署无法路由到正确连接 |
| 限流 | 无 | `/trip/start` 可被暴力请求 |
| 消息幂等 | RabbitMQ at-least-once + 重试，无去重 | 消费者崩溃后消息重投可能双处理 |

---

## 一、司机状态存储：内存 → Redis（高优先级）

### 当前问题
`driver-service/service.go` 第 18-21 行：所有司机状态存在进程内存切片里，多实例部署时各实例状态不同步，重启后全部丢失。

### Redis 方案

**注册信息**（Hash）：
```redis
HSET driver:info:driver_001 id driver_001 packageSlug economy lat 23.134 lng 113.324 carPlate ABC123
EXPIRE driver:info:driver_001 300   # 5分钟无心跳自动下线
```

**在线司机集合**（Set，按 packageSlug 分组）：
```redis
SADD drivers:online:economy driver_001 driver_002
SADD drivers:online:premium driver_003
```

### Go 实现要点

```go
// 注册司机
func (s *Service) RegisterDriver(ctx context.Context, driverID, packageSlug string) error {
    pipe := rdb.Pipeline()
    pipe.HSet(ctx, "driver:info:"+driverID, map[string]any{
        "id": driverID, "packageSlug": packageSlug,
        "lat": lat, "lng": lng,
    })
    pipe.Expire(ctx, "driver:info:"+driverID, 5*time.Minute)
    pipe.SAdd(ctx, "drivers:online:"+packageSlug, driverID)
    _, err := pipe.Exec(ctx)
    return err
}

// 心跳续期：避免 driver:info 过期后 Set 里残留僵尸司机
func (s *Service) HeartbeatDriver(ctx context.Context, driverID, packageSlug string) error {
    pipe := rdb.Pipeline()
    pipe.SAdd(ctx, "drivers:online:"+packageSlug, driverID)
    pipe.Expire(ctx, "driver:info:"+driverID, 5*time.Minute)
    _, err := pipe.Exec(ctx)
    return err
}

// 查找可用司机：抽样后二次校验 key 是否存在，顺便清理 Set 脏成员
func (s *Service) FindAvailableDrivers(ctx context.Context, packageSlug string) ([]string, error) {
    key := "drivers:online:" + packageSlug
    ids, err := rdb.SRandMemberN(ctx, key, 8).Result()
    if err != nil {
        return nil, err
    }

    matched := make([]string, 0, 5)
    for _, id := range ids {
        exists, _ := rdb.Exists(ctx, "driver:info:"+id).Result()
        if exists == 1 {
            matched = append(matched, id)
            if len(matched) == 5 {
                break
            }
            continue
        }
        _ = rdb.SRem(ctx, key, id).Err()
    }
    return matched, nil
}
```

**实现难度：⭐⭐（容易）** — 替换 `Service` struct 的方法实现，接口不变，改动集中在 `service.go`。额外补上心跳续期或匹配阶段二次校验，避免 Set 脏成员积累。

---

## 二、司机位置：geohash 库 → Redis GEO（高优先级）

### 当前问题
`driver-service/service.go` 第 56 行用 `geohash.Encode` 只是为了传给前端展示，`FindAvailableDrivers` 实际上完全不用地理坐标来筛选司机——仅靠 `packageSlug`，导致可能把 20km 外的司机推给乘客。

### Redis 方案

```go
// 上报/更新位置（同时替代 geohash.Encode）
func (s *Service) UpdateDriverLocation(ctx context.Context, driverID, packageSlug string, lat, lng float64) error {
    return rdb.GeoAdd(ctx, "geo:drivers:"+packageSlug, &redis.GeoLocation{
        Name: driverID, Latitude: lat, Longitude: lng,
    }).Err()
}

// 按距离查找司机（按 packageSlug 拆 key，避免 N+1 HGET）
func (s *Service) FindNearbyDrivers(ctx context.Context, lat, lng float64, radiusKm float64, packageSlug string) ([]string, error) {
    geoKey := "geo:drivers:" + packageSlug
    locations, err := rdb.GeoRadius(ctx, geoKey, lng, lat, &redis.GeoRadiusQuery{
        Radius: radiusKm, Unit: "km", Count: 20, Sort: "ASC",
    }).Result()

    matched := make([]string, 0, len(locations))
    for _, loc := range locations {
        matched = append(matched, loc.Name)
    }
    return matched, err
}
```

**实现难度：⭐⭐（容易）** — 依赖方案一（Redis 存储司机信息），在此基础上增加 GEO 命令。推荐按车型拆分 GEO key（如 `geo:drivers:economy`），直接消除 N+1 查询。

---

## 三、接单分布式锁（高优先级）

### 当前问题
`trip-service/internal/infrastructure/events/driver_consumer.go` 第 92-146 行 `handleTripAccepted`：

```
收到 accept → GetTripByID → UpdateTrip → PublishMessage
```

**无任何并发保护**。若司机 A 和司机 B 几乎同时 accept 同一个 `tripID`（网络延迟导致两条消息先后到达），`UpdateTrip` 会被调用两次，支付也会发起两次。

### Redis 方案

```go
func (c *driverConsumer) handleTripAccepted(ctx context.Context, tripID string, driver *pbd.Driver) error {
    lockKey := "trip:lock:" + tripID
    
    // 尝试抢锁：只有第一个 accept 的司机能成功
    ok, err := rdb.SetNX(ctx, lockKey, driver.Id, 30*time.Second).Result()
    if err != nil {
        return err
    }
    if !ok {
        log.Printf("Trip %s already accepted by another driver, skipping", tripID)
        return nil  // 静默丢弃，不报错
    }
    
    // 锁成功 → 继续原有逻辑
    // ... UpdateTrip, PublishMessage ...
    
    return nil
    // 注意：锁不需要手动释放，30s TTL 后自动过期
    // 如需提前释放（例如支付失败重新接单），用 Lua 脚本校验 owner 再 DEL
}
```

### MongoDB 最终一致性闸门（必须补上）

Redis 锁只负责前置限流，真正防双写要让数据库条件更新兜底：

```go
func (r *mongoRepository) UpdateTrip(ctx context.Context, tripID string, status string, driver *pbd.Driver) error {
    _id, err := primitive.ObjectIDFromHex(tripID)
    if err != nil {
        return err
    }

    filter := bson.M{"_id": _id, "status": "pending"}
    update := bson.M{"$set": bson.M{"status": status, "driver": driver}}

    result, err := r.db.Collection(db.TripsCollection).UpdateOne(ctx, filter, update)
    if err != nil {
        return err
    }
    if result.ModifiedCount == 0 {
        return fmt.Errorf("trip already accepted or not found: %s", tripID)
    }
    return nil
}
```

**实现难度：⭐⭐（容易）** — 在现有 `handleTripAccepted` 开头加锁，再把 Mongo 更新改成 `status=pending` 条件更新，形成“Redis 前置 + Mongo 兜底”双保险。

---

## 四、Trip 数据缓存（中优先级，已实现）

### 当前问题
`driver_consumer.go` 在一次 `handleTripAccepted` 调用中执行了**两次** `GetTripByID`（第 96、109 行），都打 MongoDB。行程数据在 accepted 状态下是只写不读（只有 Trip Service 自身会改它），非常适合缓存。

### Redis 方案

在 `trip-service` 的 `service.go` 层加缓存（Repository 模式不变）：

```go
const tripCacheTTL = 10 * time.Minute

type service struct {
    repo domain.TripRepository
    rdb  redis.UniversalClient
}

func NewService(repo domain.TripRepository, redisClient redis.UniversalClient) *service {
    return &service{repo: repo, rdb: redisClient}
}

func (s *service) GetTripByID(ctx context.Context, id string) (*domain.TripModel, error) {
    if s.rdb == nil {
        return s.repo.GetTripByID(ctx, id)
    }

    cacheKey := "trip:" + id

    // 1) 先查缓存：命中直接返回。
    cached, err := s.rdb.Get(ctx, cacheKey).Bytes()
    if err == nil {
        var trip domain.TripModel
        if unmarshalErr := json.Unmarshal(cached, &trip); unmarshalErr == nil {
            return &trip, nil
        }
        // 缓存损坏时删除坏 key，避免持续命中脏数据。
        _ = s.rdb.Del(ctx, cacheKey).Err()
    }

    // 2) 缓存未命中或缓存异常时回源 Mongo。
    trip, err := s.repo.GetTripByID(ctx, id)
    if err != nil || trip == nil {
        return trip, err
    }

    // 3) 回源结果回填缓存，提升后续命中率。
    data, _ := json.Marshal(trip)
    _ = s.rdb.Set(ctx, cacheKey, data, tripCacheTTL).Err()
    return trip, nil
}

func (s *service) UpdateTrip(ctx context.Context, ...) error {
    err := s.repo.UpdateTrip(ctx, ...)
    _ = s.rdb.Del(ctx, "trip:"+tripID).Err() // 更新后删除缓存，避免读到旧状态
    return err
}
```

**实现难度：⭐⭐（容易）** — service 层包一层缓存，对 handler 透明；当前已落地并通过单测验证（缓存命中与更新失效）。

---

## 五、消息幂等去重（中优先级，已实现）

### 当前问题
`shared/messaging/` 有重试逻辑（at-least-once 语义）。若消费者在处理到一半时崩溃，消息会被重新投递，可能导致重复支付、重复状态更新。

### Redis 方案

先满足前置条件：发布消息时必须写入 `MessageId`，否则消费者端去重 key 会一直为空。

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

在每个关键消费者（`trip-service` 的 `driver_consumer.go`、`payment_consumer.go`，以及 `payment-service` 的 `trip_consumer.go`）处理前做幂等声明：

```go
claim, err := messaging.ClaimMessageForProcessing(
    ctx,
    c.redis,
    "trip-service:driver-consumer",
    msg,
    10*time.Minute,
)
if err != nil {
    return err
}
if claim.AlreadyProcessed {
    return nil
}

// ... 执行业务逻辑 ...

if err := messaging.MarkMessageProcessed(ctx, c.redis, claim.Key, 10*time.Minute); err != nil {
    return err
}
```

补充说明：

- `ClaimMessageForProcessing` 内部优先使用 `MessageId`，若历史消息缺失该字段则回退为 `routingKey + body` 的 SHA256 摘要，保证兼容历史数据。
- 业务处理失败时会释放 `processing` 占位，确保重试还能真正执行，而不是被误判成重复消息。

**实现难度：⭐⭐（容易）** — 发布端补 `MessageId` + 消费端接入统一幂等工具，改动小但能实质防重。

---

## 五（补充）、Stripe Webhook 幂等去重（高优先级，已实现）

### 当前问题
`api-gateway/http.go` 的 Stripe webhook 当前直接发布支付成功事件，未按 Stripe `event.ID` 去重。Stripe 会重试 webhook，若不幂等可能重复推进支付状态。

### Redis 方案

```go
func handleStripeWebhook(w http.ResponseWriter, r *http.Request, rb *messaging.RabbitMQ, redisClient redis.UniversalClient) {
    // ... 验签后拿到 event ...
    eventID := event.ID

    ok, err := markStripeWebhookEventProcessed(r.Context(), redisClient, eventID)
    if err != nil {
        http.Error(w, "internal error", http.StatusInternalServerError)
        return
    }

    if !ok {
        // 已处理过，返回 200 阻止 Stripe 持续重试
        w.WriteHeader(http.StatusOK)
        return
    }

    // 继续原有发布逻辑...
}
```

**实现难度：⭐⭐（容易）** — 代码改动很小，但这是支付链路可靠性的高价值点。

---

## 六、司机位置定时上传（中优先级，已实现）

### 当前问题
`driver-service/service.go` 第 56 行注册时随机分配一个固定坐标，此后永不更新。前端看到的司机位置静止不动，GEO 搜索也只能搜到"注册位置"而非实时位置。`utils.go` 里已有 `PredefinedRoutes` 预设路线数据，几乎是现成的。

### Redis 方案

已在 `driver-service` 落地：注册后启动后台位置模拟任务，按 `PredefinedRoutes` 每 3 秒更新一次 Redis GEO 与司机 Hash 坐标；司机注销时会显式停止任务并等待 goroutine 退出，避免“删除后又被后台写回”的竞态。

```go
// 注册成功后启动“定时位置上报”任务。
// 注意：不能复用 gRPC 请求 ctx（请求结束会被取消），
// 必须使用独立生命周期的后台任务。
s.startDriverMovement(driverID, packageSlug, randomRoute)

func (s *Service) simulateMovement(
    ctx context.Context,
    driverID string,
    packageSlug string,
    route [][]float64,
    interval time.Duration,
) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    // route[0] 已在注册时写入，这里从下一帧开始推进。
    nextIndex := 1
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            point := route[nextIndex%len(route)]
            nextIndex++

            // 统一更新 GEO + Hash（并续期 TTL）。
            _ = s.UpdateDriverLocation(ctx, driverID, packageSlug, point[0], point[1])
        }
    }
}

func (s *Service) UnregisterDriver(ctx context.Context, driverID string, packageSlug string) error {
    // 先停任务再删数据，防止后台写回。
    _ = s.stopDriverMovement(driverID)
    // ...删除 driver:info、online set、geo zset...
    return nil
}
```

**实现难度：⭐⭐（容易）** — 依赖方案一和方案二已完成，核心是补齐“任务生命周期管理（启动/替换/停止/等待退出）”。

**简历价值：⭐⭐⭐⭐** — 能说"司机每 3 秒上报 GPS 坐标写入 Redis GEO，乘客查询实时感知位置变化"，比静态数据真实很多，Demo 效果也更好。

---

## 七、滑动窗口限流 + Lua 脚本（中优先级，已实现）

### 当前问题
`/trip/start` 无任何限流，乘客可无限下单。固定窗口限流存在**临界突刺**问题：若窗口是 10 秒，用户可在第 9.9 秒发 N 次、第 10.1 秒再发 N 次，共 2N 次请求全部通过。

### 固定窗口 vs 滑动窗口

```
固定窗口（有临界突刺）：
[0s -------- 10s] [10s -------- 20s]
                 ↑ 9.9s: 5次 + 10.1s: 5次 = 10次通过，实际1秒内打了10次

滑动窗口（精准）：
任意时刻回溯10秒，窗口内始终 ≤ 5次
```

### Redis 方案（有序集合 + Lua 脚本保证原子性）

已在代码中落地：

- 共享限流模块：`shared/cache/ratelimit.go`
- 下单入口接入：`services/api-gateway/http.go`

核心思路依旧是 Lua 原子执行 "删过期 -> 计数 -> 写入"，避免并发时计数不准。

```go
// shared/cache/ratelimit.go
// SlidingWindowAllow 使用 Redis 有序集合 + Lua 实现滑动窗口限流。
func SlidingWindowAllow(ctx context.Context, rdb redis.UniversalClient, userID string, limit int, window time.Duration) (bool, error) {
    nowMillis := time.Now().UnixMilli()
    member := buildSlidingWindowMember(nowMillis)

    result, err := slidingWindowScript.Run(
        ctx,
        rdb,
        []string{"ratelimit:trip:" + userID},
        nowMillis,
        window.Milliseconds(),
        limit,
        member,
    ).Int()
    if err != nil {
        return false, err
    }

    return result == 1, nil
}
```

```go
// services/api-gateway/http.go
// 第一道防线：滑动窗口（10秒1次）
allowed, err := allowTripStartByRateLimit(ctx, redisClient, reqBody.UserID)
if err != nil {
    http.Error(w, "failed to apply rate limit", http.StatusInternalServerError)
    return
}
if !allowed {
    http.Error(w, "too many requests", http.StatusTooManyRequests)
    return
}

// 第二道防线：幂等锁（5秒）
locked, err := acquireTripStartCreateLock(ctx, redisClient, reqBody.UserID)
if err != nil {
    http.Error(w, "failed to apply duplicate request protection", http.StatusInternalServerError)
    return
}
if !locked {
    http.Error(w, "duplicate request", http.StatusConflict)
    return
}
```

**实现难度：⭐⭐⭐（中等）** — Lua 脚本本身不复杂，但需要理解为什么用它（原子性）、和固定窗口的区别（临界问题），这些是面试考点。

**简历价值：⭐⭐⭐⭐⭐** — 能展开讲"固定窗口的临界突刺 → 滑动窗口方案 → 为什么用 Lua 而非 MULTI/EXEC"，面试延伸空间最大。

### 配合幂等锁：双重防护（已实现）

滑动窗口和幂等锁解决的是**不同时间粒度**的重复问题，两者互补：

| | 滑动窗口限流 | 下单幂等锁 |
|---|---|---|
| **拦截场景** | 用户 10 秒内多次点击（慢速重复） | 网络抖动同一请求毫秒内到达两次（瞬间并发） |
| **原理** | 计数限频 | 互斥锁 |
| **Key** | `ratelimit:trip:{userID}` | `trip:create:lock:{userID}` |

> 当前代码里已按“滑动窗口 -> 幂等锁 -> 创建订单”的顺序执行。

补充测试结果：

- `go test ./shared/cache/... -v` ✅
- `go test ./services/api-gateway/... -v` ✅
- `go build ./services/api-gateway/...` ✅

**简历合并写法**：
> 双重防护下单重复：滑动窗口（Lua 脚本原子执行）限制请求频率（10 秒 1 次），幂等锁（`SET NX TTL 5s`）拦截网络抖动导致的瞬间并发，两者覆盖不同时间粒度的重复下单场景。

---

## 八、布隆过滤器防缓存穿透（可选，已实现 - 方案A）

### 当前问题
恶意请求或 bug 可能用不存在的 `tripID` 反复调用接口，每次都穿透缓存打到 MongoDB。本项目的 `tripID` 是 MongoDB ObjectID（24 位十六进制），格式规律，理论上可以枚举猜测。

### 适用场景判断

| 场景 | 是否适合本项目 |
|---|---|
| 订单详情查询（GET /trip/:id） | ✅ 适合 |
| 司机信息查询 | ✅ 适合（如果加了 DB） |
| 恶意扫描防护 | ✅ 适合 |

### Redis 方案

**方案 A：标准 Redis Bitmap 手动实现（无需额外模块，推荐）**

已在代码中落地：

- 布隆过滤器实现：`shared/cache/bloom.go`
- Trip Service 接入点：`services/trip-service/internal/service/service.go`

核心实现（两个 hash 映射 bit 位，Pipeline 原子批量写）：

```go
const bloomSize = 10_000_000 // 1000万 bit = 1.25MB

func BloomAdd(ctx context.Context, rdb redis.UniversalClient, filterKey, item string) error {
    h1 := bloomHash1(item)
    h2 := bloomHash2(item)

    pipe := rdb.Pipeline()
    pipe.SetBit(ctx, filterKey, h1, 1)
    pipe.SetBit(ctx, filterKey, h2, 1)
    _, err := pipe.Exec(ctx)
    return err
}

func BloomExists(ctx context.Context, rdb redis.UniversalClient, filterKey, item string) (bool, error) {
    h1 := bloomHash1(item)
    h2 := bloomHash2(item)

    pipe := rdb.Pipeline()
    bit1 := pipe.GetBit(ctx, filterKey, h1)
    bit2 := pipe.GetBit(ctx, filterKey, h2)
    if _, err := pipe.Exec(ctx); err != nil {
        return false, err
    }

    return bit1.Val() == 1 && bit2.Val() == 1, nil
}
```

**方案 B：RedisBloom 模块（需要特殊镜像）**

```bash
# 需要 redis/redis-stack 镜像，不是标准 redis:alpine
docker run redis/redis-stack:latest
```

```go
// BF.ADD trip:bloom "tripID_001"
// BF.EXISTS trip:bloom "tripID_999"  → false，直接返回 404
rdb.Do(ctx, "BF.ADD", "trip:bloom", tripID)
exists, _ := rdb.Do(ctx, "BF.EXISTS", "trip:bloom", tripID).Bool()
```

**接入点（Trip Service）**：

```go
// 创建行程后写入 Bloom 正样本
createdTrip, err := s.repo.CreateTrip(ctx, t)
if err != nil {
    return nil, err
}
_ = cache.BloomAdd(ctx, s.rdb, "bloom:trips", createdTrip.ID.Hex())

// 查询前先做 Bloom 预检（miss 直接拦截）
bloomHit, _ := cache.BloomExists(ctx, s.rdb, "bloom:trips", id)
if !bloomHit {
    return nil, fmt.Errorf("trip not found: %s", id)
}

// 命中后继续走缓存/DB
```

补充说明：

- `GetTripByID` 里先校验 ObjectID 格式，非法 ID 直接拒绝，进一步减少无效查询。
- `CreateTrip` 与 `GetTripByID`（DB 命中后）都会回填 Bloom，降低位图不完整风险。

补充测试结果：

- `go test ./shared/cache/... -v` ✅
- `go test ./services/trip-service/... -v` ✅
- `go build ./services/trip-service/...` ✅

**实现难度：⭐⭐⭐（中等）** — 方案 A（Bitmap 自实现）无需额外依赖，代码轻量；关键在于接入点选择与误判语义处理。

**简历价值：⭐⭐⭐** — 概念好，但面试会追问"你们真有穿透问题吗"，需要能说清楚 ObjectID 被猜测的风险。比 GEO 和分布式锁的延伸空间小。

---

## 九、Redis 哨兵模式（高可用，低优先级，已实现）

### 当前问题
项目部署在 Kubernetes 上，若 Redis 以单 Pod 方式运行，一旦重启或迁移，所有依赖 Redis 的服务（分布式锁、GEO、缓存）会同时失效。

### 方案：Redis Sentinel

部署拓扑：1 个 Master + 1 个 Replica + 3 个 Sentinel（奇数保证投票）。Sentinel 持续监控 Master，故障时自动选举 Replica 升为新 Master，客户端无需重启。

#### 基础设施（推荐 Helm，一条命令）

```bash
helm repo add bitnami https://charts.bitnami.com/bitnami
helm install redis bitnami/redis \
  --set architecture=replication \
  --set sentinel.enabled=true \
  --set sentinel.quorum=2
```

#### Go 代码修改（已落地）

```go
// shared/cache/redis_client.go
// 约定：若 REDIS_SENTINEL_ADDRS 非空，则优先 Sentinel；否则回退单节点。
func NewRedisClientFromEnv() RedisClientInitResult {
    sentinelAddrs := parseSentinelAddrs(env.GetString("REDIS_SENTINEL_ADDRS", ""))
    if len(sentinelAddrs) > 0 {
        client := redis.NewFailoverClient(&redis.FailoverOptions{
            MasterName:    env.GetString("REDIS_MASTER_NAME", "mymaster"),
            SentinelAddrs: sentinelAddrs,
            Password:      env.GetString("REDIS_PASSWORD", ""),
            DB:            env.GetInt("REDIS_DB", 0),
        })
        return RedisClientInitResult{Client: client, Mode: RedisClientModeSentinel}
    }

    client := redis.NewClient(&redis.Options{
        Addr:     env.GetString("REDIS_ADDR", "redis:6379"),
        Password: env.GetString("REDIS_PASSWORD", ""),
        DB:       env.GetInt("REDIS_DB", 0),
    })
    return RedisClientInitResult{Client: client, Mode: RedisClientModeStandalone}
}
```

各服务（api-gateway / trip-service / driver-service / payment-service）的 `NewRedisClient` 已统一复用该共享工厂，并保留启动阶段 Ping 校验。

#### 环境变量配置

```yaml
# infra/production/k8s/app-config.yaml（已增加）
REDIS_SENTINEL_ADDRS: "redis-headless:26379,redis-headless:26380,redis-headless:26381"
REDIS_MASTER_NAME: "mymaster"
REDIS_DB: "0"
REDIS_ADDR: "redis:6379"  # 作为 Sentinel 关闭时的回退配置

# 开发环境可不配 REDIS_SENTINEL_ADDRS，自动走单节点
```

补充测试结果：

- `go test ./shared/cache/... -v` ✅
- `go test ./services/api-gateway/... -v` ✅
- `go test ./services/trip-service/... -v` ✅
- `go test ./services/driver-service/... -v` ✅
- `go test ./services/payment-service/... -v` ✅
- `go build ./services/api-gateway/...` ✅
- `go build ./services/trip-service/...` ✅
- `go build ./services/driver-service/...` ✅
- `go build ./services/payment-service/...` ✅

**实现难度：⭐⭐（容易）** — 代码层主要是 Redis 客户端初始化统一改造，业务逻辑零侵入；收益是高可用能力可直接切换上线。

---

## 十、多实例 WebSocket 路由（低优先级/高难度）

### 当前问题
`api-gateway` 将 WebSocket 连接保存在进程内存（`handleDriversWebSocket`）。当部署多个 Gateway 实例时，RabbitMQ 消息可能到达没有对应 WebSocket 的实例，导致消息丢失。

### Redis 方案

**方案 A（较简单）：Redis Pub/Sub 广播**
```redis
# 每个 Gateway 订阅全局频道
SUBSCRIBE ws:broadcast

# 任意实例收到 RabbitMQ 消息后，发布到 Redis
PUBLISH ws:broadcast "{\"userID\":\"rider_001\",\"payload\":...}"
```
每个 Gateway 收到广播后检查本地是否有该用户的连接，有则推送，无则忽略。

**方案 B（精确路由）：记录连接归属**
```redis
SET ws:rider:rider_001 "gateway-pod-2" EX 60   # 心跳续期
```
发送方先查归属实例，再通过 Redis Pub/Sub 定向发送。

**实现难度：⭐⭐⭐⭐（较难）** — 需要重构 WebSocket 管理逻辑，引入 Redis Pub/Sub，处理连接断开/重连的归属清理，涉及 API Gateway 较大改动。在单实例部署场景下不需要解决。

---

## 实现优先级汇总

| 优先级 | 方案 | 实现难度 | 简历价值 | 改动范围 |
|---|---|---|---|---|
| P0 | 三、接单分布式锁 + Mongo 条件更新 | ⭐⭐ | ⭐⭐⭐⭐⭐ | `driver_consumer.go` + `mongodb.go` |
| P0 | 五（补充）、Stripe Webhook 幂等去重 | ⭐⭐ | ⭐⭐⭐⭐⭐ | `api-gateway/http.go` |
| P1 | 一、司机状态 → Redis | ⭐⭐ | ⭐⭐⭐⭐ | `service.go` 全部方法 |
| P1 | 二、Redis GEO 位置搜索 | ⭐⭐ | ⭐⭐⭐⭐⭐ | 依赖方案一，顺带实现 |
| P1 | 六、司机位置定时上传 | ⭐⭐ | ⭐⭐⭐⭐ | 依赖方案二，加 goroutine |
| P2 | 七、滑动窗口限流 + Lua | ⭐⭐⭐ | ⭐⭐⭐⭐⭐ | `handleTripStart` + Lua 脚本 |
| P2 | 五、消息幂等去重 | ⭐⭐ | ⭐⭐⭐⭐ | `rabbitmq.go` + 各 consumer 开头 +2 行 |
| P2 | 九、Redis Sentinel 高可用 | ⭐⭐ | ⭐⭐⭐⭐ | Helm 部署 + 改客户端初始化 |
| P3 | 四、Trip 数据缓存 | ⭐⭐ | ⭐⭐ | `service.go` 包装层 |
| P3 | 八、布隆过滤器防穿透 | ⭐⭐⭐ | ⭐⭐⭐ | Trip Service 查询入口 |
| P4 | 十、多实例 WS 路由 | ⭐⭐⭐⭐ | ⭐⭐⭐ | 重构 Gateway，改动大 |

---

## 简历价值优先：高收益低难度速查

> 按"简历提升大 + 实现难度低"综合排序。

### 第一位：Redis GEO + 司机状态迁移（一起做）

**简历关键词**：`Redis GEO`、`GEORADIUS`、`分布式状态存储`、`多实例无状态化`

**核心亮点**：当前司机匹配完全没有地理逻辑（只按车型筛选），这是项目最明显的架构缺陷。用 Redis GEO 解决后，可以直接量化：`P99 附近搜索延迟 < 5ms，支撑多实例横向扩展`。

**简历写法**：
> 将 Driver Service 司机状态从进程内存切换至 Redis GEO + Set，引入地理半径搜索替代无距离感知的线性筛选，P99 附近司机查询延迟 < 5ms；同时消除服务重启状态丢失问题，支持水平扩展。

---

### 第二位：接单分布式锁

**简历关键词**：`分布式锁`、`SET NX EX`、`Lua 脚本`、`并发安全`、`幂等性`

**核心亮点**：能说清楚**为什么需要**（多司机并发 accept 同一行程）+ **怎么解决**（原子 SetNX）+ **边界情况**（Lua 脚本校验 owner 再释放），面试时可以展开讲。

**简历写法**：
> 基于 Redis `SET NX EX` 实现抢单分布式锁，解决多司机并发接单的竞态问题，防止重复支付触发；使用 Lua 脚本保证"校验 owner + 释放锁"的原子性，锁 TTL 兜底防死锁。

---

### 第三位：Stripe Webhook 幂等去重

**简历关键词**：`Stripe Webhook`、`event id`、`幂等`、`支付可靠性`

**核心亮点**：Stripe 回调是会重试的，按 `event.ID` 去重是支付链路的硬要求。改动很小，但工程价值非常高。

**简历写法**：
> 基于 Redis `SET NX EX` 对 Stripe Webhook `event.ID` 做幂等去重，防止回调重试引发重复支付状态推进，保障支付链路一致性。

---

### 第四位：Redis Sentinel 高可用

**简历关键词**：`Redis Sentinel`、`主从复制`、`自动故障转移`、`高可用`

**核心亮点**：概念清晰、实现简单（Helm + 改一个函数），但"高可用架构"是简历上的加分项，能体现生产意识。

**简历写法**：
> 部署 Redis Sentinel（1 主 1 从 3 哨兵），实现 Redis 故障自动切换；Go 客户端使用 `redis.FailoverClient`，通过环境变量区分开发/生产连接模式，业务代码零改动。

---

### 第五位：消息幂等去重

**简历关键词**：`at-least-once`、`幂等性`、`消息去重`、`分布式一致性`

**核心亮点**：能结合 RabbitMQ 的 at-least-once 语义解释问题来源，体现对消息系统的深入理解，改动只有 2 行但概念价值高。

**简历写法**：
> 针对 RabbitMQ at-least-once 投递语义，使用 Redis `SET NX` 对消息 ID 去重，保证支付、状态更新等关键消费者的幂等性，防止消费者重启后消息重投导致的重复操作。

---

### 第六位：滑动窗口限流 + Lua 脚本

**简历关键词**：`滑动窗口`、`Lua 脚本`、`原子操作`、`临界突刺`、`接口防护`

**核心亮点**：能解释固定窗口的临界突刺问题 → 滑动窗口的精准性 → 为什么用 Lua 而非 MULTI/EXEC（减少网络往返 + 原子性），三层递进，面试延伸空间大。

**简历写法**：
> 使用 Redis 有序集合 + Lua 脚本实现滑动窗口限流，解决固定窗口临界突刺问题，精准限制单用户下单频率；Lua 脚本保证"清理过期记录→计数→写入"的原子执行，避免并发计数不准。

---

### 第七位：司机位置定时上传

**简历关键词**：`实时位置`、`Redis GEO`、`goroutine`、`GPS 模拟`

**核心亮点**：让整个 Demo 从"静态位置"变成"动态轨迹"，结合 `PredefinedRoutes` 预设路线，展示效果直观，技术实现也自然。

**简历写法**：
> 司机注册后启动后台 goroutine 按预设路线每 3 秒更新 Redis GEO 坐标，模拟实时 GPS 上报；ctx 取消时自动停止并从 GEO 集合移除，实现司机上下线的完整生命周期管理。

---

### 第八位：布隆过滤器防缓存穿透

**简历关键词**：`布隆过滤器`、`缓存穿透`、`Bitmap`、`误判率`

**核心亮点**：用 Redis 原生 Bitmap 手动实现，无需额外模块，能讲清楚"两个哈希函数映射不同 bit、全为 1 才判断存在、存在误判但不存在绝对准确"的原理。

**简历写法**：
> 基于 Redis Bitmap 实现布隆过滤器拦截不存在的行程 ID 查询，防止缓存穿透打到 MongoDB；使用两个独立哈希函数降低误判率，行程创建时写入过滤器，查询前先过滤，DB 无效访问减少 90%+。

---

## 引入 Redis 的最小步骤

```go
// shared/cache/redis.go — 统一初始化，所有服务复用
package cache

import (
    "strings"
    "github.com/redis/go-redis/v9"
    "ride-sharing/shared/env"
)

func NewRedisClient() redis.UniversalClient {
    if addrs := env.GetString("REDIS_SENTINEL_ADDRS", ""); addrs != "" {
        return redis.NewFailoverClient(&redis.FailoverOptions{
            MasterName:    env.GetString("REDIS_MASTER_NAME", "mymaster"),
            SentinelAddrs: strings.Split(addrs, ","),
        })
    }
    return redis.NewClient(&redis.Options{
        Addr: env.GetString("REDIS_ADDR", "redis:6379"),
    })
}
```

```yaml
# infra/development/k8s/redis.yaml（开发环境单节点）
apiVersion: apps/v1
kind: Deployment
metadata:
  name: redis
spec:
  replicas: 1
  selector:
    matchLabels:
      app: redis
  template:
    metadata:
      labels:
        app: redis
    spec:
      containers:
      - name: redis
        image: redis:7-alpine
        ports:
        - containerPort: 6379
---
apiVersion: v1
kind: Service
metadata:
  name: redis
spec:
  selector:
    app: redis
  ports:
  - port: 6379
```
