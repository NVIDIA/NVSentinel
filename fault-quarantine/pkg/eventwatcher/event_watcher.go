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

package eventwatcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
	"go.opentelemetry.io/otel/attribute"
)

type EventWatcher struct {
	changeStreamWatcher  client.ChangeStreamWatcher
	databaseClient       client.DatabaseClient
	processEventCallback func(
		ctx context.Context,
		event *model.HealthEventWithStatus,
	) *model.Status
	fetchDocIDsFn                         func(ctx context.Context, nodeName string) []string
	unprocessedEventsMetricUpdateInterval time.Duration
	lastProcessedObjectID                 LastProcessedObjectIDStore
}

type LastProcessedObjectIDStore interface {
	StoreLastProcessedObjectID(objID string)
	LoadLastProcessedObjectID() (string, bool)
}

type EventWatcherInterface interface {
	Start(ctx context.Context) error
	SetProcessEventCallback(callback func(ctx context.Context, event *model.HealthEventWithStatus) *model.Status)
	SetFetchDocIDsFn(fn func(ctx context.Context, nodeName string) []string)
	CancelLatestQuarantiningEvents(ctx context.Context, nodeName string, reason string) error
}

func NewEventWatcher(
	changeStreamWatcher client.ChangeStreamWatcher,
	databaseClient client.DatabaseClient,
	unprocessedEventsMetricUpdateInterval time.Duration,
	lastProcessedObjectID LastProcessedObjectIDStore,
) *EventWatcher {
	return &EventWatcher{
		changeStreamWatcher:                   changeStreamWatcher,
		databaseClient:                        databaseClient,
		unprocessedEventsMetricUpdateInterval: unprocessedEventsMetricUpdateInterval,
		lastProcessedObjectID:                 lastProcessedObjectID,
	}
}

func (w *EventWatcher) SetProcessEventCallback(callback func(ctx context.Context,
	event *model.HealthEventWithStatus) *model.Status) {
	w.processEventCallback = callback
}

func (w *EventWatcher) SetFetchDocIDsFn(fn func(ctx context.Context, nodeName string) []string) {
	w.fetchDocIDsFn = fn
}

func (w *EventWatcher) Start(ctx context.Context) error {
	slog.InfoContext(ctx, "Starting event watcher")

	if w.changeStreamWatcher != nil {
		w.changeStreamWatcher.Start(ctx)
	} else {
		<-ctx.Done()
		return nil
	}

	go w.updateUnprocessedEventsMetric(ctx)

	watchDoneCh := make(chan error, 1)

	go func() {
		err := w.watchEvents(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "Event watcher goroutine failed", "error", err)

			watchDoneCh <- err
		} else {
			slog.ErrorContext(ctx, "Event watcher goroutine exited unexpectedly, event processing has stopped")

			watchDoneCh <- fmt.Errorf("event watcher channel closed unexpectedly")
		}
	}()

	var watchErr error

	select {
	case <-ctx.Done():
		slog.InfoContext(ctx, "Context cancelled, stopping event watcher")
	case err := <-watchDoneCh:
		slog.ErrorContext(ctx, "Event watcher terminated unexpectedly, initiating shutdown", "error", err)
		watchErr = fmt.Errorf("event watcher terminated: %w", err)
	}

	if w.changeStreamWatcher != nil {
		w.changeStreamWatcher.Close(ctx)
	}

	return watchErr
}

func (w *EventWatcher) watchEvents(ctx context.Context) error {
	for event := range w.changeStreamWatcher.Events() {
		metrics.TotalEventsReceived.Inc()

		if processErr := w.processEvent(ctx, event); processErr != nil {
			slog.ErrorContext(ctx, "Event processing failed, but still marking as processed to proceed ahead",
				"error", processErr)
		}

		// Extract the resume token from the event to avoid race condition
		// where the change stream cursor advances before we call MarkProcessed
		resumeToken := event.GetResumeToken()
		if err := w.changeStreamWatcher.MarkProcessed(ctx, resumeToken); err != nil {
			metrics.ProcessingErrors.WithLabelValues("mark_processed_error").Inc()
			slog.ErrorContext(ctx, "Failed to mark event as processed", "error", err)

			return fmt.Errorf("failed to mark event as processed: %w", err)
		}
	}

	return nil
}

