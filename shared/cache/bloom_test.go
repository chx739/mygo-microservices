package cache

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestBloomAddAndExists(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	ctx := context.Background()
	filterKey := "bloom:test"

	item := "trip_001"

	exists, err := BloomExists(ctx, rdb, filterKey, item)
	if err != nil {
		t.Fatalf("BloomExists before add failed: %v", err)
	}
	if exists {
		t.Fatalf("item should not exist before BloomAdd")
	}

	if err := BloomAdd(ctx, rdb, filterKey, item); err != nil {
		t.Fatalf("BloomAdd failed: %v", err)
	}

	exists, err = BloomExists(ctx, rdb, filterKey, item)
	if err != nil {
		t.Fatalf("BloomExists after add failed: %v", err)
	}
	if !exists {
		t.Fatalf("item should exist after BloomAdd")
	}
}

func TestBloomInputValidation(t *testing.T) {
	ctx := context.Background()

	if err := BloomAdd(ctx, nil, "bloom:test", "trip"); err == nil {
		t.Fatalf("expected error when redis client is nil")
	}
	if _, err := BloomExists(ctx, nil, "bloom:test", "trip"); err == nil {
		t.Fatalf("expected error when redis client is nil")
	}

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	if err := BloomAdd(ctx, rdb, "", "trip"); err == nil {
		t.Fatalf("expected error when filterKey is empty")
	}
	if _, err := BloomExists(ctx, rdb, "", "trip"); err == nil {
		t.Fatalf("expected error when filterKey is empty")
	}

	if err := BloomAdd(ctx, rdb, "bloom:test", ""); err == nil {
		t.Fatalf("expected error when item is empty")
	}
	if _, err := BloomExists(ctx, rdb, "bloom:test", ""); err == nil {
		t.Fatalf("expected error when item is empty")
	}
}
