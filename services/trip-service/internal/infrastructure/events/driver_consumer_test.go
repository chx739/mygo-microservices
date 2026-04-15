package events

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestTryAcquireTripAcceptLock_FirstWins(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	consumer := &driverConsumer{redis: rdb}

	ok, err := consumer.tryAcquireTripAcceptLock(context.Background(), "trip-1", "driver-a")
	if err != nil {
		t.Fatalf("first lock acquire failed: %v", err)
	}
	if !ok {
		t.Fatalf("first lock acquire should succeed")
	}

	ok, err = consumer.tryAcquireTripAcceptLock(context.Background(), "trip-1", "driver-b")
	if err != nil {
		t.Fatalf("second lock acquire failed: %v", err)
	}
	if ok {
		t.Fatalf("second lock acquire should fail because lock already held")
	}

	value, err := mr.Get(tripAcceptLockKey("trip-1"))
	if err != nil {
		t.Fatalf("read lock value failed: %v", err)
	}
	if value != "driver-a" {
		t.Fatalf("expected lock owner driver-a, got %s", value)
	}
}

func TestTryAcquireTripAcceptLock_InvalidInput(t *testing.T) {
	consumer := &driverConsumer{}

	if ok, err := consumer.tryAcquireTripAcceptLock(context.Background(), "trip-1", "driver-a"); err == nil || ok {
		t.Fatalf("expected redis required error when redis client is nil")
	}

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	consumer.redis = rdb

	if ok, err := consumer.tryAcquireTripAcceptLock(context.Background(), "", "driver-a"); err == nil || ok {
		t.Fatalf("expected tripID required error when tripID is empty")
	}

	if ok, err := consumer.tryAcquireTripAcceptLock(context.Background(), "trip-1", ""); err == nil || ok {
		t.Fatalf("expected driverID required error when driverID is empty")
	}
}

func TestReleaseTripAcceptLock_OnlyOwnerCanRelease(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	consumer := &driverConsumer{redis: rdb}

	locked, err := consumer.tryAcquireTripAcceptLock(context.Background(), "trip-2", "driver-a")
	if err != nil {
		t.Fatalf("lock acquire failed: %v", err)
	}
	if !locked {
		t.Fatalf("expected lock acquire success")
	}

	// 非 owner 尝试释放，Lua 脚本应拒绝删除。
	released, err := consumer.releaseTripAcceptLock(context.Background(), "trip-2", "driver-b")
	if err != nil {
		t.Fatalf("non-owner release returned error: %v", err)
	}
	if released {
		t.Fatalf("non-owner should not release lock")
	}

	value, err := mr.Get(tripAcceptLockKey("trip-2"))
	if err != nil {
		t.Fatalf("lock should still exist after non-owner release attempt: %v", err)
	}
	if value != "driver-a" {
		t.Fatalf("expected lock owner driver-a, got %s", value)
	}

	// owner 释放，Lua 脚本应原子删除。
	released, err = consumer.releaseTripAcceptLock(context.Background(), "trip-2", "driver-a")
	if err != nil {
		t.Fatalf("owner release returned error: %v", err)
	}
	if !released {
		t.Fatalf("owner should release lock successfully")
	}

	if mr.Exists(tripAcceptLockKey("trip-2")) {
		t.Fatalf("lock key should be deleted after owner release")
	}
}

func TestReleaseTripAcceptLock_InvalidInput(t *testing.T) {
	consumer := &driverConsumer{}

	if released, err := consumer.releaseTripAcceptLock(context.Background(), "trip-3", "driver-a"); err == nil || released {
		t.Fatalf("expected redis required error when redis client is nil")
	}

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	consumer.redis = rdb

	if released, err := consumer.releaseTripAcceptLock(context.Background(), "", "driver-a"); err == nil || released {
		t.Fatalf("expected tripID required error when tripID is empty")
	}

	if released, err := consumer.releaseTripAcceptLock(context.Background(), "trip-3", ""); err == nil || released {
		t.Fatalf("expected driverID required error when driverID is empty")
	}
}