func (w *EventWatcher) processEvent(ctx context.Context, event client.Event) error {
	healthEventWithStatus := model.HealthEventWithStatus{}

	err := event.UnmarshalDocument(&healthEventWithStatus)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("unmarshal_error").Inc()

		return fmt.Errorf("failed to unmarshal event: %w", err)
	}

	slog.DebugContext(ctx, "Processing event", "event", healthEventWithStatus)

	eventID, err := event.GetDocumentID()
	if err != nil {
		return fmt.Errorf("error getting document ID: %w", err)
	}

	// Rather than include the object ID on the model.HealthEventWithStatus struct, we will manually set the ID
	// on the nested protos.HealthEvent struct. Note that protos.HealthEvent structs are stored as part of the
	// quarantineHealthEvent annotation. We require that the object ID is available in events included in the
	// annotation so that the node-drainer can use this ID to look up if any previous drains have completed for
	// the given node.
	if healthEventWithStatus.HealthEvent != nil {
		healthEventWithStatus.HealthEvent.Id = eventID
	}

	// Get the record UUID for database updates (different from changelog ID for PostgreSQL)
	recordUUID, err := event.GetRecordUUID()
	if err != nil {
		return fmt.Errorf("error getting record UUID: %w", err)
	}

	w.lastProcessedObjectID.StoreLastProcessedObjectID(eventID)

	traceID := tracing.TraceIDFromMetadata(healthEventWithStatus.HealthEvent.GetMetadata())
	parentSpanID := tracing.ParentSpanID(healthEventWithStatus.HealthEventStatus.SpanIds, tracing.ServicePlatformConnector)

	// Short-lived span that marks the exact moment fault-quarantine received the
	// event. It ends immediately so it appears in the trace backend before
	// processing finishes, making it easy to see ingestion-to-processing latency.
	ctx, receivedSpan := tracing.StartSpanWithLinkFromTraceContext(ctx, traceID,
		parentSpanID, "fault_quarantine.event_received")
	tracing.AddHealthEventStatusAttributes(receivedSpan, healthEventWithStatus.HealthEventStatus, eventID)

	receivedSpan.End()

	// Processing span wraps the callback and subsequent DB status update so they
	// share the same trace context.
	ctx, processSpan := tracing.StartSpan(ctx, "fault_quarantine.process_event")
	defer processSpan.End()

	startTime := time.Now()

	var sourceDocIDs []string

	if healthEventWithStatus.HealthEvent.GetIsHealthy() && w.fetchDocIDsFn != nil {
		sourceDocIDs = w.fetchDocIDsFn(ctx, healthEventWithStatus.HealthEvent.GetNodeName())
	}

	status := w.processEventCallback(ctx, &healthEventWithStatus)

	if status != nil {
		if err := w.updateNodeQuarantineStatus(ctx, recordUUID, status); err != nil {
			metrics.ProcessingErrors.WithLabelValues("update_quarantine_status_error").Inc()
			slog.ErrorContext(ctx, "Failed to update node quarantine status", "error", err)

			return fmt.Errorf("failed to update node quarantine status: %w", err)
		}

		EmitNodeQuarantineDuration(status, &healthEventWithStatus)

		if *status == model.UnQuarantined {
			w.emitRemediationDurationFromDocIDs(ctx, sourceDocIDs)
		}
	}

	duration := time.Since(startTime).Seconds()
	metrics.EventHandlingDuration.Observe(duration)

	return nil
}

func EmitNodeQuarantineDuration(status *model.Status, healthEventWithStatus *model.HealthEventWithStatus) {
	if status == nil || *status != model.Quarantined {
		return
	}

	if healthEventWithStatus.HealthEvent == nil || healthEventWithStatus.HealthEvent.GetGeneratedTimestamp() == nil {
		return
	}

	genTs := healthEventWithStatus.HealthEvent.GetGeneratedTimestamp().AsTime()
	duration := time.Since(genTs).Seconds()

	slog.Info("Node quarantine duration", "duration", duration)

	if duration > 0 {
		metrics.NodeQuarantineDuration.Observe(duration)
	}
}

