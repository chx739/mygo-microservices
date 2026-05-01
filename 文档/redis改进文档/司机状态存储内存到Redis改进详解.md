# 司机状态存储：内存 -> Redis 改进详解（面试导向）

## 1. 改进背景与问题

### 1.1 业务场景
在网约车系统里，司机上线、匹配、下线都依赖“在线司机状态”。

原实现把司机状态存放在 `driver-service` 进程内存中（切片 + 互斥锁），会带来三个核心问题：

- 单实例可用，多实例不一致：每个 Pod 只知道自己内存里的司机。
- 服务重启即丢数据：司机在线状态瞬间清空，匹配能力抖动。
- 匹配效率受限：需要遍历内存列表，复杂度为 $O(n)$。

### 1.2 困难点

- 不能只“改存储”，还要保证 gRPC 注册/注销流程、RabbitMQ 匹配流程都能正常工作。
- 需要同时解决在线集合脏数据（Hash 过期但 Set 残留）问题。
- 需要最小化改动范围，避免影响 Trip/Payment 等其他服务。

---

## 2. 实现目标

- 将司机状态从进程内存迁移到 Redis（Hash + Set）。
- 保持现有对外协议不变（gRPC 接口不改字段）。
- 提供心跳续期能力，支持“无心跳自动下线”。
- 在查询时惰性清理脏成员，提升集合质量。
- 完成自动化测试并通过。

---

## 3. 核心代码与改动点

## 3.1 Redis 客户端初始化

文件：`services/driver-service/redis.go`

```go
func NewRedisClient(ctx context.Context) (redis.UniversalClient, error) {
    addr := env.GetString("REDIS_ADDR", "redis:6379")
    password := env.GetString("REDIS_PASSWORD", "")
    db := env.GetInt("REDIS_DB", 0)

    client := redis.NewClient(&redis.Options{
        Addr:     addr,
        Password: password,
        DB:       db,
    })

    pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
    defer cancel()

    if err := client.Ping(pingCtx).Err(); err != nil {
        _ = client.Close()
        return nil, fmt.Errorf("ping redis failed (addr=%s): %w", addr, err)
    }

    return client, nil
}
```

关键点：启动即 Ping，失败快速暴露，避免服务带病运行。

## 3.2 状态存储迁移

文件：`services/driver-service/service.go`

```go
type Service struct {
    rdb redis.UniversalClient
}

func (s *Service) RegisterDriver(ctx context.Context, driverID string, packageSlug string) (*pb.Driver, error) {
    // ...构造 driver ...

    pipe := s.rdb.Pipeline()
    pipe.HSet(ctx, driverInfoKey(driverID), map[string]any{
        "id":             driver.Id,
        "name":           driver.Name,
        "profilePicture": driver.ProfilePicture,
        "carPlate":       driver.CarPlate,
        "packageSlug":    driver.PackageSlug,
        "geohash":        driver.Geohash,
        "lat":            driver.Location.Latitude,
        "lng":            driver.Location.Longitude,
    })
    pipe.Expire(ctx, driverInfoKey(driverID), 5*time.Minute)
    pipe.SAdd(ctx, driversOnlineKey(packageSlug), driverID)
    _, err := pipe.Exec(ctx)
    if err != nil {
        return nil, err
    }

    return driver, nil
}
```

关键点：Pipeline 一次提交写 Hash + 过期 + 在线集合，减少 RTT。

## 3.3 查询与脏数据治理

```go
func (s *Service) FindAvailableDrivers(ctx context.Context, packageType string) ([]string, error) {
    ids, err := s.rdb.SRandMemberN(ctx, driversOnlineKey(packageType), 8).Result()
    if err != nil {
        return nil, err
    }

    matched := make([]string, 0, 5)
    for _, driverID := range ids {
        exists, err := s.rdb.Exists(ctx, driverInfoKey(driverID)).Result()
        if err != nil {
            return nil, err
        }

        if exists == 1 {
            matched = append(matched, driverID)
            if len(matched) == 5 {
                break
            }
            continue
        }

        _ = s.rdb.SRem(ctx, driversOnlineKey(packageType), driverID).Err()
    }

    return matched, nil
}
```

