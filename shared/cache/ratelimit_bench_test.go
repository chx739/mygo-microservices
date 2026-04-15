package cache

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkSlidingWindowAllow_Uncontended 衡量纯吞吐：每个迭代用不同 userID，
// 不会被 limit 拦截，体现“一次成功的限流校验”能跑多少 QPS。
func BenchmarkSlidingWindowAllow_Uncontended(b *testing.B) {
	rdb := newBenchRedisClient(b)
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		userID := fmt.Sprintf("bench_user_%d", i)
		ok, err := SlidingWindowAllow(ctx, rdb, userID, 1, time.Second)
		if err != nil {
			b.Fatalf("SlidingWindowAllow failed: %v", err)
		}
		if !ok {
			b.Fatalf("first call for new user must be allowed")
		}
	}
}

// BenchmarkSlidingWindowAllow_Parallel 衡量真实并发吞吐。
// 每个 goroutine 用独立 userID，不互相竞争同一个 key，
// 模拟“多个用户同时下单”的真实生产场景。
func BenchmarkSlidingWindowAllow_Parallel(b *testing.B) {
	rdb := newBenchRedisClient(b)
	ctx := context.Background()

	var counter uint64
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := atomic.AddUint64(&counter, 1)
			userID := fmt.Sprintf("bench_par_user_%d", i)
			if _, err := SlidingWindowAllow(ctx, rdb, userID, 1, time.Second); err != nil {
				b.Fatalf("SlidingWindowAllow failed: %v", err)
			}
		}
	})
}

// TestSlidingWindowAllow_LatencyDistribution 单线程顺序跑 N 次，记录每次延迟，
// 统计 P50 / P95 / P99，作为简历上 P99 数字的来源。
func TestSlidingWindowAllow_LatencyDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short")
	}
	rdb := newBenchRedisClient(t)
	ctx := context.Background()

	const samples = 5000
	durations := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		userID := fmt.Sprintf("lat_user_%d", i)
		start := time.Now()
		_, err := SlidingWindowAllow(ctx, rdb, userID, 1, time.Second)
		durations = append(durations, time.Since(start))
		if err != nil {
			t.Fatalf("SlidingWindowAllow failed: %v", err)
		}
	}
	reportPercentiles(t, "SlidingWindowAllow", durations)
}

// TestSlidingWindowAllow_RejectionAccuracy 验证滑动窗口的拦截精度。
// 单 user limit=5，发 100 次，应当恰好放行前 5 次、拒绝后 95 次（毫秒级窗口内）。
// 这条用来证明“滑动窗口”不是约数限流，而是精确放行。
func TestSlidingWindowAllow_RejectionAccuracy(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short")
	}
	rdb := newBenchRedisClient(t)
	ctx := context.Background()
	const total = 100
	const limit = 5

	allowed := 0
	for i := 0; i < total; i++ {
		ok, err := SlidingWindowAllow(ctx, rdb, "accuracy_user", limit, 5*time.Second)
		if err != nil {
			t.Fatalf("SlidingWindowAllow failed: %v", err)
		}
		if ok {
			allowed++
		}
	}

	if allowed != limit {
		t.Fatalf("expected exactly %d allowed (rest blocked), got %d", limit, allowed)
	}
	t.Logf("rejection accuracy: %d/%d allowed (limit=%d)", allowed, total, limit)
}

// TestSlidingWindowAllow_ConcurrentAccuracy 验证 Lua 原子性下的并发精度。
// 同一 userID，limit=10，启 200 个 goroutine 同时打过来，最后通过的不能超过 10。
// 这条直接验证了“ZREMRANGEBYSCORE → ZCARD → ZADD”三步必须由 Lua 原子化。
func TestSlidingWindowAllow_ConcurrentAccuracy(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short")
	}
	rdb := newBenchRedisClient(t)
	ctx := context.Background()
	const goroutines = 200
	const limit = 10

	var wg sync.WaitGroup
	var passed int64

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ok, err := SlidingWindowAllow(ctx, rdb, "concurrent_user", limit, 5*time.Second)
			if err != nil {
				t.Errorf("SlidingWindowAllow failed: %v", err)
				return
			}
			if ok {
				atomic.AddInt64(&passed, 1)
			}
		}()
	}
	wg.Wait()

	if passed > int64(limit) {
		t.Fatalf("concurrent passed=%d exceeded limit=%d (Lua atomicity broken)", passed, limit)
	}
	t.Logf("concurrent accuracy: %d goroutines / passed=%d / limit=%d", goroutines, passed, limit)
}
