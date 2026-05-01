package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"ride-sharing/services/trip-service/internal/domain"
	"ride-sharing/shared/contracts"
	"ride-sharing/shared/messaging"
	pbd "ride-sharing/shared/proto/driver"
	"time"

	"github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

const (
	// tripAcceptLockTTL 是“司机接单锁”的超时时间。
	// 只要第一个司机抢到锁，其它并发 accept 会被快速拦截。
	tripAcceptLockTTL = 30 * time.Second

	// driverConsumerProcessedMessageTTL 定义 driver-consumer 消息去重窗口。
	// 在该时间内，同一 MessageId 重复投递会被直接跳过。
	driverConsumerProcessedMessageTTL = 10 * time.Minute
)

var (
	// tripAcceptUnlockScript 使用 Lua 保证“校验 owner + 删除锁”原子执行。
	// 语义：只有当锁 value 与当前 driverID 完全一致时才删除，避免误删他人持有的锁。
	tripAcceptUnlockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
end
return 0
`)
)

type driverConsumer struct {
	rabbitmq *messaging.RabbitMQ
	service  domain.TripService
	redis    redis.UniversalClient
}

func NewDriverConsumer(rabbitmq *messaging.RabbitMQ, service domain.TripService, redisClient redis.UniversalClient) *driverConsumer {
	return &driverConsumer{
		rabbitmq: rabbitmq,
		service:  service,
		redis:    redisClient,
	}
}

func (c *driverConsumer) Listen() error {
	return c.rabbitmq.ConsumeMessages(messaging.DriverTripResponseQueue, func(ctx context.Context, msg amqp091.Delivery) error {
		// 第 0 步：消息幂等去重。
		// 目标：同一条消息在 at-least-once 投递语义下重复到达时，只执行一次业务逻辑。
		claim, err := messaging.ClaimMessageForProcessing(
			ctx,
			c.redis,
			"trip-service:driver-consumer",
			msg,
			driverConsumerProcessedMessageTTL,
		)
		if err != nil {
			return err
		}

		if claim.AlreadyProcessed {
			log.Printf("driver consumer skip duplicated message: messageID=%s routingKey=%s", msg.MessageId, msg.RoutingKey)
			return nil
		}

		var message contracts.AmqpMessage
		if err := json.Unmarshal(msg.Body, &message); err != nil {
			// 反序列化失败属于本次处理失败，释放 processing 占位，允许后续重试继续处理。
			_ = messaging.ReleaseMessageProcessing(ctx, c.redis, claim.Key)
			log.Printf("Failed to unmarshal message: %v", err)
			return err
		}

		var payload messaging.DriverTripResponseData
		if err := json.Unmarshal(message.Data, &payload); err != nil {
			_ = messaging.ReleaseMessageProcessing(ctx, c.redis, claim.Key)
			log.Printf("Failed to unmarshal message: %v", err)
			return err
		}

		log.Printf("driver response received message: %+v", payload)

		// 兜底：driver_assigned/decline 链路里 client 通常只发 { tripID }，
		// driver 自己的 ID 由 gateway 通过 AmqpMessage.OwnerID 透传过来
		// (services/api-gateway/ws.go:160-167)。如果 payload.Driver 没设
		// 就用 OwnerID 重建一个最小 *pbd.Driver，避免 tryAcquireTripAcceptLock
		// 因 driverID="" 直接 fail（Round 2 重跑暴露的 trip_accept 死路）。
		if payload.Driver == nil || payload.Driver.GetId() == "" {
			payload.Driver = &pbd.Driver{Id: message.OwnerID}
		}
		if payload.RiderID == "" {
			// decline 路径 trip-service 拿 RiderID 找司机重投，缺失会 panic on Trip.UserID 缓存查询；
			// gateway 同样把 driver userID 放在 OwnerID，不能直接当 rider，
			// 所以这里依赖 GetTripByID 反查 trip.UserID。handleTripDeclined 内已自取，本函数不强填。
			_ = payload.RiderID
		}

		switch msg.RoutingKey {
		case contracts.DriverCmdTripAccept:
			if err := c.handleTripAccepted(ctx, payload.TripID, payload.Driver); err != nil {
				_ = messaging.ReleaseMessageProcessing(ctx, c.redis, claim.Key)
				log.Printf("Failed to handle the trip accept: %v", err)
				return err
			}
			// 没有 return nil 时会落到 fall-through 那条「unknown trip event」误报。
			if err := messaging.MarkMessageProcessed(ctx, c.redis, claim.Key, driverConsumerProcessedMessageTTL); err != nil {
				return err
			}
			return nil
		case contracts.DriverCmdTripDecline:
			if err := c.handleTripDeclined(ctx, payload.TripID, payload.RiderID); err != nil {
				_ = messaging.ReleaseMessageProcessing(ctx, c.redis, claim.Key)
				log.Printf("Failed to handle the trip decline: %v", err)
				return err
			}
			if err := messaging.MarkMessageProcessed(ctx, c.redis, claim.Key, driverConsumerProcessedMessageTTL); err != nil {
				// 如果标记 done 失败，返回错误让框架重试，避免“业务成功但幂等状态缺失”长期放大。
				return err
			}

			return nil
		}
		log.Printf("unknown trip event: %+v", payload)

		if err := messaging.MarkMessageProcessed(ctx, c.redis, claim.Key, driverConsumerProcessedMessageTTL); err != nil {
			return err
		}

		return nil
	})
}

func (c *driverConsumer) handleTripDeclined(ctx context.Context, tripID, riderID string) error {
	// When a driver declines, we should try to find another driver

	trip, err := c.service.GetTripByID(ctx, tripID)
	if err != nil {
		return err
	}

	newPayload := messaging.TripEventData{
		Trip: trip.ToProto(),
	}

	marshalledPayload, err := json.Marshal(newPayload)
	if err != nil {
		return err
	}

	if err := c.rabbitmq.PublishMessage(ctx, contracts.TripEventDriverNotInterested,
		contracts.AmqpMessage{
			OwnerID: riderID,
			Data:    marshalledPayload,
		},
	); err != nil {
		return err
	}

	return nil
}

func (c *driverConsumer) handleTripAccepted(ctx context.Context, tripID string, driver *pbd.Driver) error {
	driverID := driver.GetId()

	// 第 0 步：分布式锁拦截并发 accept。
	// 场景：司机 A 和司机 B 几乎同时 accept 同一个 tripID。
	// 目标：只有第一个拿到锁的请求继续执行，其他请求直接返回 nil（静默丢弃）。
	ok, err := c.tryAcquireTripAcceptLock(ctx, tripID, driverID)
	if err != nil {
		return err
	}

	if !ok {
		log.Printf("Trip %s has already been accepted by another driver, skip current accept", tripID)
		return nil
	}

	// 第 0.1 步：提前释放锁（而不是只等 TTL 自动过期）。
	// 这里用 defer 确保无论后续流程成功/失败都尝试释放，避免锁空转占用。
	// 释放使用 Lua 原子脚本，只有锁 owner 匹配当前 driverID 才会删除。
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		released, releaseErr := c.releaseTripAcceptLock(unlockCtx, tripID, driverID)
		if releaseErr != nil {
			log.Printf("Failed to release trip accept lock for trip %s: %v", tripID, releaseErr)
			return
		}

		if !released {
			log.Printf("Trip accept lock for trip %s was not released because lock owner did not match", tripID)
		}
	}()

	// 1. Fetch the first
	trip, err := c.service.GetTripByID(ctx, tripID)
	if err != nil {
		return err
	}

	if trip == nil {
		return fmt.Errorf("Trip was not found %s", tripID)
	}

	// 2. Update the trip
	if err := c.service.UpdateTrip(ctx, tripID, "accepted", driver); err != nil {
		log.Printf("Failed to update the trip: %v", err)
		return err
	}

	trip, err = c.service.GetTripByID(ctx, tripID)
	if err != nil {
		return err
	}

	// 3. Driver has been assigned -> publish this event to RB
	marshalledTrip, err := json.Marshal(trip)
	if err != nil {
		return err
	}

	// Notify the rider that a driver has been assigned
	if err := c.rabbitmq.PublishMessage(ctx, contracts.TripEventDriverAssigned, contracts.AmqpMessage{
		OwnerID: trip.UserID,
		Data:    marshalledTrip,
	}); err != nil {
		return err
	}

	marshalledPayload, err := json.Marshal(messaging.PaymentTripResponseData{
		TripID:   tripID,
		UserID:   trip.UserID,
		DriverID: driverID,
		Amount:   trip.RideFare.TotalPriceInCents,
		Currency: "USD",
	})

	if err := c.rabbitmq.PublishMessage(ctx, contracts.PaymentCmdCreateSession,
		contracts.AmqpMessage{
			OwnerID: trip.UserID,
			Data:    marshalledPayload,
		},
	); err != nil {
		return err
	}

	return nil
}

// tryAcquireTripAcceptLock 通过 Redis SET NX EX 获取接单锁。
// 返回值说明：
// - true: 当前请求是第一个抢到锁的请求，可继续处理。
// - false: 锁已被其他请求持有，当前请求应立即跳过。
func (c *driverConsumer) tryAcquireTripAcceptLock(ctx context.Context, tripID, driverID string) (bool, error) {
	if c.redis == nil {
		return false, fmt.Errorf("redis client is required for trip accept lock")
	}

	if tripID == "" {
		return false, fmt.Errorf("tripID is required")
	}

	if driverID == "" {
		return false, fmt.Errorf("driverID is required")
	}

	lockKey := tripAcceptLockKey(tripID)

	ok, err := c.redis.SetNX(ctx, lockKey, driverID, tripAcceptLockTTL).Result()
	if err != nil {
		return false, err
	}

	return ok, nil
}

// releaseTripAcceptLock 使用 Lua 脚本安全释放接单锁。
// 返回值说明：
// - true: 锁已被当前 driverID 成功释放。
// - false: 没有释放（通常是锁不存在，或锁 owner 不是当前 driverID）。
func (c *driverConsumer) releaseTripAcceptLock(ctx context.Context, tripID, driverID string) (bool, error) {
	if c.redis == nil {
		return false, fmt.Errorf("redis client is required for trip accept unlock")
	}

	if tripID == "" {
		return false, fmt.Errorf("tripID is required")
	}

	if driverID == "" {
		return false, fmt.Errorf("driverID is required")
	}

	lockKey := tripAcceptLockKey(tripID)

	result, err := tripAcceptUnlockScript.Run(ctx, c.redis, []string{lockKey}, driverID).Int64()
	if err != nil {
		return false, err
	}

	return result == 1, nil
}

func tripAcceptLockKey(tripID string) string {
	return "trip:lock:" + tripID
}
