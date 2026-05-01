# 滑动窗口限流与 Lua 脚本改进详解（下单入口双重防护）

## 1. 改进目标

本次改进针对 API Gateway 的下单入口 `POST /trip/start`，实现两道防线：

1. 滑动窗口限流（Lua 原子脚本）
- 目标：限制“10 秒内最多 1 次下单”，拦截慢速重复点击。

2. 幂等锁（SetNX + TTL）
- 目标：拦截毫秒级并发重复请求（网络抖动、前端重复提交）。

最终效果是：同一用户的重复下单在不同时间粒度都可控。

---

## 2. 核心代码

## 2.1 共享限流模块

文件：`shared/cache/ratelimit.go`

```go
var slidingWindowScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local member = ARGV[4]

redis.call('ZREMRANGEBYSCORE', key, 0, now - window)
local count = redis.call('ZCARD', key)

if count < limit then
    redis.call('ZADD', key, now, member)
    redis.call('PEXPIRE', key, window)
    return 1
end

return 0
`)

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

关键点：

- 用有序集合存请求时间戳。
- 每次请求先删窗口外旧数据，再计数，再决定是否写入当前请求。
- Lua 保证三步原子执行，避免并发计数错乱。

## 2.2 下单入口接入双重防护

文件：`services/api-gateway/http.go`

