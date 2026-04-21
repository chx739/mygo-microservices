package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"ride-sharing/services/trip-service/internal/domain"
	tripTypes "ride-sharing/services/trip-service/pkg/types"
	"ride-sharing/shared/cache"
	"ride-sharing/shared/env"
	pbd "ride-sharing/shared/proto/driver"
	"ride-sharing/shared/proto/trip"
	"ride-sharing/shared/types"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	// tripCacheTTL 定义行程缓存的过期时间。
	// 行程状态在短时间内读取频繁、写入较少，10 分钟是一个兼顾命中率与一致性的默认值。
	tripCacheTTL = 10 * time.Minute

	// tripBloomFilterKey 是 Trip ID 布隆过滤器的 Redis key。
	// 用于快速判断“请求的 tripID 是否可能存在”，减少无效请求穿透到 MongoDB。
	tripBloomFilterKey = "bloom:trips"
)

type service struct {
	repo domain.TripRepository
	rdb  redis.UniversalClient
}

func NewService(repo domain.TripRepository, redisClient redis.UniversalClient) *service {
	return &service{
		repo: repo,
		rdb:  redisClient,
	}
}

func (s *service) CreateTrip(ctx context.Context, fare *domain.RideFareModel) (*domain.TripModel, error) {
	t := &domain.TripModel{
		ID:       primitive.NewObjectID(),
		UserID:   fare.UserID,
		Status:   "pending",
		RideFare: fare,
		Driver:   &trip.TripDriver{},
	}

	createdTrip, err := s.repo.CreateTrip(ctx, t)
	if err != nil {
		return nil, err
	}

	// Trip 创建成功后把 tripID 写入布隆过滤器。
	// 这是防穿透链路的“正样本来源”：后续查询时能快速判断“可能存在”。
	// 注意：布隆写入失败不阻断主流程，避免 Redis 短暂抖动影响核心下单成功率。
	if s.rdb != nil {
		if bloomErr := cache.BloomAdd(ctx, s.rdb, tripBloomFilterKey, createdTrip.ID.Hex()); bloomErr != nil {
			log.Printf("trip bloom add failed for %s: %v", createdTrip.ID.Hex(), bloomErr)
		}
	}

	return createdTrip, nil
}

func (s *service) GetRoute(ctx context.Context, pickup, destination *types.Coordinate, useOSRMApi bool) (*tripTypes.OsrmApiResponse, error) {
	if !useOSRMApi {
		// Return a simple mock response in case we don't want to rely on an external API
		return &tripTypes.OsrmApiResponse{
			Routes: []struct {
				Distance float64 `json:"distance"`
				Duration float64 `json:"duration"`
				Geometry struct {
					Coordinates [][]float64 `json:"coordinates"`
				} `json:"geometry"`
			}{
				{
					Distance: 5.0, // 5km
					Duration: 600, // 10 minutes
					Geometry: struct {
						Coordinates [][]float64 `json:"coordinates"`
					}{
						Coordinates: [][]float64{
							{pickup.Latitude, pickup.Longitude},
							{destination.Latitude, destination.Longitude},
						},
					},
				},
			},
		}, nil
	}

	// or use our self hosted API (check the course lesson: "Preparing for External API Failures")
	baseURL := env.GetString("OSRM_API", "https://osrm.selfmadeengineer.com")

	url := fmt.Sprintf(
		"%s/route/v1/driving/%f,%f;%f,%f?overview=full&geometries=geojson",
		baseURL,
		pickup.Longitude, pickup.Latitude,
		destination.Longitude, destination.Latitude,
	)

	log.Printf("Started Fetching from OSRM API: URL: %s", url)

	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch route from OSRM API: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read the response: %v", err)
	}

	log.Printf("Got response from OSRM API %s", string(body))

	var routeResp tripTypes.OsrmApiResponse
	if err := json.Unmarshal(body, &routeResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %v", err)
	}

	return &routeResp, nil
}

func (s *service) EstimatePackagesPriceWithRoute(route *tripTypes.OsrmApiResponse) []*domain.RideFareModel {
	baseFares := getBaseFares()
	estimatedFares := make([]*domain.RideFareModel, len(baseFares))

	for i, f := range baseFares {
		estimatedFares[i] = estimateFareRoute(f, route)
	}

	return estimatedFares
}

