package events

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"ride-sharing/services/payment-service/internal/domain"
	"ride-sharing/shared/contracts"
	"ride-sharing/shared/messaging"

	"github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

const (
	// paymentServiceProcessedMessageTTL 定义 payment-service 消费消息的去重窗口。
	paymentServiceProcessedMessageTTL = 10 * time.Minute
)

type TripConsumer struct {
	rabbitmq *messaging.RabbitMQ
	service  domain.Service
	redis    redis.UniversalClient
}

func NewTripConsumer(rabbitmq *messaging.RabbitMQ, service domain.Service, redisClient redis.UniversalClient) *TripConsumer {
	return &TripConsumer{
		rabbitmq: rabbitmq,
		service:  service,
		redis:    redisClient,
	}
}

func (c *TripConsumer) Listen() error {
	return c.rabbitmq.ConsumeMessages(messaging.PaymentTripResponseQueue, func(ctx context.Context, msg amqp091.Delivery) error {
		// 第 0 步：消息幂等去重。
		// 防止同一条 create_session 消息被重复消费，导致创建多个 Stripe 会话。
		claim, err := messaging.ClaimMessageForProcessing(
			ctx,
			c.redis,
			"payment-service:trip-consumer",
			msg,
			paymentServiceProcessedMessageTTL,
		)
		if err != nil {
			return err
		}

		if claim.AlreadyProcessed {
			log.Printf("payment-service trip consumer skip duplicated message: messageID=%s routingKey=%s", msg.MessageId, msg.RoutingKey)
			return nil
		}

		var message contracts.AmqpMessage
		if err := json.Unmarshal(msg.Body, &message); err != nil {
			_ = messaging.ReleaseMessageProcessing(ctx, c.redis, claim.Key)
			log.Printf("Failed to unmarshal message: %v", err)
			return err
		}

		var payload messaging.PaymentTripResponseData
		if err := json.Unmarshal(message.Data, &payload); err != nil {
			_ = messaging.ReleaseMessageProcessing(ctx, c.redis, claim.Key)
			log.Printf("Failed to unmarshal payload: %v", err)
			return err
		}

		switch msg.RoutingKey {
		case contracts.PaymentCmdCreateSession:
			if err := c.handleTripAccepted(ctx, payload); err != nil {
				_ = messaging.ReleaseMessageProcessing(ctx, c.redis, claim.Key)
				log.Printf("Failed to handle trip accepted: %v", err)
				return err
			}
		}

		if err := messaging.MarkMessageProcessed(ctx, c.redis, claim.Key, paymentServiceProcessedMessageTTL); err != nil {
			return err
		}

		return nil
	})
}

func (c *TripConsumer) handleTripAccepted(ctx context.Context, payload messaging.PaymentTripResponseData) error {
	log.Printf("Handling trip accepted by driver: %s", payload.TripID)

	paymentSession, err := c.service.CreatePaymentSession(
		ctx,
		payload.TripID,
		payload.UserID,
		payload.DriverID,
		int64(payload.Amount),
		payload.Currency,
	)
	if err != nil {
		log.Printf("Failed to create payment session: %v", err)
		return err
	}

	log.Printf("Payment session created: %s", paymentSession.StripeSessionID)

	// Publish payment session created event
	paymentPayload := messaging.PaymentEventSessionCreatedData{
		TripID:    payload.TripID,
		SessionID: paymentSession.StripeSessionID,
		Amount:    float64(paymentSession.Amount) / 100.0, // Convert from cents to dollars
		Currency:  paymentSession.Currency,
	}

	payloadBytes, err := json.Marshal(paymentPayload)
	if err != nil {
		log.Printf("Failed to marshal payment session payload: %v", err)
		return err
	}

	if err := c.rabbitmq.PublishMessage(ctx, contracts.PaymentEventSessionCreated,
		contracts.AmqpMessage{
			OwnerID: payload.UserID,
			Data:    payloadBytes,
		},
	); err != nil {
		log.Printf("Failed to publish payment session created event: %v", err)
		return err
	}

	log.Printf("Published payment session created event for trip: %s", payload.TripID)
	return nil
}