func (w *EventWatcher) emitRemediationDurationFromDocIDs(ctx context.Context, docIDs []string) {
	seen := make(map[string]struct{}, len(docIDs))

	uniqueIDs := make([]interface{}, 0, len(docIDs))
	for _, id := range docIDs {
		if id == "" {
			continue
		}

		if _, dup := seen[id]; dup {
			continue
		}

		seen[id] = struct{}{}
		uniqueIDs = append(uniqueIDs, id)
	}

	if len(uniqueIDs) == 0 {
		return
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	filter := query.New().Build(query.In("_id", uniqueIDs))

	cursor, err := w.databaseClient.Find(lookupCtx, filter, nil)
	if err != nil {
		slog.WarnContext(ctx, "emitRemediationDurationFromDocIDs: Find failed", "error", err)
		return
	}

	defer cursor.Close(lookupCtx)

	for cursor.Next(lookupCtx) {
		var doc remediationDoc
		if err := cursor.Decode(&doc); err != nil {
			slog.WarnContext(ctx, "emitRemediationDurationFromDocIDs: Decode failed", "error", err)
			continue
		}

		if doc.HealthEvent.GeneratedTimestamp == nil {
			slog.WarnContext(ctx, "emitRemediationDurationFromDocIDs: generatedTimestamp missing",
				"node", doc.HealthEvent.NodeName)

			continue
		}

		genTs := time.Unix(doc.HealthEvent.GeneratedTimestamp.Seconds,
			int64(doc.HealthEvent.GeneratedTimestamp.Nanos))

		qft := protoTsToTimePtr(doc.HealthEventStatus.QuarantineFinishTimestamp, doc.HealthEvent.NodeName)
		dft := protoTsToTimePtr(doc.HealthEventStatus.DrainFinishTimestamp, doc.HealthEvent.NodeName)

		EmitRemediationDuration(
			doc.HealthEvent.NodeName,
			genTs,
			qft,
			dft,
		)
	}

	if err := cursor.Err(); err != nil {
		slog.WarnContext(ctx, "emitRemediationDurationFromDocIDs: cursor error", "error", err)
	}
}

type remediationDoc struct {
	HealthEvent struct {
		NodeName           string       `bson:"nodename" json:"nodeName"`
		GeneratedTimestamp *dbTimestamp `bson:"generatedtimestamp" json:"generatedTimestamp"`
	} `bson:"healthevent" json:"healthEvent"`
	HealthEventStatus struct {
		QuarantineFinishTimestamp *dbTimestamp `bson:"quarantinefinishtimestamp,omitempty" json:"quarantineFinishTimestamp"`
		DrainFinishTimestamp      *dbTimestamp `bson:"drainfinishtimestamp,omitempty" json:"drainFinishTimestamp"`
	} `bson:"healtheventstatus" json:"healthEventStatus"`
}

type dbTimestamp struct {
	Seconds int64 `bson:"seconds" json:"seconds"`
	Nanos   int32 `bson:"nanos" json:"nanos"`
}

func protoTsToTimePtr(ts *dbTimestamp, nodeName string) *time.Time {
	if ts == nil {
		slog.Warn("protoTsToTimePtr: received nil timestamp", "node", nodeName)

		return nil
	}

	t := time.Unix(ts.Seconds, int64(ts.Nanos))

	return &t
}

func EmitRemediationDuration(nodeName string, genTs time.Time, qft, dft *time.Time) {
	now := time.Now()

	if duration := now.Sub(genTs).Seconds(); duration > 0 {
		metrics.NodeRemediationDurationSeconds.Observe(duration)
		slog.Info("Node remediation duration (end-to-end)", "node", nodeName, "duration_seconds", duration)
	}

	if qft != nil && dft != nil {
		drainDuration := dft.Sub(*qft).Seconds()
		endToEnd := now.Sub(genTs).Seconds()

		if durationExcludingDrain := endToEnd - drainDuration; durationExcludingDrain > 0 {
			metrics.NodeRemediationDurationExcludingDrainSeconds.Observe(durationExcludingDrain)
			slog.Info("Node remediation duration (excluding drain)",
				"node", nodeName, "duration_seconds", durationExcludingDrain)
		}
	}
}

func (w *EventWatcher) updateUnprocessedEventsMetric(ctx context.Context) {
	ticker := time.NewTicker(w.unprocessedEventsMetricUpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			objID, ok := w.lastProcessedObjectID.LoadLastProcessedObjectID()
			if !ok {
				continue
			}

			// Try to get metrics if the watcher supports it
			if metricsWatcher, ok := w.changeStreamWatcher.(client.ChangeStreamMetrics); ok {
				unprocessedCount, err := metricsWatcher.GetUnprocessedEventCount(ctx, objID)
				if err != nil {
					slog.DebugContext(ctx, "Failed to get unprocessed event count", "error", err)
					continue
				}

				metrics.EventBacklogSize.Set(float64(unprocessedCount))
				slog.DebugContext(ctx, "Updated unprocessed events metric", "count", unprocessedCount, "afterObjectID", objID)
			} else {
				slog.DebugContext(ctx, "Change stream watcher does not support metrics")
				metrics.EventBacklogSize.Set(-1)
			}
		}
	}
}

