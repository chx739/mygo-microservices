// shared/auth/jwt_test.go
// Signer 的单元测试。
// 覆盖方案 §5 测试层级 1 的全部 7 个用例 + 算法混淆防御。
package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testSecret 只在测试里用；生产通过 env 注入。
var testSecret = []byte("test-secret-at-least-32-bytes-long-xx")

// newTestSigner 构造一个通用 signer，TTL 给足够长以免影响「签发→立刻 Parse」的快路径。
func newTestSigner(t *testing.T) *Signer {
	t.Helper()
	return NewSigner(testSecret, "test-issuer", 10*time.Minute, 24*time.Hour)
}

func TestSigner_IssueAndParse_HappyPath(t *testing.T) {
	t.Parallel()

	s := newTestSigner(t)
	tok, jti, err := s.IssueAccessToken("user-123", RoleRider)
	if err != nil {
		t.Fatalf("issue access token: %v", err)
	}
	if tok == "" || jti == "" {
		t.Fatalf("token and jti must be non-empty, got tok=%q jti=%q", tok, jti)
	}

	claims, err := s.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.UserID != "user-123" {
		t.Fatalf("UserID mismatch: got %q", claims.UserID)
	}
	if claims.Role != RoleRider {
		t.Fatalf("Role mismatch: got %q", claims.Role)
	}
	if claims.TokenType != TokenTypeAccess {
		t.Fatalf("TokenType mismatch: got %q", claims.TokenType)
	}
	if claims.ID != jti {
		t.Fatalf("jti mismatch: got %q, want %q", claims.ID, jti)
	}
	if claims.Issuer != "test-issuer" {
		t.Fatalf("issuer mismatch: got %q", claims.Issuer)
	}
}

