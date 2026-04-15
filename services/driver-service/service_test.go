package main

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestService 创建一个基于 miniredis 的 Service，
// 用于在单元测试中验证 Redis 读写逻辑，不依赖外部基础设施。
func newTestService(t *testing.T) (*Service, *miniredis.Miniredis) {
	t.Helper()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	service := NewService(client)

	return service, mr
}

func TestRegisterFindAndUnregisterDriver(t *testing.T) {
	svc, mr := newTestService(t)
	defer mr.Close()

	ctx := context.Background()
	driverID := "driver_001"
	packageSlug := "economy"

	// 1) 注册司机后，Hash 与 Set 都应该存在。
	driver, err := svc.RegisterDriver(ctx, driverID, packageSlug)
	if err != nil {
		t.Fatalf("RegisterDriver returned error: %v", err)
	}
	if driver == nil || driver.Id != driverID {
		t.Fatalf("unexpected driver returned: %+v", driver)
	}

	if !mr.Exists(driverInfoKey(driverID)) {
		t.Fatalf("driver info key not found: %s", driverInfoKey(driverID))
	}

	isMember, err := svc.rdb.SIsMember(ctx, driversOnlineKey(packageSlug), driverID).Result()
	if err != nil {
		t.Fatalf("failed to check online set membership: %v", err)
	}
	if !isMember {
		t.Fatalf("driver should be in online set")
	}

	// 2) 查询可用司机应能命中刚注册的司机。
	ids, err := svc.FindAvailableDrivers(ctx, packageSlug)
	if err != nil {
		t.Fatalf("FindAvailableDrivers returned error: %v", err)
	}
	if len(ids) == 0 {
		t.Fatalf("expected at least one available driver")
	}

	found := false
	for _, id := range ids {
		if id == driverID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("registered driver %s not found in result: %v", driverID, ids)
	}

	// 3) 构造一个脏成员：只存在于 Set，不存在于 Hash。
	ghostID := "ghost_driver"
	if err := svc.rdb.SAdd(ctx, driversOnlineKey(packageSlug), ghostID).Err(); err != nil {
		t.Fatalf("failed to add ghost member: %v", err)
	}

	if _, err := svc.FindAvailableDrivers(ctx, packageSlug); err != nil {
		t.Fatalf("FindAvailableDrivers returned error when cleaning ghost member: %v", err)
	}

	ghostMember, err := svc.rdb.SIsMember(ctx, driversOnlineKey(packageSlug), ghostID).Result()
	if err != nil {
		t.Fatalf("failed to verify ghost member cleanup: %v", err)
	}
	if ghostMember {
		t.Fatalf("ghost member should be lazily removed from online set")
	}

	// 4) 注销后，司机详情和在线集合成员都应被移除。
	if err := svc.UnregisterDriver(ctx, driverID, packageSlug); err != nil {
		t.Fatalf("UnregisterDriver returned error: %v", err)
	}

	if mr.Exists(driverInfoKey(driverID)) {
		t.Fatalf("driver info key should be deleted after unregister")
	}
	isMember, err = svc.rdb.SIsMember(ctx, driversOnlineKey(packageSlug), driverID).Result()
	if err != nil {
		t.Fatalf("failed to verify driver removal from online set: %v", err)
	}
	if isMember {
		t.Fatalf("driver should be removed from online set after unregister")
	}
}

func TestHeartbeatDriverRefreshTTL(t *testing.T) {
	svc, mr := newTestService(t)
	defer mr.Close()

	ctx := context.Background()
	driverID := "driver_heartbeat"
	packageSlug := "economy"

	if _, err := svc.RegisterDriver(ctx, driverID, packageSlug); err != nil {
		t.Fatalf("RegisterDriver returned error: %v", err)
	}

	// 先把 TTL 缩短到 1 秒，模拟临近过期的状态。
	mr.SetTTL(driverInfoKey(driverID), 1)

	if err := svc.HeartbeatDriver(ctx, driverID, packageSlug); err != nil {
		t.Fatalf("HeartbeatDriver returned error: %v", err)
	}

	// 心跳后应该重新变成接近 5 分钟 TTL。
	ttl := mr.TTL(driverInfoKey(driverID))
	if ttl < (299 * time.Second) {
		t.Fatalf("expected ttl to be refreshed close to 300s, got %v", ttl)
	}
}