关键点：随机抽样 + 二次校验 + 惰性清理，解决 Set 残留僵尸成员问题。

## 3.4 调用链改造

- `services/driver-service/main.go`
  - 新增 Redis 初始化与注入：`svc := NewService(redisClient)`
- `services/driver-service/gprc_handler.go`
  - `RegisterDriver/UnregisterDriver` 改为传递 `ctx`
- `services/driver-service/trip_consumer.go`
  - 查询改为 `FindAvailableDrivers(ctx, packageSlug)` 并处理错误

## 3.5 基础设施改造（开发环境）

- 新增：`infra/development/k8s/redis-deployment.yaml`
- 更新：`infra/development/k8s/app-config.yaml`（增加 `REDIS_ADDR`）
- 更新：`infra/development/k8s/driver-service-deployment.yaml`（注入 `REDIS_ADDR`）
- 更新：`Tiltfile`（加载 Redis 资源并让 driver-service 依赖 redis）

---

## 4. 技术原理（怎么实现）

## 4.1 为什么用 Hash + Set

- Hash：存单司机结构化信息，便于扩展字段。
- Set：按车型维护在线司机索引，便于随机抽样匹配。

键设计：

- `driver:info:{driverID}`（Hash + TTL）
- `drivers:online:{packageSlug}`（Set）

## 4.2 TTL 与心跳

- 注册时对 `driver:info:{driverID}` 设置 5 分钟 TTL。
- 通过 `HeartbeatDriver` 刷新 TTL，达到“活跃保留，不活跃自动下线”。

## 4.3 脏成员治理

TTL 只作用在 Hash，不会自动清理 Set 成员。查询阶段做二次校验：

- `EXISTS driver:info:{id} == 1`：保留
- 否则 `SREM drivers:online:{slug} {id}`：惰性清理

## 4.4 复杂度变化

- 旧方案：遍历内存司机列表，$O(n)$。
- 新方案：`SRANDMEMBER` 抽样 + 常数次 Exists 校验，近似 $O(1)$（对大规模更稳定）。

---

## 5. 验证与测试结果

执行命令：

```bash
go test ./services/driver-service/... -v
```

结果：

- `TestRegisterFindAndUnregisterDriver`：PASS
- `TestHeartbeatDriverRefreshTTL`：PASS
- 总体：PASS

测试覆盖了：

- 注册后 Hash/Set 是否正确写入
- 查询是否可命中已注册司机
- 脏成员是否被惰性清理
- 注销后状态是否彻底删除
- 心跳是否刷新 TTL

---

## 6. 实现后提升（量化）

说明：以下分为“已验证指标”和“上线验收指标”。

### 6.1 已验证指标（本次改造直接产出）

- 自动化测试通过率：$2/2=100\%$
- 查询脏数据清理能力：测试场景下残留成员被清理为 0

### 6.2 上线验收指标（建议在 Prometheus/日志中验证）

- 多实例状态一致性：从“单实例内存状态”提升为“共享 Redis 状态”
- 重启后状态恢复：不再因为进程重启立即清零（受 TTL 与持久化策略约束）
- 司机匹配查询复杂度：从遍历 $O(n)$ 降为抽样近似 $O(1)$
- 匹配延迟目标：P95 < 10ms（内网 Redis 场景）

---

## 7. STAR 表述（可直接用于简历）

### S（Situation）
项目早期将在线司机状态存放在 `driver-service` 内存，导致多实例状态不一致，服务重启时在线司机全部丢失。

### T（Task）
在不改动业务协议的前提下，将司机状态升级为分布式共享存储，并具备自动下线与脏数据治理能力。

### A（Action）
我把状态存储迁移到 Redis（Hash + Set），实现注册写入、查询抽样、心跳续期、注销清理；同时改造 gRPC/消费者调用链，增加 K8s 与 Tilt 的 Redis 运行配置，并补齐单元测试覆盖核心链路。

### R（Result）
driver-service 状态从单机内存变为多实例共享，匹配逻辑复杂度由 $O(n)$ 降为近似 $O(1)$；关键链路单测 100% 通过，重启丢状态与多实例不一致问题得到结构性解决。

