# JWT 鉴权系统实现方案（零漂移执行参考）

> 本文档是 JWT 鉴权功能的实现蓝图。实现时严格按本文档执行，避免偏移。
> 配套文档：`项目改进方案.md`（宏观背景）、最终交付的 `JWT实现文档.md`（用户使用说明）。

---

## 0. Context

根据 `项目改进方案.md` 中的**改进项一**，在 `mygo-microservices` 项目的 api-gateway 中补齐 JWT 鉴权系统。目标是让项目从"学习项目"升级为"可对外服务"的状态，并为后续演唱会压测提供真实鉴权链路。

**要解决的问题**：
- 所有 HTTP 接口对外开放，无用户身份概念
- 后续压测无法模拟真实请求路径（含鉴权开销）
- 项目缺用户系统 → 面试官一眼判定学习项目

**预期成果**：
- 注册 / 登录 / 刷新 / 登出完整鉴权链路
- 网关层统一鉴权中间件
- 完整单元测试 + 集成测试覆盖
- 所有关键路径可用 curl 复现

---

## 1. 技术决策

| 项 | 决策 | 理由 |
|---|---|---|
| Token 格式 | JWT **HS256** | 对称签名够单团队，非对称过度设计 |
| Token 策略 | access 15min + refresh 7d | 双 token 降低泄露面 |
| 用户存储 | **MongoDB** `users` collection | 复用栈，零新依赖 |
| 密码存储 | **bcrypt cost=10** | 防暴力破解 |
| 撤销机制 | **Redis 黑名单**，key=`auth:revoked:{jti}`，TTL=token 剩余有效期 | 保留无状态优势 |
| Refresh 存储 | Redis，key=`auth:refresh:{tokenID}`，TTL=7d | 支持撤销 |
| 中间件位置 | api-gateway 装饰器模式（仿 `enableCORS`） | 集中配置 |
| JWT 库 | `github.com/golang-jwt/jwt/v5` | 维护活跃，v5 是推荐版本 |
| 角色 | `rider` / `driver`（enum） | 足够当前业务，不做权限树 |

---

## 2. 关键代码事实（已探索）

- Go module 名：`ride-sharing`
- JWT 库未装，需 `go get github.com/golang-jwt/jwt/v5`
- `golang.org/x/crypto/bcrypt` 已在 go.mod（v0.33.0）
- `github.com/google/uuid` 已在 go.mod（v1.6.0）
- MongoDB 客户端初始化：`shared/db/mongodb.go:35` `NewMongoClient(ctx, cfg)`；默认库名 `ride-sharing`
- **api-gateway 当前不连 MongoDB**，需要在 `main.go` 新增 Mongo 初始化
- Redis 客户端初始化：`shared/cache/redis_client.go:33` `NewRedisClientFromEnv()`
- 中间件模板：`services/api-gateway/middleware.go:5` `enableCORS` 装饰器
- JSON 写入模板：`services/api-gateway/json.go` `writeJSON(w, status, data)`
- 错误响应契约：`shared/contracts/http.go` `APIResponse{Data, Error}`, `APIError{Code, Message}`
- 环境变量工具：`shared/env/env.go` `GetString / GetInt / GetBool`
- 路由风格：`http.NewServeMux()` + `"POST /path"` HTTP 1.22 语法
- 测试模板：`services/api-gateway/http_rate_limit_test.go`，用 `miniredis.Run()`

---

## 3. 文件清单

### 新增文件

