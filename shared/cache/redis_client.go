package cache

import (
	"strings"

	"ride-sharing/shared/env"

	"github.com/redis/go-redis/v9"
)

// RedisClientMode 描述当前 Redis 客户端运行模式。
// - standalone: 直连单节点 Redis。
// - sentinel: 通过 Sentinel 自动发现主节点并故障切换。
type RedisClientMode string

const (
	RedisClientModeStandalone RedisClientMode = "standalone"
	RedisClientModeSentinel   RedisClientMode = "sentinel"
)

// RedisClientInitResult 封装 Redis 客户端初始化结果，方便上层打印启动日志与排障。
type RedisClientInitResult struct {
	Client      redis.UniversalClient
	Mode        RedisClientMode
	EndpointHint string
}

// NewRedisClientFromEnv 根据环境变量初始化 Redis 客户端。
// 约定：
// 1) 如果 REDIS_SENTINEL_ADDRS 非空，则优先启用 Sentinel 模式。
// 2) 否则回退到 REDIS_ADDR 单节点直连模式。
// 3) REDIS_PASSWORD、REDIS_DB 在两种模式下都生效。
func NewRedisClientFromEnv() RedisClientInitResult {
	password := env.GetString("REDIS_PASSWORD", "")
	db := env.GetInt("REDIS_DB", 0)

	sentinelAddrs := parseSentinelAddrs(env.GetString("REDIS_SENTINEL_ADDRS", ""))
	if len(sentinelAddrs) > 0 {
		masterName := env.GetString("REDIS_MASTER_NAME", "mymaster")
		client := redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    masterName,
			SentinelAddrs: sentinelAddrs,
			Password:      password,
			DB:            db,
		})
		return RedisClientInitResult{
			Client:      client,
			Mode:        RedisClientModeSentinel,
			EndpointHint: strings.Join(sentinelAddrs, ","),
		}
	}

	addr := env.GetString("REDIS_ADDR", "redis:6379")
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	return RedisClientInitResult{
		Client:      client,
		Mode:        RedisClientModeStandalone,
		EndpointHint: addr,
	}
}

func parseSentinelAddrs(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	addrs := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		addrs = append(addrs, trimmed)
	}

	if len(addrs) == 0 {
		return nil
	}

	return addrs
}
