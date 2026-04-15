package messaging

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		_ = rdb.Close()
		mr.Close()
	})

	return rdb
}

func TestClaimMessageForProcessing_DoneThenDuplicate(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	msg := amqp.Delivery{MessageId: "msg-1", RoutingKey: "rk", Body: []byte("hello")}

	claim, err := ClaimMessageForProcessing(ctx, rdb, "trip-service:driver", msg, 10*time.Minute)
	if err != nil {
		t.Fatalf("claim message failed: %v", err)
	}
	if claim.AlreadyProcessed {
		t.Fatalf("first claim should not be marked as processed")
	}

	if err := MarkMessageProcessed(ctx, rdb, claim.Key, 10*time.Minute); err != nil {
		t.Fatalf("mark processed failed: %v", err)
	}

	duplicateClaim, err := ClaimMessageForProcessing(ctx, rdb, "trip-service:driver", msg, 10*time.Minute)
	if err != nil {
		t.Fatalf("duplicate claim should not error: %v", err)
	}
	if !duplicateClaim.AlreadyProcessed {
		t.Fatalf("duplicate claim should be recognized as already processed")
	}
}

func TestClaimMessageForProcessing_ProcessingStateReturnsError(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	msg := amqp.Delivery{MessageId: "msg-2", RoutingKey: "rk", Body: []byte("hello")}

	firstClaim, err := ClaimMessageForProcessing(ctx, rdb, "trip-service:payment", msg, 10*time.Minute)
	if err != nil {
		t.Fatalf("first claim failed: %v", err)
	}
	if firstClaim.AlreadyProcessed {
		t.Fatalf("first claim should be processing state")
	}

	if _, err := ClaimMessageForProcessing(ctx, rdb, "trip-service:payment", msg, 10*time.Minute); err == nil {
		t.Fatalf("second claim in processing state should return error")
	}

	if err := ReleaseMessageProcessing(ctx, rdb, firstClaim.Key); err != nil {
		t.Fatalf("release processing failed: %v", err)
	}
}

func TestClaimMessageForProcessing_FallbackMessageID(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()

	msgWithoutID := amqp.Delivery{
		MessageId:  "",
		RoutingKey: "payment.event.success",
		Body:       []byte(`{"tripID":"t-1"}`),
	}

	claim, err := ClaimMessageForProcessing(ctx, rdb, "trip-service:fallback", msgWithoutID, 10*time.Minute)
	if err != nil {
		t.Fatalf("first fallback claim failed: %v", err)
	}

	if err := MarkMessageProcessed(ctx, rdb, claim.Key, 10*time.Minute); err != nil {
		t.Fatalf("mark processed failed: %v", err)
	}

	duplicateClaim, err := ClaimMessageForProcessing(ctx, rdb, "trip-service:fallback", msgWithoutID, 10*time.Minute)
	if err != nil {
		t.Fatalf("duplicate fallback claim failed: %v", err)
	}
	if !duplicateClaim.AlreadyProcessed {
		t.Fatalf("fallback message id should still dedupe correctly")
	}
}
