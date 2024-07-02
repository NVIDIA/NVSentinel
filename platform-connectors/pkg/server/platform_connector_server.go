package server

import (
	"context"

	"github.com/golang/protobuf/ptypes/empty"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"k8s.io/klog/v2"
)

var ringBufferQueue []*ringbuffer.RingBuffer

type PlatformConnectorServer struct {
	pb.UnimplementedPlatformConnectorServer
}

func (p *PlatformConnectorServer) HealthEventOccuredV1(ctx context.Context, he *pb.HealthEvents) (*empty.Empty, error) {
	klog.Infof("Health events %+v received", he)
	healthEvents := he.Events

	for _, event := range healthEvents {
		for _, buffer := range ringBufferQueue {
			buffer.Enqueue(event)
		}
	}

	return nil, nil
}

func InitializeAndAttachRingBufferForConnectors(buffer *ringbuffer.RingBuffer) {
	ringBufferQueue = append(ringBufferQueue, buffer)
}
