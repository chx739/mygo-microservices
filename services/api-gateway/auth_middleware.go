package main

// auth_middleware.go 定义 authRequired 装饰器：在业务 handler 之前完成 Bearer 解析、
// JWT 校验、黑名单查询，并把 userID / role / jti / expiry 注入 context 供下游使用。
//
// 设计要点：
//  1. 复用 `enableCORS` 的装饰器签名 `func(http.HandlerFunc) http.HandlerFunc`，
//     便于 `enableCORS(authRequired(...)(handler))` 组合，不引入新抽象。
//  2. Bearer 关键字大小写不敏感（用户 / 浏览器 / curl 写法各异）。
//  3. 只接受 TokenType=access 的 token；refresh 不允许访问业务接口。
//  4. Redis 黑名单命中 → 401；不区分「token 本身无效」和「token 被登出」的错误信息，
//     避免给攻击者额外线索。
//  5. 错误响应统一走 writeJSON + APIResponse，和 /auth/* 路径风格一致。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ride-sharing/shared/auth"
	"ride-sharing/shared/contracts"

	"github.com/redis/go-redis/v9"
)

// ctxKey 是 context value 的类型键。
// 定义成独立类型防止和其他包的 key 字符串冲突。
type ctxKey string

const (
	// CtxUserID：当前请求的业务用户 UUID。
	CtxUserID ctxKey = "auth.userID"
	// CtxRole：当前请求的角色（rider / driver）。
	CtxRole ctxKey = "auth.role"
	// CtxJTI：当前请求 token 的 jti；/auth/logout 需要用它写黑名单。
	CtxJTI ctxKey = "auth.jti"
	// CtxExpiry：token 的过期时间；/auth/logout 用它计算黑名单 TTL，避免永久占用 Redis。
	CtxExpiry ctxKey = "auth.expiry"
)

// revokedKeyPrefix 是黑名单 Redis key 前缀。
// 黑名单的含义：jti 存在于本集合 → 该 token 已被撤销（登出）。
const revokedKeyPrefix = "auth:revoked:"

// refreshKeyPrefix 是 refresh 白名单 Redis key 前缀。
// refresh 采用白名单模型（必须在 Redis 里才有效），方便登出时单条删除。
const refreshKeyPrefix = "auth:refresh:"

// authRequired 是本服务唯一的鉴权装饰器。
// 外层调用方式：enableCORS(authRequired(signer, rdb)(actualHandler))
//
// 参数：
//   - signer：JWT 解析器，注意 signer 状态不可为 nil。
//   - rdb：Redis 客户端，用于黑名单查询，不可为 nil。
func authRequired(signer *auth.Signer, rdb redis.UniversalClient) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			claims, err := extractAndVerifyAccessToken(r, signer, rdb)
			if err != nil {
				writeUnauthorized(w, err.Error())
				return
			}

			// 把关键信息塞进 context，下游 handler 从 context 读，不再解析 header。
			ctx := r.Context()
			ctx = context.WithValue(ctx, CtxUserID, claims.UserID)
			ctx = context.WithValue(ctx, CtxRole, string(claims.Role))
			ctx = context.WithValue(ctx, CtxJTI, claims.ID)
			if claims.ExpiresAt != nil {
				ctx = context.WithValue(ctx, CtxExpiry, claims.ExpiresAt.Time)
			}
			next(w, r.WithContext(ctx))
		}
	}
}

// extractAndVerifyAccessToken 是 authRequired 的核心验证流程，抽出来方便测试。
// 返回的 Claims 只在 TokenType == "access" 且未被撤销的前提下才有效。
func extractAndVerifyAccessToken(r *http.Request, signer *auth.Signer, rdb redis.UniversalClient) (*auth.Claims, error) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return nil, errors.New("missing authorization header")
	}
	// Bearer 前缀大小写不敏感。
	parts := strings.SplitN(raw, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(strings.TrimSpace(parts[0]), "bearer") {
		return nil, errors.New("malformed authorization header")
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return nil, errors.New("empty bearer token")
	}

	claims, err := signer.Parse(token)
	if err != nil {
		return nil, fmt.Errorf("invalid token")
	}
	// 只有 access 才能访问业务接口；refresh 不行。
	if claims.TokenType != auth.TokenTypeAccess {
		return nil, errors.New("refresh token not allowed on business endpoints")
	}

	// 黑名单命中（登出）→ 拒绝。
	if claims.ID != "" {
		revoked, err := rdb.Exists(r.Context(), revokedKeyPrefix+claims.ID).Result()
		if err != nil {
			return nil, fmt.Errorf("check revocation: %w", err)
		}
		if revoked > 0 {
			return nil, errors.New("token has been revoked")
		}
	}
	return claims, nil
}

// writeUnauthorized 是统一的 401 响应入口。
// 给 Code 固定值 "unauthorized"，Message 来自调用方；不要把具体 JWT 错误信息直接返回，
// 避免透露「签名错」/「过期」/「黑名单」的差异，让攻击者难以分辨失败原因。
func writeUnauthorized(w http.ResponseWriter, message string) {
	_ = writeJSON(w, http.StatusUnauthorized, contracts.APIResponse{
		Error: &contracts.APIError{
			Code:    "unauthorized",
			Message: message,
		},
	})
}

// blacklistTTL 计算一个 token 进入黑名单时应使用的 TTL。
// 直接用 token 剩余有效期，过期后自动从 Redis 清除，不会无限增长。
// 若已过期，返回 0 表示本次写入可忽略（调用方可跳过）。
func blacklistTTL(exp time.Time) time.Duration {
	ttl := time.Until(exp)
	if ttl <= 0 {
		return 0
	}
	return ttl
}
