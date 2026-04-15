package messaging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

const (
	idempotencyStateProcessing = "processing"
	idempotencyStateDone       = "done"
)

// MessageProcessClaim 表示一次“消息处理占位”结果。
// - Key: Redis 中用于幂等去重的键。
// - AlreadyProcessed: true 表示该消息已被成功处理过，当前可直接跳过。
type MessageProcessClaim struct {
	Key              string
	AlreadyProcessed bool
}

// ClaimMessageForProcessing 尝试为消息声明处理权。
// 设计目标：
// 1) 第一次处理：写入 processing 状态并继续执行业务逻辑。
// 2) 重复消息（已完成）：检测到 done 状态后直接跳过。
// 3) processing 状态冲突：返回错误触发重试，避免并发重复执行。
func ClaimMessageForProcessing(
	ctx context.Context,
	rdb redis.UniversalClient,
	namespace string,
	msg amqp.Delivery,
	ttl time.Duration,
) (*MessageProcessClaim, error) {
	if rdb == nil {
		return nil, fmt.Errorf("redis client is required for message idempotency")
	}
	if namespace == "" {
		return nil, fmt.Errorf("namespace is required for message idempotency")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("ttl must be greater than zero")
	}

	resolvedMessageID := resolveMessageID(msg)
	key := buildProcessedMessageKey(namespace, resolvedMessageID)

	ok, err := rdb.SetNX(ctx, key, idempotencyStateProcessing, ttl).Result()
	if err != nil {
		return nil, err
	}

	if ok {
		return &MessageProcessClaim{Key: key, AlreadyProcessed: false}, nil
	}

	state, err := rdb.Get(ctx, key).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, fmt.Errorf("idempotency key disappeared: %s", key)
		}
		return nil, err
	}

	if state == idempotencyStateDone {
		return &MessageProcessClaim{Key: key, AlreadyProcessed: true}, nil
	}

	return nil, fmt.Errorf("message is still processing, key=%s", key)
}

// MarkMessageProcessed 把消息状态从 processing 置为 done。
// 只在业务逻辑成功后调用，保证“完成态”只记录成功处理的消息。
func MarkMessageProcessed(ctx context.Context, rdb redis.UniversalClient, key string, ttl time.Duration) error {
	if rdb == nil {
		return fmt.Errorf("redis client is required for marking processed message")
	}
	if key == "" {
		return fmt.Errorf("idempotency key is required")
	}

	return rdb.Set(ctx, key, idempotencyStateDone, ttl).Err()
}

// ReleaseMessageProcessing 在业务失败时释放 processing 占位。
// 这样同一条消息在 retry/backoff 重试时还能继续真正执行业务，而不是被误判为重复。
func ReleaseMessageProcessing(ctx context.Context, rdb redis.UniversalClient, key string) error {
	if rdb == nil {
		return fmt.Errorf("redis client is required for releasing message processing")
	}
	if key == "" {
		return fmt.Errorf("idempotency key is required")
	}

	return rdb.Del(ctx, key).Err()
}

func buildProcessedMessageKey(namespace, messageID string) string {
	trimmedNamespace := strings.TrimSpace(namespace)
	trimmedMessageID := strings.TrimSpace(messageID)
	return "idem:" + trimmedNamespace + ":" + trimmedMessageID
}

// resolveMessageID 优先使用 RabbitMQ MessageId。
// 如果历史消息没有 MessageId，则回退为 routingKey + body 的 SHA256 摘要，保持去重能力向后兼容。
func resolveMessageID(msg amqp.Delivery) string {
	if strings.TrimSpace(msg.MessageId) != "" {
		return msg.MessageId
	}

	h := sha256.New()
	_, _ = h.Write([]byte(msg.RoutingKey))
	_, _ = h.Write(msg.Body)
	return "fallback:" + hex.EncodeToString(h.Sum(nil))
}
