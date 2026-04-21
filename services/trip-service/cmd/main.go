package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"ride-sharing/services/trip-service/internal/infrastructure/events"
	"ride-sharing/services/trip-service/internal/infrastructure/grpc"
	"ride-sharing/services/trip-service/internal/infrastructure/repository"
	"ride-sharing/services/trip-service/internal/service"
	"ride-sharing/shared/db"
	"ride-sharing/shared/env"
	"ride-sharing/shared/messaging"
	"ride-sharing/shared/metrics"
	"ride-sharing/shared/tracing"
	"syscall"

	grpcserver "google.golang.org/grpc"
)

var GrpcAddr = ":9093"

// metricsAddr 是暴露 /metrics 的独立 HTTP 端口（方案 § 3.Step 1 决策 D3）。
// 三个后端 gRPC 服务统一用 :9100，避免和各自 gRPC 主端口冲突。
var metricsAddr = env.GetString("METRICS_ADDR", ":9100")

func main() {
	// 注册 Prometheus 指标（方案 § 3.Step 1）。
	metrics.Register("trip-service")
	startMetricsServer()

	// Initialize Tracing
	tracerCfg := tracing.Config{
		ServiceName:    "trip-service",
		Environment:    env.GetString("ENVIRONMENT", "development"),
		JaegerEndpoint: env.GetString("JAEGER_ENDPOINT", "http://jaeger:14268/api/traces"),
	}

	sh, err := tracing.InitTracer(tracerCfg)
	if err != nil {
		log.Fatalf("Failed to initialize the tracer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer sh(ctx)

	// Initialize MongoDB
	mongoClient, err := db.NewMongoClient(ctx, db.NewMongoDefaultConfig())
	if err != nil {
		log.Fatalf("Failed to initialize MongoDB, err: %v", err)
	}
	defer mongoClient.Disconnect(ctx)

	mongoDb := db.GetDatabase(mongoClient, db.NewMongoDefaultConfig())

	log.Printf(mongoDb.Name())

	// inmemRepo := repository.NewInmemRepository()
	// svc := service.NewService(inmemRepo, redisClient)

	// 初始化 Redis（用于接单分布式锁 + Trip 读缓存）。
	redisClient, err := NewRedisClient(ctx)
	if err != nil {
		log.Fatalf("Failed to initialize Redis, err: %v", err)
	}
	defer redisClient.Close()

	mongoDBRepo := repository.NewMongoRepository(mongoDb)
	svc := service.NewService(mongoDBRepo, redisClient)

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		cancel()
	}()

	lis, err := net.Listen("tcp", GrpcAddr)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	// RabbitMQ connection
	rabbitMqURI := env.GetString("RABBITMQ_URI", "amqp://guest:guest@rabbitmq:5672/")
	rabbitmq, err := messaging.NewRabbitMQ(rabbitMqURI)
	if err != nil {
		log.Fatal(err)
	}
	defer rabbitmq.Close()

	log.Println("Starting RabbitMQ connection")

	publisher := events.NewTripEventPublisher(rabbitmq)

	// Start driver consumer
	driverConsumer := events.NewDriverConsumer(rabbitmq, svc, redisClient)
	go driverConsumer.Listen()

	// Start payment consumer
	paymentConsumer := events.NewPaymentConsumer(rabbitmq, svc, redisClient)
	go paymentConsumer.Listen()

	// Starting the gRPC server
	grpcServer := grpcserver.NewServer(tracing.WithTracingInterceptors()...)
	grpc.NewGRPCHandler(grpcServer, svc, publisher)

	log.Printf("Starting gRPC server Trip service on port %s", lis.Addr().String())

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("failed to serve: %v", err)
			cancel()
		}
	}()

	// wait for the shutdown signal
	<-ctx.Done()
	log.Println("Shutting down the server...")
	grpcServer.GracefulStop()
}

// startMetricsServer 起一个独立的 HTTP server，暴露 /metrics 供 Prometheus 抓取。
// 为什么独立 server：trip-service 主服务是 gRPC（:9093），原本无 HTTP；新开 :9100
// 专用于 metrics，不与 gRPC 共端口，也不影响主流程启动路径。
// 失败仅记日志不退出：监控面板丢失不应拖垮业务。
func startMetricsServer() {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	go func() {
		log.Printf("metrics server listening on %s", metricsAddr)
		if err := http.ListenAndServe(metricsAddr, mux); err != nil {
			log.Printf("metrics server exited: %v", err)
		}
	}()
}
