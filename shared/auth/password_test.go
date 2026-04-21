// shared/auth/password_test.go
// HashPassword / VerifyPassword 的单元测试。
// 覆盖方案 §5 测试层级 1 的 4 个用例。
package auth

import (
	"errors"
	"testing"
)

func TestHashPassword_Basic(t *testing.T) {
	t.Parallel()

	hashed, err := HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hashed == "" || hashed == "s3cret!" {
		t.Fatalf("hash must be non-empty and differ from plaintext, got %q", hashed)
	}
	if err := VerifyPassword(hashed, "s3cret!"); err != nil {
		t.Fatalf("verify happy-path: %v", err)
	}
}

func TestHashPassword_WrongPassword(t *testing.T) {
	t.Parallel()

	hashed, err := HashPassword("correct-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := VerifyPassword(hashed, "WRONG"); err == nil {
		t.Fatalf("verify should fail on wrong password")
	}
}

func TestHashPassword_EmptyInput(t *testing.T) {
	t.Parallel()

	if _, err := HashPassword(""); !errors.Is(err, ErrEmptyPassword) {
		t.Fatalf("expected ErrEmptyPassword from HashPassword(\"\"), got %v", err)
	}
	// Verify 空明文也直接拒绝，不走 bcrypt，避免泄露时序信息。
	if err := VerifyPassword("anything", ""); !errors.Is(err, ErrEmptyPassword) {
		t.Fatalf("expected ErrEmptyPassword from VerifyPassword(_, \"\"), got %v", err)
	}
}

// TestHashPassword_Determinism：同一明文两次 hash 不应相等（bcrypt 每次自带随机盐）。
func TestHashPassword_Determinism(t *testing.T) {
	t.Parallel()

	h1, err := HashPassword("same-input")
	if err != nil {
		t.Fatalf("hash #1: %v", err)
	}
	h2, err := HashPassword("same-input")
	if err != nil {
		t.Fatalf("hash #2: %v", err)
	}
	if h1 == h2 {
		t.Fatalf("two hashes of the same plaintext should differ (salt); got identical %q", h1)
	}
	// 但两者都必须能 verify 通过。
	if err := VerifyPassword(h1, "same-input"); err != nil {
		t.Fatalf("verify h1: %v", err)
	}
	if err := VerifyPassword(h2, "same-input"); err != nil {
		t.Fatalf("verify h2: %v", err)
	}
}
