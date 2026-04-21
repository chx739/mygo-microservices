package main

// auth_handler_test.go 覆盖 /auth/* 的 5 个 handler。
//
// 设计要点：
//  1. 用内存 fake repo（fakeUserRepo）替代 Mongo —— handler 层测试只关心 UserRepository
//     接口契约，不需要真实数据库。真实 Mongo 的测试放在 user_repo_test.go。
//  2. Redis 用 miniredis，与 auth_middleware_test.go / http_rate_limit_test.go 一致。
//  3. 每个测试独立 fake repo + miniredis，避免串数据；全部 t.Parallel()。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ride-sharing/shared/auth"
	"ride-sharing/shared/contracts"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ---- fakeUserRepo：内存实现，仅用于本文件 -----------------------------------

type fakeUserRepo struct {
	mu        sync.Mutex
	byPhone   map[string]*UserDoc
	byUUID    map[string]*UserDoc
	createErr error // 注入 repo 非 ErrPhoneTaken 错误以覆盖 500 路径（当前未启用）
}

func newFakeUserRepo() *fakeUserRepo {
	return &fakeUserRepo{
		byPhone: make(map[string]*UserDoc),
		byUUID:  make(map[string]*UserDoc),
	}
}

func (f *fakeUserRepo) Create(_ context.Context, u *UserDoc) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	if _, ok := f.byPhone[u.Phone]; ok {
		return ErrPhoneTaken
	}
	if u.UUID == "" {
		u.UUID = uuid.NewString()
	}
	now := time.Now().UTC()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = now
	}
	cp := *u
	f.byPhone[u.Phone] = &cp
	f.byUUID[u.UUID] = &cp
	return nil
}

