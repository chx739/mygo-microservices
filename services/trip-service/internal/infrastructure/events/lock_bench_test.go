package events

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkTripAcceptLock_Uncontended 衡量纯加锁吞吐：每次新 tripID，
// 只命中 SetNX 成功分支。
func BenchmarkTripAcceptLock_Uncontended(b *testing.B) {
	rdb := newBenchRedisClient(b)
	consumer := &driverConsumer{redis: rdb}
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tripID := fmt.Sprintf("bench-trip-%d", i)
		ok, err := consumer.tryAcquireTripAcceptLock(ctx, tripID, "driver-a")
		if err != nil {
			b.Fatalf("tryAcquireTripAcceptLock failed: %v", err)
		}
		if !ok {
			b.Fatalf("first acquire on fresh tripID must succeed")
		}
	}
}

// BenchmarkTripAcceptLock_AcquireRelease 衡量「加锁 + Lua 释放」完整往返。
// 这是真实生产路径：成功的 accept 流程末尾 defer release 都会走这条。
func BenchmarkTripAcceptLock_AcquireRelease(b *testing.B) {
	rdb := newBenchRedisClient(b)
	consumer := &driverConsumer{redis: rdb}
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tripID := fmt.Sprintf("bench-trip-rel-%d", i)
		ok, err := consumer.tryAcquireTripAcceptLock(ctx, tripID, "driver-a")
		if err != nil {
			b.Fatalf("tryAcquireTripAcceptLock failed: %v", err)
		}
		if !ok {
			b.Fatalf("first acquire on fresh tripID must succeed")
		}
		released, err := consumer.releaseTripAcceptLock(ctx, tripID, "driver-a")
		if err != nil {
			b.Fatalf("releaseTripAcceptLock failed: %v", err)
		}
		if !released {
			b.Fatalf("owner should release lock successfully")
		}
	}
}

// BenchmarkTripAcceptLock_Parallel 衡量并发吞吐：多 goroutine 各自抢不同的 tripID，
// 模拟「多个不同行程同时被多个司机接单」的真实场景。
func BenchmarkTripAcceptLock_Parallel(b *testing.B) {
	rdb := newBenchRedisClient(b)
	consumer := &driverConsumer{redis: rdb}
	ctx := context.Background()

	var counter uint64
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := atomic.AddUint64(&counter, 1)
			tripID := fmt.Sprintf("bench-trip-par-%d", i)
			ok, err := consumer.tryAcquireTripAcceptLock(ctx, tripID, "driver-a")
			if err != nil {
				b.Fatalf("tryAcquireTripAcceptLock failed: %v", err)
			}
			if !ok {
				b.Fatalf("fresh tripID must acquire successfully")
			}
			if _, err := consumer.releaseTripAcceptLock(ctx, tripID, "driver-a"); err != nil {
				b.Fatalf("releaseTripAcceptLock failed: %v", err)
			}
		}
	})
}

// TestTripAcceptLockConcurrentSafety 验证并发互斥正确性：
// 100 个 goroutine 同时抢同一个 tripID，恰好 1 个成功，其他 99 个返回 false。
// 这是简历上「并发接单防护」最关键的正确性证明。
func TestTripAcceptLockConcurrentSafety(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short")
	}
	rdb := newBenchRedisClient(t)
	consumer := &driverConsumer{redis: rdb}
	ctx := context.Background()

	const goroutines = 100
	tripID := "concurrent-trip"

	var (
		wg       sync.WaitGroup
		acquired int64
		rejected int64
	)

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		driverID := fmt.Sprintf("driver-%d", i)
		go func(d string) {
			defer wg.Done()
			ok, err := consumer.tryAcquireTripAcceptLock(ctx, tripID, d)
			if err != nil {
				t.Errorf("tryAcquireTripAcceptLock failed: %v", err)
				return
			}
			if ok {
				atomic.AddInt64(&acquired, 1)
			} else {
				atomic.AddInt64(&rejected, 1)
			}
		}(driverID)
	}
	wg.Wait()

	if acquired != 1 {
		t.Fatalf("expected exactly 1 driver to acquire lock, got %d (rejected=%d)", acquired, rejected)
	}
	if acquired+rejected != goroutines {
		t.Fatalf("counters mismatch: acquired=%d rejected=%d total=%d", acquired, rejected, goroutines)
	}
	t.Logf("concurrent safety: %d goroutines / acquired=%d / rejected=%d", goroutines, acquired, rejected)
}

// TestTripAcceptLockLatencyDistribution 串行 5000 次完整加锁+释放，
// 输出 P50 / P95 / P99，作为简历上 P99 数字的来源。
func TestTripAcceptLockLatencyDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short")
	}
	rdb := newBenchRedisClient(t)
	consumer := &driverConsumer{redis: rdb}
	ctx := context.Background()

	const samples = 5000
	durations := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		tripID := fmt.Sprintf("lat-trip-%d", i)
		start := time.Now()
		ok, err := consumer.tryAcquireTripAcceptLock(ctx, tripID, "driver-a")
		if err != nil {
			t.Fatalf("tryAcquireTripAcceptLock failed: %v", err)
		}
		if !ok {
			t.Fatalf("fresh tripID must acquire")
		}
		_, err = consumer.releaseTripAcceptLock(ctx, tripID, "driver-a")
		durations = append(durations, time.Since(start))
		if err != nil {
			t.Fatalf("releaseTripAcceptLock failed: %v", err)
		}
	}
	reportPercentiles(t, "TripAcceptLock_AcquireRelease", durations)
}
