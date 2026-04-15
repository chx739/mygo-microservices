package main

import (
	"context"
	"fmt"
	"ride-sharing/shared/cache"
	"time"

	"github.com/redis/go-redis/v9"
)

// NewRedisClient 创建并校验 Redis 连接。
// 这里在启动阶段就做 Ping，有两个好处：
// 1) 配置错误时可以立即失败，避免运行中才暴露问题。
// 2) 把 Redis 可用性前置成启动条件，保持服务行为可预期。
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
