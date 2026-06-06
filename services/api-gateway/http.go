package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"ride-sharing/services/api-gateway/grpc_clients"
	"ride-sharing/shared/cache"
	"ride-sharing/shared/contracts"
	"ride-sharing/shared/env"
	"ride-sharing/shared/messaging"
	"ride-sharing/shared/tracing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/webhook"
)

var tracer = tracing.GetTracer("api-gateway")

const (
	// stripeWebhookIdempotencyTTL 控制 Stripe 事件去重窗口。
	// Stripe 可能在较长时间内重试同一事件，24h 能覆盖大部分重试场景。
	stripeWebhookIdempotencyTTL = 24 * time.Hour

	// tripStartRateLimitWindow 定义下单滑动窗口时长。
	tripStartRateLimitWindow = 10 * time.Second

	// tripStartRateLimitLimit 定义滑动窗口内的最大请求数。
	// 放宽到 10s 内 5 次：仍挡高频滥用，但不会卡住正常节奏（含压测每 VU
	// ~16s 一单的合成流量）。原值 1 过严，会把弱网重试/连续下单全判 429。
	// 注：原“第二道防线”SET NX EX 幂等锁已移除——它按 userID 实现，本质是
	// 更粗的限流，与本滑动窗口功能重叠（冗余）。真正的请求去重应由客户端
	// Idempotency-Key / 操作指纹承担，不在此处用 userID 锁兼任。
	tripStartRateLimitLimit = 5
)

func handleTripStart(w http.ResponseWriter, r *http.Request, redisClient redis.UniversalClient) {
	ctx, span := tracer.Start(r.Context(), "handleTripStart")
	defer span.End()

	var reqBody startTripRequest
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		http.Error(w, "failed to parse JSON data", http.StatusBadRequest)
		return
	}

	defer r.Body.Close()

	if reqBody.UserID == "" {
		http.Error(w, "user ID is required", http.StatusBadRequest)
		return
	}

	// 第一道防线：滑动窗口限流。
	// 目标：拦截“10 秒内多次点击下单”这类慢速重复请求。
	allowed, err := allowTripStartByRateLimit(ctx, redisClient, reqBody.UserID)
	if err != nil {
		log.Printf("failed to apply trip start rate limit: %v", err)
		http.Error(w, "failed to apply rate limit", http.StatusInternalServerError)
		return
	}
	if !allowed {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	// 已移除原“第二道防线：SET NX EX 幂等锁”。它按 userID 实现，本质是更粗
	// 粒度的限流，与上面的滑动窗口功能重叠（冗余）；压测中限流先触发，该锁
	// 几乎从未生效。真正的请求去重应由客户端 Idempotency-Key / 操作指纹承担。

	// Why we need to create a new client for each connection:
	// because if a service is down, we don't want to block the whole application
	// so we create a new client for each connection
	tripService, err := grpc_clients.NewTripServiceClient()
	if err != nil {
		log.Fatal(err)
	}

	// Don't forget to close the client to avoid resource leaks!
	defer tripService.Close()

	trip, err := tripService.Client.CreateTrip(ctx, reqBody.toProto())
	if err != nil {
		log.Printf("Failed to start a trip: %v", err)
		http.Error(w, "Failed to start trip", http.StatusInternalServerError)
		return
	}

	response := contracts.APIResponse{Data: trip}

	writeJSON(w, http.StatusCreated, response)
}

// allowTripStartByRateLimit 使用 Redis Lua 滑动窗口判断本次下单是否允许。
// 返回 false 不代表系统错误，而是命中限流阈值。
func allowTripStartByRateLimit(ctx context.Context, redisClient redis.UniversalClient, userID string) (bool, error) {
	if redisClient == nil {
		return false, fmt.Errorf("redis client is required for trip start rate limit")
	}

	return cache.SlidingWindowAllow(ctx, redisClient, userID, tripStartRateLimitLimit, tripStartRateLimitWindow)
}

