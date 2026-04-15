package main

import (
	"context"
	"encoding/json"
	"log"
	"math/rand"
	"ride-sharing/shared/contracts"
	"ride-sharing/shared/messaging"

	"github.com/rabbitmq/amqp091-go"
)

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

	// Get a random index from the matching drivers
	randomIndex := rand.Intn(len(suitableIDs))

	suitableDriverID := suitableIDs[randomIndex]

	marshalledEvent, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	// Notify the driver about a potential trip
	if err := c.rabbitmq.PublishMessage(ctx, contracts.DriverCmdTripRequest, contracts.AmqpMessage{
		OwnerID: suitableDriverID,
		Data:    marshalledEvent,
	}); err != nil {
		log.Printf("Failed to publish message to exchange: %v", err)
		return err
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