| 文件 | 职责 |
|---|---|
| `shared/auth/claims.go` | Claims 结构体 + Role 常量 |
| `shared/auth/jwt.go` | Signer 实现签发/验证 |
| `shared/auth/jwt_test.go` | Signer 单元测试 |
| `shared/auth/password.go` | bcrypt 包装 |
| `shared/auth/password_test.go` | 密码单元测试 |
| `services/api-gateway/mongo.go` | Mongo 客户端初始化（仿 `redis.go`） |
| `services/api-gateway/user_repo.go` | Mongo users repository |
| `services/api-gateway/user_repo_test.go` | Repo 集成测试（真 Mongo） |
| `services/api-gateway/auth_middleware.go` | Bearer 解析 + 黑名单校验 |
| `services/api-gateway/auth_middleware_test.go` | 中间件单测 |
| `services/api-gateway/auth_handler.go` | 注册/登录/刷新/登出 handler |
| `services/api-gateway/auth_handler_test.go` | Handler 集成测试 |
| `JWT实现文档.md` | 最终用户使用文档（含 curl 脚本） |

### 修改文件

| 文件 | 修改内容 |
|---|---|
| `services/api-gateway/main.go` | 注入 Mongo + signer，挂载新路由 |
| `go.mod` / `go.sum` | 新增 `golang-jwt/jwt/v5` |
| `infra/development/k8s/app-config.yaml` | 加 `JWT_ISSUER` / `JWT_ACCESS_TTL` / `JWT_REFRESH_TTL` |
| `infra/development/k8s/secrets.yaml` | 加 `jwt-secrets` |
| `infra/development/k8s/api-gateway-deployment.yaml` | 注入 JWT_* 和 MONGODB_URI |

---

## 4. 实施步骤

### 步骤 1：安装依赖（15 分钟）

```bash
cd /home/hx/workspace/code/mygo/mygo-microservices
go get github.com/golang-jwt/jwt/v5
go mod tidy
```

**验证**：`grep golang-jwt go.mod` 能看到 v5 条目。

---

### 步骤 2：实现 `shared/auth` 核心包（半天）

#### 2.1 `shared/auth/claims.go`

```go
package auth

import "github.com/golang-jwt/jwt/v5"

type Role string

const (
    RoleRider  Role = "rider"
    RoleDriver Role = "driver"
)

const (
    TokenTypeAccess  = "access"
    TokenTypeRefresh = "refresh"
)

type Claims struct {
    UserID    string `json:"uid"`
    Role      Role   `json:"role"`
    TokenType string `json:"typ"`
    jwt.RegisteredClaims
}
```

#### 2.2 `shared/auth/jwt.go`

```go
type Signer struct {
    secret     []byte
    issuer     string
    accessTTL  time.Duration
    refreshTTL time.Duration
}

func NewSigner(secret []byte, issuer string, accessTTL, refreshTTL time.Duration) *Signer

func (s *Signer) IssueAccessToken(userID string, role Role) (token string, jti string, err error)
func (s *Signer) IssueRefreshToken(userID string, role Role) (token string, jti string, err error)
func (s *Signer) Parse(tokenStr string) (*Claims, error)  // 校验签名、过期、issuer
```

**关键点**：
- 每次签发用 `uuid.NewString()` 作 JTI（放 `RegisteredClaims.ID`）
- 时间用 `jwt.NewNumericDate(time.Now())`
- `Parse` 用 `jwt.ParseWithClaims` + `jwt.WithIssuer(issuer)` + `jwt.WithLeeway(30*time.Second)`
- 校验算法必须是 HS256（防算法混淆攻击）

#### 2.3 `shared/auth/password.go`

```go
func HashPassword(plain string) (string, error)   // bcrypt cost=10
func VerifyPassword(hashed, plain string) error   // 返回 nil 匹配
```

**要点**：空字符串输入返回 error（防止 bcrypt 对空字符串产生 hash）。

---

### 步骤 3：MongoDB 用户 Repository（2 小时）

#### 3.1 `services/api-gateway/mongo.go`

仿 `redis.go` 写：

```go
func NewMongoClient(ctx context.Context) (*mongo.Client, *mongo.Database, error)
```

- 读 `MONGODB_URI` 环境变量
- 用 `shared/db.NewMongoClient` + `GetDatabase`
- 启动时 Ping 验证（**重试 3 次、每次间隔 2s**，规避 tilt up 下 mongodb 未就绪）
- **不在此处创建索引**。索引统一由 `main.go` 在连线阶段通过 `EnsureUserIndexes(ctx, db)` 创建（见步骤 6），避免多处重复。

