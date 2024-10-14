package main

// Sample Client application to connect to nodeHealthEvents connector server
import (
	"context"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
	"k8s.io/klog"
	protos "nodehealtheventsudsconnectorclient/protos"
)

const nodeHealthEventsSocketPath = "/var/run/nodeHealthEvents.sock"

func ListenClient(ctx context.Context) {
	klog.Infof("Inside ListenClient")

	opts := grpc.WithTransportCredentials(insecure.NewCredentials())

	conn, err := grpc.NewClient("unix://"+nodeHealthEventsSocketPath, opts)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	defer conn.Close()

	klog.Info("creating the client")

	client := protos.NewNodeHealthEventsUDSConnectorClient(conn)

	stream, err := client.HealthEventStreamV1(ctx, &emptypb.Empty{})

	if err != nil {
		klog.Errorf("nodeHealthEventsClient Error calling ReceiveMessages: %v", err)
		return
	}

	for {
		klog.Infof("New Message from Nvsentinel")

		healthEvent, err := stream.Recv()

		if err != nil {
			klog.Errorf("Error receiving message: %v", err)
			continue
		}

		log.Printf("Received message: %s", healthEvent)
	}
}

func main() {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	ListenClient(ctx)
	cancel()
}
