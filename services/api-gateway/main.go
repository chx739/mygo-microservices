package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ride-sharing/shared/auth"
	"ride-sharing/shared/env"
	"ride-sharing/shared/messaging"
	"ride-sharing/shared/metrics"
	"ride-sharing/shared/tracing"
)

var (
	httpAddr    = env.GetString("HTTP_ADDR", ":8081")
	rabbitMqURI = env.GetString("RABBITMQ_URI", "amqp://guest:guest@localhost:5672/")
)

// jwtMinSecretLen 是 JWT_SECRET 的最小字节数。
// 32 字节是 HS256 安全底线（256 bit）；短于此值启动直接 log.Fatalf。
const jwtMinSecretLen = 32

func main() {
	log.Println("Starting API Gateway")

	// 注册 Prometheus 指标（演唱会压测方案 § 3.Step 1）。
	// 放在最前面，后续任何 init 失败都能让 /metrics 仍可抓取注册阶段的状态。
	metrics.Register("api-gateway")

	// Initialize Tracing
	tracerCfg := tracing.Config{
		ServiceName:    "api-gateway",
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

	mux := http.NewServeMux()

	// RabbitMQ connection
	rabbitmq, err := messaging.NewRabbitMQ(rabbitMqURI)
	if err != nil {
		log.Fatal(err)
	}
	defer rabbitmq.Close()

	log.Println("Starting RabbitMQ connection")

	// 初始化 Redis（用于 Stripe webhook 幂等、限流、以及 JWT 黑/白名单）。
	redisClient, err := NewRedisClient(ctx)
	if err != nil {
		log.Fatalf("Failed to initialize Redis, err: %v", err)
	}
	defer redisClient.Close()

	// 初始化 JWT Signer。
	// 1) JWT_SECRET 必须存在且长度 >= 32 字节，缺失 / 过短 → fail-fast；
	// 2) TTL 通过环境变量可调，避免重新编译。
	secret := env.GetString("JWT_SECRET", "")
	if len(secret) < jwtMinSecretLen {
		log.Fatalf("JWT_SECRET must be set and >= %d bytes (got %d)", jwtMinSecretLen, len(secret))
	}
	accessTTL := time.Duration(env.GetInt("JWT_ACCESS_TTL_SECONDS", 900)) * time.Second
	refreshTTL := time.Duration(env.GetInt("JWT_REFRESH_TTL_SECONDS", 7*24*3600)) * time.Second
	issuer := env.GetString("JWT_ISSUER", "mygo-api-gateway")
	signer := auth.NewSigner([]byte(secret), issuer, accessTTL, refreshTTL)
	log.Printf("JWT signer ready: issuer=%q access=%s refresh=%s", issuer, accessTTL, refreshTTL)

	// 初始化 Mongo + user repo + 唯一索引。
	// 索引仅在此处创建，业务代码不重复创建（避免 drift）。
	mongoClient, mongoDB, err := NewMongoClient(ctx)
	if err != nil {
		log.Fatalf("Failed to initialize MongoDB, err: %v", err)
	}
	defer func() {
		if err := mongoClient.Disconnect(context.Background()); err != nil {
			log.Printf("mongo disconnect: %v", err)
		}
	}()
	if err := EnsureUserIndexes(ctx, mongoDB); err != nil {
		log.Fatalf("Failed to ensure user indexes: %v", err)
	}
	userRepo := NewMongoUserRepo(mongoDB)
	log.Println("Mongo user repo ready with indexes")

	// 鉴权装饰器；组合顺序：enableCORS(authRequired(handler))。
	requireAuth := authRequired(signer, redisClient)

	// /auth/* 路由：register / login / refresh 无需鉴权；logout / me 需要合法 access。
	mux.HandleFunc("POST /auth/register", enableCORS(handleRegister(userRepo)))
	mux.HandleFunc("POST /auth/login", enableCORS(handleLogin(userRepo, signer, redisClient, accessTTL, refreshTTL)))
	mux.HandleFunc("POST /auth/refresh", enableCORS(handleRefresh(signer, redisClient, accessTTL, refreshTTL)))
	mux.HandleFunc("POST /auth/logout", enableCORS(requireAuth(handleLogout(signer, redisClient))))
	mux.HandleFunc("GET /auth/me", enableCORS(requireAuth(handleMe())))

	mux.HandleFunc("POST /trip/preview", enableCORS(handleTripPreview))
	// /trip/start 现在需要鉴权；authRequired 在 enableCORS 内层，保证 OPTIONS preflight 不被 401 挡掉。
	mux.HandleFunc("POST /trip/start", enableCORS(requireAuth(func(w http.ResponseWriter, r *http.Request) {
		handleTripStart(w, r, redisClient)
	})))
	// GET /trip/{id}：演唱会压测方案 § 3.Step 4.5 新增的只读透传路由。
	// 走 authRequired 和 /trip/start 保持鉴权语义一致，压测 attacker VU 必须带合法 rider token。
	mux.HandleFunc("GET /trip/{id}", enableCORS(requireAuth(handleGetTrip)))
	mux.HandleFunc("/ws/drivers", func(w http.ResponseWriter, r *http.Request) {
		handleDriversWebSocket(w, r, rabbitmq)
	})
	mux.HandleFunc("/ws/riders", func(w http.ResponseWriter, r *http.Request) {
		handleRidersWebSocket(w, r, rabbitmq)
	})
	mux.HandleFunc("/webhook/stripe", func(w http.ResponseWriter, r *http.Request) {
		handleStripeWebhook(w, r, rabbitmq, redisClient)
	})

	// /metrics 挂到主 mux，不走鉴权中间件 —— Prometheus 抓取器不带 token。
	mux.Handle("GET /metrics", metrics.Handler())

	server := &http.Server{
		Addr: httpAddr,
		// metrics.Middleware 包在最外层，记录所有路由（含 /metrics 本身，归 "/metrics" 模板）。
		Handler: metrics.Middleware(mux),
	}

	serverErrors := make(chan error, 1)

	go func() {
		log.Printf("Server listening on %s", httpAddr)
		serverErrors <- server.ListenAndServe()
	}()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErrors:
		log.Printf("Error starting the server: %v", err)

	case sig := <-shutdown:
		log.Printf("Server is shutting down due to %v signal", sig)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			log.Printf("Could not stop the server gracefully: %v", err)
			server.Close()
		}
	}
}