#### 3.2 `services/api-gateway/user_repo.go`

```go
type UserDoc struct {
    ID           primitive.ObjectID `bson:"_id,omitempty"`
    UUID         string             `bson:"uuid"`
    Phone        string             `bson:"phone"`
    PasswordHash string             `bson:"password_hash"`
    Role         string             `bson:"role"`
    CreatedAt    time.Time          `bson:"created_at"`
    UpdatedAt    time.Time          `bson:"updated_at"`
}

var (
    ErrUserNotFound = errors.New("user not found")
    ErrPhoneTaken   = errors.New("phone already registered")
)

type UserRepository interface {
    Create(ctx context.Context, u *UserDoc) error
    FindByPhone(ctx context.Context, phone string) (*UserDoc, error)
    FindByUUID(ctx context.Context, uuid string) (*UserDoc, error)
}

type mongoUserRepo struct{ col *mongo.Collection }
func NewMongoUserRepo(db *mongo.Database) UserRepository
```

**要点**：
- `FindByPhone` 找不到返回 `ErrUserNotFound`
- `Create` 遇 duplicate key 返回 `ErrPhoneTaken`（检查 `mongo.IsDuplicateKeyError(err)`）
- UUID 用 `google/uuid` 生成

---

### 步骤 4：鉴权中间件（2 小时）

#### 4.1 `services/api-gateway/auth_middleware.go`

```go
type ctxKey string
const (
    CtxUserID ctxKey = "userID"
    CtxRole   ctxKey = "role"
    CtxJTI    ctxKey = "jti"
    CtxExpiry ctxKey = "expiry"
)

func authRequired(signer *auth.Signer, rdb redis.UniversalClient) func(http.HandlerFunc) http.HandlerFunc {
    return func(next http.HandlerFunc) http.HandlerFunc {
        return func(w http.ResponseWriter, r *http.Request) {
            // 1. 提取 Bearer token（strings.EqualFold 容忍大小写）
            // 2. 调 signer.Parse
            // 3. 检查 TokenType == "access"（refresh 不能访问业务接口）
            // 4. 检查 Redis 黑名单 "auth:revoked:<jti>"
            // 5. 把 userID/role/jti/expiry 放 context
            // 6. 调 next
        }
    }
}
```

**错误响应**：`writeJSON(w, 401, APIResponse{Error: &APIError{Code: "unauthorized", Message: "<原因>"}})`

---

### 步骤 5：Auth Handler（半天）

#### 5.1 `services/api-gateway/auth_handler.go`

**`handleRegister`** — `POST /auth/register`
- 入参：`{phone, password, role}`
- 校验：phone 长度 6~20、password 长度 ≥6、role ∈ {rider, driver}
- 流程：`HashPassword` → `userRepo.Create` → 返回 `{userID: uuid}`，状态 201
- 错误：phone 冲突 409、其他校验 400、内部错误 500

**`handleLogin`** — `POST /auth/login`
- 入参：`{phone, password}`
- 流程：`FindByPhone` → `VerifyPassword` → `IssueAccessToken` + `IssueRefreshToken`
- 副作用：refresh token 的 jti 写 Redis `auth:refresh:<jti>` = `userID`，TTL=7d
- 返回：`{access_token, refresh_token, expires_in: int(accessTTL.Seconds())}`（动态，不得硬编码）
- 错误：密码错或用户不存在**统一返回 401**（防枚举）

**`handleRefresh`** — `POST /auth/refresh`
- 入参：`{refresh_token}`
- 流程：
  1. `signer.Parse(refreshToken)` 检查签名和过期
  2. 校验 `TokenType == "refresh"`
  3. 检查 `auth:refresh:<jti>` 还在 Redis（没被撤销）
  4. 旋转：删旧 refresh 的 key、签发新 access + 新 refresh、写新 refresh 到 Redis
  5. 返回新的双 token
