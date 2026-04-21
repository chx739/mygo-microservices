package grpc_clients

import (
	"os"
	pb "ride-sharing/shared/proto/trip"
	"ride-sharing/shared/tracing"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type tripServiceClient struct {
	Client pb.TripServiceClient
	conn   *grpc.ClientConn
}

// 压测专用（方案 § 5）：复用单例 gRPC 连接，避免每次 handler 新建 ClientConn
// 导致的 per-request HTTP/2 preface + subConn 初始化开销（实测 ~4s）。
// 原始代码注释说「每次新建便于某服务挂掉时不阻塞」，但 grpc.NewClient 默认非阻塞，
// 连接失败不会传播到其他 RPC；loadtest 后 git restore 回原版本。
var (
	tripClientOnce sync.Once
	tripClient     *tripServiceClient
	tripClientErr  error
)

func NewTripServiceClient() (*tripServiceClient, error) {
	tripClientOnce.Do(func() {
		tripServiceURL := os.Getenv("TRIP_SERVICE_URL")
		if tripServiceURL == "" {
			tripServiceURL = "trip-service:9093"
		}

		dialOptions := append(
			tracing.DialOptionsWithTracing(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)

		conn, err := grpc.NewClient(tripServiceURL, dialOptions...)
		if err != nil {
			tripClientErr = err
			return
		}
		tripClient = &tripServiceClient{
			Client: pb.NewTripServiceClient(conn),
			conn:   conn,
		}
	})
	return tripClient, tripClientErr
}

func (c *tripServiceClient) Close() {
	// 单例不关闭：每次 handler 都 defer Close 会把进程唯一连接关掉。
	// 进程退出时内核会回收，这里留空即可。
}
