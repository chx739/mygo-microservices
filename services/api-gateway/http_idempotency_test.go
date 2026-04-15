package main

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestMarkStripeWebhookEventProcessed_FirstThenDuplicate(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	ok, err := markStripeWebhookEventProcessed(context.Background(), rdb, "evt_1")
	if err != nil {
		t.Fatalf("first mark failed: %v", err)
	}
	if !ok {
		t.Fatalf("first webhook event should be marked as new")
	}

	ok, err = markStripeWebhookEventProcessed(context.Background(), rdb, "evt_1")
	if err != nil {
		t.Fatalf("second mark failed: %v", err)
	}
	if ok {
		t.Fatalf("duplicate webhook event should be detected")
	}
}

func TestMarkStripeWebhookEventProcessed_InvalidInput(t *testing.T) {
	if ok, err := markStripeWebhookEventProcessed(context.Background(), nil, "evt_x"); err == nil || ok {
		t.Fatalf("expected error when redis client is nil")
	}

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	if ok, err := markStripeWebhookEventProcessed(context.Background(), rdb, ""); err == nil || ok {
		t.Fatalf("expected error when event id is empty")
	}
}