- 错误：refresh 失效/过期/被撤销统一返回 401

**`handleLogout`** — `POST /auth/logout`
- 需要 `authRequired` 中间件
- 入参：可选 `{refresh_token}`
- 流程：
  1. 从 context 取 access token 的 jti，写 Redis `auth:revoked:<jti>` = 1，TTL = access 剩余有效期
  2. 如果带 refresh_token，也解析 jti 并删除 `auth:refresh:<jti>`
- 返回：`{"message": "logged out"}`

---

### 步骤 6：main.go 连线（1 小时）

```go
// 1. 初始化 Mongo（带 3 次重试、间隔 2s）
mongoClient, db, err := NewMongoClient(ctx)
if err != nil { log.Fatalf("mongo: %v", err) }
defer mongoClient.Disconnect(ctx)

// 1.1 统一在 main.go 创建索引（mongo.go / repo 均不再重复创建）
if err := EnsureUserIndexes(ctx, db); err != nil {
    log.Fatalf("ensure user indexes: %v", err)
}

// 2. 构建 signer —— JWT_SECRET 必须非空，缺失直接 fail fast
secret := env.GetString("JWT_SECRET", "")
if len(secret) < 32 {
    log.Fatalf("JWT_SECRET missing or too short (need >=32 bytes)")
}
accessTTL, err := time.ParseDuration(env.GetString("JWT_ACCESS_TTL", "15m"))
if err != nil { log.Fatalf("parse JWT_ACCESS_TTL: %v", err) }
refreshTTL, err := time.ParseDuration(env.GetString("JWT_REFRESH_TTL", "168h"))
if err != nil { log.Fatalf("parse JWT_REFRESH_TTL: %v", err) }
signer := auth.NewSigner(
    []byte(secret),
    env.GetString("JWT_ISSUER", "mygo-gateway"),
    accessTTL,
    refreshTTL,
)

// 3. 构建 repo
userRepo := NewMongoUserRepo(db)

// 4. 注册新路由
mux.HandleFunc("POST /auth/register", enableCORS(handleRegister(userRepo)))
mux.HandleFunc("POST /auth/login",    enableCORS(handleLogin(userRepo, signer, redisClient, accessTTL)))
mux.HandleFunc("POST /auth/refresh",  enableCORS(handleRefresh(signer, redisClient, accessTTL)))
mux.HandleFunc("POST /auth/logout",   enableCORS(authRequired(signer, redisClient)(handleLogout(redisClient))))

// 4.1 新增一个轻量受保护探针接口，供 E2E 明确验证鉴权链路（与业务前置条件解耦）
mux.HandleFunc("GET /auth/me", enableCORS(authRequired(signer, redisClient)(handleMe())))

// 5. 保护业务路由（示范 1~2 个）
mux.HandleFunc("POST /trip/start", enableCORS(authRequired(signer, redisClient)(func(w http.ResponseWriter, r *http.Request) {
    handleTripStart(w, r, redisClient)
})))
```

**`handleMe`**：`GET /auth/me`，从 context 取 userID/role 原样返回 `{userID, role}`，状态 200。
作用：在 E2E 脚本中作为“鉴权是否通过”的稳定探针，避免和业务 handler（/trip/start）的前置条件耦合。

**`expires_in` 取值**：登录/刷新返回体中的 `expires_in` 必须等于 `int(accessTTL.Seconds())`，不得硬编码 900。这也是 `handleLogin` / `handleRefresh` 需要接收 `accessTTL` 参数的原因。

---

### 步骤 7：k8s 配置（1 小时）

#### 7.1 `infra/development/k8s/secrets.yaml` 追加

```yaml
---
apiVersion: v1
kind: Secret
metadata:
  name: jwt-secrets
type: Opaque
stringData:
  jwt-secret: "local-dev-secret-at-least-32-bytes-long-xx"
```

