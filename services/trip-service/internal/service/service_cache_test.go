package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"ride-sharing/shared/cache"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"ride-sharing/services/trip-service/internal/domain"
	pbd "ride-sharing/shared/proto/driver"
)

// mockTripRepository 是 TripRepository 的测试替身。
// 通过计数器我们可以验证“是否回源数据库”，从而确认缓存是否命中。
//
// dbLatency 仅用于多级缓存对比基准（service_cache_compare_test.go）：
// 在 GetTripByID 中 time.Sleep 模拟 Mongo Atlas 跨网络延迟。
// 默认 0 不 sleep，旧测试不受影响。
type mockTripRepository struct {
	trips           map[string]*domain.TripModel
	getTripCalls    int
	updateTripCalls int
	updateTripErr   error
	dbLatency       time.Duration
}

func (m *mockTripRepository) CreateTrip(_ context.Context, trip *domain.TripModel) (*domain.TripModel, error) {
	return trip, nil
}

func (m *mockTripRepository) SaveRideFare(_ context.Context, _ *domain.RideFareModel) error {
	return nil
}

func (m *mockTripRepository) GetRideFareByID(_ context.Context, _ string) (*domain.RideFareModel, error) {
	return nil, nil
}

func (m *mockTripRepository) GetTripByID(_ context.Context, id string) (*domain.TripModel, error) {
	m.getTripCalls++
	if m.dbLatency > 0 {
		time.Sleep(m.dbLatency)
	}
	trip, ok := m.trips[id]
	if !ok {
		return nil, fmt.Errorf("trip not found: %s", id)
	}
	return trip, nil
}

func (m *mockTripRepository) UpdateTrip(_ context.Context, _ string, _ string, _ *pbd.Driver) error {
	m.updateTripCalls++
	return m.updateTripErr
}

// newTestServiceWithRedis 返回带 miniredis 的 service，避免依赖外部 Redis。
func newTestServiceWithRedis(t *testing.T, repo *mockTripRepository) (*service, *miniredis.Miniredis) {
	t.Helper()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		_ = rdb.Close()
		mr.Close()
	})

	return NewService(repo, rdb, nil), mr
}

// newTestServiceWithL1L2 同时启用 miniredis 和真实 ristretto L1，便于验证 L1 命中路径。
func newTestServiceWithL1L2(t *testing.T, repo *mockTripRepository) (*service, *miniredis.Miniredis, *cache.LocalCache) {
	t.Helper()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis failed: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	lc, err := cache.NewLocalCache(1000, 1<<20)
	if err != nil {
		_ = rdb.Close()
		mr.Close()
		t.Fatalf("NewLocalCache failed: %v", err)
	}

	t.Cleanup(func() {
		lc.Close()
		_ = rdb.Close()
		mr.Close()
	})

	return NewService(repo, rdb, lc), mr, lc
}

func TestGetTripByID_CacheMissThenHit(t *testing.T) {
	ctx := context.Background()
	tripID := primitive.NewObjectID().Hex()

	repo := &mockTripRepository{
		trips: map[string]*domain.TripModel{
			tripID: {
				ID:     primitive.NewObjectID(),
				UserID: "user-1",
				Status: "pending",
				RideFare: &domain.RideFareModel{
					ID:                primitive.NewObjectID(),
					UserID:            "user-1",
					PackageSlug:       "sedan",
					TotalPriceInCents: 100,
				},
			},
		},
	}

	svc, mr := newTestServiceWithRedis(t, repo)

	if err := cache.BloomAdd(ctx, svc.rdb, tripBloomFilterKey, tripID); err != nil {
		t.Fatalf("failed to seed bloom filter: %v", err)
	}

	// 第一次读取：缓存未命中，应当回源 repo，并写入缓存。
	trip, err := svc.GetTripByID(ctx, tripID)
	if err != nil {
		t.Fatalf("first GetTripByID returned error: %v", err)
	}
	if trip == nil || trip.UserID != "user-1" {
		t.Fatalf("unexpected trip from first query: %+v", trip)
	}
	if repo.getTripCalls != 1 {
		t.Fatalf("expected repo call count = 1 after cache miss, got %d", repo.getTripCalls)
	}
	if !mr.Exists(tripCacheKey(tripID)) {
		t.Fatalf("expected cache key %s to be created", tripCacheKey(tripID))
	}

	// 第二次读取：应命中缓存，不再回源 repo。
	trip, err = svc.GetTripByID(ctx, tripID)
	if err != nil {
		t.Fatalf("second GetTripByID returned error: %v", err)
	}
	if trip == nil || trip.UserID != "user-1" {
		t.Fatalf("unexpected trip from second query: %+v", trip)
	}
	if repo.getTripCalls != 1 {
		t.Fatalf("expected repo call count to remain 1 after cache hit, got %d", repo.getTripCalls)
	}
}

