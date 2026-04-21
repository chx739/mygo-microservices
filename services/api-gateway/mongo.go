package main

// mongo.go 负责在 api-gateway 内初始化 MongoDB 连接。
//
// 与 trip-service 的差异：
//   - trip-service 不做重试，因为它在 Deployment 中有 initContainer 等外部编排保障 Mongo 已就绪。
//   - api-gateway 在 `tilt up` 场景下可能先于 MongoDB 启动完成，所以这里给 Ping 加 3 次重试、间隔 2s，
//     避免 Pod 因偶发 Ping 超时而 CrashLoopBackOff。
//
// 索引创建不放在本文件，统一由 main.go 的 EnsureUserIndexes 创建（见 user_repo.go）。

import (
	"context"
	"fmt"
	"log"
	"time"

	"ride-sharing/shared/db"

	"go.mongodb.org/mongo-driver/mongo"
)

const (
	// mongoPingRetries 是启动阶段 Ping 的总次数。3 次已覆盖绝大多数 tilt up 冷启动场景。
	mongoPingRetries = 3
	// mongoPingInterval 是两次 Ping 之间的等待时间。2s 给 mongodb 容器一个启动窗口。
	mongoPingInterval = 2 * time.Second
)

// NewMongoClient 构造 MongoDB 客户端并返回同一个 DB 句柄。
//
// 内部先调用 shared/db.NewMongoClient（里面已经 Ping 了一次），然后再自带一个带重试的 Ping，
// 双保险。这样即便 shared/db 里的 Ping 偶发失败，本函数也有机会挽救。
//
// 返回三元组 (*mongo.Client, *mongo.Database, error)：
//   - Client：供调用方 defer Disconnect 使用。
//   - Database：直接复用，避免调用方再持一份 cfg。
//   - error：fail-fast；所有重试耗尽仍失败即返回。
func NewMongoClient(ctx context.Context) (*mongo.Client, *mongo.Database, error) {
	cfg := db.NewMongoDefaultConfig()
	client, err := db.NewMongoClient(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("connect mongo: %w", err)
	}

	// 手工再 Ping N 次；shared/db 里的 Ping 只做了一次，网络抖动可能偶发失败。
	var pingErr error
	for attempt := 1; attempt <= mongoPingRetries; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		pingErr = client.Ping(pingCtx, nil)
		cancel()
		if pingErr == nil {
			break
		}
		log.Printf("mongo ping attempt %d/%d failed: %v", attempt, mongoPingRetries, pingErr)
		if attempt < mongoPingRetries {
			time.Sleep(mongoPingInterval)
		}
	}
	if pingErr != nil {
		_ = client.Disconnect(ctx)
		return nil, nil, fmt.Errorf("ping mongo after %d attempts: %w", mongoPingRetries, pingErr)
	}

	return client, db.GetDatabase(client, cfg), nil
}