#### 7.2 `infra/development/k8s/app-config.yaml` 追加

```yaml
JWT_ISSUER: "mygo-gateway"
JWT_ACCESS_TTL: "15m"
JWT_REFRESH_TTL: "168h"
```

#### 7.3 `infra/development/k8s/api-gateway-deployment.yaml` env 追加

```yaml
- name: JWT_SECRET
  valueFrom:
    secretKeyRef: { name: jwt-secrets, key: jwt-secret }
- name: JWT_ISSUER
  valueFrom:
    configMapKeyRef: { name: app-config, key: JWT_ISSUER }
- name: JWT_ACCESS_TTL
  valueFrom:
    configMapKeyRef: { name: app-config, key: JWT_ACCESS_TTL }
- name: JWT_REFRESH_TTL
  valueFrom:
    configMapKeyRef: { name: app-config, key: JWT_REFRESH_TTL }
- name: MONGODB_URI
  valueFrom:
    secretKeyRef: { name: mongodb-credentials, key: uri }
```

---

## 5. 测试规划

**覆盖率目标 ≥ 80%**。测试分 5 层：4 层自动化（单元 / repo 集成 / 中间件 / handler 集成） + 1 层 E2E（curl 手动）。

### 测试层级 1：`shared/auth` 单元测试

**`shared/auth/jwt_test.go`**

| 测试 | 场景 | 校验 |
|---|---|---|
| `TestSigner_IssueAndParse_HappyPath` | 签发 access → parse | claims 的 userID/role/jti 正确 |
| `TestSigner_ExpiredToken` | 签发 1ns TTL，等 10ms | Parse 返回 `jwt.ErrTokenExpired` |
| `TestSigner_InvalidSignature` | secret A 签、secret B 校验 | signature invalid |
| `TestSigner_WrongIssuer` | 签后改 signer.issuer | Parse 拒绝 |
| `TestSigner_TokenType_Differentiated` | access/refresh TokenType 不同 | 能区分 |
| `TestSigner_JTI_Unique` | 连续签发 100 个 | 100 个 jti 不重复 |
| `TestSigner_AlgorithmConfusion` | 篡改 header alg=none | Parse 拒绝 |

**`shared/auth/password_test.go`**

| 测试 | 场景 | 校验 |
|---|---|---|
| `TestHashPassword_Basic` | hash → verify | 匹配成功 |
| `TestHashPassword_WrongPassword` | 错密码 verify | 返回 error |
| `TestHashPassword_EmptyInput` | 空字符串 | 返回 error |
| `TestHashPassword_Determinism` | 同明文 hash 两次 | 结果不同（含 salt） |

### 测试层级 2：`user_repo` 集成测试

**`user_repo_test.go`** — 用**真 MongoDB**：

- Helper `startMongoForTest(t)` 读 `MONGODB_URI_TEST`，缺省 `mongodb://localhost:27017/test_jwt`
- 每测试独立 collection（timestamp 后缀），测试结束 drop
- CI 跳过：`MONGODB_URI_TEST` 未设置 `t.Skip("requires mongodb")`

| 测试 | 场景 | 校验 |
|---|---|---|
| `TestMongoUserRepo_Create_Basic` | 创建用户 | 能 FindByPhone 查回 |
| `TestMongoUserRepo_Create_DuplicatePhone` | 同手机号创建两次 | 第二次 `ErrPhoneTaken` |
| `TestMongoUserRepo_FindByPhone_NotFound` | 不存在手机号 | `ErrUserNotFound` |
| `TestMongoUserRepo_FindByUUID_Basic` | 创建后按 UUID 查 | 能查到 |
| `TestMongoUserRepo_Create_IndexEnforced` | 并发创建同 phone | 恰一个成功 |

### 测试层级 3：`auth_middleware` 单元测试

**`auth_middleware_test.go`** — `miniredis` + 真 signer：

