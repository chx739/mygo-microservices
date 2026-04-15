package events

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"ride-sharing/services/trip-service/internal/domain"
	"ride-sharing/shared/contracts"
	"ride-sharing/shared/messaging"

	"github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

const (
	// paymentConsumerProcessedMessageTTL 定义 payment-consumer 的消息幂等窗口。
	// 在该 TTL 内，重复消息会被识别为已处理并跳过。
	paymentConsumerProcessedMessageTTL = 10 * time.Minute
)

type paymentConsumer struct {
	rabbitmq *messaging.RabbitMQ
	service  domain.TripService
	redis    redis.UniversalClient
}

func NewPaymentConsumer(rabbitmq *messaging.RabbitMQ, service domain.TripService, redisClient redis.UniversalClient) *paymentConsumer {
	return &paymentConsumer{
		rabbitmq: rabbitmq,
		service:  service,
		redis:    redisClient,
	}
}

func (c *paymentConsumer) Listen() error {
	return c.rabbitmq.ConsumeMessages(messaging.NotifyPaymentSuccessQueue, func(ctx context.Context, msg amqp091.Delivery) error {
		// 第 0 步：消息幂等去重，避免支付成功事件被重复推进 Trip 状态。
		claim, err := messaging.ClaimMessageForProcessing(
			ctx,
			c.redis,
			"trip-service:payment-consumer",
			msg,
			paymentConsumerProcessedMessageTTL,
		)
		if err != nil {
			return err
		}

		if claim.AlreadyProcessed {
			log.Printf("payment consumer skip duplicated message: messageID=%s routingKey=%s", msg.MessageId, msg.RoutingKey)
			return nil
		}

		var message contracts.AmqpMessage
		if err := json.Unmarshal(msg.Body, &message); err != nil {
			_ = messaging.ReleaseMessageProcessing(ctx, c.redis, claim.Key)
			log.Printf("Failed to unmarshal message: %v", err)
			return err
		}
		var payload messaging.PaymentStatusUpdateData
		if err := json.Unmarshal(message.Data, &payload); err != nil {
			_ = messaging.ReleaseMessageProcessing(ctx, c.redis, claim.Key)
			log.Printf("Failed to unmarshal payload: %v", err)
			return err
		}

		log.Printf("Trip has been completed and payed.")

		if err := c.service.UpdateTrip(
			ctx,
			payload.TripID,
			"payed",
			nil,
		); err != nil {
			_ = messaging.ReleaseMessageProcessing(ctx, c.redis, claim.Key)
			return err
		}

		if err := messaging.MarkMessageProcessed(ctx, c.redis, claim.Key, paymentConsumerProcessedMessageTTL); err != nil {
			return err
		}

		return nil
	})
}
