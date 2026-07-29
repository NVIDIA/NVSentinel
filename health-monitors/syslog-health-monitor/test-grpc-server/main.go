package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type server struct {
	pb.UnimplementedPlatformConnectorServer
}

func (s *server) HealthEventOccurredV1(ctx context.Context, events *pb.HealthEvents) (*emptypb.Empty, error) {
	for _, e := range events.Events {
		fmt.Printf("[RECEIVED EVENT] node=%s check=%s fatal=%v healthy=%v action=%s msg=%s\n",
			e.NodeName, e.CheckName, e.IsFatal, e.IsHealthy, e.RecommendedAction, e.Message)
	}
	return &emptypb.Empty{}, nil
}

func main() {
	sock := "/tmp/nvsentinel-test/pc.sock"
	os.Remove(sock)
	lis, err := net.Listen("unix", sock)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	fmt.Printf("gRPC PlatformConnector listening on %s\n", sock)

	grpcServer := grpc.NewServer()
	pb.RegisterPlatformConnectorServer(grpcServer, &server{})

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		grpcServer.GracefulStop()
	}()

	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
