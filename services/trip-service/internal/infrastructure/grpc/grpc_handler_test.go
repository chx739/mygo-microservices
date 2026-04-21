// 演唱会压测方案 § 3.Step 4.4 的 GetTrip gRPC handler 单测。
//
// 覆盖三条关键路径：
//  1. Hit      —— service 返回合法 Trip，handler 应回 codes.OK + 对齐字段的 Response；
//  2. NotFound —— service 返回 "trip not found: ..."（Bloom miss / Mongo 未命中的统一信号），
//                 handler 应回 codes.NotFound；
//  3. Internal —— service 返回其他错误，handler 应回 codes.Internal。
//
// 不启 gRPC server / Mongo / Redis，直接构造 stubTripService 注入 handler，
// 属于方案 § 4 层级 1「纯单元」的测试。

package grpc

import (
	"context"
	"errors"
	"testing"

	"ride-sharing/services/trip-service/internal/domain"
	pbd "ride-sharing/shared/proto/driver"
	pb "ride-sharing/shared/proto/trip"
	"ride-sharing/shared/types"

	tripTypes "ride-sharing/services/trip-service/pkg/types"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubTripService 只实现测试里实际会被 GetTrip 用到的方法；
// 其他方法保持 panic，以便无意调用时测试立刻炸出来。
type stubTripService struct {
	trip *domain.TripModel
	err  error
}

func (s *stubTripService) GetTripByID(_ context.Context, _ string) (*domain.TripModel, error) {
	return s.trip, s.err
}

// 以下方法在当前测试中不会被调用。命名空间要求实现 domain.TripService 接口全集。
func (s *stubTripService) CreateTrip(context.Context, *domain.RideFareModel) (*domain.TripModel, error) {
	panic("not expected")
}
func (s *stubTripService) GetRoute(context.Context, *types.Coordinate, *types.Coordinate, bool) (*tripTypes.OsrmApiResponse, error) {
	panic("not expected")
}
func (s *stubTripService) EstimatePackagesPriceWithRoute(*tripTypes.OsrmApiResponse) []*domain.RideFareModel {
	panic("not expected")
}
func (s *stubTripService) GenerateTripFares(context.Context, []*domain.RideFareModel, string, *tripTypes.OsrmApiResponse) ([]*domain.RideFareModel, error) {
	panic("not expected")
}
func (s *stubTripService) GetAndValidateFare(context.Context, string, string) (*domain.RideFareModel, error) {
	panic("not expected")
}
func (s *stubTripService) UpdateTrip(context.Context, string, string, *pbd.Driver) error {
	panic("not expected")
}

// newHandler 构造只含 service 字段的 handler；publisher 在 GetTrip 路径不会被触达。
func newHandler(svc domain.TripService) *gRPCHandler {
	return &gRPCHandler{service: svc}
}

func TestGetTrip_Hit(t *testing.T) {
	oid := primitive.NewObjectID()
	// OsrmApiResponse.ToProto 要求至少一条 Route，否则会 panic。
	// 压测用不到真实坐标，填一个最小合法结构即可。
	route := &tripTypes.OsrmApiResponse{}
	route.Routes = append(route.Routes, struct {
		Distance float64 `json:"distance"`
		Duration float64 `json:"duration"`
		Geometry struct {
			Coordinates [][]float64 `json:"coordinates"`
		} `json:"geometry"`
	}{Distance: 1000, Duration: 60})
	stub := &stubTripService{
		trip: &domain.TripModel{
			ID:     oid,
			UserID: "user-1",
			Status: "pending",
			RideFare: &domain.RideFareModel{
				ID:                oid,
				UserID:            "user-1",
				PackageSlug:       "sedan",
				TotalPriceInCents: 350,
				Route:             route,
			},
		},
	}
	h := newHandler(stub)

	resp, err := h.GetTrip(context.Background(), &pb.GetTripRequest{TripID: oid.Hex()})
	if err != nil {
		t.Fatalf("GetTrip returned err: %v", err)
	}
	if resp.GetTrip().GetId() != oid.Hex() {
		t.Fatalf("resp.Trip.Id got %q want %q", resp.GetTrip().GetId(), oid.Hex())
	}
	if resp.GetTrip().GetUserID() != "user-1" {
		t.Fatalf("resp.Trip.UserID got %q want %q", resp.GetTrip().GetUserID(), "user-1")
	}
}

func TestGetTrip_NotFound_BloomMiss(t *testing.T) {
	// 模拟 service.GetTripByID 对 Bloom miss / 非法 ObjectID 的统一返回。
	stub := &stubTripService{err: errors.New("trip not found: deadbeef")}
	h := newHandler(stub)

	_, err := h.GetTrip(context.Background(), &pb.GetTripRequest{TripID: "deadbeef"})
	if err == nil {
		t.Fatal("expected NotFound err, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected grpc status, got %T", err)
	}
	if st.Code() != codes.NotFound {
		t.Fatalf("code got %v want NotFound", st.Code())
	}
}

func TestGetTrip_Internal_OnOtherErr(t *testing.T) {
	stub := &stubTripService{err: errors.New("redis connection reset")}
	h := newHandler(stub)

	_, err := h.GetTrip(context.Background(), &pb.GetTripRequest{TripID: "507f1f77bcf86cd799439011"})
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Fatalf("code got %v want Internal", st.Code())
	}
}

func TestGetTrip_InvalidArgument_EmptyID(t *testing.T) {
	stub := &stubTripService{}
	h := newHandler(stub)

	_, err := h.GetTrip(context.Background(), &pb.GetTripRequest{TripID: ""})
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("code got %v want InvalidArgument", st.Code())
	}
}
