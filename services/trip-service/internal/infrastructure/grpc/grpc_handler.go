package grpc

import (
	"context"
	"log"
	"ride-sharing/services/trip-service/internal/domain"
	"ride-sharing/services/trip-service/internal/infrastructure/events"
	pb "ride-sharing/shared/proto/trip"
	"ride-sharing/shared/types"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type gRPCHandler struct {
	pb.UnimplementedTripServiceServer

	service   domain.TripService
	publisher *events.TripEventPublisher
}

func NewGRPCHandler(server *grpc.Server, service domain.TripService, publisher *events.TripEventPublisher) *gRPCHandler {
	handler := &gRPCHandler{
		service:   service,
		publisher: publisher,
	}

	pb.RegisterTripServiceServer(server, handler)
	return handler
}

func (h *gRPCHandler) CreateTrip(ctx context.Context, req *pb.CreateTripRequest) (*pb.CreateTripResponse, error) {
	fareID := req.GetRideFareID()
	userID := req.GetUserID()

	rideFare, err := h.service.GetAndValidateFare(ctx, fareID, userID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to validate the fare: %v", err)
	}

	trip, err := h.service.CreateTrip(ctx, rideFare)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create the trip: %v", err)
	}

	if err := h.publisher.PublishTripCreated(ctx, trip); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to publish the trip created event: %v", err)
	}

	return &pb.CreateTripResponse{
		TripID: trip.ID.Hex(),
	}, nil
}

// GetTrip 按 tripID 取单条 Trip。演唱会压测方案 § 3.Step 4.4 新增。
//
// 职责：纯粹把 service.GetTripByID 的结果包装成 gRPC response。业务逻辑（Bloom 预检、
// 缓存读写、Mongo 回源）全部复用现有 service 实现，不在 handler 里绕过。
//
// 错误语义：
//   - service 返回形如 "trip not found: xxx" 的错误（Bloom miss / 非法 ObjectID / Mongo 未命中）
//     → 映射到 codes.NotFound，api-gateway 透传为 HTTP 404；
//   - 其他错误 → codes.Internal，对应 HTTP 500。
//
// 为什么用 strings.Contains 而不是 errors.Is：现有 service 层返回 fmt.Errorf
// 而非 sentinel 错误（参考 service.GetTripByID 的三条 "trip not found" 分支），
// 本期禁止改业务代码，因此按字符串判别；未来引入 sentinel 时再替换。
func (h *gRPCHandler) GetTrip(ctx context.Context, req *pb.GetTripRequest) (*pb.GetTripResponse, error) {
	tripID := req.GetTripID()
	if tripID == "" {
		return nil, status.Error(codes.InvalidArgument, "tripID is required")
	}

	trip, err := h.service.GetTripByID(ctx, tripID)
	if err != nil {
		if strings.Contains(err.Error(), "trip not found") {
			return nil, status.Errorf(codes.NotFound, "trip %s not found", tripID)
		}
		return nil, status.Errorf(codes.Internal, "get trip: %v", err)
	}
	if trip == nil {
		return nil, status.Errorf(codes.NotFound, "trip %s not found", tripID)
	}

	return &pb.GetTripResponse{Trip: trip.ToProto()}, nil
}

func (h *gRPCHandler) PreviewTrip(ctx context.Context, req *pb.PreviewTripRequest) (*pb.PreviewTripResponse, error) {
	pickup := req.GetStartLocation()
	destination := req.GetEndLocation()

	pickupCoord := &types.Coordinate{
		Latitude:  pickup.Latitude,
		Longitude: pickup.Longitude,
	}
	destinationCoord := &types.Coordinate{
		Latitude:  destination.Latitude,
		Longitude: destination.Longitude,
	}

	userID := req.GetUserID()

	// 压测专用（方案 § 5）：走 mock route，避开外部 OSRM 抖动遮蔽内部瓶颈。loadtest 后 git restore。
	route, err := h.service.GetRoute(ctx, pickupCoord, destinationCoord, false)
	if err != nil {
		log.Println(err)
		return nil, status.Errorf(codes.Internal, "failed to get route: %v", err)
	}

	estimatedFares := h.service.EstimatePackagesPriceWithRoute(route)

	fares, err := h.service.GenerateTripFares(ctx, estimatedFares, userID, route)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to generate the ride fares: %v", err)
	}

	return &pb.PreviewTripResponse{
		Route:     route.ToProto(),
		RideFares: domain.ToRideFaresProto(fares),
	}, nil
}