func (s *service) GenerateTripFares(ctx context.Context, rideFares []*domain.RideFareModel, userID string, route *tripTypes.OsrmApiResponse) ([]*domain.RideFareModel, error) {
	// 压测专用（方案 § 5）：Mongo Atlas 远端 insert 单次 ~1s，把 preview 顶成外部延迟瓶颈。
	// 改走 Redis（in-cluster ~1ms）存 fare，GetAndValidateFare 也先查 Redis。
	// CreateTrip 内嵌 fare，不再依赖 Mongo rideFares 集合。loadtest 后 git restore。
	fares := make([]*domain.RideFareModel, len(rideFares))
	errs := make([]error, len(rideFares))
	var wg sync.WaitGroup

	for i, f := range rideFares {
		wg.Add(1)
		go func(i int, f *domain.RideFareModel) {
			defer wg.Done()
			fare := &domain.RideFareModel{
				UserID:            userID,
				ID:                primitive.NewObjectID(),
				TotalPriceInCents: f.TotalPriceInCents,
				PackageSlug:       f.PackageSlug,
				Route:             route,
			}
			if s.rdb != nil {
				b, mErr := json.Marshal(fare)
				if mErr != nil {
					errs[i] = fmt.Errorf("marshal fare: %w", mErr)
					return
				}
				if err := s.rdb.Set(ctx, fareCacheKey(fare.ID.Hex()), b, fareCacheTTL).Err(); err != nil {
					errs[i] = fmt.Errorf("redis save fare: %w", err)
					return
				}
				fares[i] = fare
				return
			}
			if err := s.repo.SaveRideFare(ctx, fare); err != nil {
				errs[i] = fmt.Errorf("failed to save trip fare: %w", err)
				return
			}
			fares[i] = fare
		}(i, f)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return fares, nil
}

func fareCacheKey(id string) string { return "fare:" + id }

const fareCacheTTL = 10 * time.Minute

func (s *service) GetAndValidateFare(ctx context.Context, fareID, userID string) (*domain.RideFareModel, error) {
	// 压测专用（方案 § 5）：先查 Redis，miss 再 fallback Mongo。
	// GenerateTripFares 已改走 Redis，这里保持对称；loadtest 后 git restore。
	var fare *domain.RideFareModel
	if s.rdb != nil {
		raw, rErr := s.rdb.Get(ctx, fareCacheKey(fareID)).Bytes()
		if rErr == nil && len(raw) > 0 {
			var cached domain.RideFareModel
			if jErr := json.Unmarshal(raw, &cached); jErr == nil {
				fare = &cached
			}
		}
	}
	if fare == nil {
		f, err := s.repo.GetRideFareByID(ctx, fareID)
		if err != nil {
			return nil, fmt.Errorf("failed to get trip fare: %w", err)
		}
		fare = f
	}

	if fare == nil {
		return nil, fmt.Errorf("fare does not exist")
	}

	// User fare validation (user is owner of this fare?)
	if userID != fare.UserID {
		return nil, fmt.Errorf("fare does not belong to the user")
	}

	return fare, nil
}

func estimateFareRoute(f *domain.RideFareModel, route *tripTypes.OsrmApiResponse) *domain.RideFareModel {
	pricingCfg := tripTypes.DefaultPricingConfig()
	carPackagePrice := f.TotalPriceInCents

	distanceKm := route.Routes[0].Distance
	durationInMinutes := route.Routes[0].Duration

	distanceFare := distanceKm * pricingCfg.PricePerUnitOfDistance
	timeFare := durationInMinutes * pricingCfg.PricingPerMinute
	totalPrice := carPackagePrice + distanceFare + timeFare

	return &domain.RideFareModel{
		TotalPriceInCents: totalPrice,
		PackageSlug:       f.PackageSlug,
	}
}

func getBaseFares() []*domain.RideFareModel {
	return []*domain.RideFareModel{
		{
			PackageSlug:       "suv",
			TotalPriceInCents: 200,
		},
		{
			PackageSlug:       "sedan",
			TotalPriceInCents: 350,
		},
		{
			PackageSlug:       "van",
			TotalPriceInCents: 400,
		},
		{
			PackageSlug:       "luxury",
			TotalPriceInCents: 1000,
		},
	}
}

func (s *service) GetTripByID(ctx context.Context, id string) (*domain.TripModel, error) {
	// 缓存是可选能力：当 Redis 未注入时，保持原行为（直接回源仓储）。
	if s.rdb == nil {
		return s.repo.GetTripByID(ctx, id)
	}

	// 先做一次 ObjectID 格式校验。
	// 非法 ID 可以直接判定“不存在”，避免无意义访问 Redis/Mongo。
	if _, objectIDErr := primitive.ObjectIDFromHex(id); objectIDErr != nil {
		return nil, fmt.Errorf("trip not found: %s", id)
	}

	// 第 0 步：布隆过滤器预检。
	// - miss（false）: 一定不存在，可直接拦截，防止穿透到 DB。
	// - hit（true）: 可能存在，继续走缓存/数据库查询。
	bloomHit, bloomErr := cache.BloomExists(ctx, s.rdb, tripBloomFilterKey, id)
	if bloomErr != nil {
		// 布隆检查失败时降级到原流程，保证业务可用性优先。
		log.Printf("trip bloom exists check failed for %s: %v", id, bloomErr)
	} else if !bloomHit {
		return nil, fmt.Errorf("trip not found: %s", id)
	}

	cacheKey := tripCacheKey(id)

	// 第 1 步：优先读缓存。
	// 命中则直接返回，避免重复访问 MongoDB。
	cached, err := s.rdb.Get(ctx, cacheKey).Bytes()
	if err == nil {
		var cachedTrip domain.TripModel
		if unmarshalErr := json.Unmarshal(cached, &cachedTrip); unmarshalErr == nil {
			return &cachedTrip, nil
		}

		// 缓存反序列化失败通常意味着历史脏数据或结构变更。
		// 这里删除坏缓存，防止后续请求持续命中错误数据。
		_ = s.rdb.Del(ctx, cacheKey).Err()
	} else if err != redis.Nil {
		// 非 nil/非 redis.Nil 的错误视为缓存层瞬时异常，降级回源而不阻断主流程。
		log.Printf("trip cache get failed for %s: %v", id, err)
	}

	// 第 2 步：缓存未命中或缓存异常，回源数据库。
	tripModel, repoErr := s.repo.GetTripByID(ctx, id)
	if repoErr != nil || tripModel == nil {
		return tripModel, repoErr
	}

	// 通过 DB 命中的真实数据反哺布隆过滤器，避免过滤器位图长期不完整。
	if bloomAddErr := cache.BloomAdd(ctx, s.rdb, tripBloomFilterKey, id); bloomAddErr != nil {
		log.Printf("trip bloom add-after-hit failed for %s: %v", id, bloomAddErr)
	}

	// 第 3 步：把回源结果写回缓存，提升后续请求命中率。
	// 缓存写入失败不影响主流程，避免 Redis 问题放大成业务不可用。
	marshalled, marshalErr := json.Marshal(tripModel)
	if marshalErr != nil {
		log.Printf("trip cache marshal failed for %s: %v", id, marshalErr)
		return tripModel, nil
	}

	if setErr := s.rdb.Set(ctx, cacheKey, marshalled, tripCacheTTL).Err(); setErr != nil {
		log.Printf("trip cache set failed for %s: %v", id, setErr)
	}

	return tripModel, nil
}

func (s *service) UpdateTrip(ctx context.Context, tripID string, status string, driver *pbd.Driver) error {
	if err := s.repo.UpdateTrip(ctx, tripID, status, driver); err != nil {
		return err
	}

	// Trip 状态更新后，立即删除缓存，避免读取到旧状态。
	// 采用“删除缓存”而不是“直接覆盖”，可避免不同写路径造成的缓存值分歧。
	if s.rdb != nil {
		if err := s.rdb.Del(ctx, tripCacheKey(tripID)).Err(); err != nil {
			log.Printf("trip cache delete failed for %s: %v", tripID, err)
		}
	}

	return nil
}

func tripCacheKey(id string) string {
	return "trip:" + id
}
