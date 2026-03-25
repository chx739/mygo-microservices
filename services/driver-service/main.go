package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	grpcserver "google.golang.org/grpc"
)

var GrpcAddr = ":9092"

func main() {

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

	service := NewService()

	// starting the gRPC server
	grpcServer := grpcserver.NewServer()
	NewGrpcHandler(grpcServer, service)

	log.Printf("starting gRPC server Driver service on port %s", lis.Addr().String())

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
