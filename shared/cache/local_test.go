package cache

import (
	"testing"
	"time"
)

func newTestLocalCache(t *testing.T) *LocalCache {
	t.Helper()
	lc, err := NewLocalCache(1000, 1<<20)
	if err != nil {
		t.Fatalf("NewLocalCache failed: %v", err)
	}
	t.Cleanup(lc.Close)
	return lc
}

func TestLocalCache_SetThenGet(t *testing.T) {
	lc := newTestLocalCache(t)

	if !lc.Set("foo", []byte("bar"), time.Second) {
		t.Fatalf("Set returned false; ristretto refused admission")
	}
	lc.Wait()

	got, ok := lc.Get("foo")
	if !ok {
		t.Fatalf("expected hit after Set+Wait, got miss")
	}
	if string(got) != "bar" {
		t.Fatalf("expected value 'bar', got %q", got)
	}
}

func TestLocalCache_TTLExpires(t *testing.T) {
	lc := newTestLocalCache(t)

	if !lc.Set("k", []byte("v"), 50*time.Millisecond) {
		t.Fatalf("Set returned false")
	}
	lc.Wait()

	if _, ok := lc.Get("k"); !ok {
		t.Fatalf("expected immediate hit")
	}

	// 等待 TTL 过期；ristretto 默认 500ms 清扫一次，给一个宽裕窗口。
	time.Sleep(800 * time.Millisecond)
	if v, ok := lc.Get("k"); ok {
		t.Fatalf("expected miss after TTL expiry, got hit with %q", v)
	}
}

func TestLocalCache_Del(t *testing.T) {
	lc := newTestLocalCache(t)

	lc.Set("k", []byte("v"), time.Minute)
	lc.Wait()

	if _, ok := lc.Get("k"); !ok {
		t.Fatalf("expected hit before Del")
	}

	lc.Del("k")
	// ristretto Del 也走异步 buffer，需要 Wait。
	lc.Wait()

	if v, ok := lc.Get("k"); ok {
		t.Fatalf("expected miss after Del, got %q", v)
	}
}

func TestLocalCache_NilSafe(t *testing.T) {
	var lc *LocalCache // 显式 nil receiver

	if v, ok := lc.Get("k"); ok || v != nil {
		t.Fatalf("nil Get expected (nil,false), got (%v,%v)", v, ok)
	}
	if ok := lc.Set("k", []byte("v"), time.Second); ok {
		t.Fatalf("nil Set expected false, got true")
	}
	// 下面两个不会 panic 就算过。
	lc.Del("k")
	lc.Wait()
	lc.Close()
}