```go
// 第一道防线：滑动窗口限流（10 秒内 1 次）
allowed, err := allowTripStartByRateLimit(ctx, redisClient, reqBody.UserID)
if err != nil {
    http.Error(w, "failed to apply rate limit", http.StatusInternalServerError)
    return
}
if !allowed {
    http.Error(w, "too many requests", http.StatusTooManyRequests)
    return
}

// 第二道防线：下单幂等锁（5 秒）
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

补充：

- 429 表示频率超限。
- 409 表示瞬时重复提交。
- 两种状态码语义清晰，便于前端差异化提示。

## 2.3 路由注入 Redis 客户端

文件：`services/api-gateway/main.go`

```go
mux.HandleFunc("POST /trip/start", enableCORS(func(w http.ResponseWriter, r *http.Request) {
    handleTripStart(w, r, redisClient)
}))
```

---

## 3. 为什么要实现（场景与困难）

## 3.1 场景

1. 用户连续点击“立即叫车”
- 短时间内重复下单，造成业务噪音与资源浪费。

2. 网络抖动导致请求重发
- 同一请求在毫秒级重复到达，容易生成重复订单。

3. 高并发时计数准确性
- 非原子实现容易出现“同一时刻都认为可通过”的并发穿透。

## 3.2 困难

1. 不能用简单固定窗口
- 固定窗口有临界突刺，边界时刻可在 1 秒内通过双倍请求。

2. 不能把删旧、计数、写入拆开
- 拆开执行会有竞态条件，必须原子化。

3. 还要处理瞬时并发重复
- 仅有频率限制不够，需要额外幂等锁。

---

## 4. 怎么实现（技术原理）

## 4.1 滑动窗口

窗口定义为“当前时刻向前回溯 N 毫秒”。

每次请求做三件事：

1. 清理窗口外请求：`ZREMRANGEBYSCORE`
2. 统计窗口内请求：`ZCARD`
3. 若未超限，写入当前请求：`ZADD`

满足条件：

$$count < limit$$

则允许，否则拒绝。

## 4.2 Lua 原子执行

Redis 单线程执行 Lua 脚本，因此三步不会被其他请求插队。

这保证了并发下判断的一致性，避免计数偏差。

## 4.3 双重防护模型

- 滑动窗口：处理“慢速重复”（秒级频率问题）。
- 幂等锁：处理“瞬时并发”（毫秒级重复提交）。

两者互补，覆盖不同时间尺度的重复请求。

---

## 5. 实现后提升（量化）

## 5.1 已验证结果（本次自测）

- `shared/cache` 限流单测：3/3 通过
- `api-gateway` 限流 + webhook 测试：6/6 通过
- `api-gateway` 构建：通过

## 5.2 建议上线观测指标

1. `trip_start_rate_limited_total`
- 429 命中次数，衡量慢速重复请求规模。

2. `trip_start_duplicate_locked_total`
- 409 命中次数，衡量瞬时并发重复规模。

3. `trip_start_success_total`
- 成功下单数，用于与限流命中比值联合分析。

4. `trip_start_rejected_ratio`
- 公式：
$$
\frac{rate\_limited + duplicate\_locked}{all\_trip\_start\_requests}
$$
- 反映限流策略强度，便于调参数。

---

## 6. STAR 面试叙述

## S（Situation）
下单接口没有限流，用户连点和网络重试会造成重复请求，高并发时存在并发穿透风险。

## T（Task）
在不改动核心下单业务逻辑的前提下，为下单入口增加稳定、可观测、可解释的限流与防重能力。

## A（Action）
我新增了共享滑动窗口模块，使用 Redis 有序集合与 Lua 原子脚本实现“删旧-计数-写入”；然后在 API Gateway 的 `/trip/start` 先做滑动窗口，再做 `SetNX + TTL` 幂等锁，分别返回 429 和 409。最后补齐共享模块与网关侧单测并验证构建通过。

## R（Result）
实现了下单入口的双时间粒度防护：秒级频率可控、毫秒级并发可控，测试与构建全通过，具备上线条件。

---

## 7. 可替代方案与取舍

## 7.1 固定窗口限流
可实现，但不选。

原因：

- 临界突刺明显（窗口边界可瞬间放大通过量）。
- 精度不如滑动窗口。

## 7.2 令牌桶/漏桶
可实现，也常用。

本次不选原因：

- 本需求强调“任意回溯窗口内最多 N 次”的面试表达，滑动窗口更直观。
- 当前下单接口流量规模下，ZSet + Lua 已足够。

## 7.3 只做幂等锁，不做限流
不够。

原因：

- 幂等锁只能挡住短窗口并发重复，无法限制用户长期高频请求。
- 需要与滑动窗口组合才能兼顾频率和并发。

---

## 8. 面试表达

## 8.1 简历项目表述
在 API Gateway 下单入口实现 Redis 滑动窗口限流（Lua 原子脚本）与 `SetNX+TTL` 幂等锁双重防护，分别拦截慢速重复与瞬时并发重复请求，统一返回 429/409 并完成单测与构建验证。

## 8.2 口头表述（1-2 分钟）
我们发现 `/trip/start` 没有限流，用户连点和网络重试会带来重复下单。我做了双重防护：第一层是滑动窗口限流，用 Redis ZSet + Lua 脚本把“删过期、计数、写入”做成原子操作，保证并发计数准确，限制 10 秒内 1 次；第二层是 5 秒幂等锁，用 `SetNX` 拦截毫秒级并发重复请求。这样慢速重复和瞬时重复都能覆盖，而且返回码也区分开，429 是频率超限，409 是重复提交。最后我补了共享模块和网关侧测试并跑通构建。

---

## 9. 面试高频 10 问 10 答

1. 问：为什么不用固定窗口？
答：固定窗口有临界突刺，边界时刻短时间可通过双倍请求，滑动窗口更精准。

2. 问：为什么滑动窗口要用 Lua？
答：删旧、计数、写入必须原子，拆成多个命令会有并发竞态。

3. 问：为什么用了 ZSet？
答：ZSet 用 score 存时间戳，天然适合按时间范围删除和计数。

4. 问：`EXPIRE`/`PEXPIRE` 的意义是什么？
答：避免 key 常驻内存，窗口结束后自动回收。

5. 问：为什么还要幂等锁？
答：滑动窗口管频率，幂等锁管瞬时并发，二者处理不同粒度问题。

6. 问：为什么返回 429 和 409 两种状态码？
答：便于区分“频率超限”和“瞬时重复提交”，前端可以给不同提示。

7. 问：如何调限流参数？
答：结合拒绝率和业务转化率调 `window/limit`，先保守上线再逐步放宽。

8. 问：Lua 脚本执行失败怎么办？
答：当前实现返回 500，避免在限流不可用时放开流量造成雪崩。

9. 问：为什么 member 需要带序列号？
答：避免同毫秒内 member 冲突导致记录覆盖，保证计数准确。

10. 问：这个方案如何扩展到按 IP 或设备限流？
答：把 key 维度从 `userID` 改为 `ip` 或 `deviceID`，模块本身无需改动。

---

## 10. 代码阅读学习顺序教程

建议按“模块 -> 接入 -> 测试 -> 运行验证”顺序学习：

1. 先看共享限流模块
- 文件：`shared/cache/ratelimit.go`
- 重点：Lua 脚本参数含义、`SlidingWindowAllow` 输入校验与返回语义。

2. 再看下单入口接入
- 文件：`services/api-gateway/http.go`
- 重点：`allowTripStartByRateLimit`、`acquireTripStartCreateLock`、429/409 分支。

3. 看路由注入
- 文件：`services/api-gateway/main.go`
- 重点：`handleTripStart` 如何拿到 `redisClient`。

4. 看共享模块单测
- 文件：`shared/cache/ratelimit_test.go`
- 重点：基本限流、窗口恢复、非法参数。

5. 看网关侧单测
- 文件：`services/api-gateway/http_rate_limit_test.go`
- 重点：双重防护 helper 的行为验证。

6. 本地复现命令
```bash
go test ./shared/cache/... -v
go test ./services/api-gateway/... -v
go build ./services/api-gateway/...
```

完成上述顺序后，你会形成完整认知链路：

- 原理层：为什么滑动窗口 + Lua
- 工程层：如何在网关无侵入接入
- 验证层：如何证明功能和边界条件都正确
