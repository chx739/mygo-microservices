package main

// trip_handler.go 只负责 GET /trip/:id 这一条只读路由的 HTTP → gRPC 透传。
// 演唱会压测方案 § 3.Step 4.5 新增，为 k6 attackerFlow 提供可打的接口（让 Bloom 链路被真实穿透）。
//
// 设计要点：
//   - 走 authRequired 中间件保持鉴权语义一致（压测阶段攻击者也必须带合法 rider token）；
//   - 复用 grpc_clients.NewTripServiceClient()，与 /trip/preview、/trip/start 的现有风格对齐；
//   - 错误映射：gRPC NotFound → HTTP 404，Internal → HTTP 500，其余兜底 500。

import (
	"log"
	"net/http"
	"strings"

	"ride-sharing/services/api-gateway/grpc_clients"
	"ride-sharing/shared/contracts"
	pb "ride-sharing/shared/proto/trip"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// handleGetTrip 提供 GET /trip/{id} 的 HTTP 入口。
// 不接 redis / rabbitmq 参数：GetTrip 是纯只读路径，所有缓存/Bloom 逻辑都在 trip-service 内部。
func handleGetTrip(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handleGetTrip")
	defer span.End()

	// 从 ServeMux 的路径模板取 id（Go 1.22+ 的 r.PathValue）。
	// 不做格式校验：让 trip-service 的 Bloom 预检 + ObjectID 解析兜底，
	// 这样压测打 "deadbeef" 这类伪造 ID 能真实触达 Bloom miss 指标。
	tripID := strings.TrimSpace(r.PathValue("id"))
	if tripID == "" {
		http.Error(w, "trip id is required", http.StatusBadRequest)
		return
	}

	client, err := grpc_clients.NewTripServiceClient()
	if err != nil {
		log.Printf("new trip client: %v", err)
		http.Error(w, "trip service unavailable", http.StatusInternalServerError)
		return
	}
	defer client.Close()

	resp, err := client.Client.GetTrip(ctx, &pb.GetTripRequest{TripID: tripID})
	if err != nil {
		// 把 gRPC status code 映射到 HTTP。保留 NotFound，其他统一 500；
		// 不暴露 trip-service 的内部错误消息给客户端，只打 log。
		if st, ok := status.FromError(err); ok {
			switch st.Code() {
			case codes.NotFound:
				http.Error(w, "trip not found", http.StatusNotFound)
				return
			case codes.InvalidArgument:
				http.Error(w, st.Message(), http.StatusBadRequest)
				return
			}
		}
		log.Printf("get trip %s: %v", tripID, err)
		http.Error(w, "failed to get trip", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, contracts.APIResponse{Data: resp.GetTrip()})
}
