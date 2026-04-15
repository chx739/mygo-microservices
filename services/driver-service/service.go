package main

import (
	"context"
	"fmt"
	"log"
	math "math/rand/v2"
	pb "ride-sharing/shared/proto/driver"
	"ride-sharing/shared/util"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// driverInfoTTL 表示司机信息在 Redis 中的存活时间。
	// 如果司机在这个时间内没有心跳续期，会被认为离线。
	driverInfoTTL = 5 * time.Minute

	// findCandidateCount 控制每次从在线集合中随机抽样的候选数量。
	// 这里略大于最终返回数量，是为了在清理脏数据时仍有足够候选可选。
	findCandidateCount int64 = 8

	// findResultLimit 表示对外返回的最多司机数量。
	findResultLimit = 5

	// defaultGeoSearchRadiusKm 表示默认的附近司机搜索半径（单位：公里）。
	// 该值可按业务需要调整，例如城区用 3km，郊区可放宽到 5-8km。
	defaultGeoSearchRadiusKm = 3.0

	// driverMovementUpdateInterval 定义司机位置模拟上报间隔。
	// 这里默认 3 秒，能在 Demo 中明显看到位置变化，同时不会给 Redis 带来高频写压力。
	driverMovementUpdateInterval = 3 * time.Second
)

type Service struct {
	// rdb 是 driver-service 唯一的数据源。
	// 司机注册信息、在线集合都从这里读写，确保多实例状态一致。
	rdb redis.UniversalClient

	// movementMu 保护 driverMovements，避免并发注册/注销时出现 map 读写冲突。
	movementMu sync.Mutex

	// driverMovements 记录每个司机对应的位置模拟任务。
	// key: driverID
	// value: 取消函数 + 退出通知通道（用于 stop 时等待 goroutine 真正退出）。
	driverMovements map[string]driverMovementTask

	// movementInterval 允许测试环境把定时周期调短，减少等待时间。
	// 生产默认值使用 driverMovementUpdateInterval。
	movementInterval time.Duration
}

// driverMovementTask 表示一个司机位置模拟任务的控制句柄。
// - cancel: 主动停止该司机的后台位置更新任务。
// - done: goroutine 退出时 close，用于等待安全停止，避免数据回写竞态。
type driverMovementTask struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func NewService(rdb redis.UniversalClient) *Service {
	return &Service{
		rdb:             rdb,
		driverMovements: make(map[string]driverMovementTask),
		movementInterval: driverMovementUpdateInterval,
	}
}

// RegisterDriver 把司机基础信息写入 Redis，并加入对应车型的在线集合。
// 这样即使 driver-service 横向扩容，所有实例看到的在线司机状态也一致。
func (s *Service) RegisterDriver(ctx context.Context, driverID string, packageSlug string) (*pb.Driver, error) {
	if driverID == "" {
		return nil, fmt.Errorf("driverID is required")
	}
	if packageSlug == "" {
		return nil, fmt.Errorf("packageSlug is required")
	}

	randomIndex := math.IntN(len(PredefinedRoutes))
	randomRoute := PredefinedRoutes[randomIndex]

	randomPlate := GenerateRandomPlate()
	randomAvatar := util.GetRandomAvatar(randomIndex)

	driver := &pb.Driver{
		Id: driverID,
		// geohash 字段继续保留是为了兼容前端展示。
		// 这里不再依赖第三方 geohash 库，而是后续由 Redis GEOHASH 命令生成（可选）。
		Geohash:        "",
		Location:       &pb.Location{Latitude: randomRoute[0][0], Longitude: randomRoute[0][1]},
		Name:           "Lando Norris",
		PackageSlug:    packageSlug,
		ProfilePicture: randomAvatar,
		CarPlate:       randomPlate,
	}

	// 使用 Pipeline 合并多条命令，减少网络往返开销。
	// 这里一次性完成：写 Hash、设置过期、加入在线集合、写入 GEO 空间索引。
	pipe := s.rdb.Pipeline()
	pipe.HSet(ctx, driverInfoKey(driverID), map[string]any{
		"id":             driver.Id,
		"name":           driver.Name,
		"profilePicture": driver.ProfilePicture,
		"carPlate":       driver.CarPlate,
		"packageSlug":    driver.PackageSlug,
		"lat":            driver.Location.Latitude,
		"lng":            driver.Location.Longitude,
	})
	pipe.Expire(ctx, driverInfoKey(driverID), driverInfoTTL)
	pipe.SAdd(ctx, driversOnlineKey(packageSlug), driverID)
	pipe.GeoAdd(ctx, driversGeoKey(packageSlug), &redis.GeoLocation{
		Name:      driverID,
		Latitude:  driver.Location.Latitude,
		Longitude: driver.Location.Longitude,
	})

	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}

	// 如果 Redis 支持 GEOHASH，则生成 geohash 并写回 Hash。
	// 注意：geohash 仅用于展示，不影响匹配正确性，因此失败时不阻断主流程。
	geoHashes, geoErr := s.rdb.GeoHash(ctx, driversGeoKey(packageSlug), driverID).Result()
	if geoErr == nil && len(geoHashes) > 0 {
		driver.Geohash = geoHashes[0]
		_ = s.rdb.HSet(ctx, driverInfoKey(driverID), "geohash", driver.Geohash).Err()
	}

	// 注册成功后启动“定时位置上报”任务。
	// 注意：这里不能复用 gRPC 请求 ctx，因为请求返回后 ctx 会被取消，
	// 必须使用独立生命周期的后台 ctx，并在 UnregisterDriver 时显式停止。
	s.startDriverMovement(driverID, packageSlug, randomRoute)

	return driver, nil
}

