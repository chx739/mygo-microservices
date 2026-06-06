package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestAllowTripStartByRateLimit_Basic(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	ctx := context.Background()
	userID := "rider_1"

	// 窗口内前 tripStartRateLimitLimit 次应放行（限流已放宽到 5 次/10s）。
	for i := 0; i < tripStartRateLimitLimit; i++ {
		allowed, err := allowTripStartByRateLimit(ctx, rdb, userID)
		if err != nil {
			t.Fatalf("request %d failed: %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("request %d should be allowed (within limit %d)", i+1, tripStartRateLimitLimit)
		}
	}

	// 第 limit+1 次应被滑动窗口拦截。
	allowed, err := allowTripStartByRateLimit(ctx, rdb, userID)
	if err != nil {
		t.Fatalf("over-limit request failed: %v", err)
	}
	if allowed {
		t.Fatalf("request %d should be blocked by sliding window", tripStartRateLimitLimit+1)
	}

	mr.FastForward(tripStartRateLimitWindow + 20*time.Millisecond)

	allowed, err = allowTripStartByRateLimit(ctx, rdb, userID)
	if err != nil {
		t.Fatalf("post-window request failed: %v", err)
	}
	if !allowed {
		t.Fatalf("request should be allowed after window expires")
	}
}

func TestAllowTripStartByRateLimit_InvalidInput(t *testing.T) {
	if allowed, err := allowTripStartByRateLimit(context.Background(), nil, "rider_1"); err == nil || allowed {
		t.Fatalf("expected error when redis client is nil")
	}
}

// 原 TestAcquireTripStartCreateLock_* 两个用例已随 SET NX EX 幂等锁一并移除：
// 该锁按 userID 实现，与上面的滑动窗口限流功能重叠（冗余），已删除生产代码。
