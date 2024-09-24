package devicepluginconnectorclient

// Sample Client application to connect to nvidiadeviceplugin connector server
import (
	"context"
	"log"

	devicePluginPb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/devicepluginconnector/protos"

	"k8s.io/klog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

const devicePluginSocketPath = "/var/run/nvidiadeviceplugin.sock"

func ListenClient(ctx context.Context) {
	klog.Infof("Inside ListenClient")

	opts := grpc.WithTransportCredentials(insecure.NewCredentials())

	conn, err := grpc.NewClient("unix://"+devicePluginSocketPath, opts)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	defer conn.Close()

	klog.Info("creating the client")

	client := devicePluginPb.NewNVIDIADevicePluginConnectorClient(conn)

	stream, err := client.HealthEventStreamV1(ctx, &emptypb.Empty{})

	if err != nil {
		klog.Errorf("devicepluginclient Error calling ReceiveMessages: %v", err)
		return
	}

	for {
		klog.Infof("Ready to receive the messages from device pluginserver")

		healthEvent, err := stream.Recv()

		if err != nil {
			klog.Errorf("Error receiving message: %v", err)
			continue
		}

		log.Printf("Received message: %s", healthEvent)
	}
}