| 测试 | 场景 | 校验 |
|---|---|---|
| `TestAuthRequired_ValidToken` | 正常 access token | next 被调，context 有 userID |
| `TestAuthRequired_MissingHeader` | 无 Authorization | 401 |
| `TestAuthRequired_MalformedHeader` | `Authorization: foo` | 401 |
| `TestAuthRequired_ExpiredToken` | 过期 token | 401 |
| `TestAuthRequired_RefreshTokenRejected` | refresh token 访问业务接口 | 401 |
| `TestAuthRequired_RevokedToken` | token 在黑名单 | 401 |
| `TestAuthRequired_ContextPropagation` | 成功鉴权 | next 能取到 userID、role |
| `TestAuthRequired_CaseInsensitiveBearer` | `authorization: BEARER xxx` | 接受 |

### 测试层级 4：`auth_handler` 集成测试

**`auth_handler_test.go`** — `httptest.NewRecorder` + miniredis + 真 Mongo：

| 测试 | 场景 | 校验 |
|---|---|---|
| `TestHandleRegister_Happy` | 正常注册 | 201，返回 userID |
| `TestHandleRegister_InvalidPhone` | 空/太短 | 400 |
| `TestHandleRegister_DuplicatePhone` | 两次同手机号 | 第二次 409 |
| `TestHandleRegister_WeakPassword` | 密码 <6 位 | 400 |
| `TestHandleRegister_InvalidRole` | role=admin | 400 |
| `TestHandleLogin_Happy` | 注册后登录 | 返回双 token，refresh jti 入 Redis |
| `TestHandleLogin_WrongPassword` | 密码错 | 401 |
| `TestHandleLogin_UserNotExist` | 不存在 phone | 401（防枚举） |
| `TestHandleRefresh_Happy` | refresh 换 token | 返回新双 token，旧 refresh 从 Redis 删除 |
| `TestHandleRefresh_WithAccessToken` | 用 access 换 | 401 |
| `TestHandleRefresh_Revoked` | 登出后用旧 refresh | 401 |
| `TestHandleRefresh_Rotation` | 连续 refresh 3 次 | 每次旧 jti 失效 |
| `TestHandleLogout_Happy` | 带 access 登出 | jti 入黑名单；再访问 authRequired 返回 401 |
| `TestHandleLogout_WithRefresh` | 带 refresh 登出 | refresh jti 从 Redis 删除 |

### 测试层级 5：端到端 curl 手动验证

放在 `JWT实现文档.md` 中，7 步脚本：

```bash
# 1. 注册
curl -sX POST localhost:8081/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"phone":"13800138000","password":"test123","role":"rider"}'

# 2. 登录，保存 token
TOKENS=$(curl -sX POST localhost:8081/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"phone":"13800138000","password":"test123"}')
ACCESS=$(echo $TOKENS | jq -r .access_token)
REFRESH=$(echo $TOKENS | jq -r .refresh_token)

# 3. 带 token 访问受保护探针 → 200
curl -sfX GET localhost:8081/auth/me \
  -H "Authorization: Bearer $ACCESS"

# 4. 无 token 访问探针 → 401
curl -s -o /dev/null -w '%{http_code}\n' \
  -X GET localhost:8081/auth/me   # 期望输出 401

# 5. 刷新 token
NEW=$(curl -sX POST localhost:8081/auth/refresh \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH\"}")

# 6. 登出
curl -sX POST localhost:8081/auth/logout \
  -H "Authorization: Bearer $ACCESS"

# 7. 登出后用旧 token 访问探针 → 401
curl -s -o /dev/null -w '%{http_code}\n' \
  -X GET localhost:8081/auth/me \
  -H "Authorization: Bearer $ACCESS"   # 期望输出 401
```

> 业务接口 `/trip/start` 的 E2E 验证单独做：由于它有业务前置条件（有无可用 driver、RabbitMQ 是否就绪等），
> 只验证「无 token → 401」、「带伪造 token → 401」、「带合法 token → 非 401」这三条，
> 不要求业务返回 200。

### 测试运行命令

