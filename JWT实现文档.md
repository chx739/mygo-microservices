# JWT 鉴权使用文档

> 面向开发 / 运维 / 前端联调。设计细节见 `JWT实现方案.md`，此处只讲「怎么用、怎么排障」。

## 1. 架构一张图

```
        Client
          │
          │  Authorization: Bearer <access>
          ▼
  ┌───────────────────┐        ┌─────────────────┐
  │   api-gateway     │──────▶│  Redis          │
  │  - authRequired   │ 黑/白  │  auth:revoked:  │
  │  - /auth/*        │       │  auth:refresh:  │
  └────────┬──────────┘        └─────────────────┘
           │
           │ gRPC / RabbitMQ
           ▼
  ┌────────────────────────────────────────────┐
  │ trip-service / driver-service / payment    │
  └────────────────────────────────────────────┘

  用户登录 → MongoDB 校验 → 签发双 token
    access  15min  （HS256）
    refresh 7d     （白名单写 Redis）
```

关键点：

- **access token** 校验在 api-gateway 本地完成（HS256 + Redis 黑名单查询），下游服务不验证 JWT。
- **refresh token** 采用白名单模型：登录时写 Redis，登出 / 旋转时删除。离开白名单即失效。
- **算法锁定 HS256**：`alg=none`、`RS256` 伪造都会被拒。
- **防用户枚举**：登录时 phone 不存在和密码错误返回完全相同的响应。

## 2. 环境变量

| Key                        | 来源                  | 默认值                  | 说明                                              |
|---------------------------|----------------------|-------------------------|--------------------------------------------------|
| `JWT_SECRET`              | Secret `jwt-secrets` | （无）必填              | HS256 密钥。**必须 ≥ 32 字节**，否则 fail-fast。 |
| `JWT_ISSUER`              | ConfigMap            | `mygo-api-gateway`      | iss 字段；校验时必须匹配。                        |
| `JWT_ACCESS_TTL_SECONDS`  | ConfigMap            | `900`                   | access 有效期；`expires_in` 响应字段取自此。      |
| `JWT_REFRESH_TTL_SECONDS` | ConfigMap            | `604800`                | refresh 有效期；也是 Redis 白名单 TTL。           |
| `MONGODB_URI`             | Secret `mongodb`     | （无）必填              | 注册 / 登录用户数据来源。                         |
| `REDIS_ADDR`              | ConfigMap            | `redis:6379`            | 黑 / 白名单存储；缺失 → 登录、限流都会 fail。     |

K8s 清单位置：`infra/development/k8s/{app-config,secrets,api-gateway-deployment}.yaml`。

## 3. API 速查 + curl 7 步

以 `GATEWAY=http://localhost:8081` 为例。

```bash
# 1) 注册
curl -s -X POST $GATEWAY/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"phone":"13800138001","password":"password123","role":"rider"}'
# → 201 { "data": { "userID": "<uuid>" } }

# 2) 登录
curl -s -X POST $GATEWAY/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"phone":"13800138001","password":"password123"}'
# → 200 { "data": {
#     "access_token": "...", "refresh_token": "...",
#     "expires_in": 900, "token_type": "Bearer" } }

# 3) 访问受保护接口
curl -s $GATEWAY/auth/me -H "Authorization: Bearer $ACCESS"
# → 200 { "data": { "userID": "...", "role": "rider" } }

# 4) 无 token
curl -s -o /dev/null -w '%{http_code}\n' $GATEWAY/auth/me
# → 401

# 5) 刷新（旋转）
curl -s -X POST $GATEWAY/auth/refresh \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH\"}"
# → 200 新的 access + 新的 refresh；旧 refresh 立即失效

# 6) 登出
curl -s -X POST $GATEWAY/auth/logout \
  -H "Authorization: Bearer $ACCESS" \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH\"}"
# → 200 { "data": { "message": "logged out" } }

# 7) 登出后再用旧 access
curl -s -o /dev/null -w '%{http_code}\n' $GATEWAY/auth/me -H "Authorization: Bearer $ACCESS"
# → 401
```