func TestSigner_ExpiredToken(t *testing.T) {
	t.Parallel()

	// 故意给一个极短的 accessTTL，让 token 几乎签发即过期。
	// 叠加 clockSkew (30s)，需要等 >30s 才会真正过期 —— 不现实。
	// 所以这里直接构造一个「签发时间在过去」的 claims，绕过 clockSkew。
	s := newTestSigner(t)

	// 手工签发一个 exp 在过去 (now - 2*clockSkew) 的 token。
	past := time.Now().Add(-2 * clockSkew).Add(-time.Minute)
	claims := Claims{
		UserID:    "u1",
		Role:      RoleRider,
		TokenType: TokenTypeAccess,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   "u1",
			ID:        "jti-expired",
			IssuedAt:  jwt.NewNumericDate(past),
			NotBefore: jwt.NewNumericDate(past),
			ExpiresAt: jwt.NewNumericDate(past.Add(time.Second)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	_, err = s.Parse(signed)
	if err == nil {
		t.Fatalf("expected parse to fail on expired token")
	}
	if !errors.Is(err, jwt.ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired, got: %v", err)
	}
}

func TestSigner_InvalidSignature(t *testing.T) {
	t.Parallel()

	signerA := NewSigner([]byte("secret-A-at-least-32-bytes-long-xxxx"), "iss", time.Minute, time.Hour)
	signerB := NewSigner([]byte("secret-B-at-least-32-bytes-long-xxxx"), "iss", time.Minute, time.Hour)

	tok, _, err := signerA.IssueAccessToken("u", RoleRider)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	_, err = signerB.Parse(tok)
	if err == nil {
		t.Fatalf("expected parse failure due to bad signature")
	}
	if !errors.Is(err, jwt.ErrSignatureInvalid) {
		t.Fatalf("expected ErrSignatureInvalid, got: %v", err)
	}
}

func TestSigner_WrongIssuer(t *testing.T) {
	t.Parallel()

	issuerA := NewSigner(testSecret, "issuer-A", time.Minute, time.Hour)
	issuerB := NewSigner(testSecret, "issuer-B", time.Minute, time.Hour)

	tok, _, err := issuerA.IssueAccessToken("u", RoleRider)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// 密钥相同 + issuer 不同 → 必须被 Parse 拒绝。
	_, err = issuerB.Parse(tok)
	if err == nil {
		t.Fatalf("expected parse failure due to issuer mismatch")
	}
}

func TestSigner_TokenType_Differentiated(t *testing.T) {
	t.Parallel()

	s := newTestSigner(t)
	accessTok, _, err := s.IssueAccessToken("u", RoleRider)
	if err != nil {
		t.Fatalf("issue access: %v", err)
	}
	refreshTok, _, err := s.IssueRefreshToken("u", RoleRider)
	if err != nil {
		t.Fatalf("issue refresh: %v", err)
	}

	ac, err := s.Parse(accessTok)
	if err != nil {
		t.Fatalf("parse access: %v", err)
	}
	rc, err := s.Parse(refreshTok)
	if err != nil {
		t.Fatalf("parse refresh: %v", err)
	}

	if ac.TokenType != TokenTypeAccess {
		t.Fatalf("access TokenType mismatch: %q", ac.TokenType)
	}
	if rc.TokenType != TokenTypeRefresh {
		t.Fatalf("refresh TokenType mismatch: %q", rc.TokenType)
	}
	if ac.TokenType == rc.TokenType {
		t.Fatalf("access/refresh token types should differ")
	}
}

func TestSigner_JTI_Unique(t *testing.T) {
	t.Parallel()

	s := newTestSigner(t)
	seen := make(map[string]struct{}, 100)
	for i := range 100 {
		_, jti, err := s.IssueAccessToken("u", RoleRider)
		if err != nil {
			t.Fatalf("issue #%d: %v", i, err)
		}
		if _, dup := seen[jti]; dup {
			t.Fatalf("duplicate jti detected at #%d: %q", i, jti)
		}
		seen[jti] = struct{}{}
	}
	if len(seen) != 100 {
		t.Fatalf("expected 100 unique jti, got %d", len(seen))
	}
}

// TestSigner_AlgorithmConfusion_None 验证：
// 若攻击者把 header 的 alg 改成 "none"，Parse 必须拒绝。
func TestSigner_AlgorithmConfusion_None(t *testing.T) {
	t.Parallel()

	s := newTestSigner(t)

	// 手工构造一个 alg=none 的 "token"：header.payload.（空签名）。
	// 格式：base64url(headerJSON) + "." + base64url(payloadJSON) + "."
	//
	// 这里用 jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)。
	claims := Claims{
		UserID:    "attacker",
		Role:      RoleRider,
		TokenType: TokenTypeAccess,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   "attacker",
			ID:        "fake-jti",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
		},
	}
	noneTok, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("craft none-alg token: %v", err)
	}

	_, err = s.Parse(noneTok)
	if err == nil {
		t.Fatalf("Parse must reject alg=none token")
	}
	// 错误信息里应体现算法问题；不强校验具体 sentinel。
	if !strings.Contains(err.Error(), "signing method") &&
		!errors.Is(err, jwt.ErrTokenSignatureInvalid) &&
		!errors.Is(err, jwt.ErrTokenUnverifiable) {
		t.Fatalf("expected algorithm-related error, got: %v", err)
	}
}

// TestSigner_IssueEmptyUserID 验证：签发空 userID 必须报错。
// 不在方案列表里，但属于「显而易见」的输入防御。
func TestSigner_IssueEmptyUserID(t *testing.T) {
	t.Parallel()

	s := newTestSigner(t)
	if _, _, err := s.IssueAccessToken("", RoleRider); err == nil {
		t.Fatalf("expected error for empty userID")
	}
}

func TestSigner_TTLAccessors(t *testing.T) {
	t.Parallel()
	access := 7 * time.Minute
	refresh := 72 * time.Hour
	s := NewSigner(testSecret, "ttl-test", access, refresh)
	if s.AccessTTL() != access {
		t.Fatalf("AccessTTL mismatch: got %v want %v", s.AccessTTL(), access)
	}
	if s.RefreshTTL() != refresh {
		t.Fatalf("RefreshTTL mismatch: got %v want %v", s.RefreshTTL(), refresh)
	}
}

func TestRole_IsValid(t *testing.T) {
	t.Parallel()
	cases := map[Role]bool{
		RoleRider:  true,
		RoleDriver: true,
		"admin":    false,
		"":         false,
	}
	for r, want := range cases {
		if got := r.IsValid(); got != want {
			t.Fatalf("Role(%q).IsValid(): got %v want %v", r, got, want)
		}
	}
}
