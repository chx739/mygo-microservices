package main

import (
	"encoding/json"
	"log"
	"net/http"
	"ride-sharing/services/api-gateway/grpc_clients"
	"ride-sharing/shared/contracts"
	"ride-sharing/shared/messaging"
	"ride-sharing/shared/proto/driver"
)

var (
	connManager = messaging.NewConnectionManager()
)

// startNotificationConsumers 在网关启动期为每个通知/命令队列起单个消费者，
// 通过进程级全局 connManager 按消息 OwnerID 路由到对应 WS 连接。
//
// 为什么不在每个 WS 连接里起：原实现 handleRidersWebSocket /
// handleDriversWebSocket 每来一个连接就 NewQueueConsumer().Start()，且连接关闭后
// 该消费者 goroutine 永不退出（queue_consumer 无取消机制）。N 个连接 → 同一共享
// 队列上 N 个竞争消费者，RabbitMQ 轮询投递 + autoAck 无重投，driver_assigned /
// trip_request 大量被投给已泄漏的消费者后丢失，压测实测派单成功率塌到 ~2.5%。
// 启动期单例消费者 + 全局 connManager 路由即可消除该结构性丢失。
//
// 队列在 messaging.NewRabbitMQ → setupExchangesAndQueues 中已声明并绑定，
// 故此处 Consume 不会因队列不存在而失败；启动期单次调用，失败即 fail-fast。
func startNotificationConsumers(rb *messaging.RabbitMQ) {
	queues := []string{
		messaging.NotifyDriverNoDriversFoundQueue,
		messaging.NotifyDriverAssignQueue,
		messaging.NotifyPaymentSessionCreatedQueue,
		messaging.DriverCmdTripRequestQueue,
	}
	for _, q := range queues {
		consumer := messaging.NewQueueConsumer(rb, connManager, q)
		if err := consumer.Start(); err != nil {
			log.Fatalf("startNotificationConsumers: queue %s: %v", q, err)
		}
	}
	log.Printf("notification consumers started for %d queues", len(queues))
}

func handleRidersWebSocket(w http.ResponseWriter, r *http.Request, rb *messaging.RabbitMQ) {
	conn, err := connManager.Upgrade(w, r)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	defer conn.Close()

	userID := r.URL.Query().Get("userID")
	if userID == "" {
		log.Println("No user ID provided")
		return
	}

	// Add connection to manager
	connManager.Add(userID, conn)
	defer connManager.Remove(userID)

	// 通知队列消费者已在网关启动期统一起单例（见 startNotificationConsumers）。
	// 这里不再 per-connection 起消费者：原实现每个连接 spawn 一组消费者且连接关闭后
	// goroutine 永不退出，共享队列上堆积竞争消费者 + autoAck 无重投 → driver_assigned
	// 被轮询投给已泄漏消费者后丢失。rb 现仅为保持 handler 签名一致而保留。
	_ = rb

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Error reading message: %v", err)
			break
		}

		log.Printf("Received message: %s", message)
	}
}

func handleDriversWebSocket(w http.ResponseWriter, r *http.Request, rb *messaging.RabbitMQ) {
	conn, err := connManager.Upgrade(w, r)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	defer conn.Close()

	userID := r.URL.Query().Get("userID")
	if userID == "" {
		log.Println("No user ID provided")
		return
	}

	packageSlug := r.URL.Query().Get("packageSlug")
	if packageSlug == "" {
		log.Println("No package slug provided")
		return
	}

	// Add connection to manager
	connManager.Add(userID, conn)

	ctx := r.Context()

	driverService, err := grpc_clients.NewDriverServiceClient()
	if err != nil {
		log.Fatal(err)
	}

	// Closing connections
	defer func() {
		connManager.Remove(userID)

		driverService.Client.UnregisterDriver(ctx, &driver.RegisterDriverRequest{
			DriverID:    userID,
			PackageSlug: packageSlug,
		})

		driverService.Close()

		log.Println("Driver unregistered: ", userID)
	}()

	driverData, err := driverService.Client.RegisterDriver(ctx, &driver.RegisterDriverRequest{
		DriverID:    userID,
		PackageSlug: packageSlug,
	})
	if err != nil {
		log.Printf("Error registering driver: %v", err)
		return
	}

	if err := connManager.SendMessage(userID, contracts.WSMessage{
		Type: contracts.DriverCmdRegister,
		Data: driverData.Driver,
	}); err != nil {
		log.Printf("Error sending message: %v", err)
		return
	}

	// trip_request 消费者同样在网关启动期统一起单例（见 startNotificationConsumers），
	// 不再 per-connection 起，原因同 riders 侧。

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Error reading message: %v", err)
			break
		}

		type driverMessage struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}

		var driverMsg driverMessage
		if err := json.Unmarshal(message, &driverMsg); err != nil {
			log.Printf("Error unmarshaling driver message: %v", err)
			continue
		}

		// Handle the different message type
		switch driverMsg.Type {
		case contracts.DriverCmdLocation:
			// Handle driver location update in the future
			continue
		case contracts.DriverCmdTripAccept, contracts.DriverCmdTripDecline:
			// Forward the message to RabbitMQ
			if err := rb.PublishMessage(ctx, driverMsg.Type, contracts.AmqpMessage{
				OwnerID: userID,
				Data:    driverMsg.Data,
			}); err != nil {
				log.Printf("Error publishing message to RabbitMQ: %v", err)
			}
		default:
			log.Printf("Unknown message type: %s", driverMsg.Type)
		}
	}
}
