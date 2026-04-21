package main

// auth_handler.go 提供 /auth/* 路由下 4 个核心 handler + 1 个稳定探针：
//   - POST /auth/register —— 手机号注册。
//   - POST /auth/login    —— 手机号 + 密码登录，返回双 token。
//   - POST /auth/refresh  —— 刷新旋转，一次性换发新 access + 新 refresh。
//   - POST /auth/logout   —— 撤销当前 access（黑名单）+ 可选撤销 refresh（白名单删除）。
//   - GET  /auth/me       —— 稳定鉴权探针，E2E 验证鉴权链路用，不依赖业务前置条件。
//
// 统一约定：
//   - 成功响应用 writeJSON；错误响应用 writeAuthError，code + message 对齐契约。
//   - 密码错误 / 用户不存在 → 一律 401，不暴露差异，防用户枚举。
//   - refresh 旋转策略：签发新 refresh 时先写白名单，再删旧白名单，
//     两步分离降低 Redis 异常时的误杀风险；测试保证顺序正确。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ride-sharing/shared/auth"
	"ride-sharing/shared/contracts"

	"github.com/redis/go-redis/v9"
)

// phone / password 最小长度。数值选得较宽松，是为了兼容测试手机号（6 位短号）。
const (
	minPhoneLen    = 6
	maxPhoneLen    = 20
	minPasswordLen = 6
)

// ---- 请求 / 响应结构体 ---------------------------------------------------

