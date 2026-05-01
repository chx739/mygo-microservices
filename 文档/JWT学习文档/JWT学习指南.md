# JWT 学习指南

这不是「JWT 是什么」的科普文，而是**本仓库这套 JWT 系统怎么想、怎么搭、怎么测**的导读。

- 设计 SSOT：`JWT实现方案.md`
- 使用/运维：`JWT实现文档.md`
- **学习路线（你正在看的）**：`JWT学习指南.md`

---

## 1. 一张图

```
 Client
   │  Authorization: Bearer <access>
   ▼
┌──────────────────────────────┐       ┌───────────────────┐
│  api-gateway                  │──读──▶│  Redis            │
│    enableCORS                 │       │  auth:revoked:*   │  （黑名单）
│      └─ authRequired          │       │  auth:refresh:*   │  （白名单）
│           └─ handler          │       └───────────────────┘
│    /auth/*                    │
│    /trip/start (受保护)       │──查──▶┌───────────────────┐
│    /auth/me (稳定探针)        │       │  MongoDB users    │
└──────────────────────────────┘       │  { uuid, phone,   │
   │                                    │    hash, role }   │
   │ gRPC / RabbitMQ                   └───────────────────┘
   ▼
 trip / driver / payment （不校验 JWT）
```

关键事实：
- **token 只在 api-gateway 校验**，下游服务信任 gateway 传来的 userID。
- access 15 分钟用**黑名单**；refresh 7 天用**白名单**。
- 登录拒绝用户枚举：phone 不存在和密码错误返回完全相同的响应。

---

## 2. 技术栈与为什么是它

| 职责 | 用的库 | 为什么 |
|---|---|---|
| JWT 签发/解析 | `github.com/golang-jwt/jwt/v5` | Go 社区事实标准；v5 内建 `WithValidMethods` 防 alg=none。 |
| 密码哈希 | `golang.org/x/crypto/bcrypt` cost=10 | bcrypt 带盐 + 自适应 cost，社区默认 10 是算力/安全平衡点。 |
| 用户存储 | MongoDB + 业务 UUID | 保持与其它服务一致（trip-service 也用 Mongo）。UUID 代替 ObjectID 是避免暴露 Mongo 内部结构。 |
| 撤销机制 | Redis（复用已有实例）| access 黑名单用 `Exists` O(1)；refresh 白名单用 `Set` / `Del` 精确单条。无需引入专门的 session store。 |
| 算法 | HS256（对称密钥）| 单发布方（api-gateway 自签自验）；RS256/ECDSA 是多方验证场景才划算。 |
| 测试 Redis | `miniredis/v2` | 纯内存、无需 docker；handler/middleware 单测跑 < 100ms。 |
| 测试 Mongo | 真 Mongo + 独立集合 | Mongo 的 duplicate-key 行为没有可信赖的 mock；集成测试层级唯一解。 |

---

## 3. 代码阅读顺序（推荐路线）

**从内到外，每一层只依赖前面的层**。按这个顺序读不会卡住。

### 3.1 原语（纯函数层，无 IO）
1. `shared/auth/claims.go` — 自定义 Claims、Role、TokenType 枚举。
2. `shared/auth/password.go` — bcrypt 封装 + 空串防御。
3. `shared/auth/jwt.go` — `Signer.IssueAccessToken` / `IssueRefreshToken` / `Parse`。
   - 重点看 `issue` 内部 jti 生成、`Parse` 的 `WithValidMethods` 与内层类型断言。

### 3.2 持久化层
4. `services/api-gateway/user_repo.go` —
   - `UserDoc` bson 标签。
   - `ErrUserNotFound` / `ErrPhoneTaken` 哨兵错误。
   - `Create` 里 `mongo.IsDuplicateKeyError` 转业务错误。
   - **`EnsureUserIndexes`**：唯一索引只在这里建一次（main.go 启动时调用），业务代码不重建。
5. `services/api-gateway/mongo.go` — Ping 3 次重试（`tilt up` 冷启动容忍）。

### 3.3 Web 层
6. `services/api-gateway/auth_middleware.go` —
   - `ctxKey` 自定义类型，避免 context key 冲突。
   - `authRequired` 装饰器签名 `func(http.HandlerFunc) http.HandlerFunc`，**和 `enableCORS` 同构**可嵌套。
   - `extractAndVerifyAccessToken` 是核心：Bearer 解析 → Parse → 检查 `typ=access` → 查黑名单。
