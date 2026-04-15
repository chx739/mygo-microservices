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

	allowed, err := allowTripStartByRateLimit(ctx, rdb, userID)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	if !allowed {
		t.Fatalf("first request should be allowed")
	}

	allowed, err = allowTripStartByRateLimit(ctx, rdb, userID)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	if allowed {
		t.Fatalf("second request should be blocked by sliding window")
	}

	mr.FastForward(tripStartRateLimitWindow + 20*time.Millisecond)

	allowed, err = allowTripStartByRateLimit(ctx, rdb, userID)
	if err != nil {
		t.Fatalf("third request failed: %v", err)
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

func TestAcquireTripStartCreateLock_Basic(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	ctx := context.Background()
	userID := "rider_2"

	ok, err := acquireTripStartCreateLock(ctx, rdb, userID)
	if err != nil {
		t.Fatalf("first lock failed: %v", err)
	}
	if !ok {
		t.Fatalf("first lock should be acquired")
	}

	ok, err = acquireTripStartCreateLock(ctx, rdb, userID)
	if err != nil {
		t.Fatalf("second lock failed: %v", err)
	}
	if ok {
		t.Fatalf("second lock should be rejected within lock ttl")
	}

	mr.FastForward(tripStartIdempotencyLockTTL + 20*time.Millisecond)

	ok, err = acquireTripStartCreateLock(ctx, rdb, userID)
	if err != nil {
		t.Fatalf("third lock failed: %v", err)
	}
	if !ok {
		t.Fatalf("lock should be acquirable after ttl expires")
	}
}

func TestAcquireTripStartCreateLock_InvalidInput(t *testing.T) {
	if ok, err := acquireTripStartCreateLock(context.Background(), nil, "rider_3"); err == nil || ok {
		t.Fatalf("expected error when redis client is nil")
	}

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	if ok, err := acquireTripStartCreateLock(context.Background(), rdb, ""); err == nil || ok {
		t.Fatalf("expected error when userID is empty")
	}
}
