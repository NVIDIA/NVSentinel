// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubernetes

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
	ringBuffer                    *ringbuffer.RingBuffer
	stopCh                        <-chan struct{}
	ctx                           context.Context
	maxNodeConditionMessageLength int64
}

func NewK8sConnector(
	client kubernetes.Interface,
	ringBuffer *ringbuffer.RingBuffer,
	stopCh <-chan struct{}, ctx context.Context, maxNodeConditionMessageLength int64) *K8sConnector {
	return &K8sConnector{
		clientset:                     client,
		ringBuffer:                    ringBuffer,
		stopCh:                        stopCh,
		ctx:                           ctx,
		maxNodeConditionMessageLength: maxNodeConditionMessageLength,
	}
}

func InitializeK8sConnector(ctx context.Context, ringbuffer *ringbuffer.RingBuffer,
	qps float32, burst int, stopCh <-chan struct{}, maxNodeConditionMessageLength int64,
) (*K8sConnector, kubernetes.Interface, error) {
	if maxNodeConditionMessageLength <= 0 {
		return nil, nil, fmt.Errorf("maxNodeConditionMessageLength must be greater than 0, got %d",
			maxNodeConditionMessageLength)
	}

	// Create the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("error creating in-cluster config: %w", err)
	}

	config.Burst = burst
	config.QPS = qps

	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}

	kubernetesConnector := NewK8sConnector(clientSet, ringbuffer, stopCh, ctx, maxNodeConditionMessageLength)

	return kubernetesConnector, clientSet, nil
}

func (r *K8sConnector) FetchAndProcessHealthMetric(ctx context.Context) {
	for {
		select {
		case <-r.stopCh:
			slog.Info("k8sConnector queue received stop signal")
			return
		default:
			healthEvents, quit := r.ringBuffer.Dequeue()
			if quit {
				slog.Info("Queue signaled shutdown, exiting processing loop")
				return
			}

			if err := r.processHealthEvents(ctx, healthEvents); err != nil {
				slog.Error("Not able to process healthEvent", "error", err)
				r.ringBuffer.HealthMetricEleProcessingFailed(healthEvents)
			} else {
				r.ringBuffer.HealthMetricEleProcessingCompleted(healthEvents)
			}
		}
	}
}