7. `services/api-gateway/auth_handler.go` — 5 个 handler（register / login / refresh / logout / me）。
   - 所有 handler 都是**闭包工厂**（`handleLogin(...) http.HandlerFunc`），方便注入依赖，也便于单测。

### 3.4 组装
8. `services/api-gateway/main.go` —
   - `jwtMinSecretLen = 32` + `log.Fatalf` fail-fast。
   - TTL 从环境变量解析。
   - `requireAuth := authRequired(signer, redisClient)` 复用同一个装饰器实例。
   - `/auth/login` 无需鉴权；`/auth/logout` 和 `/auth/me` 必须在 `requireAuth` 内层。
9. `infra/development/k8s/{secrets,app-config,api-gateway-deployment}.yaml` — 配置下发。

### 3.5 测试（阅读顺序同上）
10. `shared/auth/*_test.go` → `auth_middleware_test.go` → `auth_handler_test.go` → `user_repo_test.go` → `scripts/jwt_e2e.sh`。

---

## 4. 非显然的设计决策

读代码时会冒出「为什么要这样」的五个点，先放这里。

### 4.1 双 token + 黑白名单混用
不是冗余。
- **access**（15min）走**黑名单**：默认有效，登出 / 被偷才写一条，命中即拒。
- **refresh**（7d）走**白名单**：登录时写入，旋转/登出时删除，不在白名单 = 失效。
- 反过来（refresh 黑名单）要维护 7 天大小的集合，空间更大；access 白名单要每次请求读 Redis，性能更差。

### 4.2 refresh 旋转：先写新，再删旧
```go
rdb.Set(refreshKeyPrefix+newJTI, ...)   // 先写
rdb.Del(refreshKeyPrefix+oldJTI)        // 后删
```
顺序反了，如果中间 Redis 挂：用户手里两条 refresh 都没白名单 → 只能重新登录。
当前顺序：中间挂也至少有一条可用。

### 4.3 算法锁定的双层防御
```go
jwt.ParseWithClaims(tok, ..., jwt.WithValidMethods([]string{"HS256"}))
// 内层回调里再断言：
if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok { ... }
```
外层 `WithValidMethods` 已能拦住 `alg=none`、RSA 伪造。内层是防御库未来 bug —— 历史上有过 `alg=none/NONE/nonE` 绕过。防御代码几乎无成本。

### 4.4 登录统一 401 防枚举
用户不存在和密码错误：同 HTTP status、同 error code、同 message。
注意 `VerifyPassword("", any)` 也立即返回错误，避免「空密码 hash 碰撞」的攻击面。

### 4.5 handler 用闭包而非 struct
看起来 `handleLogin(repo, signer, rdb, ttl1, ttl2) http.HandlerFunc` 参数很长，但：
- 和项目既有风格一致（`handleTripStart(w, r, redis)` 也是函数）。
- 测试时注入 fake 比 mock struct 方法少一层抽象。
- 无状态，不需要 `sync.Mutex`。

---

## 5. 隐藏的陷阱防御清单

读到这些 5-10 行的小片段时，别以为是废话：

| 位置 | 防御什么 |
|---|---|
| `password.go` 空串拒绝 | 防止用户表里意外存在空 hash → 空密码登陆 |
| `mongo.IsDuplicateKeyError` 转换 | 不把 Mongo 错误直接回给客户端 |
| `ctxKey` 自定义类型 | 防止和其他包的 context key 字符串冲突 |
| Bearer `strings.EqualFold` | 宽容浏览器/curl/Postman 大小写差异 |
| Claims `typ != access` 拒绝 | 防 refresh 被当 access 访问业务接口 |
| `WithLeeway(30s)` | 容忍时钟漂移，避免分布式下「明明没过期」报 401 |
| `blacklistTTL == 0 跳过` | token 已过期再写黑名单是浪费 Redis |
| 索引**只在 main 建一次** | 业务代码重复建索引会产生 drift |

---

## 6. 测试分层为什么这样切

```
┌────────────────────────┐
│ E2E（scripts/jwt_e2e）│  真网关，验证装配正确
├────────────────────────┤
│ user_repo_test（真 Mongo）│  唯一索引、duplicate 行为
├────────────────────────┤
│ auth_handler_test（fake repo + miniredis）│  handler 纯逻辑
├────────────────────────┤
│ auth_middleware_test（miniredis）│  Bearer 解析 + 黑名单
├────────────────────────┤
│ shared/auth/*_test（纯函数）│  算法、签名、过期
└────────────────────────┘
```

