package ringbuffer

import (
	"context"
	"errors"

	"k8s.io/klog/v2"

	platformconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"k8s.io/client-go/util/workqueue"
)

type RingBuffer struct {
	ringBufferIdentifier string
	healthMetricQueue    workqueue.RateLimitingInterface
	ctx                  context.Context
}

func NewRingBuffer(ringBufferName string, ctx context.Context) *RingBuffer {
	workqueue.SetProvider(prometheusMetricsProvider{})
	queue := workqueue.NewNamedRateLimitingQueue(
		workqueue.DefaultControllerRateLimiter(),
		ringBufferName,
	)

	return &RingBuffer{
		ringBufferIdentifier: ringBufferName,
		healthMetricQueue:    queue,
		ctx:                  ctx,
	}
}

func (rb *RingBuffer) Enqueue(data *platformconnector.HealthEvent) {
	rb.healthMetricQueue.Add(data)
}

func (rb *RingBuffer) Dequeue() *platformconnector.HealthEvent {
	healthEvent, quit := rb.healthMetricQueue.Get()
	if quit {
		klog.Infof("quitting from queue processing")
		return nil
	}

	klog.Infof("Successfully got item %v ", healthEvent)

	if errors.Is(rb.ctx.Err(), context.Canceled) {
		klog.Info("Processing cancelled")
		return nil
	}

	return healthEvent.(*platformconnector.HealthEvent)
}

func (rb *RingBuffer) HealthMetricEleProcessingCompleted(data *platformconnector.HealthEvent) {
	rb.healthMetricQueue.Done(data)
}

func (rb *RingBuffer) HealthMetricEleProcessingFailed(data *platformconnector.HealthEvent) {
	rb.healthMetricQueue.Forget(data)
}

func (rb *RingBuffer) ShutDownHealthMetricQueue() {
	rb.healthMetricQueue.ShutDown()
}
