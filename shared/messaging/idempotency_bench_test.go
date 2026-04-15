package messaging

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	idemBenchNamespace = "bench:trip-service:driver-consumer"
	idemBenchTTL       = 10 * time.Minute
)

// BenchmarkClaim_FirstTime 衡量首次声明路径吞吐：每次新 MessageId，命中 SetNX 成功分支。
// 这是消息处理热路径，每条消息进入消费者第一时间走的就是这条。
func BenchmarkClaim_FirstTime(b *testing.B) {
	rdb := newBenchRedisClient(b)
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		msg := amqp.Delivery{
			MessageId:  fmt.Sprintf("bench-msg-%d", i),
			RoutingKey: "trip.event.created",
			Body:       []byte(`{"tripID":"t-1"}`),
		}
		claim, err := ClaimMessageForProcessing(ctx, rdb, idemBenchNamespace, msg, idemBenchTTL)
		if err != nil {
			b.Fatalf("ClaimMessageForProcessing failed: %v", err)
		}
		if claim.AlreadyProcessed {
			b.Fatalf("first claim must not be already processed")
		}
	}
}

// BenchmarkClaim_AlreadyDone 衡量重复消息快速跳过路径：固定 MessageId，
// 第一次 claim 后立即 markDone，后续都走「检测到 done → 直接跳过」分支。
// 这条体现：重复消息的去重开销几乎为零。
func BenchmarkClaim_AlreadyDone(b *testing.B) {
	rdb := newBenchRedisClient(b)
	ctx := context.Background()

	msg := amqp.Delivery{
		MessageId:  "bench-msg-fixed",
		RoutingKey: "trip.event.created",
		Body:       []byte(`{"tripID":"t-1"}`),
	}
	claim, err := ClaimMessageForProcessing(ctx, rdb, idemBenchNamespace, msg, idemBenchTTL)
	if err != nil {
		b.Fatalf("warm claim failed: %v", err)
	}
	if err := MarkMessageProcessed(ctx, rdb, claim.Key, idemBenchTTL); err != nil {
		b.Fatalf("warm mark done failed: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dup, err := ClaimMessageForProcessing(ctx, rdb, idemBenchNamespace, msg, idemBenchTTL)
		if err != nil {
			b.Fatalf("ClaimMessageForProcessing failed: %v", err)
		}
		if !dup.AlreadyProcessed {
			b.Fatalf("expected already processed for duplicate")
		}
	}
}

// BenchmarkClaimAndMarkDone_FullFlow 衡量完整业务链路：claim → 模拟业务 → markDone。
// 数字最贴近真实消费者每条消息的端到端开销。
func BenchmarkClaimAndMarkDone_FullFlow(b *testing.B) {
	rdb := newBenchRedisClient(b)
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		msg := amqp.Delivery{
			MessageId:  fmt.Sprintf("bench-full-%d", i),
			RoutingKey: "trip.event.created",
			Body:       []byte(`{"tripID":"t-1"}`),
		}
		claim, err := ClaimMessageForProcessing(ctx, rdb, idemBenchNamespace, msg, idemBenchTTL)
		if err != nil {
			b.Fatalf("ClaimMessageForProcessing failed: %v", err)
		}
		// 模拟空的业务处理
		if err := MarkMessageProcessed(ctx, rdb, claim.Key, idemBenchTTL); err != nil {
			b.Fatalf("MarkMessageProcessed failed: %v", err)
		}
	}
}