核心原则：**越内层的东西跑得越快、数量越多**。改代码先等底层单测挂掉再上层追查；不要反过来从 E2E 定位 bug。

为什么 handler 不用真 Mongo？`UserRepository` 是 interface 就是为这个 —— handler 只需证明「和契约契合」，Repo 实现自己有测试。两者解耦后：
- handler 改不用跑 docker。
- Repo 改（比如换 Postgres）不影响 handler 测试。

---

## 7. 常见改动手册

### 7.1 加一个受保护接口
```go
// main.go
mux.HandleFunc("POST /trip/history",
    enableCORS(requireAuth(handleTripHistory(tripService))))

// handler 里从 context 取身份
func handleTripHistory(...) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        userID, _ := r.Context().Value(CtxUserID).(string)
        // ...
    }
}
```

### 7.2 加角色限制
```go
role, _ := r.Context().Value(CtxRole).(string)
if role != string(auth.RoleDriver) {
    writeAuthError(w, 403, "forbidden", "drivers only")
    return
}
```

### 7.3 轮换 `JWT_SECRET`
1. 改 `jwt-secrets`。
2. 重启 api-gateway。
3. **所有现存 token 立即失效，用户需重新登录。**
4. 要平滑轮换需支持 `kid`（key id）+ 多密钥并存，当前方案没做。

### 7.4 调 TTL
- 改 `JWT_ACCESS_TTL_SECONDS` / `JWT_REFRESH_TTL_SECONDS` 即可，热生效仅对新签发 token。
- 权衡：access 越长，服务端压力越小，但被盗风险窗口越大。15 分钟是社区默认甜点。

### 7.5 加 API Key 类型
1. `claims.go` 新增 `TokenTypeAPIKey = "api_key"`。
2. `jwt.go` 扩 `IssueAPIKey`（TTL 可能是无限，存储在 DB 里用 jti 查）。
3. **不**挤进 `authRequired` 分支 —— 独立个 `apiKeyRequired` 装饰器更清晰。

### 7.6 给 WebSocket 加鉴权（当前未实现）
浏览器 WS 不允许自定义 header，只能从 query string 取 token：
```go
token := r.URL.Query().Get("access_token")
// 调 signer.Parse + 黑名单查询，复用 extractAndVerifyAccessToken 的内部逻辑
```

---

## 8. 踩坑速查

| 现象 | 99% 的原因 |
|---|---|
| 登录拿到 token，调业务接口立刻 401 | 忘带 `Authorization: Bearer`，或 k8s 配错重启过导致签名对不上 |
| refresh 连续两次都成功 | 不可能；前端存的不是最新 refresh，排查客户端存储 |
| `scripts/jwt_e2e.sh` step 2 就 500 | 看 api-gateway 日志，Mongo/Redis 连接错的概率最高 |
| `JWT_SECRET must be set and >= 32 bytes` | 配置缺失/过短，这是期望行为（fail-fast） |
| miniredis 测试偶尔超时 | 并发测试 + `t.Parallel()`，看看是否误用了 `t.Setenv`（和并行不兼容） |
| 改了 handler 签名后编译炸一片 | 闭包工厂签名变化需要改 main.go + 对应测试，全量编译即可定位 |

---

## 9. 学习时间切片

| 有多少时间 | 读什么 |
|---|---|
| 30 分钟 | §3.1 + §3.3 + §4（概念打通） |
| 2 小时 | §3 全部 + §4 + §5（能独立理解代码） |
| 半天 | +§6 + §7（能独立改功能/排障） |
| 要排线上 bug | 直接 §8 + 查对应日志 + 看 `auth_handler_test.go` 里对应场景 |

---

## 10. 延伸阅读

- RFC 7519（JWT）—— 5 分钟扫一遍 Registered Claims 定义就够。
- RFC 8725（JWT 最佳实践）—— 算法混淆、alg=none 的历史漏洞在此。
- `golang-jwt/jwt/v5` README —— 看 `Parse` 系列的几个 Option。
- 项目内 `JWT实现方案.md` §5 —— 测试用例表可以当「还能怎么测」的 checklist。

看不懂任何一处设计，先问自己：**如果不这样写会怎样？** 99% 的决策在这个问题上有答案。
