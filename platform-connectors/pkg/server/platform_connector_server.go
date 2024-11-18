package server

import (
	"context"

	"github.com/golang/protobuf/ptypes/empty"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"k8s.io/klog/v2"
)

/*
In the code coverage report, this file is contributing 0%. Reason is since the healthEvents message send
by the gpu health monitor is received by function HealthEventOccuredV1 and in order to test the functionality
completely, we need simulate the queue enqueue and dequeue operations along with initializing the
PlatformConnectorServer. it will get really complex.Hence, ignoring this file as part of unit testing for now.
*/

var ringBufferQueue []*ringbuffer.RingBuffer

// prometheus metrics
var (
	healthEventsReceived = promauto.NewCounter(prometheus.CounterOpts{
		Name: "platform_connector_health_events_received_total",
		Help: "The total number of health events that the platform connector has received",
	})
)

type PlatformConnectorServer struct {
	pb.UnimplementedPlatformConnectorServer
}

func (p *PlatformConnectorServer) HealthEventOccuredV1(ctx context.Context, he *pb.HealthEvents) (*empty.Empty, error) {
	klog.Infof("Health events %+v received", he)

	healthEventsReceived.Add(float64(len(he.Events)))

	for _, buffer := range ringBufferQueue {
		buffer.Enqueue(he)
	}

	return nil, nil
}

func InitializeAndAttachRingBufferForConnectors(buffer *ringbuffer.RingBuffer) {
	ringBufferQueue = append(ringBufferQueue, buffer)
}
