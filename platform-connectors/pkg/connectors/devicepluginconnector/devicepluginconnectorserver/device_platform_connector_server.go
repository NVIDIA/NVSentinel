package devicepluginconnectorserver

import (
	"context"
	"log"
	"net"
	"os"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/devicepluginconnector/deviceplugincore"
	devicePluginPb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/devicepluginconnector/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/server"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"k8s.io/klog/v2"
)

type DevicePluginConnectorServer struct {
	devicePluginPb.UnimplementedNVIDIADevicePluginConnectorServer
	healthEventChan chan *devicePluginPb.HealthEvent
}

// HealthEventStreamV1 streams HealthEvents to the client
func (s *DevicePluginConnectorServer) HealthEventStreamV1(empty *emptypb.Empty,
	stream devicePluginPb.NVIDIADevicePluginConnector_HealthEventStreamV1Server) error {
	// Signal that the client has connected
	klog.Infof("Client connected, starting to stream events...")

	for {
		select {
		case event := <-s.healthEventChan:
			if err := stream.Send(event); err != nil {
				klog.Errorf("Error sending event: %v", err)
				return err
			}
		case <-stream.Context().Done():
			klog.Infof("Client disconnected")
			return nil
		}
	}
}

func NewDevicePluginConnectorServer(healthEventChan chan *devicePluginPb.HealthEvent) *DevicePluginConnectorServer {
	return &DevicePluginConnectorServer{
		healthEventChan: healthEventChan,
	}
}

func serveDevicePlugin(s *grpc.Server, devicePluginListener net.Listener) {
	if err := s.Serve(devicePluginListener); err != nil {
		log.Fatalf("devicePluginServer Failed to serve: %v", err)
	}
}

func CreateAndStartDevicePluginServer(devicePluginSocket *string, devicePluginListener *net.Listener,
	devicePluginRingBuffer *ringbuffer.RingBuffer, nodeName string, stopCh chan struct{},
	ctx context.Context) *deviceplugincore.DevicePluginConnector {
	os.Remove(*devicePluginSocket)

	var err error
	// Create Device plugin Socket
	*devicePluginListener, err = net.Listen("unix", *devicePluginSocket)

	if err != nil {
		klog.Fatalf("Error creating devicePluginsocket %s", err)
		return nil
	}

	server.InitializeAndAttachRingBufferForConnectors(devicePluginRingBuffer)

	healthEventChan := make(chan *devicePluginPb.HealthEvent, 1000)
	devicePluginConnector := deviceplugincore.InitializeDevicePluginConnector(devicePluginRingBuffer,
		*devicePluginListener, string(nodeName), stopCh, ctx, healthEventChan)

	var opts []grpc.ServerOption
	s := grpc.NewServer(opts...)
	devicePluginServer := NewDevicePluginConnectorServer(healthEventChan)
	devicePluginPb.RegisterNVIDIADevicePluginConnectorServer(s, devicePluginServer)
	klog.Infof("Server is listening on %s", *devicePluginSocket)

	go serveDevicePlugin(s, *devicePluginListener)
	go devicePluginConnector.FetchAndProcessHealthMetric(ctx)

	return devicePluginConnector
}
