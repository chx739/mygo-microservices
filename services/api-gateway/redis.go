package main

import (
	"context"
	"fmt"
	"ride-sharing/shared/cache"
	"time"

	"github.com/redis/go-redis/v9"
)

// NewRedisClient 创建并校验 Redis 连接。
// API Gateway 主要用于 Stripe webhook 幂等，启动即 Ping 可尽早暴露配置问题。
func NewRedisClient(ctx context.Context) (redis.UniversalClient, error) {
	result := cache.NewRedisClientFromEnv()
	client := result.Client

	// 单次 Ping 使用短超时，避免网络异常导致启动长时间阻塞。
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis failed (mode=%s, endpoint=%s): %w", result.Mode, result.EndpointHint, err)
	}

	return client, nil
}
