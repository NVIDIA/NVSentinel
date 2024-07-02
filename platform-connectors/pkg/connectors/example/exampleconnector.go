package example

import (
	"context"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"k8s.io/klog/v2"
)

type ExampleConnector struct {
	ringBuffer *ringbuffer.RingBuffer
}

func InitializeExampleConnector(ringBuffer *ringbuffer.RingBuffer, nodeName string) *ExampleConnector {
	return &ExampleConnector{
		ringBuffer: ringBuffer,
	}
}

func (r *ExampleConnector) FetchAndProcessHealthMetric(ctx context.Context) {
	for {
		healthEvent := r.ringBuffer.Dequeue()
		klog.Infof("Received health event %v", healthEvent)
	}
}
