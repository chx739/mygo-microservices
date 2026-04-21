package grpc_clients

import (
	"os"
	pb "ride-sharing/shared/proto/driver"
	"ride-sharing/shared/tracing"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type driverServiceClient struct {
	Client pb.DriverServiceClient
	conn   *grpc.ClientConn
}

// 压测专用（方案 § 5）：见 trip_client.go 同样的单例注释。loadtest 后 git restore。
var (
	driverClientOnce sync.Once
	driverClient     *driverServiceClient
	driverClientErr  error
)

func NewDriverServiceClient() (*driverServiceClient, error) {
	driverClientOnce.Do(func() {
		driverServiceURL := os.Getenv("DRIVER_SERVICE_URL")
		if driverServiceURL == "" {
			driverServiceURL = "driver-service:9092"
		}

		dialOptions := append(
			tracing.DialOptionsWithTracing(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)

		conn, err := grpc.NewClient(driverServiceURL, dialOptions...)
		if err != nil {
			driverClientErr = err
			return
		}
		driverClient = &driverServiceClient{
			Client: pb.NewDriverServiceClient(conn),
			conn:   conn,
		}
	})
	return driverClient, driverClientErr
}

func (c *driverServiceClient) Close() {}
