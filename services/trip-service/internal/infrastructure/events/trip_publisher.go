package events

import (
	"context"
	"ride-sharing/shared/messaging"
)

type TripEventPublisher struct {
	rabbitmq *messaging.RabbitMQ
}

func NewTripPublisher(rabbitMQ *messaging.RabbitMQ) *TripEventPublisher {
	return &TripEventPublisher{
		rabbitmq: rabbitMQ,
	}
}

func (p *TripEventPublisher) PublishTripCreated(ctx context.Context) error {
	return p.rabbitmq.PublishMessage(ctx, "hello", "hello world")
}