// startDriverMovement 启动或替换指定司机的位置模拟任务。
// 设计要点：
// 1) 同一司机重复注册时，先停旧任务再启新任务，避免重复写位置。
// 2) 停旧任务时会等待 goroutine 退出，防止“旧任务晚到写回”覆盖新状态。
func (s *Service) startDriverMovement(driverID, packageSlug string, route [][]float64) {
	if driverID == "" || packageSlug == "" || len(route) == 0 {
		return
	}

	interval := s.movementInterval
	if interval <= 0 {
		interval = driverMovementUpdateInterval
	}

	var previousTask *driverMovementTask

	s.movementMu.Lock()
	if task, ok := s.driverMovements[driverID]; ok {
		t := task
		previousTask = &t
	}

	movementCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.driverMovements[driverID] = driverMovementTask{cancel: cancel, done: done}
	s.movementMu.Unlock()

	if previousTask != nil {
		previousTask.cancel()
		<-previousTask.done
	}

	go func() {
		defer close(done)
		s.simulateMovement(movementCtx, driverID, packageSlug, route, interval)
	}()
}

// stopDriverMovement 停止指定司机的位置模拟任务并等待退出。
// 返回值：
// - true: 确实存在并停止了任务。
// - false: 该司机当前没有运行中的任务。
func (s *Service) stopDriverMovement(driverID string) bool {
	if driverID == "" {
		return false
	}

	s.movementMu.Lock()
	task, ok := s.driverMovements[driverID]
	if ok {
		delete(s.driverMovements, driverID)
	}
	s.movementMu.Unlock()

	if !ok {
		return false
	}

	task.cancel()
	<-task.done
	return true
}

// simulateMovement 按路线循环上报司机坐标。
// 规则：
// - 注册时已写入 route[0]，这里从 route[1] 开始推进。
// - 每次 tick 同步更新 GEO 索引与 Hash 坐标（通过 UpdateDriverLocation 统一写入）。
// - 收到取消信号后立即退出，不再写入任何位置。
func (s *Service) simulateMovement(
	ctx context.Context,
	driverID string,
	packageSlug string,
	route [][]float64,
	interval time.Duration,
) {
	if len(route) == 0 {
		return
	}

	if interval <= 0 {
		interval = driverMovementUpdateInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	nextIndex := 1

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			point := route[nextIndex%len(route)]
			nextIndex++

			if len(point) < 2 {
				continue
			}

			// 给单次更新一个短超时，避免 Redis 异常时 goroutine 长时间阻塞。
			updateCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := s.UpdateDriverLocation(updateCtx, driverID, packageSlug, point[0], point[1])
			cancel()
			if err != nil {
				log.Printf("failed to update simulated driver location: driverID=%s err=%v", driverID, err)
			}
		}
	}
}

// UpdateDriverLocation 更新司机位置到 Redis GEO 索引，并同步更新 Hash 中的坐标。
// 该方法可被后续“司机实时上报位置”链路复用。
func (s *Service) UpdateDriverLocation(ctx context.Context, driverID string, packageSlug string, lat float64, lng float64) error {
	if driverID == "" || packageSlug == "" {
		return fmt.Errorf("driverID and packageSlug are required")
	}

	pipe := s.rdb.Pipeline()
	pipe.GeoAdd(ctx, driversGeoKey(packageSlug), &redis.GeoLocation{
		Name:      driverID,
		Latitude:  lat,
		Longitude: lng,
	})
	pipe.HSet(ctx, driverInfoKey(driverID), map[string]any{
		"lat": lat,
		"lng": lng,
	})
	pipe.Expire(ctx, driverInfoKey(driverID), driverInfoTTL)

	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}

	// geohash 依旧只用于展示，更新失败不影响主流程。
	geoHashes, geoErr := s.rdb.GeoHash(ctx, driversGeoKey(packageSlug), driverID).Result()
	if geoErr == nil && len(geoHashes) > 0 {
		_ = s.rdb.HSet(ctx, driverInfoKey(driverID), "geohash", geoHashes[0]).Err()
	}

	return nil
}

