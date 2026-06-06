package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"ride-sharing/services/trip-service/internal/domain"
	"ride-sharing/shared/cache"
)

// TestGetTripByID_SingleflightCoalescesConcurrentMisses 验证缓存击穿防护：
// L1+L2 同时冷的瞬间，N 个并发 GetTripByID 同一 tripID 应通过 singleflight 合并成
// 1 次 repo.GetTripByID 调用，其他 waiter 等同一份结果。
//
// 制造并发窗口的关键：mockTripRepository.dbLatency = 50ms，给 50 个 goroutine
// 足够时间一起堆到 sfg.Do，再让 leader 单独慢慢打 DB。
//
// 该测试与 service_cache_compare_test.go（顺序 workload）互补——后者跑不到 sf 路径。
func TestGetTripByID_SingleflightCoalescesConcurrentMisses(t *testing.T) {
	ctx := context.Background()
	tripID := primitive.NewObjectID().Hex()

	repo := &mockTripRepository{
		trips: map[string]*domain.TripModel{
			tripID: {
				ID:     primitive.NewObjectID(),
				UserID: "user-sf",
				Status: "pending",
				RideFare: &domain.RideFareModel{
					ID:                primitive.NewObjectID(),
					UserID:            "user-sf",
					PackageSlug:       "sedan",
					TotalPriceInCents: 100,
				},
			},
		},
		dbLatency: 50 * time.Millisecond,
	}

	svc, _ := newTestServiceWithRedis(t, repo)
	if err := cache.BloomAdd(ctx, svc.rdb, tripBloomFilterKey, tripID); err != nil {
		t.Fatalf("BloomAdd failed: %v", err)
	}

	const N = 50

	// 用 ready chan 让所有 goroutine 起跑线对齐，最大化并发挤进 sfg.Do 的概率。
	ready := make(chan struct{})
	var wg sync.WaitGroup
	var errCount atomic.Int32
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-ready
			tr, err := svc.GetTripByID(ctx, tripID)
			if err != nil {
				t.Errorf("GetTripByID returned error: %v", err)
				errCount.Add(1)
				return
			}
			if tr == nil || tr.UserID != "user-sf" {
				t.Errorf("unexpected trip: %+v", tr)
				errCount.Add(1)
			}
		}()
	}
	close(ready)
	wg.Wait()

	if errCount.Load() > 0 {
		t.Fatalf("%d goroutines failed", errCount.Load())
	}

	// 核心断言：50 个并发只触发 1 次 DB 调用。
	if repo.getTripCalls != 1 {
		t.Fatalf("expected exactly 1 DB call after singleflight coalescing of %d concurrent reads, got %d",
			N, repo.getTripCalls)
	}
}

// TestGetTripByID_SingleflightAllowsSequentialCacheHits 验证：合并完成后，
// 后续顺序读应直接走 L1/L2 命中路径，repo 不再被调用（singleflight 不会留下副作用）。
func TestGetTripByID_SingleflightAllowsSequentialCacheHits(t *testing.T) {
	ctx := context.Background()
	tripID := primitive.NewObjectID().Hex()

	repo := &mockTripRepository{
		trips: map[string]*domain.TripModel{
			tripID: {
				ID:     primitive.NewObjectID(),
				UserID: "user-sf-2",
				Status: "pending",
				RideFare: &domain.RideFareModel{
					ID:                primitive.NewObjectID(),
					UserID:            "user-sf-2",
					PackageSlug:       "sedan",
					TotalPriceInCents: 100,
				},
			},
		},
	}

	svc, _ := newTestServiceWithRedis(t, repo)
	if err := cache.BloomAdd(ctx, svc.rdb, tripBloomFilterKey, tripID); err != nil {
		t.Fatalf("BloomAdd failed: %v", err)
	}

	// 第一次：冷读穿透到 repo，写入 L1+L2。
	if _, err := svc.GetTripByID(ctx, tripID); err != nil {
		t.Fatalf("first GetTripByID: %v", err)
	}
	if repo.getTripCalls != 1 {
		t.Fatalf("expected 1 DB call after first miss, got %d", repo.getTripCalls)
	}

	// 第二/第三次：缓存命中，repo 不再被调用。
	for i := 0; i < 2; i++ {
		if _, err := svc.GetTripByID(ctx, tripID); err != nil {
			t.Fatalf("warm GetTripByID #%d: %v", i+2, err)
		}
	}
	if repo.getTripCalls != 1 {
		t.Fatalf("expected DB call count to stay at 1 after cache hits, got %d", repo.getTripCalls)
	}
}
