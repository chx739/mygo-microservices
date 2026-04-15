package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestSlidingWindowAllow_BasicLimit(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	ctx := context.Background()
	userID := "user_1"

	allowed, err := SlidingWindowAllow(ctx, rdb, userID, 2, 10*time.Second)
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	if !allowed {
		t.Fatalf("first request should be allowed")
	}

	allowed, err = SlidingWindowAllow(ctx, rdb, userID, 2, 10*time.Second)
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if !allowed {
		t.Fatalf("second request should be allowed")
	}

	allowed, err = SlidingWindowAllow(ctx, rdb, userID, 2, 10*time.Second)
	if err != nil {
		t.Fatalf("third call failed: %v", err)
	}
	if allowed {
		t.Fatalf("third request should be rate limited")
	}
}

func TestSlidingWindowAllow_WindowReset(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	ctx := context.Background()
	userID := "user_2"
	window := 2 * time.Second

	allowed, err := SlidingWindowAllow(ctx, rdb, userID, 1, window)
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	if !allowed {
		t.Fatalf("first request should be allowed")
	}

	allowed, err = SlidingWindowAllow(ctx, rdb, userID, 1, window)
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if allowed {
		t.Fatalf("second request should be rate limited before window expires")
	}

	mr.FastForward(window + 20*time.Millisecond)

	allowed, err = SlidingWindowAllow(ctx, rdb, userID, 1, window)
	if err != nil {
		t.Fatalf("third call failed: %v", err)
	}
	if !allowed {
		t.Fatalf("request should be allowed after window expires")
	}
}

func TestSlidingWindowAllow_InvalidInput(t *testing.T) {
	ctx := context.Background()

	if allowed, err := SlidingWindowAllow(ctx, nil, "user_x", 1, time.Second); err == nil || allowed {
		t.Fatalf("expected error when redis client is nil")
	}

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	if allowed, err := SlidingWindowAllow(ctx, rdb, "", 1, time.Second); err == nil || allowed {
		t.Fatalf("expected error when userID is empty")
	}

	if allowed, err := SlidingWindowAllow(ctx, rdb, "user_x", 0, time.Second); err == nil || allowed {
		t.Fatalf("expected error when limit is zero")
	}

	if allowed, err := SlidingWindowAllow(ctx, rdb, "user_x", 1, 0); err == nil || allowed {
		t.Fatalf("expected error when window is zero")
	}
}
