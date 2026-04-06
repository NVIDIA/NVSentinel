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
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
)

/*
In the code coverage report, this file is contributing only 4%. Reason is most of the code in this part is
initializing the k8sClientset from kubernetes config   and since in unit tests, it is there is no k8s cluster,
hence it is complex to test this. Hence, ignoring this initilization part for now as part of unit testing
Hence, ignoring this file as part of unit testing for now.
*/

// K8sConnectorConfig holds tunable parameters for the K8sConnector.
type K8sConnectorConfig struct {
	MaxNodeConditionMessageLength int64
	CompactedHealthEventMsgLen    int64
}

type K8sConnector struct {
	clientset  kubernetes.Interface
	ringBuffer *ringbuffer.RingBuffer
	stopCh     <-chan struct{}
	ctx        context.Context
	config     K8sConnectorConfig
}

// NewK8sConnector creates a K8sConnector with the given Kubernetes client, ring buffer, and configuration.
func NewK8sConnector(
	client kubernetes.Interface,
	ringBuffer *ringbuffer.RingBuffer,
	stopCh <-chan struct{}, ctx context.Context,
	cfg K8sConnectorConfig) *K8sConnector {
	return &K8sConnector{
		clientset:  client,
		ringBuffer: ringBuffer,
		stopCh:     stopCh,
		ctx:        ctx,
		config:     cfg,
	}
}

func InitializeK8sConnector(ctx context.Context, ringbuffer *ringbuffer.RingBuffer,
	qps float32, burst int, stopCh <-chan struct{}, cfg K8sConnectorConfig,
) (*K8sConnector, kubernetes.Interface, error) {
	if cfg.MaxNodeConditionMessageLength <= 0 {
		return nil, nil, fmt.Errorf("maxNodeConditionMessageLength must be greater than 0, got %d",
			cfg.MaxNodeConditionMessageLength)
	}

	if cfg.CompactedHealthEventMsgLen <= 0 {
		return nil, nil, fmt.Errorf("CompactedHealthEventMsgLen must be greater than 0, got %d",
			cfg.CompactedHealthEventMsgLen)
	}

	// Create the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("error creating in-cluster config: %w", err)
	}

	config.Burst = burst
	config.QPS = qps

	config.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return auditlogger.NewAuditingRoundTripper(rt)
	})

	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}

	kubernetesConnector := NewK8sConnector(clientSet, ringbuffer, stopCh, ctx, cfg)

	return kubernetesConnector, clientSet, nil
}

func (r *K8sConnector) FetchAndProcessHealthMetric(ctx context.Context) {
	for {
		select {
		case <-r.stopCh:
			slog.InfoContext(r.ctx, "k8sConnector queue received stop signal")
			return
		default:
			queuedHealthEvents, quit := r.ringBuffer.Dequeue()
			if quit {
				slog.InfoContext(ctx, "Queue signaled shutdown, exiting processing loop")
				return
			}

			healthEvents := queuedHealthEvents.Events

			batchCtx, span := tracing.StartSpanWithLinkFromSpanContext(
				ctx, queuedHealthEvents.ParentSpanContext, "platform_connector.k8s.fetch_and_process_health_metric")

			if err := r.processHealthEvents(batchCtx, healthEvents); err != nil {
				slog.ErrorContext(batchCtx, "Not able to process healthEvent", "error", err)
				tracing.RecordError(span, err)
				span.SetAttributes(
					attribute.String("platform_connector.k8s.error.type", "not_able_to_process_health_event"),
					attribute.String("platform_connector.k8s.error.message", err.Error()),
				)
				r.ringBuffer.HealthMetricEleProcessingFailed(queuedHealthEvents)
			} else {
				r.ringBuffer.HealthMetricEleProcessingCompleted(queuedHealthEvents)
			}

			span.End()
		}
	}
}
