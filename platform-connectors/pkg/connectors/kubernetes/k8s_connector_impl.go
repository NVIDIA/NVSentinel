package kubernetes

import (
	"context"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

type K8sConnector struct {
	// clientset is the Kubernetes client
	clientset kubernetes.Interface
	// ringBuffer are client for pushing data to the resource count sink
	ringBuffer *ringbuffer.RingBuffer
	nodeName   string
	stopCh     <-chan struct{}
}

func NewK8sConnector(
	client kubernetes.Interface,
	ringBuffer *ringbuffer.RingBuffer,
	nodeName string,
	stopCh <-chan struct{}) *K8sConnector {
	return &K8sConnector{
		clientset:  client,
		ringBuffer: ringBuffer,
		nodeName:   nodeName,
		stopCh:     stopCh,
	}
}

func InitializeK8sConnector(ringbuffer *ringbuffer.RingBuffer, nodeName string, stopCh <-chan struct{}) *K8sConnector {
	// Create the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Error creating Kubernetes client: %s", err.Error())
	}

	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("error creating clientset with err %s", err.Error())
	}

	return NewK8sConnector(clientSet, ringbuffer, nodeName, stopCh)
}

func (r *K8sConnector) FetchAndProcessHealthMetric(ctx context.Context) {
	for {
		select {
		case <-r.stopCh:
			klog.Infof("k8sConnector queue received stop signal")
			return
		default:
			healthEvent := r.ringBuffer.Dequeue()
			if err := r.processHealthEvents(ctx, healthEvent); err != nil {
				klog.Errorf("Not able to process healthEvent.Error is %s", err)
				r.ringBuffer.HealthMetricEleProcessingFailed(healthEvent)
			} else {
				r.ringBuffer.HealthMetricEleProcessingCompleted(healthEvent)
			}
		}
	}
}
