// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package prom exposes health events reaching the platform connector as Prometheus
// counters. Every monitor publishes through the platform connector, so one connector
// here covers gpu-health-monitor, syslog-health-monitor, nic-health-monitor,
// csp-health-monitor, kubernetes-object-monitor and health-events-analyzer without
// touching any of them individually.
package prom

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
)

// PromConnector observes health events and records them as Prometheus counters.
type PromConnector struct {
	ringBuffer *ringbuffer.RingBuffer
}

// InitializePromConnector creates a connector that records health events as metrics.
// It has no external dependency, so unlike the other connectors it cannot fail.
func InitializePromConnector(ringBuffer *ringbuffer.RingBuffer) *PromConnector {
	return &PromConnector{ringBuffer: ringBuffer}
}

// FetchAndProcessHealthMetric drains the ring buffer, recording one counter increment
// per event. Recording cannot fail, so an item is always completed and never requeued:
// a metrics connector must not be able to hold up the queue it shares with the sinks.
func (p *PromConnector) FetchAndProcessHealthMetric(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "Context canceled, exiting prom connector processing loop")

			return
		default:
			queuedHealthEvents, quit := p.ringBuffer.Dequeue()
			if quit {
				slog.InfoContext(ctx, "Queue signaled shutdown, exiting prom connector processing loop")

				return
			}

			for _, event := range queuedHealthEvents.Events.GetEvents() {
				recordEvent(event)
			}

			p.ringBuffer.HealthMetricEleProcessingCompleted(queuedHealthEvents)
		}
	}
}

// recordEvent increments the counter for one health event.
func recordEvent(event *protos.HealthEvent) {
	if event == nil {
		return
	}

	healthEventsTotal.WithLabelValues(
		event.GetNodeName(),
		event.GetAgent(),
		event.GetCheckName(),
		event.GetRecommendedAction().String(),
		strconv.FormatBool(event.GetIsFatal()),
		strconv.FormatBool(event.GetIsHealthy()),
	).Inc()
}

// ShutdownRingBuffer drains the connector's ring buffer.
func (p *PromConnector) ShutdownRingBuffer(ctx context.Context) {
	if p.ringBuffer != nil {
		slog.InfoContext(ctx, "Shutting down prom connector ring buffer with drain")
		p.ringBuffer.ShutDownHealthMetricQueue()
		slog.InfoContext(ctx, "Prom connector ring buffer drained successfully")
	}
}