---

## 8. 替代方案与取舍

## 8.1 用 MongoDB 存在线状态可以吗

可以，但不优先：

- 高频在线状态写入（心跳）会放大数据库写压力。
- 查询随机抽样、低延迟匹配不如 Redis 原生结构高效。

结论：Mongo 更适合持久化业务数据（Trip/Fare），在线状态更适合 Redis。

## 8.2 用本地缓存 + 定时同步可以吗

不推荐：

- 引入最终一致性窗口，匹配可能读到旧数据。
- 方案复杂度高于直接共享 Redis，且问题边界更难控制。

## 8.3 为什么不用 etcd

etcd 擅长配置与协调，不擅长高频在线状态读写与随机匹配场景；Redis 在数据结构与吞吐上更贴合该业务。

---

## 9. 面试项目表述（简历版 + 口述版）

### 9.1 简历一句话版本

将 Driver Service 在线状态从进程内存迁移到 Redis（Hash + Set + TTL），实现多实例状态共享、自动下线与脏数据惰性清理，匹配查询复杂度由 $O(n)$ 降为近似 $O(1)$，核心链路单测通过率 100%。

### 9.2 口头展开版本（1-2 分钟）

我们最早把司机状态放在 `driver-service` 内存里，这在单机 Demo 可行，但上线后会遇到两个问题：第一，多实例状态不一致；第二，Pod 重启后在线司机全部丢失。我做了一个状态层重构：用 Redis Hash 存司机详情，用 Set 按车型维护在线索引。注册时通过 Pipeline 一次写入 Hash、设置 5 分钟 TTL、加入在线集合；查询时用 `SRANDMEMBER` 抽样，然后 `EXISTS` 二次校验，如果发现 Hash 已过期就 `SREM` 做惰性清理，避免僵尸成员污染集合。这样把匹配从遍历内存的 $O(n)$ 改成近似 $O(1)$，并且天然支持多实例共享状态。最后我补了单元测试，覆盖注册、查询、心跳续期、注销和脏数据清理，测试全部通过。

---

## 10. 高频面试问题（10 问 10 答）

## Q1：为什么不用内存继续做？
A：内存只能保证单实例正确，多实例会状态分裂；服务重启会直接丢失在线司机，不满足线上稳定性要求。

## Q2：为什么选 Hash + Set，而不是一个大 JSON？
A：Hash 字段可增量更新、易扩展；Set 适合做在线索引和随机抽样。结构分离可读性、性能和治理都更好。

## Q3：Set 残留脏数据是怎么出现的？
A：`EXPIRE` 只作用于 Hash key，不会联动删除 Set 成员。Hash 过期后，Set 里会残留 driverID。

## Q4：你怎么治理脏数据？
A：查询阶段二次校验 `EXISTS driver:info:{id}`，不存在就 `SREM` 惰性清理。这个策略无需额外离线任务，成本低且有效。

## Q5：为什么匹配从 $O(n)$ 变成近似 $O(1)$？
A：以前要全量遍历内存列表；现在通过 `SRANDMEMBER` 在集合中直接抽样固定数量候选，再做常数次校验。

## Q6：TTL 过期导致在线司机被删了怎么办？
A：通过心跳机制续期 TTL。心跳失败本身就可视为司机不活跃，自动下线符合业务预期。

## Q7：为什么要用 Pipeline？
A：注册时有多条 Redis 命令（HSET/EXPIRE/SADD），Pipeline 可以降低网络往返，提升吞吐与一致性体验。

## Q8：Redis 不可用时系统会怎样？
A：当前实现是启动即 Ping，Redis 不可用会 fail-fast，防止服务处于“看似存活但不能匹配”的不一致状态。

## Q9：这个改造对 API 有破坏吗？
A：没有。gRPC 协议字段不变，只是服务内部从内存改为 Redis，并补了 context 透传与错误处理。

## Q10：下一步还能怎么优化？
A：可以继续落地 GEO（按车型拆 key）、分布式锁 + Mongo 条件更新、消息幂等（MessageId + SetNX）和 Webhook 幂等，形成完整可靠性闭环。