func TestFindNearbyDriversByGeo(t *testing.T) {
	svc, mr := newTestService(t)
	defer mr.Close()

	ctx := context.Background()
	packageSlug := "economy"

	nearDriverID := "driver_near"
	farDriverID := "driver_far"

	if _, err := svc.RegisterDriver(ctx, nearDriverID, packageSlug); err != nil {
		t.Fatalf("failed to register near driver: %v", err)
	}
	if _, err := svc.RegisterDriver(ctx, farDriverID, packageSlug); err != nil {
		t.Fatalf("failed to register far driver: %v", err)
	}

	// 把司机位置更新到两个相距很远的城市，确保地理检索结果可预测。
	// near: 旧金山；far: 洛杉矶。
	if err := svc.UpdateDriverLocation(ctx, nearDriverID, packageSlug, 37.7749, -122.4194); err != nil {
		t.Fatalf("failed to update near driver location: %v", err)
	}
	if err := svc.UpdateDriverLocation(ctx, farDriverID, packageSlug, 34.0522, -118.2437); err != nil {
		t.Fatalf("failed to update far driver location: %v", err)
	}

	ids, err := svc.FindNearbyDrivers(ctx, 37.7749, -122.4194, 5, packageSlug)
	if err != nil {
		t.Fatalf("FindNearbyDrivers returned error: %v", err)
	}

	if !contains(ids, nearDriverID) {
		t.Fatalf("expected near driver %s in result, got %v", nearDriverID, ids)
	}
	if contains(ids, farDriverID) {
		t.Fatalf("did not expect far driver %s in nearby result, got %v", farDriverID, ids)
	}
}

func TestDriverMovementLifecycle(t *testing.T) {
	svc, mr := newTestService(t)
	defer mr.Close()

	ctx := context.Background()
	driverID := "driver_moving"
	packageSlug := "economy"

	// 为了让测试快速收敛，把定时上传周期缩短。
	svc.movementInterval = 10 * time.Millisecond

	driver, err := svc.RegisterDriver(ctx, driverID, packageSlug)
	if err != nil {
		t.Fatalf("RegisterDriver returned error: %v", err)
	}

	initialLat := driver.GetLocation().GetLatitude()
	initialLng := driver.GetLocation().GetLongitude()

	// 1) 注册后应在短时间内看到位置变化（定时上传生效）。
	eventually(t, 800*time.Millisecond, 20*time.Millisecond, func() bool {
		lat, lng, readErr := readDriverLatLng(ctx, svc, driverID)
		if readErr != nil {
			return false
		}

		return !almostEqualFloat64(lat, initialLat) || !almostEqualFloat64(lng, initialLng)
	})

	// 2) 注销后应停止更新并清理 Redis 数据。
	if err := svc.UnregisterDriver(ctx, driverID, packageSlug); err != nil {
		t.Fatalf("UnregisterDriver returned error: %v", err)
	}

	if mr.Exists(driverInfoKey(driverID)) {
		t.Fatalf("driver info key should be deleted after unregister")
	}

	if _, zErr := svc.rdb.ZScore(ctx, driversGeoKey(packageSlug), driverID).Result(); zErr != redis.Nil {
		t.Fatalf("driver should be removed from geo index after unregister, got err=%v", zErr)
	}

	// 再等待一段时间，确认后台 goroutine 已停止，不会把已删除数据写回来。
	time.Sleep(80 * time.Millisecond)
	if mr.Exists(driverInfoKey(driverID)) {
		t.Fatalf("driver info key should not be recreated after unregister")
	}

	svc.movementMu.Lock()
	_, exists := svc.driverMovements[driverID]
	svc.movementMu.Unlock()
	if exists {
		t.Fatalf("movement task should be removed after unregister")
	}
}

func readDriverLatLng(ctx context.Context, svc *Service, driverID string) (float64, float64, error) {
	latStr, err := svc.rdb.HGet(ctx, driverInfoKey(driverID), "lat").Result()
	if err != nil {
		return 0, 0, err
	}

	lngStr, err := svc.rdb.HGet(ctx, driverInfoKey(driverID), "lng").Result()
	if err != nil {
		return 0, 0, err
	}

	lat, err := strconv.ParseFloat(latStr, 64)
	if err != nil {
		return 0, 0, err
	}

	lng, err := strconv.ParseFloat(lngStr, 64)
	if err != nil {
		return 0, 0, err
	}

	return lat, lng, nil
}

func eventually(t *testing.T, timeout time.Duration, interval time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(interval)
	}

	t.Fatalf("condition not met within timeout: %v", timeout)
}

func almostEqualFloat64(a, b float64) bool {
	const epsilon = 1e-9
	return math.Abs(a-b) <= epsilon
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}

	return false
}
