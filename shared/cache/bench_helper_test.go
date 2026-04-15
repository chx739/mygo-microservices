package cache

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// benchRedisAddr 返回基准测试使用的 Redis 地址。
// 默认使用 docker run 起来的独立 Redis 容器（端口 6380），
// 与 Tilt 的开发环境 Redis (6379) 解耦，避免冲突。
func benchRedisAddr() string {
	if v := os.Getenv("BENCH_REDIS_ADDR"); v != "" {
		return v
	}
	return "localhost:6380"
}

// newBenchRedisClient 给基准测试 / 延迟测试使用的真实 Redis 客户端。
// 启动前会 PING 一次，连不上直接 SkipNow（不污染 CI）。
// 每次调用都会清空当前 DB，确保 benchmark 之间互不影响。
func newBenchRedisClient(tb testing.TB) redis.UniversalClient {
	tb.Helper()

	// DB 0: shared/cache 包独占。不同包使用不同 DB，避免 `go test ./...` 并行
	// 跑多个包时一个包的 FlushDB 把另一个包的 key 抹掉。
	rdb := redis.NewClient(&redis.Options{
		Addr:         benchRedisAddr(),
		DB:           0,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     64,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		tb.Skipf("benchmark requires real Redis at %s (set BENCH_REDIS_ADDR or `docker run -d --rm -p 6380:6379 redis:7-alpine`): %v", benchRedisAddr(), err)
		return nil
	}

	if err := rdb.FlushDB(ctx).Err(); err != nil {
		tb.Fatalf("flush bench redis failed: %v", err)
	}

	tb.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// reportPercentiles 对一组耗时按 P50 / P95 / P99 / max 输出，便于贴入文档与 CV。
func reportPercentiles(tb testing.TB, name string, durations []time.Duration) {
	tb.Helper()
	if len(durations) == 0 {
		tb.Logf("%s: no samples", name)
		return
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	pick := func(q float64) time.Duration {
		idx := int(float64(len(durations)) * q)
		if idx >= len(durations) {
			idx = len(durations) - 1
		}
		return durations[idx]
	}
	tb.Logf("%s n=%d P50=%v P95=%v P99=%v max=%v",
		name, len(durations), pick(0.5), pick(0.95), pick(0.99), durations[len(durations)-1])
}