func (w *EventWatcher) updateNodeQuarantineStatus(
	ctx context.Context,
	eventID string,
	nodeQuarantinedStatus *model.Status,
) error {
	// Create a span for the DB status update. This span's ID becomes the parent
	// for downstream consumers (node-drainer) because the status write is the
	// trigger point for their change stream.
	spanCtx, dbSpan := tracing.StartSpan(ctx, "fault_quarantine.db.update_status")
	defer dbSpan.End()

	dbSpan.SetAttributes(
		attribute.String("fault_quarantine.event.status", string(*nodeQuarantinedStatus)),
	)

	err := client.UpdateHealthEventNodeQuarantineStatus(spanCtx, w.databaseClient, eventID,
		string(*nodeQuarantinedStatus), tracing.SpanIDFromSpan(dbSpan))
	if err != nil {
		return fmt.Errorf("error updating node quarantine status: %w", err)
	}

	slog.InfoContext(ctx, "Document updated with status", "id", eventID, "status", *nodeQuarantinedStatus)

	return nil
}

func (w *EventWatcher) CancelLatestQuarantiningEvents(
	ctx context.Context,
	nodeName string,
	reason string,
) error {
	_, span := tracing.StartSpan(ctx, "fault_quarantine.cancel_latest_quarantining_events")
	defer span.End()

	span.SetAttributes(
		attribute.String("fault_quarantine.node_name", nodeName),
		attribute.String("fault_quarantine.cancel.reason", reason),
	)

	// Find the latest Quarantined or UnQuarantined event to check current state of node
	filter := query.New().Build(query.And(
		query.Eq("healthevent.nodename", nodeName),
		query.In("healtheventstatus.nodequarantined",
			[]interface{}{string(model.Quarantined), string(model.UnQuarantined)}),
	))

	findOptions := &client.FindOneOptions{
		Sort: map[string]interface{}{"createdAt": -1},
	}

	var latestEvent struct {
		ID          string    `bson:"_id" json:"_id"`
		CreatedAt   time.Time `bson:"createdAt" json:"createdAt"`
		HealthEvent struct {
			NodeName           string            `bson:"nodename" json:"nodeName"`
			GeneratedTimestamp *dbTimestamp      `bson:"generatedtimestamp" json:"generatedTimestamp"`
			Metadata           map[string]string `bson:"metadata" json:"metadata"`
		} `bson:"healthevent" json:"healthEvent"`
		HealthEventStatus struct {
			NodeQuarantined           string            `bson:"nodequarantined" json:"nodeQuarantined"`
			QuarantineFinishTimestamp *dbTimestamp      `bson:"quarantinefinishtimestamp,omitempty" json:"quarantineFinishTimestamp"` //nolint:lll
			DrainFinishTimestamp      *dbTimestamp      `bson:"drainfinishtimestamp,omitempty" json:"drainFinishTimestamp"`
			SpanIDs                   map[string]string `bson:"spanids"`
		} `bson:"healtheventstatus" json:"healthEventStatus"`
	}

	result, err := w.databaseClient.FindOne(ctx, filter, findOptions)
	if err != nil {
		if errors.Is(err, client.ErrNoDocuments) {
			slog.WarnContext(ctx, "No quarantining/unquarantining events found for node", "node", nodeName)

			span.SetAttributes(
				attribute.String("fault_quarantine.error.type", "no_quarantining_events_found"),
				attribute.String("fault_quarantine.error.message", err.Error()),
			)

			return nil
		}

		slog.ErrorContext(ctx, "Error finding latest quarantining event", "node", nodeName, "error", err)
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_quarantine.error.type", "error_finding_latest_quarantining_event"),
			attribute.String("fault_quarantine.error.message", err.Error()),
		)

		return fmt.Errorf("error finding latest quarantining event for node %s: %w", nodeName, err)
	}

	if err := result.Decode(&latestEvent); err != nil {
		if errors.Is(err, client.ErrNoDocuments) || client.IsNoDocumentsError(err) {
			slog.WarnContext(ctx, "No quarantining/unquarantining events found for node", "node", nodeName)

			span.SetAttributes(
				attribute.String("fault_quarantine.error.type", "no_quarantining_events_found"),
				attribute.String("fault_quarantine.error.message", err.Error()),
			)

			return nil
		}

		slog.ErrorContext(ctx, "Error decoding latest event", "node", nodeName, "error", err)

		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_quarantine.error.type", "error_decoding_latest_quarantining_event"),
			attribute.String("fault_quarantine.error.message", err.Error()),
		)

		return fmt.Errorf("error decoding latest quarantining event for node %s: %w", nodeName, err)
	}

	slog.DebugContext(ctx, "Found latest event",
		"node", nodeName,
		"eventID", latestEvent.ID,
		"status", latestEvent.HealthEventStatus.NodeQuarantined)

	// Only cancel if latest status is Quarantined (not if already UnQuarantined by healthy event)
	if latestEvent.HealthEventStatus.NodeQuarantined == "" ||
		latestEvent.HealthEventStatus.NodeQuarantined != string(model.Quarantined) {
		slog.InfoContext(ctx, "Latest event is not Quarantined, no events to cancel", "node", nodeName)

		span.SetAttributes(
			attribute.String("fault_quarantine.error.type", "latest_event_not_quarantined"),
			attribute.String("fault_quarantine.error.message", "latest event is not Quarantined"),
		)

		return nil
	}

	latestTraceID := tracing.TraceIDFromMetadata(latestEvent.HealthEvent.Metadata)
	platformConnectorSpanId := tracing.ParentSpanID(
		latestEvent.HealthEventStatus.SpanIDs, tracing.ServicePlatformConnector)

	ctx, eventSpan := tracing.StartSpanWithLinkFromTraceContext(
		context.Background(), latestTraceID, platformConnectorSpanId,
		"fault_quarantine.cancel_latest_quarantining_events")
	defer eventSpan.End()

	// Update all events from the current quarantine session (Quarantined + AlreadyQuarantined)
	// This includes the first event and all subsequent events that occurred after it
	updateFilter := query.New().Build(query.And(
		query.Eq("healthevent.nodename", nodeName),
		query.Gte("createdAt", latestEvent.CreatedAt),
		query.In("healtheventstatus.nodequarantined",
			[]interface{}{string(model.Quarantined), string(model.AlreadyQuarantined)}),
	))

	update := map[string]interface{}{
		"$set": map[string]interface{}{
			"healtheventstatus.nodequarantined": string(model.Cancelled),
		},
	}

	updateResult, err := w.databaseClient.UpdateManyDocuments(ctx, updateFilter, update)
	if err != nil {
		tracing.RecordError(eventSpan, err)
		eventSpan.SetAttributes(
			attribute.String("fault_quarantine.error.type", "error_cancelling_quarantining_events"),
			attribute.String("fault_quarantine.error.message", err.Error()),
		)

		return fmt.Errorf("error cancelling quarantining events for node %s: %w", nodeName, err)
	}

	slog.InfoContext(ctx, "Updated quarantining events to cancelled status",
		"node", nodeName,
		"firstEventId", latestEvent.ID,
		"documentsUpdated", updateResult.ModifiedCount)

	emitCancelledRemediationDuration(
		latestEvent.HealthEvent.NodeName,
		latestEvent.HealthEvent.GeneratedTimestamp,
		latestEvent.HealthEventStatus.QuarantineFinishTimestamp,
		latestEvent.HealthEventStatus.DrainFinishTimestamp,
		nodeName,
	)

	eventSpan.SetAttributes(
		attribute.String("fault_quarantine.event.node_quarantined", string(model.Cancelled)),
		attribute.String("fault_quarantine.event.reason", reason),
	)

	return nil
}

func emitCancelledRemediationDuration(
	nodeName string,
	genTS, qfTS, dfTS *dbTimestamp,
	logNode string,
) {
	if genTS == nil {
		slog.Warn("Cannot emit remediation duration: generatedTimestamp missing in latest event", "node", logNode)
		return
	}

	genTs := time.Unix(genTS.Seconds, int64(genTS.Nanos))

	EmitRemediationDuration(
		nodeName,
		genTs,
		protoTsToTimePtr(qfTS, nodeName),
		protoTsToTimePtr(dfTS, nodeName),
	)
}
