package cache

import (
	"context"
	"fmt"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	bloomBenchFilterKey = "bench:bloom:trips"
)

// BenchmarkBloomAdd 测量单次 BloomAdd 的吞吐：两次 SetBit 走 Pipeline。
func BenchmarkBloomAdd(b *testing.B) {
	rdb := newBenchRedisClient(b)
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		item := fmt.Sprintf("bench_add_%d", i)
		if err := BloomAdd(ctx, rdb, bloomBenchFilterKey, item); err != nil {
			b.Fatalf("BloomAdd failed: %v", err)
		}
	}
}

// BenchmarkBloomExists_Hit 提前预热一批正样本，循环查命中。
func BenchmarkBloomExists_Hit(b *testing.B) {
	rdb := newBenchRedisClient(b)
	ctx := context.Background()

	const warm = 100_000
	for i := 0; i < warm; i++ {
		if err := BloomAdd(ctx, rdb, bloomBenchFilterKey, fmt.Sprintf("hit_%d", i)); err != nil {
			b.Fatalf("warm up BloomAdd failed: %v", err)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		item := fmt.Sprintf("hit_%d", i%warm)
		ok, err := BloomExists(ctx, rdb, bloomBenchFilterKey, item)
		if err != nil {
			b.Fatalf("BloomExists failed: %v", err)
		}
		if !ok {
			b.Fatalf("expected hit for %s", item)
		}
	}
}

// BenchmarkBloomExists_Miss 测量未命中路径：能否快速拒绝无效 ID（核心防穿透目标）。
func BenchmarkBloomExists_Miss(b *testing.B) {
	rdb := newBenchRedisClient(b)
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		item := fmt.Sprintf("miss_%d", i)
		if _, err := BloomExists(ctx, rdb, bloomBenchFilterKey, item); err != nil {
			b.Fatalf("BloomExists failed: %v", err)
		}
	}
}

// BenchmarkBloomExists_Parallel 衡量并发查询吞吐。
func BenchmarkBloomExists_Parallel(b *testing.B) {
	rdb := newBenchRedisClient(b)
	ctx := context.Background()

	const warm = 100_000
	for i := 0; i < warm; i++ {
		if err := BloomAdd(ctx, rdb, bloomBenchFilterKey, fmt.Sprintf("par_%d", i)); err != nil {
			b.Fatalf("warm up BloomAdd failed: %v", err)
		}
	}

	var counter uint64
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := atomic.AddUint64(&counter, 1)
			item := fmt.Sprintf("par_%d", i%warm)
			if _, err := BloomExists(ctx, rdb, bloomBenchFilterKey, item); err != nil {
				b.Fatalf("BloomExists failed: %v", err)
			}
		}
	})
}

// TestBloomFalsePositiveRate 写入 N 个真实 ObjectID，再用 N 个未写入的 ObjectID 探测，
// 统计实际误判率，与理论 ~0.01% 对照。这是简历能写「实测误判率 ~X%」的依据。
func TestBloomFalsePositiveRate(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short")
	}
	rdb := newBenchRedisClient(t)
	ctx := context.Background()

	const insertCount = 200_000
	const queryCount = 200_000

	// 1) 用 ObjectID 模拟真实 tripID 写入。
	inserted := make(map[string]struct{}, insertCount)
	for i := 0; i < insertCount; i++ {
		id := primitive.NewObjectID().Hex()
		inserted[id] = struct{}{}
		if err := BloomAdd(ctx, rdb, "bench:bloom:fp", id); err != nil {
			t.Fatalf("BloomAdd failed: %v", err)
		}
	}

	// 2) 生成完全独立的查询样本，跳过任何已写入的 ID。
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	_ = rng
	falsePositives := 0
	for i := 0; i < queryCount; i++ {
		var id string
		for {
			id = primitive.NewObjectID().Hex()
			if _, ok := inserted[id]; !ok {
				break
			}
		}
		hit, err := BloomExists(ctx, rdb, "bench:bloom:fp", id)
		if err != nil {
			t.Fatalf("BloomExists failed: %v", err)
		}
		if hit {
			falsePositives++
		}
	}
	rate := float64(falsePositives) / float64(queryCount)
	t.Logf("BloomFalsePositive: inserted=%d queried=%d false_positives=%d rate=%.4f%%",
		insertCount, queryCount, falsePositives, rate*100)
}

// TestBloomLatencyDistribution 单线程顺序跑 5000 次 BloomExists（命中路径），
// 输出 P50 / P95 / P99，作为简历上 P99 数字的来源。
func TestBloomLatencyDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short")
	}
	rdb := newBenchRedisClient(t)
	ctx := context.Background()

	const warm = 10_000
	for i := 0; i < warm; i++ {
		if err := BloomAdd(ctx, rdb, "bench:bloom:lat", fmt.Sprintf("lat_%d", i)); err != nil {
			t.Fatalf("BloomAdd failed: %v", err)
		}
	}

	const samples = 5000
	durations := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		item := fmt.Sprintf("lat_%d", i%warm)
		start := time.Now()
		_, err := BloomExists(ctx, rdb, "bench:bloom:lat", item)
		durations = append(durations, time.Since(start))
		if err != nil {
			t.Fatalf("BloomExists failed: %v", err)
		}
	}
	reportPercentiles(t, "BloomExists", durations)
}