func (f *fakeUserRepo) FindByPhone(_ context.Context, phone string) (*UserDoc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byPhone[phone]
	if !ok {
		return nil, ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (f *fakeUserRepo) FindByUUID(_ context.Context, id string) (*UserDoc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byUUID[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

// ---- 测试基础设施 ---------------------------------------------------------

var hTestSecret = []byte("test-secret-at-least-32-bytes-long-xx")

type handlerEnv struct {
	repo       *fakeUserRepo
	signer     *auth.Signer
	mr         *miniredis.Miniredis
	rdb        redis.UniversalClient
	accessTTL  time.Duration
	refreshTTL time.Duration
}

func setupHandler(t *testing.T) *handlerEnv {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	accessTTL := 5 * time.Minute
	refreshTTL := 24 * time.Hour
	signer := auth.NewSigner(hTestSecret, "handler-test", accessTTL, refreshTTL)
	t.Cleanup(func() {
		_ = rdb.Close()
		mr.Close()
	})
	return &handlerEnv{
		repo:       newFakeUserRepo(),
		signer:     signer,
		mr:         mr,
		rdb:        rdb,
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
	}
}

func doJSON(t *testing.T, h http.HandlerFunc, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// decodeAPI 从响应体还原 APIResponse，data 留给调用方再 Unmarshal。
func decodeAPI(t *testing.T, rec *httptest.ResponseRecorder) contracts.APIResponse {
	t.Helper()
	var resp contracts.APIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v, body=%s", err, rec.Body.String())
	}
	return resp
}

// mustRegister 创建一个注册好的用户，返回 UUID（便于登录测试复用）。
func mustRegister(t *testing.T, env *handlerEnv, phone, password string, role auth.Role) string {
	t.Helper()
	h := handleRegister(env.repo)
	rec := doJSON(t, h, http.MethodPost, "/auth/register", registerRequest{
		Phone: phone, Password: password, Role: string(role),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d, body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeAPI(t, rec)
	raw, _ := json.Marshal(resp.Data)
	var out registerResponse
	_ = json.Unmarshal(raw, &out)
	if out.UserID == "" {
		t.Fatalf("register: empty userID")
	}
	return out.UserID
}

// ---- /auth/register ------------------------------------------------------

func TestHandleRegister_Happy(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	uid := mustRegister(t, env, "13800138000", "password123", auth.RoleRider)
	if got, _ := env.repo.FindByPhone(context.Background(), "13800138000"); got == nil || got.UUID != uid {
		t.Fatalf("user not persisted: %+v", got)
	}
}

func TestHandleRegister_InvalidPhone(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	h := handleRegister(env.repo)

	cases := []struct {
		name string
		body registerRequest
	}{
		{"too short", registerRequest{Phone: "123", Password: "password123", Role: "rider"}},
		{"too long", registerRequest{Phone: strings.Repeat("1", 21), Password: "password123", Role: "rider"}},
		{"empty", registerRequest{Phone: "", Password: "password123", Role: "rider"}},
	}
	for _, c := range cases {
		rec := doJSON(t, h, http.MethodPost, "/auth/register", c.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d", c.name, rec.Code)
		}
		resp := decodeAPI(t, rec)
		if resp.Error == nil || resp.Error.Code != "invalid_phone" {
			t.Fatalf("%s: expected code invalid_phone, got %+v", c.name, resp.Error)
		}
	}
}

func TestHandleRegister_WeakPassword(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	h := handleRegister(env.repo)
	rec := doJSON(t, h, http.MethodPost, "/auth/register", registerRequest{
		Phone: "13800138001", Password: "123", Role: "rider",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	resp := decodeAPI(t, rec)
	if resp.Error == nil || resp.Error.Code != "weak_password" {
		t.Fatalf("expected code weak_password, got %+v", resp.Error)
	}
}

func TestHandleRegister_InvalidRole(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	h := handleRegister(env.repo)
	rec := doJSON(t, h, http.MethodPost, "/auth/register", registerRequest{
		Phone: "13800138002", Password: "password123", Role: "admin",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	resp := decodeAPI(t, rec)
	if resp.Error == nil || resp.Error.Code != "invalid_role" {
		t.Fatalf("expected code invalid_role, got %+v", resp.Error)
	}
}

func TestHandleRegister_DuplicatePhone(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	mustRegister(t, env, "13800138003", "password123", auth.RoleRider)

	h := handleRegister(env.repo)
	rec := doJSON(t, h, http.MethodPost, "/auth/register", registerRequest{
		Phone: "13800138003", Password: "password123", Role: "rider",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
	resp := decodeAPI(t, rec)
	if resp.Error == nil || resp.Error.Code != "phone_taken" {
		t.Fatalf("expected code phone_taken, got %+v", resp.Error)
	}
}

func TestHandleRegister_BadJSON(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	h := handleRegister(env.repo)
	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader("{bad json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// ---- /auth/login ---------------------------------------------------------

func TestHandleLogin_Happy(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	mustRegister(t, env, "13800138010", "password123", auth.RoleRider)

	h := handleLogin(env.repo, env.signer, env.rdb, env.accessTTL, env.refreshTTL)
	rec := doJSON(t, h, http.MethodPost, "/auth/login", loginRequest{
		Phone: "13800138010", Password: "password123",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeAPI(t, rec)
	raw, _ := json.Marshal(resp.Data)
	var pair tokenPairResponse
	if err := json.Unmarshal(raw, &pair); err != nil {
		t.Fatalf("unmarshal pair: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatalf("missing tokens: %+v", pair)
	}
	if pair.TokenType != "Bearer" {
		t.Fatalf("token_type should be Bearer, got %q", pair.TokenType)
	}
	if pair.ExpiresIn != int(env.accessTTL.Seconds()) {
		t.Fatalf("expires_in should equal accessTTL seconds: got %d want %d",
			pair.ExpiresIn, int(env.accessTTL.Seconds()))
	}
	// 校验 refresh 写入白名单。
	claims, err := env.signer.Parse(pair.RefreshToken)
	if err != nil {
		t.Fatalf("parse refresh: %v", err)
	}
	if !env.mr.Exists(refreshKeyPrefix + claims.ID) {
		t.Fatalf("refresh jti should be in whitelist")
	}
}

func TestHandleLogin_WrongPassword(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	mustRegister(t, env, "13800138011", "password123", auth.RoleRider)

	h := handleLogin(env.repo, env.signer, env.rdb, env.accessTTL, env.refreshTTL)
	rec := doJSON(t, h, http.MethodPost, "/auth/login", loginRequest{
		Phone: "13800138011", Password: "wrong-pass",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	resp := decodeAPI(t, rec)
	if resp.Error == nil || resp.Error.Code != "invalid_credentials" {
		t.Fatalf("expected code invalid_credentials, got %+v", resp.Error)
	}
}

func TestHandleLogin_UserNotExist(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	h := handleLogin(env.repo, env.signer, env.rdb, env.accessTTL, env.refreshTTL)
	rec := doJSON(t, h, http.MethodPost, "/auth/login", loginRequest{
		Phone: "nonexistent", Password: "password123",
	})
	// 防枚举：和 wrong-password 返回同一 code / status。
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	resp := decodeAPI(t, rec)
	if resp.Error == nil || resp.Error.Code != "invalid_credentials" {
		t.Fatalf("expected code invalid_credentials, got %+v", resp.Error)
	}
}

// ---- /auth/refresh -------------------------------------------------------

func TestHandleRefresh_Happy(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	mustRegister(t, env, "13800138020", "password123", auth.RoleRider)

	// 先登录拿 refresh。
	loginH := handleLogin(env.repo, env.signer, env.rdb, env.accessTTL, env.refreshTTL)
	rec := doJSON(t, loginH, http.MethodPost, "/auth/login", loginRequest{
		Phone: "13800138020", Password: "password123",
	})
	var loginPair tokenPairResponse
	loginRaw, _ := json.Marshal(decodeAPI(t, rec).Data)
	_ = json.Unmarshal(loginRaw, &loginPair)

	oldClaims, _ := env.signer.Parse(loginPair.RefreshToken)

	refreshH := handleRefresh(env.signer, env.rdb, env.accessTTL, env.refreshTTL)
	rec = doJSON(t, refreshH, http.MethodPost, "/auth/refresh", refreshRequest{
		RefreshToken: loginPair.RefreshToken,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rec.Code, rec.Body.String())
	}
	var newPair tokenPairResponse
	newRaw, _ := json.Marshal(decodeAPI(t, rec).Data)
	_ = json.Unmarshal(newRaw, &newPair)
	if newPair.AccessToken == "" || newPair.RefreshToken == "" {
		t.Fatalf("missing new tokens")
	}
	if newPair.RefreshToken == loginPair.RefreshToken {
		t.Fatalf("refresh should rotate (new != old)")
	}

	// 旧 refresh 白名单应被删除。
	if env.mr.Exists(refreshKeyPrefix + oldClaims.ID) {
		t.Fatalf("old refresh jti should be removed from whitelist")
	}
	// 新 refresh 应写入。
	newClaims, _ := env.signer.Parse(newPair.RefreshToken)
	if !env.mr.Exists(refreshKeyPrefix + newClaims.ID) {
		t.Fatalf("new refresh jti should be in whitelist")
	}
}

func TestHandleRefresh_WithAccessToken(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	access, _, err := env.signer.IssueAccessToken("u-1", auth.RoleRider)
	if err != nil {
		t.Fatalf("issue access: %v", err)
	}
	h := handleRefresh(env.signer, env.rdb, env.accessTTL, env.refreshTTL)
	rec := doJSON(t, h, http.MethodPost, "/auth/refresh", refreshRequest{RefreshToken: access})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when using access as refresh, got %d", rec.Code)
	}
}

func TestHandleRefresh_Revoked(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	refresh, _, err := env.signer.IssueRefreshToken("u-1", auth.RoleRider)
	if err != nil {
		t.Fatalf("issue refresh: %v", err)
	}
	// 故意不写白名单 —— 模拟登出后再来刷新。
	h := handleRefresh(env.signer, env.rdb, env.accessTTL, env.refreshTTL)
	rec := doJSON(t, h, http.MethodPost, "/auth/refresh", refreshRequest{RefreshToken: refresh})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on revoked refresh, got %d", rec.Code)
	}
	resp := decodeAPI(t, rec)
	if resp.Error == nil || resp.Error.Code != "invalid_refresh" {
		t.Fatalf("expected code invalid_refresh, got %+v", resp.Error)
	}
}

func TestHandleRefresh_UsedTwice(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	mustRegister(t, env, "13800138021", "password123", auth.RoleDriver)

	loginH := handleLogin(env.repo, env.signer, env.rdb, env.accessTTL, env.refreshTTL)
	rec := doJSON(t, loginH, http.MethodPost, "/auth/login", loginRequest{
		Phone: "13800138021", Password: "password123",
	})
	var pair tokenPairResponse
	raw, _ := json.Marshal(decodeAPI(t, rec).Data)
	_ = json.Unmarshal(raw, &pair)

	refreshH := handleRefresh(env.signer, env.rdb, env.accessTTL, env.refreshTTL)
	// 第一次刷新成功。
	rec1 := doJSON(t, refreshH, http.MethodPost, "/auth/refresh", refreshRequest{RefreshToken: pair.RefreshToken})
	if rec1.Code != http.StatusOK {
		t.Fatalf("first refresh should succeed, got %d", rec1.Code)
	}
	// 第二次用同一 refresh 应失败（已被旋转消费）。
	rec2 := doJSON(t, refreshH, http.MethodPost, "/auth/refresh", refreshRequest{RefreshToken: pair.RefreshToken})
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("reuse of old refresh should fail, got %d", rec2.Code)
	}
}

// ---- /auth/logout --------------------------------------------------------

// withAuthContext 模拟 authRequired 已经跑过，把 jti/exp/userID 注入 context。
// logout handler 依赖这些 context 值，所以单测时必须手工注入。
func withAuthContext(r *http.Request, userID, role, jti string, exp time.Time) *http.Request {
	ctx := r.Context()
	ctx = context.WithValue(ctx, CtxUserID, userID)
	ctx = context.WithValue(ctx, CtxRole, role)
	ctx = context.WithValue(ctx, CtxJTI, jti)
	ctx = context.WithValue(ctx, CtxExpiry, exp)
	return r.WithContext(ctx)
}

func TestHandleLogout_Happy(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	jti := "logout-jti-1"
	exp := time.Now().Add(5 * time.Minute)

	h := handleLogout(env.signer, env.rdb)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req = withAuthContext(req, "u-1", string(auth.RoleRider), jti, exp)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !env.mr.Exists(revokedKeyPrefix + jti) {
		t.Fatalf("access jti should be in blacklist")
	}
}

func TestHandleLogout_WithRefresh(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)

	// 先签一个 refresh 并写入白名单，模拟登录后的状态。
	refreshTok, refreshJTI, err := env.signer.IssueRefreshToken("u-2", auth.RoleDriver)
	if err != nil {
		t.Fatalf("issue refresh: %v", err)
	}
	env.mr.Set(refreshKeyPrefix+refreshJTI, "u-2")

	accessJTI := "logout-jti-2"
	exp := time.Now().Add(5 * time.Minute)

	h := handleLogout(env.signer, env.rdb)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout",
		strings.NewReader(fmt.Sprintf(`{"refresh_token":%q}`, refreshTok)))
	req.Header.Set("Content-Type", "application/json")
	req = withAuthContext(req, "u-2", string(auth.RoleDriver), accessJTI, exp)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !env.mr.Exists(revokedKeyPrefix + accessJTI) {
		t.Fatalf("access jti should be in blacklist")
	}
	if env.mr.Exists(refreshKeyPrefix + refreshJTI) {
		t.Fatalf("refresh jti should be removed from whitelist")
	}
}

func TestHandleLogout_EmptyBody(t *testing.T) {
	t.Parallel()
	env := setupHandler(t)
	jti := "logout-jti-3"
	exp := time.Now().Add(5 * time.Minute)

	h := handleLogout(env.signer, env.rdb)
	// 完全空 body —— logout 允许这种情况（只撤销 access）。
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req = withAuthContext(req, "u-3", string(auth.RoleRider), jti, exp)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with empty body, got %d", rec.Code)
	}
	if !env.mr.Exists(revokedKeyPrefix + jti) {
		t.Fatalf("access jti should still be blacklisted")
	}
}

// ---- /auth/me ------------------------------------------------------------

func TestHandleMe_Happy(t *testing.T) {
	t.Parallel()

	h := handleMe()
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req = withAuthContext(req, "u-42", string(auth.RoleDriver), "me-jti", time.Now().Add(time.Minute))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	resp := decodeAPI(t, rec)
	raw, _ := json.Marshal(resp.Data)
	var me meResponse
	if err := json.Unmarshal(raw, &me); err != nil {
		t.Fatalf("unmarshal me: %v", err)
	}
	if me.UserID != "u-42" || me.Role != string(auth.RoleDriver) {
		t.Fatalf("context not propagated: %+v", me)
	}
}
