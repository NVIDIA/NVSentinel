package kubernetes

import (
	"context"
	"fmt"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

/*
In the code coverage report, this file is contributing only 4%. Reason is most of the code in this part is
initializing the k8sClientset from kubernetes config   and since in unit tests, it is there is no k8s cluster,
hence it is complex to test this. Hence, ignoring this initilization part for now as part of unit testing
Hence, ignoring this file as part of unit testing for now.
*/

type K8sConnector struct {
	// clientset is the Kubernetes client
	clientset kubernetes.Interface
	// ringBuffer are client for pushing data to the resource count sink
	ringBuffer     *ringbuffer.RingBuffer
	nodeName       string
	stopCh         <-chan struct{}
	eventCache     map[string]*corev1.Event
	ctx            context.Context
	eventCacheSize uint32
}

const (
	EventCacheSizeUpperLimit = 1000
)

func NewK8sConnector(
	client kubernetes.Interface,
	ringBuffer *ringbuffer.RingBuffer,
	nodeName string,
	stopCh <-chan struct{}, ctx context.Context) *K8sConnector {
	return &K8sConnector{
		clientset:      client,
		ringBuffer:     ringBuffer,
		nodeName:       nodeName,
		stopCh:         stopCh,
		eventCache:     make(map[string]*corev1.Event),
		ctx:            ctx,
		eventCacheSize: EventCacheSizeUpperLimit,
	}
}

func (k8sConnector *K8sConnector) resetAndRefillCacheFromNodeEvents() error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		events, err := k8sConnector.clientset.CoreV1().Events("").List(k8sConnector.ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.kind=Node,involvedObject.name=%s", k8sConnector.nodeName),
		})
		if err != nil {
			return err
		}

		for _, event := range events.Items {
			k8sConnector.AggregateEvent(&event)
		}

		return nil
	})

	return err
}
func InitializeK8sConnector(ringbuffer *ringbuffer.RingBuffer, nodeName string,
	stopCh <-chan struct{}, ctx context.Context) *K8sConnector {
	// Create the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Error creating Kubernetes client: %s", err.Error())
	}

	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("error creating clientset with err %s", err.Error())
	}

	kubernetesConnector := NewK8sConnector(clientSet, ringbuffer, nodeName, stopCh, ctx)

	err = kubernetesConnector.resetAndRefillCacheFromNodeEvents()
	if err != nil {
		klog.Fatalf("Failed to reset and refill cache from node events.Error is %s", err)
	}

	return kubernetesConnector
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
