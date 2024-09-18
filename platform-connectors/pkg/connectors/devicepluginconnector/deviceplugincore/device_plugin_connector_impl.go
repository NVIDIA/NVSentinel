package deviceplugincore

import (
	"context"
	"net"

	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/devicepluginconnector/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"k8s.io/klog/v2"
)

type DevicePluginConnector struct {
	// ringBuffer are client for pushing data to the resource count sink
	ringBuffer      *ringbuffer.RingBuffer
	nodeName        string
	stopCh          <-chan struct{}
	ctx             context.Context
	listener        net.Listener
	healthEventChan chan *pb.HealthEvent
}

func NewDevicePluginConnector(
	ringBuffer *ringbuffer.RingBuffer,
	listener net.Listener,
	nodeName string,
	stopCh <-chan struct{}, ctx context.Context, healthEventChan chan *pb.HealthEvent) *DevicePluginConnector {
	return &DevicePluginConnector{
		ringBuffer:      ringBuffer,
		nodeName:        nodeName,
		stopCh:          stopCh,
		ctx:             ctx,
		listener:        listener,
		healthEventChan: healthEventChan,
	}
}

func InitializeDevicePluginConnector(ringbuffer *ringbuffer.RingBuffer, listener net.Listener, nodeName string,
	stopCh <-chan struct{}, ctx context.Context, healthEventChan chan *pb.HealthEvent) *DevicePluginConnector {
	devicePluginConnector := NewDevicePluginConnector(ringbuffer, listener, nodeName, stopCh, ctx, healthEventChan)
	return devicePluginConnector
}

func (r *DevicePluginConnector) FetchAndProcessHealthMetric(ctx context.Context) {
	for {
		select {
		case <-r.stopCh:
			klog.Infof("k8sConnector queue received stop signal")
			return
		default:
			healthEvent := r.ringBuffer.Dequeue()
			r.ProcessHealthEvents(ctx, healthEvent)
		}
	}
}
