package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"ride-sharing/services/trip-service/internal/infrastructure/events"
	"ride-sharing/services/trip-service/internal/infrastructure/grpc"
	"ride-sharing/services/trip-service/internal/infrastructure/repository"
	"ride-sharing/services/trip-service/internal/service"
	"ride-sharing/shared/env"
	"ride-sharing/shared/messaging"
	"syscall"

	grpcserver "google.golang.org/grpc"
)

/*
curl -X POST http://127.0.0.1:8081/trip/preview \
  -H "Content-Type: application/json" \
  -d '{
    "userID": "12",
    "destination": {
      "latitude": 37.7960377140795,
      "longitude": -122.42859007644311
    },
    "pickup": {
      "latitude": 37.783678087129466,
      "longitude": -122.40777303207217
    }
  }'
*/

var GrpcAddr = ":9093"

func main() {

	inmemRepo := repository.NewInmemRepository()
	svc := service.NewService(inmemRepo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("Received signal: %v. Shutting down...", sig)
		cancel()
	}()

	lis, err := net.Listen("tcp", GrpcAddr)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	// RabbitMQ connection
	rabbitMqURI := env.GetString("RABBITMQ_URI", "amqp://guest:guest@rabbitmq:5672/")

	rabbmq, err := messaging.NewRabbitMQ(rabbitMqURI)
	if err != nil {
		log.Fatalf("Failed to connect to RabbitMQ: %v", err)
	}
	defer rabbmq.Close()

	log.Println("starting RabbitMQ connection")

	publisher := events.NewTripPublisher(rabbmq)

	// starting the gRPC server
	grpcServer := grpcserver.NewServer()
	grpc.NewGRPCHandler(grpcServer, svc, publisher)

	log.Printf("starting gRPC server Trip service on port %s", lis.Addr().String())

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("Failed to serve gRPC server: %v", err)
		}
		cancel()
	}()

	// wait for the shutdown signal
	<-ctx.Done()
	log.Println("Shutting down gRPC server...")
	grpcServer.GracefulStop()
	log.Println("gRPC server stopped")
}