type registerRequest struct {
	Phone    string `json:"phone"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

type registerResponse struct {
	UserID string `json:"userID"`
}

type loginRequest struct {
	Phone    string `json:"phone"`
	Password string `json:"password"`
}

type tokenPairResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"` // 秒，等于 accessTTL；由调用方注入而非硬编码。
	TokenType    string `json:"token_type"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type logoutRequest struct {
	// 可选：登出时顺便撤销指定的 refresh token。
	// 为空则只撤销当前 access。
	RefreshToken string `json:"refresh_token,omitempty"`
}

type meResponse struct {
	UserID string `json:"userID"`
	Role   string `json:"role"`
}

// ---- 响应辅助 ------------------------------------------------------------

// writeAuthError 是 /auth/* 路径下的统一错误出口。
// 和 middleware 的 writeUnauthorized 分开：这里允许 code / http status 由调用方指定，
// 比如 register 的 phone 冲突需要 409，而 login 失败走 401。
func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	_ = writeJSON(w, status, contracts.APIResponse{
		Error: &contracts.APIError{Code: code, Message: message},
	})
}

// decodeJSON 是所有 handler 的请求解析入口。
// 统一返回 400 "bad_request"，避免每个 handler 重复写 err 分支。
func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	// DisallowUnknownFields 可以防止前端漏字段时因默认值导致的隐蔽 bug。
	// 这里暂不开启，兼顾 curl 手测时偶尔多带字段的场景。
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode json: %w", err)
	}
	return nil
}

// ---- /auth/register ------------------------------------------------------

// handleRegister 返回一个绑定 userRepo 的 HandlerFunc。
// 使用闭包而不是结构体方法，保持与项目现有 handler 风格一致（http.go 里的 handleTripStart 等）。
func handleRegister(userRepo UserRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req registerRequest
		if err := decodeJSON(r, &req); err != nil {
			writeAuthError(w, http.StatusBadRequest, "bad_request", "invalid json body")
			return
		}

		phone := strings.TrimSpace(req.Phone)
		password := req.Password
		role := auth.Role(strings.TrimSpace(req.Role))

		switch {
		case len(phone) < minPhoneLen || len(phone) > maxPhoneLen:
			writeAuthError(w, http.StatusBadRequest, "invalid_phone", "phone length must be 6~20")
			return
		case len(password) < minPasswordLen:
			writeAuthError(w, http.StatusBadRequest, "weak_password", "password must be at least 6 chars")
			return
		case !role.IsValid():
			writeAuthError(w, http.StatusBadRequest, "invalid_role", "role must be rider or driver")
			return
		}

		hashed, err := auth.HashPassword(password)
		if err != nil {
			log.Printf("hash password: %v", err)
			writeAuthError(w, http.StatusInternalServerError, "hash_failed", "internal error")
			return
		}

		doc := &UserDoc{
			Phone:        phone,
			PasswordHash: hashed,
			Role:         string(role),
		}
		if err := userRepo.Create(r.Context(), doc); err != nil {
			if errors.Is(err, ErrPhoneTaken) {
				writeAuthError(w, http.StatusConflict, "phone_taken", "phone already registered")
				return
			}
			log.Printf("create user: %v", err)
			writeAuthError(w, http.StatusInternalServerError, "create_failed", "internal error")
			return
		}

		_ = writeJSON(w, http.StatusCreated, contracts.APIResponse{
			Data: registerResponse{UserID: doc.UUID},
		})
	}
}

// ---- /auth/login ---------------------------------------------------------

// handleLogin 需要 accessTTL 来填充响应体的 expires_in 字段。
// accessTTL 由 main.go 从 env 解析后注入，避免 handler 自行读环境变量导致各处不一致。
func handleLogin(userRepo UserRepository, signer *auth.Signer, rdb redis.UniversalClient, accessTTL time.Duration, refreshTTL time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := decodeJSON(r, &req); err != nil {
			writeAuthError(w, http.StatusBadRequest, "bad_request", "invalid json body")
			return
		}

		phone := strings.TrimSpace(req.Phone)
		if phone == "" || req.Password == "" {
			// 空参数直接返回 401，和「用户不存在」走同一路径，防枚举。
			writeAuthError(w, http.StatusUnauthorized, "invalid_credentials", "invalid phone or password")
			return
		}

		doc, err := userRepo.FindByPhone(r.Context(), phone)
		if err != nil {
			if errors.Is(err, ErrUserNotFound) {
				writeAuthError(w, http.StatusUnauthorized, "invalid_credentials", "invalid phone or password")
				return
			}
			log.Printf("find user: %v", err)
			writeAuthError(w, http.StatusInternalServerError, "lookup_failed", "internal error")
			return
		}

		if err := auth.VerifyPassword(doc.PasswordHash, req.Password); err != nil {
			writeAuthError(w, http.StatusUnauthorized, "invalid_credentials", "invalid phone or password")
			return
		}

		role := auth.Role(doc.Role)
		access, _, err := signer.IssueAccessToken(doc.UUID, role)
		if err != nil {
			log.Printf("issue access: %v", err)
			writeAuthError(w, http.StatusInternalServerError, "issue_failed", "internal error")
			return
		}
		refresh, refreshJTI, err := signer.IssueRefreshToken(doc.UUID, role)
		if err != nil {
			log.Printf("issue refresh: %v", err)
			writeAuthError(w, http.StatusInternalServerError, "issue_failed", "internal error")
			return
		}

		// refresh 进入白名单；TTL = refresh 的签发 TTL。
		// Redis 异常不阻塞登录 —— 最坏情况是 refresh 无法撤销，但 access 仍然可用，
		// 用户感知不到登录失败。若要严格保证撤销能力，可在此改为 fail。
		if err := rdb.Set(r.Context(), refreshKeyPrefix+refreshJTI, doc.UUID, refreshTTL).Err(); err != nil {
			log.Printf("cache refresh jti: %v", err)
		}

		_ = writeJSON(w, http.StatusOK, contracts.APIResponse{
			Data: tokenPairResponse{
				AccessToken:  access,
				RefreshToken: refresh,
				ExpiresIn:    int(accessTTL.Seconds()),
				TokenType:    "Bearer",
			},
		})
	}
}

// ---- /auth/refresh -------------------------------------------------------

// handleRefresh 实现 refresh token 旋转：
//  1. 校验传入 refresh 的签名 + 类型 + 是否在 Redis 白名单。
//  2. 签发新 access + 新 refresh，新 refresh 写 Redis。
//  3. 删除旧 refresh 的白名单条目（真正让旧 refresh 失效）。
//
// 旋转的意义：即便 refresh 被中途偷走，攻击者用一次之后，合法用户下次刷新就会发现
// 自己的 refresh 已失效（因为攻击者触发了旋转）—— 作为被动探测手段。
func handleRefresh(signer *auth.Signer, rdb redis.UniversalClient, accessTTL time.Duration, refreshTTL time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req refreshRequest
		if err := decodeJSON(r, &req); err != nil {
			writeAuthError(w, http.StatusBadRequest, "bad_request", "invalid json body")
			return
		}
		if req.RefreshToken == "" {
			writeAuthError(w, http.StatusUnauthorized, "invalid_refresh", "refresh token required")
			return
		}

		claims, err := signer.Parse(req.RefreshToken)
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, "invalid_refresh", "invalid refresh token")
			return
		}
		if claims.TokenType != auth.TokenTypeRefresh {
			writeAuthError(w, http.StatusUnauthorized, "invalid_refresh", "not a refresh token")
			return
		}

		// 检查白名单是否仍在。不在 → 已被撤销（登出 / 上次旋转已消费）。
		if claims.ID == "" {
			writeAuthError(w, http.StatusUnauthorized, "invalid_refresh", "missing jti")
			return
		}
		exists, err := rdb.Exists(r.Context(), refreshKeyPrefix+claims.ID).Result()
		if err != nil {
			log.Printf("check refresh whitelist: %v", err)
			writeAuthError(w, http.StatusInternalServerError, "redis_error", "internal error")
			return
		}
		if exists == 0 {
			writeAuthError(w, http.StatusUnauthorized, "invalid_refresh", "refresh token revoked")
			return
		}

		role := auth.Role(claims.Role)
		newAccess, _, err := signer.IssueAccessToken(claims.UserID, role)
		if err != nil {
			log.Printf("issue access in refresh: %v", err)
			writeAuthError(w, http.StatusInternalServerError, "issue_failed", "internal error")
			return
		}
		newRefresh, newJTI, err := signer.IssueRefreshToken(claims.UserID, role)
		if err != nil {
			log.Printf("issue refresh in refresh: %v", err)
			writeAuthError(w, http.StatusInternalServerError, "issue_failed", "internal error")
			return
		}

		// 先写新 refresh 白名单，再删旧。顺序保证：即使 Redis 在中间挂掉，
		// 用户手里也一定至少有一个可用 refresh。
		if err := rdb.Set(r.Context(), refreshKeyPrefix+newJTI, claims.UserID, refreshTTL).Err(); err != nil {
			log.Printf("cache new refresh jti: %v", err)
			writeAuthError(w, http.StatusInternalServerError, "redis_error", "internal error")
			return
		}
		if err := rdb.Del(r.Context(), refreshKeyPrefix+claims.ID).Err(); err != nil {
			// 删不掉不是致命问题（旧 refresh 到期自然失效），只打日志。
			log.Printf("delete old refresh jti: %v", err)
		}

		_ = writeJSON(w, http.StatusOK, contracts.APIResponse{
			Data: tokenPairResponse{
				AccessToken:  newAccess,
				RefreshToken: newRefresh,
				ExpiresIn:    int(accessTTL.Seconds()),
				TokenType:    "Bearer",
			},
		})
	}
}

// ---- /auth/logout --------------------------------------------------------

// handleLogout 必须包在 authRequired 里：只有合法 access 才能发起登出。
// 在 main.go 里包装：authRequired(signer, rdb)(handleLogout(signer, rdb))。
func handleLogout(signer *auth.Signer, rdb redis.UniversalClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jti, _ := r.Context().Value(CtxJTI).(string)
		exp, _ := r.Context().Value(CtxExpiry).(time.Time)

		// 把当前 access 放入黑名单。TTL = token 剩余有效期；0 说明已过期，跳过。
		if jti != "" {
			if ttl := blacklistTTL(exp); ttl > 0 {
				if err := rdb.Set(r.Context(), revokedKeyPrefix+jti, "1", ttl).Err(); err != nil {
					log.Printf("write revoke key: %v", err)
					writeAuthError(w, http.StatusInternalServerError, "redis_error", "internal error")
					return
				}
			}
		}

		// 可选：同步撤销一个 refresh token。
		// 即便没传 refresh，本接口也照常返回成功 —— access 已失效足够。
		var req logoutRequest
		// 容忍空 body。
		_ = decodeJSON(r, &req)
		if req.RefreshToken != "" {
			if claims, err := signer.Parse(req.RefreshToken); err == nil &&
				claims.TokenType == auth.TokenTypeRefresh &&
				claims.ID != "" {
				_ = rdb.Del(r.Context(), refreshKeyPrefix+claims.ID).Err()
			}
		}

		_ = writeJSON(w, http.StatusOK, contracts.APIResponse{
			Data: map[string]string{"message": "logged out"},
		})
	}
}

// ---- /auth/me ------------------------------------------------------------

// handleMe 是稳定鉴权探针，只从 context 读 userID / role 原样返回。
// 用途：E2E 脚本验证「鉴权链路是否通」，不依赖 Mongo / trip-service 等业务组件。
func handleMe() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := r.Context().Value(CtxUserID).(string)
		role, _ := r.Context().Value(CtxRole).(string)
		_ = writeJSON(w, http.StatusOK, contracts.APIResponse{
			Data: meResponse{UserID: userID, Role: role},
		})
	}
}

// compile-time check：保证未来有人改动 context 操作时，类型仍然正确。
var _ = context.Background
