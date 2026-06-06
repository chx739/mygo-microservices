package service

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"ride-sharing/services/trip-service/internal/domain"
	"ride-sharing/shared/cache"
)

// TestCacheModeComparison 用同一份 Zipf 工作负载分别跑 NoCache / L2Only / L1L2
// 三种模式，输出 P50/P95/P99 延迟分布与 DB 命中次数，量化每一层缓存的提升。
//
// 注意事项：
//   - 需要真实 Redis（默认 localhost:6380，可用 BENCH_REDIS_ADDR 覆盖）。
//     未提供则 t.Skip，不阻塞 CI。
//   - DB 延迟用 mockTripRepository.dbLatency = 10ms 合成，**不是真 Mongo Atlas**。
//     选 10ms 是因为它是 in-region Atlas 跨网络读的常见量级；最终结论应说
//     "相对提升 N×"，而非把 NoCache 绝对值当生产基线。
//   - 三模式共享同一份 *rand.Rand（NewSource(42)），调用序列完全一致以保证公平。
//   - 用 `go test -run TestCacheModeComparison -v` 看输出。
func TestCacheModeComparison(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short")
	}

	const (
		uniqueKeys = 200                   // 唯一 tripID 数量
		nReads     = 2000                  // 每模式读次数（NoCache 约 nReads*dbLatency 长）
		dbLatency  = 10 * time.Millisecond // 合成 Mongo 延迟
		zipfS      = 1.07                  // 偏斜系数（>1，越大越尖峰）
		zipfV      = 1.0
	)

	ctx := context.Background()
	rdb := dialBenchRedis(t)

	// 预生成 uniqueKeys 个 tripID + 完整 Trip 模型。
	tripIDs := make([]string, uniqueKeys)
	tripMap := make(map[string]*domain.TripModel, uniqueKeys)
	for i := 0; i < uniqueKeys; i++ {
		oid := primitive.NewObjectID()
		id := oid.Hex()
		tripIDs[i] = id
		tripMap[id] = &domain.TripModel{
			ID:     oid,
			UserID: fmt.Sprintf("user-%d", i),
			Status: "pending",
			RideFare: &domain.RideFareModel{
				ID:                primitive.NewObjectID(),
				UserID:            fmt.Sprintf("user-%d", i),
				PackageSlug:       "sedan",
				TotalPriceInCents: 100,
			},
		}
	}

	// 预生成共享访问序列（Zipf 采样，定种子 42）。
	r := rand.New(rand.NewSource(42))
	zipf := rand.NewZipf(r, zipfS, zipfV, uint64(uniqueKeys-1))
	if zipf == nil {
		t.Fatalf("rand.NewZipf returned nil (s must be > 1)")
	}
	accessSeq := make([]string, nReads)
	for i := range accessSeq {
		accessSeq[i] = tripIDs[zipf.Uint64()]
	}

	// 工厂：每个模式开始前重新构造服务、清空 Redis、重置 mock 计数。
	type modeResult struct {
		name    string
		dur     []time.Duration
		dbCalls int
	}
	runMode := func(t *testing.T, name string, withRedis, withL1 bool) modeResult {
		t.Helper()

		// 清空 Redis（DB 1，与 shared/cache bench 解耦）。
		if err := rdb.FlushDB(ctx).Err(); err != nil {
			t.Fatalf("[%s] flush redis failed: %v", name, err)
		}

		repo := &mockTripRepository{trips: tripMap, dbLatency: dbLatency}

		var redisClient redis.UniversalClient
		if withRedis {
			redisClient = rdb
		}

		var lc *cache.LocalCache
		if withL1 {
			var err error
			lc, err = cache.NewLocalCache(int64(uniqueKeys*10), 1<<22) // 4MB 足够
			if err != nil {
				t.Fatalf("[%s] NewLocalCache failed: %v", name, err)
			}
			defer lc.Close()
		}

		svc := NewService(repo, redisClient, lc)

		// 如果有 Redis，需要预先把所有 tripID 写进布隆过滤器，
		// 否则 GetTripByID 的 Bloom 预检会判否直接返回 "not found"，
		// 永远不会回源 repo，对比就失去意义。
		if withRedis {
			for _, id := range tripIDs {
				if err := cache.BloomAdd(ctx, rdb, tripBloomFilterKey, id); err != nil {
					t.Fatalf("[%s] BloomAdd failed: %v", name, err)
				}
			}
		}

		durs := make([]time.Duration, 0, nReads)
		for _, id := range accessSeq {
			start := time.Now()
			trip, err := svc.GetTripByID(ctx, id)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("[%s] GetTripByID(%s) failed: %v", name, id, err)
			}
			if trip == nil {
				t.Fatalf("[%s] GetTripByID(%s) returned nil trip", name, id)
			}
			durs = append(durs, elapsed)
		}

		return modeResult{name: name, dur: durs, dbCalls: repo.getTripCalls}
	}

	// 三态运行（按"先慢后快"顺序便于阅读 stdout 实时进度）。
	results := []modeResult{
		runMode(t, "NoCache", false, false), // (nil rdb, nil L1) → 每次 sleep
		runMode(t, "L2Only ", true, false),  // (rdb, nil L1)     → 原 Bloom→Redis→DB
		runMode(t, "L1L2   ", true, true),   // (rdb, L1)         → 完整三级
	}

	// 输出对比表（banner 明确合成口径）。
	t.Logf("=== Cache Mode Comparison ===")
	t.Logf("Workload: nReads=%d, uniqueKeys=%d, Zipf(s=%.2f,v=%.1f), mock DB sleep=%v (synthetic, NOT real Mongo)",
		nReads, uniqueKeys, zipfS, zipfV, dbLatency)
	t.Logf("%-8s %8s %8s %8s %8s %8s %10s",
		"mode", "n", "P50", "P95", "P99", "max", "db_calls")
	for _, r := range results {
		p50, p95, p99, max := percentiles(r.dur)
		t.Logf("%-8s %8d %8s %8s %8s %8s %10d",
			r.name, len(r.dur),
			fmtDur(p50), fmtDur(p95), fmtDur(p99), fmtDur(max),
			r.dbCalls)
	}
	t.Logf("提升口径示例：L2Only.P50 / NoCache.P50 = L2 相对 DB 的加速倍数；")
	t.Logf("                L1L2.P50  / L2Only.P50  = L1 相对 L2 的加速倍数。")
}

// dialBenchRedis 复用 docker bench-redis（默认 :6380），未启则 Skip 整个测试。
// 用 DB 1 与 shared/cache bench（DB 0）解耦，避免 `go test ./...` 互相 FlushDB。
func dialBenchRedis(tb testing.TB) redis.UniversalClient {
	tb.Helper()

	addr := os.Getenv("BENCH_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6380"
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		DB:           1,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     64,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		tb.Skipf("benchmark requires real Redis at %s (set BENCH_REDIS_ADDR or `docker run -d --rm -p 6380:6379 redis:7-alpine`): %v", addr, err)
		return nil
	}
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		tb.Fatalf("flush bench redis failed: %v", err)
	}
	tb.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// percentiles 计算 P50/P95/P99/max。durations 会被原地排序。
func percentiles(durations []time.Duration) (p50, p95, p99, max time.Duration) {
	if len(durations) == 0 {
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
	return pick(0.5), pick(0.95), pick(0.99), durations[len(durations)-1]
}

// fmtDur 让输出表对齐：µs 级以下用 µs，1ms+ 用 ms。
func fmtDur(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
}