完整自动化版本：`bash scripts/jwt_e2e.sh`（需要 `curl + jq`）。

## 4. 错误码表

| HTTP | code                  | 触发场景                                                 |
|------|-----------------------|----------------------------------------------------------|
| 400  | `bad_request`         | body 不是合法 JSON                                       |
| 400  | `invalid_phone`       | 手机号长度 < 6 或 > 20                                   |
| 400  | `weak_password`       | 密码长度 < 6                                             |
| 400  | `invalid_role`        | role 不是 `rider` / `driver`                             |
| 401  | `unauthorized`        | 中间件层：缺 token / 签名错 / 黑名单命中 / 过期          |
| 401  | `invalid_credentials` | 登录失败（用户不存在 / 密码错误；同一错误防枚举）        |
| 401  | `invalid_refresh`     | refresh 非法 / 已撤销 / 类型错                           |
| 409  | `phone_taken`         | 注册时手机号已存在                                       |
| 500  | `*_failed` / `redis_error` | 内部错误（查日志）                                  |

## 5. 排障 FAQ

**Q: 启动就退出，日志 `JWT_SECRET must be set and >= 32 bytes`。**  
A: 忘了注入 `JWT_SECRET` 或值过短。`kubectl get secret jwt-secrets -o yaml` 检查；改到 ≥ 32 字节后重启。

**Q: `tilt up` 后 api-gateway CrashLoopBackOff，日志 `ping mongo after 3 attempts`。**  
A: MongoDB Pod 还没就绪或 `MONGODB_URI` 错。先 `kubectl logs mongodb-xxx`，确认 Mongo 起来；再 `kubectl get secret mongodb -o yaml` 核对 URI。

**Q: 登录 200，但调业务接口立刻 401。**  
A: 三种可能：
1. 忘记带 `Authorization: Bearer <token>`；
2. 复制 token 时漏字符 / 前后空格；
3. `JWT_SECRET` 运行期轮换过导致旧 token 签名对不上 —— 重新登录。

**Q: refresh 返回 401 `invalid_refresh`。**  
A: 最常见原因是 refresh 已被上一次旋转消费；再用一次自然失败。前端应当只保留最新一对 token。

**Q: 如何确认某个 jti 是否在黑名单？**  
A: `kubectl exec -it redis-xxx -- redis-cli KEYS 'auth:revoked:*'`。白名单同理 `auth:refresh:*`。

**Q: WS 路由 `/ws/*` 需要 JWT 吗？**  
A: 当前未接入鉴权（与历史行为一致）。要加的话在 upgrade 前调用 `extractAndVerifyAccessToken`，从 query string 取 token（浏览器 WS 不允许自定义 header）。

## 6. 测试与覆盖率

```bash
# 单元 + 中间件 + handler
go test -race -count=1 ./shared/auth/... ./services/api-gateway/...

# 真 Mongo 集成测试（需要本地/远端 Mongo）
docker run -d --rm --name test-mongo -p 27017:27017 mongo:7
MONGODB_URI_TEST=mongodb://localhost:27017/test_jwt \
  go test -race -count=1 ./services/api-gateway/... -run Repo

# 覆盖率
go test -coverprofile=cover.out ./shared/auth/... ./services/api-gateway/...
go tool cover -func=cover.out | grep -E "total|auth"
```

## 7. 后续扩展点（当前未实现）

- WebSocket 鉴权（见 FAQ）。
- `/auth/me` 返回更多 profile 字段（目前只返 uid + role）。
- 修改密码（需要 `UpdatedAt` 字段 + 旧密码校验）。
- RSA / ECDSA 签名方案切换：仅需在 `shared/auth` 扩展；算法锁定处 `WithValidMethods` 要同步更新。