func handleTripPreview(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handleTripPreview")
	defer span.End()

	var reqBody previewTripRequest
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		http.Error(w, "failed to parse JSON data", http.StatusBadRequest)
		return
	}

	defer r.Body.Close()

	// validation
	if reqBody.UserID == "" {
		http.Error(w, "user ID is required", http.StatusBadRequest)
		return
	}

	// Why we need to create a new client for each connection:
	// because if a service is down, we don't want to block the whole application
	// so we create a new client for each connection
	tripService, err := grpc_clients.NewTripServiceClient()
	if err != nil {
		log.Fatal(err)
	}

	// Don't forget to close the client to avoid resource leaks!
	defer tripService.Close()

	tripPreview, err := tripService.Client.PreviewTrip(ctx, reqBody.toProto())
	if err != nil {
		log.Printf("Failed to preview a trip: %v", err)
		http.Error(w, "Failed to preview trip", http.StatusInternalServerError)
		return
	}

	response := contracts.APIResponse{Data: tripPreview}

	writeJSON(w, http.StatusCreated, response)
}

func handleStripeWebhook(w http.ResponseWriter, r *http.Request, rb *messaging.RabbitMQ, redisClient redis.UniversalClient) {
	ctx, span := tracer.Start(r.Context(), "handleStripeWebhook")
	defer span.End()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	webhookKey := env.GetString("STRIPE_WEBHOOK_KEY", "")
	if webhookKey == "" {
		log.Printf("Webhook key is required")
		return
	}

	event, err := webhook.ConstructEventWithOptions(
		body,
		r.Header.Get("Stripe-Signature"),
		webhookKey,
		webhook.ConstructEventOptions{
			IgnoreAPIVersionMismatch: true,
		},
	)
	if err != nil {
		log.Printf("Error verifying webhook signature: %v", err)
		http.Error(w, "Invalid signature", http.StatusBadRequest)
		return
	}

	log.Printf("Received Stripe event: %v", event)

	processed, err := markStripeWebhookEventProcessed(ctx, redisClient, event.ID)
	if err != nil {
		log.Printf("Error marking Stripe event as processed: %v", err)
		http.Error(w, "Failed to process webhook idempotency", http.StatusInternalServerError)
		return
	}

	if !processed {
		// 幂等命中：该事件已经处理过，直接返回 200 阻止 Stripe 持续重试。
		w.WriteHeader(http.StatusOK)
		return
	}

	switch event.Type {
	case "checkout.session.completed":
		var session stripe.CheckoutSession

		err := json.Unmarshal(event.Data.Raw, &session)
		if err != nil {
			log.Printf("Error parsing webhook JSON: %v", err)
			http.Error(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		payload := messaging.PaymentStatusUpdateData{
			TripID:   session.Metadata["trip_id"],
			UserID:   session.Metadata["user_id"],
			DriverID: session.Metadata["driver_id"],
		}

		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			log.Printf("Error marshalling payload: %v", err)
			http.Error(w, "Failed to marshal payload", http.StatusInternalServerError)
			return
		}

		message := contracts.AmqpMessage{
			OwnerID: session.Metadata["user_id"],
			Data:    payloadBytes,
		}

		if err := rb.PublishMessage(
			ctx,
			contracts.PaymentEventSuccess,
			message,
		); err != nil {
			log.Printf("Error publishing payment event: %v", err)
			http.Error(w, "Failed to publish payment event", http.StatusInternalServerError)
			return
		}
	}
}

// markStripeWebhookEventProcessed 使用 Redis SetNX 标记 Stripe 事件幂等状态。
// 返回值说明：
// - true: 当前事件首次处理，可继续执行业务逻辑。
// - false: 当前事件已处理过，应直接返回 200。
func markStripeWebhookEventProcessed(ctx context.Context, redisClient redis.UniversalClient, eventID string) (bool, error) {
	if redisClient == nil {
		return false, fmt.Errorf("redis client is required for stripe webhook idempotency")
	}

	if eventID == "" {
		return false, fmt.Errorf("stripe event id is required")
	}

	key := "stripe:event:" + eventID
	ok, err := redisClient.SetNX(ctx, key, 1, stripeWebhookIdempotencyTTL).Result()
	if err != nil {
		return false, err
	}

	return ok, nil
}