```bash
# 1) shared/auth 单元测试（无外部依赖）
go test -race -count=1 ./shared/auth/...

# 2) api-gateway 全量测试 —— 验收必须全量跑，不允许用 -run 过滤
#    （包含既有业务测试 + 新增鉴权测试，避免遗漏未匹配命名的用例）
docker run -d --rm --name test-mongo -p 27017:27017 mongo:7
export MONGODB_URI_TEST="mongodb://localhost:27017/test_jwt"
go test -race -count=1 ./services/api-gateway/...
docker stop test-mongo

# 3) 定位问题时可用 -run 聚焦，但验收以 2) 全量 PASS 为准
# go test -race -count=1 ./services/api-gateway/... -run "Auth|JWT|Middleware|MongoUserRepo|Handle(Register|Login|Refresh|Logout)"

# 4) E2E（tilt up 运行中） —— 脚本不是 markdown 本身
#    把本文档「端到端 curl 手动验证」段落内容复制为 scripts/jwt_e2e.sh 后执行
bash scripts/jwt_e2e.sh

# 5) 覆盖率
go test -coverprofile=cover.out ./shared/auth/... ./services/api-gateway/...
go tool cover -func=cover.out | grep -E "total|auth"
```

---

## 6. 验证 Checklist

实现结束后逐项勾选：

- [ ] `go build ./...` 通过
- [ ] `go vet ./...` 通过
- [ ] `go test -race ./shared/auth/...` 全 PASS
- [ ] `go test -race ./services/api-gateway/...` **全量** PASS（不得 -run 过滤）
- [ ] 集成测试（真 Mongo）全 PASS
- [ ] `tilt up` 启动 api-gateway 无错误日志；JWT_SECRET 缺失/过短时进程直接退出（验证 fail-fast）
- [ ] `scripts/jwt_e2e.sh` 7 步按预期返回
- [ ] `/auth/me` 未带 token → 401；带正确 token → 200 且返回体含 userID/role
- [ ] `/trip/start` 未带 token → 401；带伪造 token → 401；带合法 token → **非 401**（不要求业务 200）
- [ ] 登出后旧 token 立即失效（`/auth/me` 返回 401）
- [ ] Refresh 旋转后旧 refresh 失效（Redis 无该 jti）
- [ ] 登录/刷新返回体 `expires_in == int(accessTTL.Seconds())`（由配置驱动，非硬编码）
- [ ] auth 相关代码总覆盖率 ≥ 80%
- [ ] `JWT实现文档.md` 交付完整

---

## 7. Risks & Mitigations

| 风险 | Mitigation |
|---|---|
| JWT secret 明文提交到 secrets.yaml | 只放开发用弱 secret；生产 secret 通过 CI 注入 |
| MongoDB 索引竞态（多 pod 启动） | `Indexes().CreateOne` 幂等，Mongo 保证 |
| Bearer 大小写敏感 | 用 `strings.EqualFold` |
| 时钟漂移导致校验错误 | `jwt.WithLeeway(30*time.Second)` |
| tilt up 下 mongodb 未就绪 Ping 超时 | `NewMongoClient` 内置 retry 3 次、间隔 2s（已写入步骤 3.1） |
| JWT_SECRET 环境变量漏配，回退到空密钥签发 | `main.go` 启动时强校验 `len(JWT_SECRET) >= 32`，缺失/过短直接 `log.Fatalf` |
| `expires_in` 与 `JWT_ACCESS_TTL` 不一致误导客户端 | 返回体 `expires_in` 统一由 `accessTTL.Seconds()` 生成，测试断言一致性 |
| 测试 Mongo 污染生产数据 | 独立 db，测试结束 drop；不连生产 |
| 现有 /trip/start 业务测试被鉴权打破 | 测试手动签 test-only token 注入 header |
| 算法混淆攻击（header alg=none） | Parse 里强制 `SigningMethodHMAC` |

---

## 8. 实现时使用的 Prompt

