package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"ride-sharing/shared/contracts"
	"ride-sharing/shared/messaging"

	"github.com/rabbitmq/amqp091-go"
)

// maxFanoutCandidates 限制每单 trip_request 广播给多少个候选司机。
// 取 2：结构顶 = 1−(1-p)^2 = 75%（司机 p=50% 接单），换取 2× 而非 5× 的
// 后端消息放大，缓解单机 trip-service 在全候选 fan-out 下被压饱和的问题
// （Round 2 全量 ~5 候选时 start p99 飙到 34s 即此故障）。
const maxFanoutCandidates = 2

type tripConsumer struct {
	rabbitmq *messaging.RabbitMQ
	service  *Service
}

func NewTripConsumer(rabbitmq *messaging.RabbitMQ, service *Service) *tripConsumer {
	return &tripConsumer{
		rabbitmq: rabbitmq,
		service:  service,
	}
}

func (c *tripConsumer) Listen() error {
	return c.rabbitmq.ConsumeMessages(messaging.FindAvailableDriversQueue, func(ctx context.Context, msg amqp091.Delivery) error {
		var tripEvent contracts.AmqpMessage
		if err := json.Unmarshal(msg.Body, &tripEvent); err != nil {
			log.Printf("Failed to unmarshal message: %v", err)
			return err
		}

		var payload messaging.TripEventData
		if err := json.Unmarshal(tripEvent.Data, &payload); err != nil {
			log.Printf("Failed to unmarshal message: %v", err)
			return err
		}

		log.Printf("driver received message: %+v", payload)

		switch msg.RoutingKey {
		case contracts.TripEventCreated, contracts.TripEventDriverNotInterested:
			return c.handleFindAndNotifyDrivers(ctx, payload)
		}

		log.Printf("unknown trip event: %+v", payload)

		return nil
	})
}

func (c *tripConsumer) handleFindAndNotifyDrivers(ctx context.Context, payload messaging.TripEventData) error {
	packageSlug := payload.Trip.SelectedFare.PackageSlug

	// 优先使用订单起点做地理检索，匹配“最近的同车型司机”。
	// 如果消息里没有有效坐标（例如某些历史事件数据不完整），则回退到集合抽样逻辑。
	var suitableIDs []string
	lat, lng, hasPickup := extractPickupLocation(payload)

	var err error
	if hasPickup {
		suitableIDs, err = c.service.FindNearbyDrivers(ctx, lat, lng, defaultGeoSearchRadiusKm, packageSlug)
		if err != nil {
			log.Printf("failed to find nearby drivers from redis geo: %v", err)
			return err
		}
	} else {
		suitableIDs, err = c.service.FindAvailableDrivers(ctx, packageSlug)
		if err != nil {
			log.Printf("failed to fallback find available drivers from redis set: %v", err)
			return err
		}
	}

	log.Printf("Found suitable drivers %v", len(suitableIDs))

	if len(suitableIDs) == 0 {
		// Notify the driver that no drivers are available
		if err := c.rabbitmq.PublishMessage(ctx, contracts.TripEventNoDriversFound, contracts.AmqpMessage{
			OwnerID: payload.Trip.UserID,
		}); err != nil {
			log.Printf("Failed to publish message to exchange: %v", err)
			return err
		}

		return nil
	}

	// Fan-out：向**前 maxFanoutCandidates(=2) 个**候选司机广播 trip_request，
	// 谁先 accept 谁得（first-accept-wins）。trip-service driver_consumer 用
	// Redis 接单锁 + DB 条件更新（status=pending）保证只有一个司机真正成单，
	// 输家被状态幂等守卫静默跳过。
	//
	// 取代原“随机挑 1 个、不重试”：单发命中率受司机接单率天花板压制
	// （50% 接单 → 派单成功率 ≤50%）；fan-out 2 个 → 结构上限升到 1−0.5²=75%，
	// 同时把后端放大控制在 2×（而非全候选~5× 压垮单机 trip-service）。
	//
	// 单个司机 publish 失败只记日志并继续，不因一个坏司机拖垮整批；
	// 仅当全部候选都 publish 失败才返回错误触发上层重试。
	marshalledEvent, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	candidates := suitableIDs
	if len(candidates) > maxFanoutCandidates {
		candidates = candidates[:maxFanoutCandidates]
	}

	publishedCount := 0
	for _, driverID := range candidates {
		if err := c.rabbitmq.PublishMessage(ctx, contracts.DriverCmdTripRequest, contracts.AmqpMessage{
			OwnerID: driverID,
			Data:    marshalledEvent,
		}); err != nil {
			log.Printf("Failed to publish trip_request to driver %s: %v", driverID, err)
			continue
		}
		publishedCount++
	}

	if publishedCount == 0 {
		return fmt.Errorf("failed to publish trip_request to any of %d candidate drivers", len(candidates))
	}

	return nil
}

// extractPickupLocation 从 trip payload 中提取上车点坐标。
// 约定：route.geometry[0].coordinates[0] 为路线起点。
func extractPickupLocation(payload messaging.TripEventData) (lat float64, lng float64, ok bool) {
	if payload.Trip == nil || payload.Trip.Route == nil || len(payload.Trip.Route.Geometry) == 0 {
		return 0, 0, false
	}

	geometry := payload.Trip.Route.Geometry[0]
	if geometry == nil || len(geometry.Coordinates) == 0 || geometry.Coordinates[0] == nil {
		return 0, 0, false
	}

	pickup := geometry.Coordinates[0]
	return pickup.Latitude, pickup.Longitude, true
}
