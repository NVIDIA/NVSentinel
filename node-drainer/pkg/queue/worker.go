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

package queue

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// fetchEventFromDB retrieves the full event document from the database by its native ID.
func fetchEventFromDB(ctx context.Context, documentID interface{}, database DataStore) (datastore.Event, error) {
	filter := map[string]interface{}{"_id": documentID}

	result, err := database.FindDocument(ctx, filter, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch event %v from database: %w", documentID, err)
	}

	if result.Err() != nil {
		return nil, fmt.Errorf("event %v not found in database: %w", documentID, result.Err())
	}

	var event datastore.Event
	if err := result.Decode(&event); err != nil {
		return nil, fmt.Errorf("failed to decode event %v: %w", documentID, err)
	}

	if _, hasID := event["_id"]; !hasID {
		event["_id"] = documentID
	}

	return event, nil
}

// Interfaces are defined in types.go

func (m *eventQueueManager) Start(ctx context.Context) {
	slog.InfoContext(ctx, "Starting workqueue processor")

	go m.runWorker(ctx)
}

func (m *eventQueueManager) runWorker(ctx context.Context) {
	for m.processNextWorkItem(ctx) {
	}

	slog.InfoContext(ctx, "Worker stopped")
}

func (m *eventQueueManager) processNextWorkItem(ctx context.Context) bool {
	nodeEvent, shutdown := m.queue.Get()
	if shutdown {
		return false
	}

	defer m.queue.Done(nodeEvent)

	if nodeEvent.Database == nil || nodeEvent.HealthEventStore == nil {
		slog.ErrorContext(ctx, "NodeEvent missing database or health event store, dropping",
			"node", nodeEvent.NodeName, "eventID", nodeEvent.EventID)
		m.queue.Forget(nodeEvent)
		metrics.QueueDepth.Set(float64(m.queue.Len()))

		return true
	}

	event, fetchErr := fetchEventFromDB(ctx, nodeEvent.DocumentID, nodeEvent.Database)
	if fetchErr != nil {
		slog.WarnContext(ctx, "Failed to fetch event from database (will retry)",
			"node", nodeEvent.NodeName, "eventID", nodeEvent.EventID, "error", fetchErr)
		m.queue.AddRateLimited(nodeEvent)
		metrics.QueueDepth.Set(float64(m.queue.Len()))

		return true
	}

	traceID := ""
	parentSpanID := ""

	if healthEvent, err := eventutil.ParseHealthEventFromEvent(event); err == nil {
		traceID = tracing.TraceIDFromMetadata(healthEvent.HealthEvent.GetMetadata())
		parentSpanID = tracing.ParentSpanID(healthEvent.HealthEventStatus.SpanIds, tracing.ServiceFaultQuarantine)
	}

	raw, _ := m.sessions.LoadOrStore(nodeEvent.EventID, &DrainSession{})
	session := raw.(*DrainSession)

	slog.DebugContext(ctx, "Drain session trace context",
		"node", nodeEvent.NodeName,
		"eventID", nodeEvent.EventID,
		"docTraceID", traceID,
		"docParentSpanID", parentSpanID,
		"drainSessionSpanExists", session.DrainSessionSpan != nil,
	)

	processCtx := ctx

	if session.DrainSessionSpan == nil {
		if traceID != "" {
			var spanCtx context.Context

			spanCtx, session.DrainSessionSpan = tracing.StartSpanWithLinkFromTraceContext(
				ctx, traceID, parentSpanID, "node_drainer.drain_session")
			processCtx = spanCtx
		}
	} else {
		processCtx = trace.ContextWithSpan(ctx, session.DrainSessionSpan)
	}

	processCtx = ContextWithDrainSession(processCtx, session)

	err := m.processEventGeneric(processCtx, event, nodeEvent.Database, nodeEvent.HealthEventStore, nodeEvent.NodeName)
	if err != nil {
		slog.WarnContext(processCtx, "Error processing event for node (will retry)",
			"node", nodeEvent.NodeName,
			"attempt", m.queue.NumRequeues(nodeEvent)+1,
			"error", err)
		m.queue.AddRateLimited(nodeEvent)
	} else {
		if session.DrainSessionSpan != nil {
			session.DrainSessionSpan.End()
		}

		m.sessions.Delete(nodeEvent.EventID)
		m.queue.Forget(nodeEvent)
	}

	metrics.QueueDepth.Set(float64(m.queue.Len()))

	return true
}

func (m *eventQueueManager) processEventGeneric(ctx context.Context,
	event datastore.Event, database DataStore, healthEventStore datastore.HealthEventStore, nodeName string) error {
	if m.dataStoreEventProcessor == nil {
		return fmt.Errorf("no datastore event processor configured")
	}

	return m.dataStoreEventProcessor.ProcessEventGeneric(ctx, event, database, healthEventStore, nodeName)
}