> 将下面整段 prompt 粘贴给实现阶段的 Claude，可在无上下文情况下直接执行。

```
你现在要在 /home/hx/workspace/code/mygo/mygo-microservices 项目里实现 JWT 鉴权系统。

【第一步】必读以下两份文档，严格按其中描述的文件路径、函数签名、测试清单执行，不要自行发挥：
1. /home/hx/workspace/code/mygo/mygo-microservices/JWT实现方案.md —— 本次任务的零漂移执行参考（本文档）
2. /home/hx/workspace/code/mygo/mygo-microservices/项目改进方案.md —— 宏观背景，理解为什么这么做

【第二步】读完后立刻探索以下 4 个文件，理解项目代码风格（中间件装饰器、JSON 响应、错误处理、路由注册）：
- services/api-gateway/main.go
- services/api-gateway/middleware.go
- services/api-gateway/redis.go
- services/api-gateway/http_rate_limit_test.go

【执行硬规则】
1. 严格按方案文件「实施步骤」的 1~7 顺序执行，每完成一步用 TaskUpdate 标 completed
2. 每个新文件写完立刻运行对应测试，PASS 再进下一步；不要所有文件写完才测
3. 所有错误处理用 fmt.Errorf 配 %w wrapping，日志用 stdlib log（项目没有 zap/slog）
4. 中间件一律用装饰器模式，仿 enableCORS（services/api-gateway/middleware.go:5）
5. HTTP 响应统一用 writeJSON（services/api-gateway/json.go），错误用 shared/contracts/http.APIResponse
6. 单元测试用 miniredis，集成测试跳过逻辑照抄方案的「MONGODB_URI_TEST」约定
7. MongoDB users collection 的索引（phone 唯一）在 main.go 启动时创建，不要在业务代码里重复创建
8. JWT_SECRET 不允许硬编码到 Go 代码，一律从环境变量读；测试里用 test-only 密钥
9. 单元测试每个 test 加 t.Parallel()，除非显式依赖共享状态
10. 不要跳过方案列出的任何测试用例，尤其是：JTI 唯一性、refresh 不能当 access、登出后旧 token 失效、refresh 旋转后旧 refresh 失效、算法混淆攻击防护
11. 代码里附上详细中文注释

【禁止的行为】
- 不要自行添加 OAuth、多因素认证、邮箱验证、角色权限树等超出范围的功能
- 不要引入 zap/slog/logrus 等新日志库
- 不要引入 gin/fiber/chi 等新 HTTP 框架（继续用 net/http + ServeMux）
- 不要修改 services/api-gateway 以外的业务 handler 的内部逻辑，只给它们包装 authRequired 中间件
- 不要在 Go 代码里写注释解释"为什么加 JWT"（这些写进文档）

【验证流程】
全部代码写完后按方案文件「验证 Checklist」逐项勾选，任何一项未通过都必须修到通过再交付。最后产出的 JWT实现文档.md 必须包含可复现的 curl 7 步脚本。

【遇到歧义时】
优先级：JWT实现方案.md > 项目改进方案.md > 现有代码风格 > 你的判断。如果方案和现有代码有冲突，停下来问用户。

开始。先用 TaskCreate 把方案中的 7 个实施步骤 + 5 层测试（4 层自动化 + 1 层 E2E）写成 TodoList，然后从步骤 1 开始。
```

---

## 9. 完工交付物

1. **代码**：12 个新文件 + 5 个修改
2. **测试**：单元 + 集成 + E2E 三层，覆盖率 ≥ 80%
3. **文档**：`JWT实现文档.md`（用户使用说明），包含：
   - 架构图（注册→登录→鉴权→刷新→登出 5 步流程）
   - 环境变量说明表
   - curl 7 步验证脚本
   - 常见问题排查（token 过期、签名失败、黑名单命中）
4. **简历回填**：在 CV.md / cv.tex 加「JWT 双 token + Redis 黑名单鉴权链路，覆盖全部业务接口」