// HeartbeatDriver 在司机长连接在线期间被周期性调用。
// 作用：刷新司机信息 TTL，并确保司机仍在在线集合里，避免被误清理。
func (s *Service) HeartbeatDriver(ctx context.Context, driverID string, packageSlug string) error {
	if driverID == "" || packageSlug == "" {
		return fmt.Errorf("driverID and packageSlug are required")
	}

	pipe := s.rdb.Pipeline()
	pipe.SAdd(ctx, driversOnlineKey(packageSlug), driverID)
	pipe.Expire(ctx, driverInfoKey(driverID), driverInfoTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// FindAvailableDrivers 从指定车型在线集合里随机抽样可用司机。
// 关键点：会二次校验 driver:info 是否存在，并顺手清理 Set 里的脏成员。
func (s *Service) FindAvailableDrivers(ctx context.Context, packageType string) ([]string, error) {
	if packageType == "" {
		return []string{}, nil
	}

	ids, err := s.rdb.SRandMemberN(ctx, driversOnlineKey(packageType), findCandidateCount).Result()
	if err != nil {
		return nil, err
	}

	matchingDrivers := make([]string, 0, findResultLimit)

	for _, driverID := range ids {
		exists, existsErr := s.rdb.Exists(ctx, driverInfoKey(driverID)).Result()
		if existsErr != nil {
			return nil, existsErr
		}

		if exists == 1 {
			matchingDrivers = append(matchingDrivers, driverID)
			if len(matchingDrivers) == findResultLimit {
				break
			}
			continue
		}

		// Hash 已过期但 Set 还残留的情况属于脏数据。
		// 这里进行惰性清理，避免僵尸司机长期污染在线集合。
		_ = s.rdb.SRem(ctx, driversOnlineKey(packageType), driverID).Err()
	}

	if len(matchingDrivers) == 0 {
		return []string{}, nil
	}

	return matchingDrivers, nil
}

// FindNearbyDrivers 使用 Redis GEO 做“按距离”司机检索。
// 当前做法是按车型拆分 GEO key（geo:drivers:{packageSlug}），避免额外 HGET 过滤带来的 N+1 往返。
func (s *Service) FindNearbyDrivers(ctx context.Context, lat float64, lng float64, radiusKm float64, packageType string) ([]string, error) {
	if packageType == "" {
		return []string{}, nil
	}

	if radiusKm <= 0 {
		radiusKm = defaultGeoSearchRadiusKm
	}

	locations, err := s.rdb.GeoRadius(ctx, driversGeoKey(packageType), lng, lat, &redis.GeoRadiusQuery{
		Radius: radiusKm,
		Unit:   "km",
		Count:  int(findCandidateCount),
		Sort:   "ASC",
	}).Result()
	if err != nil {
		return nil, err
	}

	matched := make([]string, 0, findResultLimit)
	for _, loc := range locations {
		driverID := loc.Name

		exists, existsErr := s.rdb.Exists(ctx, driverInfoKey(driverID)).Result()
		if existsErr != nil {
			return nil, existsErr
		}

		if exists == 1 {
			matched = append(matched, driverID)
			if len(matched) == findResultLimit {
				break
			}
			continue
		}

		// 如果详情键不存在，说明是脏成员：同时清理 Set 与 GEO 索引。
		_ = s.rdb.SRem(ctx, driversOnlineKey(packageType), driverID).Err()
		_ = s.rdb.ZRem(ctx, driversGeoKey(packageType), driverID).Err()
	}

	if len(matched) == 0 {
		return []string{}, nil
	}

	return matched, nil
}

// UnregisterDriver 在司机主动下线时调用。
// 这里会同时删除司机详情和在线集合成员，避免下次匹配命中已下线司机。
func (s *Service) UnregisterDriver(ctx context.Context, driverID string, packageSlug string) error {
	if driverID == "" {
		return nil
	}

	// 先停止后台位置模拟任务，再删除 Redis 数据。
	// 这样可以避免“goroutine 仍在跑，刚删完又写回”的竞态问题。
	_ = s.stopDriverMovement(driverID)

	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, driverInfoKey(driverID))

	// 网关当前会把 packageSlug 传进来；如果未来没有传，也不会影响删除详情键。
	if packageSlug != "" {
		pipe.SRem(ctx, driversOnlineKey(packageSlug), driverID)
		// Redis GEO 底层是 zset，直接按成员删除即可。
		pipe.ZRem(ctx, driversGeoKey(packageSlug), driverID)
	}

	_, err := pipe.Exec(ctx)
	return err
}

// driverInfoKey 统一管理司机详情键，避免拼接散落在业务逻辑里。
func driverInfoKey(driverID string) string {
	return "driver:info:" + driverID
}

// driversOnlineKey 统一管理车型在线集合键，便于后续迁移和治理。
func driversOnlineKey(packageSlug string) string {
	return "drivers:online:" + packageSlug
}

// driversGeoKey 统一管理车型 GEO 索引键。
// 采用按车型拆 key 的方式，避免查询后再按车型过滤造成的 N+1 读取。
func driversGeoKey(packageSlug string) string {
	return "geo:drivers:" + packageSlug
}