// BenchmarkClaim_Parallel 衡量并发吞吐，模拟多消费者实例同时拉消息。
func BenchmarkClaim_Parallel(b *testing.B) {
	rdb := newBenchRedisClient(b)
	ctx := context.Background()

	var counter uint64
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := atomic.AddUint64(&counter, 1)
			msg := amqp.Delivery{
				MessageId:  fmt.Sprintf("bench-par-%d", i),
				RoutingKey: "trip.event.created",
				Body:       []byte(`{"tripID":"t-1"}`),
			}
			claim, err := ClaimMessageForProcessing(ctx, rdb, idemBenchNamespace, msg, idemBenchTTL)
			if err != nil {
				b.Fatalf("ClaimMessageForProcessing failed: %v", err)
			}
			if err := MarkMessageProcessed(ctx, rdb, claim.Key, idemBenchTTL); err != nil {
				b.Fatalf("MarkMessageProcessed failed: %v", err)
			}
		}
	})
}

// TestIdempotencyLatencyDistribution 串行跑 5000 次完整流程，
// 输出 P50 / P95 / P99，作为简历上 P99 数字的来源。
func TestIdempotencyLatencyDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short")
	}
	rdb := newBenchRedisClient(t)
	ctx := context.Background()

	const samples = 5000
	durations := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		msg := amqp.Delivery{
			MessageId:  fmt.Sprintf("lat-msg-%d", i),
			RoutingKey: "trip.event.created",
			Body:       []byte(`{"tripID":"t-1"}`),
		}
		start := time.Now()
		claim, err := ClaimMessageForProcessing(ctx, rdb, idemBenchNamespace, msg, idemBenchTTL)
		if err != nil {
			t.Fatalf("ClaimMessageForProcessing failed: %v", err)
		}
		if err := MarkMessageProcessed(ctx, rdb, claim.Key, idemBenchTTL); err != nil {
			t.Fatalf("MarkMessageProcessed failed: %v", err)
		}
		durations = append(durations, time.Since(start))
	}
	reportPercentiles(t, "Idempotency_FullFlow", durations)
}

// TestIdempotencyConcurrentClaim 验证 100 个 goroutine 抢同一 MessageId 时的并发收敛性：
// 恰好 1 个进入业务（AlreadyProcessed=false 且 SetNX 成功），
// 其他 99 个要么命中 done 跳过，要么命中 processing 报错。
// 这条直接证明 RabbitMQ at-least-once 重投下「同一条消息只会被业务处理一次」。
func TestIdempotencyConcurrentClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short")
	}
	rdb := newBenchRedisClient(t)
	ctx := context.Background()

	const goroutines = 100
	msg := amqp.Delivery{
		MessageId:  "concurrent-msg",
		RoutingKey: "trip.event.created",
		Body:       []byte(`{"tripID":"t-99"}`),
	}

	var (
		wg               sync.WaitGroup
		claimedFirstTime int64 // SetNX 成功获得处理权
		alreadyDone      int64 // 命中 done 快速跳过
		processingErr    int64 // 命中 processing 状态被拒
	)

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			claim, err := ClaimMessageForProcessing(ctx, rdb, idemBenchNamespace, msg, idemBenchTTL)
			if err != nil {
				atomic.AddInt64(&processingErr, 1)
				return
			}
			if claim.AlreadyProcessed {
				atomic.AddInt64(&alreadyDone, 1)
				return
			}
			// 模拟业务执行成功后 markDone
			atomic.AddInt64(&claimedFirstTime, 1)
			if err := MarkMessageProcessed(ctx, rdb, claim.Key, idemBenchTTL); err != nil {
				t.Errorf("MarkMessageProcessed failed: %v", err)
			}
		}()
	}
	wg.Wait()

	if claimedFirstTime != 1 {
		t.Fatalf("expected exactly 1 goroutine to acquire processing right, got %d", claimedFirstTime)
	}
	if claimedFirstTime+alreadyDone+processingErr != goroutines {
		t.Fatalf("counters don't sum: first=%d done=%d err=%d total=%d",
			claimedFirstTime, alreadyDone, processingErr, goroutines)
	}
	t.Logf("idempotency convergence: goroutines=%d firstClaim=%d alreadyDone=%d processingConflict=%d",
		goroutines, claimedFirstTime, alreadyDone, processingErr)
}