func TestUpdateTrip_InvalidatesTripCache(t *testing.T) {
	ctx := context.Background()
	tripID := primitive.NewObjectID().Hex()

	repo := &mockTripRepository{
		trips: map[string]*domain.TripModel{
			tripID: {
				ID:     primitive.NewObjectID(),
				UserID: "user-2",
				Status: "accepted",
				RideFare: &domain.RideFareModel{
					ID:                primitive.NewObjectID(),
					UserID:            "user-2",
					PackageSlug:       "van",
					TotalPriceInCents: 200,
				},
			},
		},
	}

	svc, mr := newTestServiceWithRedis(t, repo)

	if err := cache.BloomAdd(ctx, svc.rdb, tripBloomFilterKey, tripID); err != nil {
		t.Fatalf("failed to seed bloom filter: %v", err)
	}

	// 先读取一次，把 trip 写进缓存。
	if _, err := svc.GetTripByID(ctx, tripID); err != nil {
		t.Fatalf("warm cache GetTripByID returned error: %v", err)
	}
	if !mr.Exists(tripCacheKey(tripID)) {
		t.Fatalf("expected cache key %s before update", tripCacheKey(tripID))
	}

	// 更新后应删除缓存，避免后续读到旧状态。
	if err := svc.UpdateTrip(ctx, tripID, "payed", nil); err != nil {
		t.Fatalf("UpdateTrip returned error: %v", err)
	}
	if repo.updateTripCalls != 1 {
		t.Fatalf("expected updateTripCalls = 1, got %d", repo.updateTripCalls)
	}
	if mr.Exists(tripCacheKey(tripID)) {
		t.Fatalf("expected cache key %s to be deleted after update", tripCacheKey(tripID))
	}
}

func TestCreateTrip_AddsTripIDToBloomFilter(t *testing.T) {
	ctx := context.Background()

	repo := &mockTripRepository{trips: map[string]*domain.TripModel{}}
	svc, _ := newTestServiceWithRedis(t, repo)

	fare := &domain.RideFareModel{
		ID:                primitive.NewObjectID(),
		UserID:            "user-bloom",
		PackageSlug:       "sedan",
		TotalPriceInCents: 320,
	}

	createdTrip, err := svc.CreateTrip(ctx, fare)
	if err != nil {
		t.Fatalf("CreateTrip returned error: %v", err)
	}

	if createdTrip == nil {
		t.Fatalf("CreateTrip returned nil trip")
	}

	exists, err := cache.BloomExists(ctx, svc.rdb, tripBloomFilterKey, createdTrip.ID.Hex())
	if err != nil {
		t.Fatalf("BloomExists returned error: %v", err)
	}
	if !exists {
		t.Fatalf("created trip id should be present in bloom filter")
	}
}

// TestGetTripByID_L1HitDoesNotTouchRepoOrRedis 验证：当 L1 命中时，
// 既不查 Redis 也不查 repo——L1 是真正的第一道返回路径。
// 做法：第一次正常查询预热 L1；之后关掉 miniredis 模拟 Redis 不可达，
// 把 repo.getTripCalls 计数固定下来；第二次查询若仍能返回正确值且计数不变，
// 证明 L1 命中独立于 L2/DB。
func TestGetTripByID_L1HitDoesNotTouchRepoOrRedis(t *testing.T) {
	ctx := context.Background()
	tripID := primitive.NewObjectID().Hex()

	repo := &mockTripRepository{
		trips: map[string]*domain.TripModel{
			tripID: {
				ID:     primitive.NewObjectID(),
				UserID: "user-l1",
				Status: "pending",
				RideFare: &domain.RideFareModel{
					ID:                primitive.NewObjectID(),
					UserID:            "user-l1",
					PackageSlug:       "sedan",
					TotalPriceInCents: 100,
				},
			},
		},
	}

	svc, mr, lc := newTestServiceWithL1L2(t, repo)

	if err := cache.BloomAdd(ctx, svc.rdb, tripBloomFilterKey, tripID); err != nil {
		t.Fatalf("failed to seed bloom filter: %v", err)
	}

	// 第一次：正常走 Bloom→Redis→repo，回填 L1+L2。
	if _, err := svc.GetTripByID(ctx, tripID); err != nil {
		t.Fatalf("warm GetTripByID returned error: %v", err)
	}
	if repo.getTripCalls != 1 {
		t.Fatalf("expected repo call=1 after first miss, got %d", repo.getTripCalls)
	}

	// ristretto Set 是异步 buffer，Wait 后 L1 才一定命中。
	lc.Wait()

	// 关掉 miniredis：模拟 L2 整体不可达；同时把 repo 计数当前值锁定。
	mr.Close()
	repoCallsBeforeL1Hit := repo.getTripCalls

	// 第二次：L1 应命中并直接返回；不应该走到（已关闭的）Redis 或 repo。
	trip, err := svc.GetTripByID(ctx, tripID)
	if err != nil {
		t.Fatalf("L1-hit GetTripByID returned error: %v", err)
	}
	if trip == nil || trip.UserID != "user-l1" {
		t.Fatalf("unexpected trip from L1 hit: %+v", trip)
	}
	if repo.getTripCalls != repoCallsBeforeL1Hit {
		t.Fatalf("expected repo not to be called on L1 hit, before=%d after=%d",
			repoCallsBeforeL1Hit, repo.getTripCalls)
	}
}

func TestGetTripByID_BloomMissShortCircuit(t *testing.T) {
	ctx := context.Background()
	tripID := primitive.NewObjectID().Hex()

	repo := &mockTripRepository{
		trips: map[string]*domain.TripModel{
			tripID: {
				ID:     primitive.NewObjectID(),
				UserID: "user-miss",
				Status: "pending",
				RideFare: &domain.RideFareModel{
					ID:                primitive.NewObjectID(),
					UserID:            "user-miss",
					PackageSlug:       "suv",
					TotalPriceInCents: 300,
				},
			},
		},
	}

	svc, _ := newTestServiceWithRedis(t, repo)

	// 不向布隆过滤器写入 tripID，模拟“明确不存在”的快速拦截路径。
	trip, err := svc.GetTripByID(ctx, tripID)
	if err == nil {
		t.Fatalf("expected bloom miss to short-circuit with not found error")
	}
	if trip != nil {
		t.Fatalf("expected nil trip on bloom miss")
	}
	if repo.getTripCalls != 0 {
		t.Fatalf("expected repo not to be called on bloom miss, got %d", repo.getTripCalls)
	}
}
