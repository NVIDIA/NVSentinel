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
	"slices"
	"sync"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	recoveredEventDedupRetention      = 24 * time.Hour
	liveRecoveryStoreRetryAttempts    = 4
	liveRecoveryStoreRetryInitialWait = 100 * time.Millisecond
)

type liveSkipCompletionError struct {
	err error
}

func (e *liveSkipCompletionError) Error() string       { return e.err.Error() }
func (e *liveSkipCompletionError) Unwrap() error       { return e.err }
func (e *liveSkipCompletionError) withholdCheckpoint() {}

type recoveryOverlapLookupError struct {
	err error
}

func (e *recoveryOverlapLookupError) Error() string       { return e.err.Error() }
func (e *recoveryOverlapLookupError) Unwrap() error       { return e.err }
func (e *recoveryOverlapLookupError) withholdCheckpoint() {}

type checkpointWithholdingError interface {
	error
	withholdCheckpoint()
}

type EventWatcher struct {
	changeStreamWatcher  client.ChangeStreamWatcher
	databaseClient       client.DatabaseClient
	processEventCallback func(
		ctx context.Context,
		event *model.HealthEventWithStatus,
	) (*model.Status, error)
	fetchDocIDsFn                         func(ctx context.Context, nodeName string) []string
	unprocessedEventsMetricUpdateInterval time.Duration
	lastProcessedObjectID                 LastProcessedObjectIDStore
	coldStartCallback                     func(ctx context.Context) error
	recoveredEventIDs                     sync.Map
	recoveryOverlapCutoff                 time.Time
	// healthEventStore lists stored events with their IDs. The cancellation of a session needs
	// the IDs, and a PostgreSQL document does not hold its own ID.
	healthEventStore datastore.HealthEventStore
}

type LastProcessedObjectIDStore interface {
	StoreLastProcessedObjectID(objID string)
	LoadLastProcessedObjectID() (string, bool)
}

type EventWatcherInterface interface {
	Start(ctx context.Context) error
	SetProcessEventCallback(callback func(ctx context.Context, event *model.HealthEventWithStatus) (*model.Status, error))
	SetFetchDocIDsFn(fn func(ctx context.Context, nodeName string) []string)
	SetColdStartCallback(callback func(ctx context.Context) error)
	ProcessStoredEvent(
		ctx context.Context,
		event model.HealthEventWithStatus,
		documentID string,
	) (coldstart.ProcessResult, error)
	CompleteStoredEvents(
		ctx context.Context,
		completions []coldstart.StoredEventCompletion,
	) error
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
	event *model.HealthEventWithStatus) (*model.Status, error)) {
	w.processEventCallback = callback
}

func (w *EventWatcher) SetFetchDocIDsFn(fn func(ctx context.Context, nodeName string) []string) {
	w.fetchDocIDsFn = fn
}

// SetHealthEventStore sets the store that the cancellation of a session lists events from.
func (w *EventWatcher) SetHealthEventStore(store datastore.HealthEventStore) {
	w.healthEventStore = store
}

func (w *EventWatcher) SetColdStartCallback(callback func(ctx context.Context) error) {
	w.coldStartCallback = callback
}

func (w *EventWatcher) Start(ctx context.Context) error {
	slog.InfoContext(ctx, "Starting event watcher")

	if w.changeStreamWatcher != nil {
		// Events at or before this boundary may also be handled by cold start.
		// Keep it before opening the stream so there is no unchecked overlap gap.
		w.recoveryOverlapCutoff = time.Now().UTC()
		w.changeStreamWatcher.Start(ctx)
	} else {
		<-ctx.Done()
		return nil
	}

	metricCtx, stopMetric := context.WithCancel(ctx)
	defer stopMetric()

	go w.updateUnprocessedEventsMetric(metricCtx)

	if err := w.runColdStart(ctx); err != nil {
		w.closeAfterColdStart(ctx)

		return err
	}

	if ctx.Err() != nil {
		w.closeAfterColdStart(ctx)

		return nil //nolint:nilerr // Cancellation is the expected shutdown path.
	}

	w.armRecoveredEventExpiry(time.Now())
	go w.expireRecoveredEventIDs(ctx)

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

func (w *EventWatcher) runColdStart(ctx context.Context) error {
	if w.coldStartCallback == nil {
		return nil
	}

	err := w.coldStartCallback(ctx)
	if err == nil {
		return nil
	}

	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		slog.InfoContext(ctx, "Cold-start recovery stopped during shutdown")

		return nil //nolint:nilerr // Cancellation is the expected shutdown path.
	}

	return fmt.Errorf("cold-start recovery failed: %w", err)
}

func (w *EventWatcher) closeAfterColdStart(ctx context.Context) {
	if err := w.changeStreamWatcher.Close(ctx); err != nil {
		slog.ErrorContext(ctx, "Failed to close event watcher after cold-start failure", "error", err)
	}
}

func (w *EventWatcher) watchEvents(ctx context.Context) error {
	for event := range w.changeStreamWatcher.Events() {
		metrics.TotalEventsReceived.Inc()

		if processErr := w.processEvent(ctx, event); processErr != nil {
			var withholdErr checkpointWithholdingError
			if errors.As(processErr, &withholdErr) {
				return fmt.Errorf("live event requires replay before checkpointing: %w", processErr)
			}

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

	// GetRecordUUID returns the actual database primary key:
	//   MongoDB  → ObjectID hex (same as GetDocumentID)
	//   PostgreSQL → UUID (different from the changelog sequence ID returned by GetDocumentID)
	recordUUID, err := event.GetRecordUUID()
	if err != nil {
		return fmt.Errorf("error getting record UUID: %w", err)
	}

	// Store the record UUID on the proto so that when it is serialized into the
	// quarantineHealthEvent annotation, node-drainer can use it for DB lookups
	// (e.g. checking whether previous drains completed for the node).
	if healthEventWithStatus.HealthEvent != nil {
		healthEventWithStatus.HealthEvent.Id = recordUUID
	}

	w.lastProcessedObjectID.StoreLastProcessedObjectID(eventID)

	skip, err := w.shouldSkipRecoveredLiveEvent(
		ctx, healthEventWithStatus.CreatedAt, recordUUID)
	if err != nil {
		return err
	}

	if skip {
		return nil
	}

	processed, err := w.processHealthEvent(ctx, &healthEventWithStatus, recordUUID, eventID)
	if err != nil {
		return err
	}

	return w.completeLiveEventIfSkipped(ctx, recordUUID, processed)
}

func (w *EventWatcher) shouldSkipRecoveredLiveEvent(
	ctx context.Context,
	createdAt time.Time,
	recordUUID string,
) (bool, error) {
	if expiry, recovered := w.recoveredEventIDs.LoadAndDelete(recordUUID); recovered {
		expiresAt, ok := expiry.(time.Time)
		if ok && (expiresAt.IsZero() || time.Now().Before(expiresAt)) {
			slog.DebugContext(ctx, "Skipping live duplicate of a recovered event", "eventID", recordUUID)

			return true, nil
		}
	}

	alreadyTerminal, err := w.recoveryOverlapEventAlreadyTerminal(ctx, createdAt, recordUUID)
	if err != nil {
		return false, err
	}

	if alreadyTerminal {
		slog.DebugContext(ctx, "Skipping durably completed recovery-overlap event", "eventID", recordUUID)
	}

	return alreadyTerminal, nil
}

func (w *EventWatcher) recoveryOverlapEventAlreadyTerminal(
	ctx context.Context,
	createdAt time.Time,
	recordUUID string,
) (bool, error) {
	if w.recoveryOverlapCutoff.IsZero() ||
		(!createdAt.IsZero() && createdAt.After(w.recoveryOverlapCutoff)) {
		return false, nil
	}

	nodeStatusPath := "healtheventstatus.nodequarantined"
	terminalStatus := query.Or(
		query.Eq(coldstart.RecoveryCompletionStatusPath, coldstart.RecoveryCompletionValue),
		query.And(
			query.Ne(nodeStatusPath, nil),
			query.Ne(nodeStatusPath, ""),
			query.Ne(nodeStatusPath, string(model.StatusNotStarted)),
		),
	)
	filter := query.New().Build(query.And(query.Eq("_id", recordUUID), terminalStatus))
	limit := int64(1)

	var count int64

	err := retryLiveRecoveryStoreOperation(ctx, func() error {
		var err error

		count, err = w.databaseClient.CountDocuments(ctx, filter, &client.CountOptions{Limit: &limit})

		return err
	})
	if err != nil {
		return false, &recoveryOverlapLookupError{err: fmt.Errorf(
			"failed to check recovery-overlap state for event %s: %w", recordUUID, err)}
	}

	return count > 0, nil
}

func (w *EventWatcher) completeLiveEventIfSkipped(
	ctx context.Context,
	recordUUID string,
	processed bool,
) error {
	if processed {
		return nil
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// A successful callback with no status is an intentional terminal skip. Record
	// that decision while the event is live so a later cold start cannot reapply it
	// under a different rule configuration.
	err := retryLiveRecoveryStoreOperation(ctx, func() error {
		return w.databaseClient.UpdateDocumentStatus(
			ctx, recordUUID, coldstart.RecoveryCompletionStatusPath, coldstart.RecoveryCompletionValue)
	})
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("update_recovery_completion_status_error").Inc()

		return &liveSkipCompletionError{err: fmt.Errorf(
			"failed to record skipped live event completion: %w", err)}
	}

	return nil
}

func retryLiveRecoveryStoreOperation(ctx context.Context, operation func() error) error {
	var lastErr error

	for attempt := range liveRecoveryStoreRetryAttempts {
		if err := ctx.Err(); err != nil {
			return err
		}

		lastErr = operation()
		if lastErr == nil {
			return nil
		}

		if attempt == liveRecoveryStoreRetryAttempts-1 {
			break
		}

		delay := liveRecoveryStoreRetryInitialWait * time.Duration(1<<attempt)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}

			return ctx.Err()
		case <-timer.C:
		}
	}

	return lastErr
}

// ProcessStoredEvent sends a durable health-event document through the same
// callback and status update path used for live change-stream events.
func (w *EventWatcher) ProcessStoredEvent(
	ctx context.Context,
	healthEventWithStatus model.HealthEventWithStatus,
	recordUUID string,
) (coldstart.ProcessResult, error) {
	recoveryCtx := coldstart.WithRecoveryContext(ctx)

	processed, err := w.processHealthEvent(
		recoveryCtx, &healthEventWithStatus, recordUUID, recordUUID)
	if err != nil {
		if !coldstart.IsPermanentError(err) {
			return coldstart.ProcessResultFailed, fmt.Errorf("recovered event processing failed: %w", err)
		}

		slog.ErrorContext(ctx, "Skipping stored event with a permanent processing error", "error", err)

		if !processed {
			return coldstart.ProcessResultInvalid, nil
		}
	}

	if !processed {
		return coldstart.ProcessResultSkipped, nil
	}

	w.rememberRecoveredEvent(recordUUID)

	return coldstart.ProcessResultProcessed, nil
}

// CompleteStoredEvents prevents terminal recovery decisions from becoming part
// of every subsequent startup scan. One update per scan batch avoids a database
// round trip for every historical event.
func (w *EventWatcher) CompleteStoredEvents(
	ctx context.Context,
	completions []coldstart.StoredEventCompletion,
) error {
	if len(completions) == 0 {
		return nil
	}

	ids, deduplicated := completionIDs(completions)

	if len(ids) == 0 {
		return nil
	}

	filter := query.New().Build(query.In("_id", ids))
	update := query.NewUpdate().Set(
		coldstart.RecoveryCompletionStatusPath, coldstart.RecoveryCompletionValue)

	if _, err := w.databaseClient.UpdateManyDocuments(ctx, filter, update); err != nil {
		return fmt.Errorf("failed to update recovery completion statuses: %w", err)
	}

	for id := range deduplicated {
		w.rememberRecoveredEvent(id)
	}

	return nil
}

func completionIDs(completions []coldstart.StoredEventCompletion) ([]any, map[string]struct{}) {
	ids := make([]any, 0, len(completions))
	seen := make(map[string]struct{}, len(completions))
	deduplicated := make(map[string]struct{}, len(completions))

	for _, completion := range completions {
		id := completion.DocumentID
		if id.String == "" || id.Native == nil {
			continue
		}

		// Every completion is a terminal recovery decision. Suppress its buffered
		// live copy regardless of whether recovery processed, skipped, rejected, or
		// superseded it; replaying any of those decisions under a different state
		// or rule configuration is precisely what the completion marker prevents.
		deduplicated[id.String] = struct{}{}

		if _, exists := seen[id.String]; exists {
			continue
		}

		seen[id.String] = struct{}{}
		ids = append(ids, id.Native)
	}

	return ids, deduplicated
}

func (w *EventWatcher) rememberRecoveredEvent(eventID string) {
	// Keep an unarmed deadline until Start gives every recovered ID the same
	// retention window. Starting it here could expire early IDs during a long
	// cold start.
	w.recoveredEventIDs.Store(eventID, time.Time{})
}

func (w *EventWatcher) armRecoveredEventExpiry(now time.Time) {
	expiresAt := now.Add(recoveredEventDedupRetention)

	w.recoveredEventIDs.Range(func(key, _ any) bool {
		w.recoveredEventIDs.Store(key, expiresAt)

		return true
	})
}

func (w *EventWatcher) expireRecoveredEventIDs(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			w.recoveredEventIDs.Range(func(key, value any) bool {
				expiresAt, ok := value.(time.Time)
				if !ok || expiresAt.IsZero() || !now.Before(expiresAt) {
					w.recoveredEventIDs.Delete(key)
				}

				return true
			})
		}
	}
}

func (w *EventWatcher) processHealthEvent(
	ctx context.Context,
	healthEventWithStatus *model.HealthEventWithStatus,
	recordUUID string,
	eventID string,
) (bool, error) {
	if healthEventWithStatus.HealthEvent == nil || healthEventWithStatus.HealthEventStatus == nil {
		return false, fmt.Errorf("health event or status is nil")
	}

	if w.processEventCallback == nil {
		return false, fmt.Errorf("process event callback is not configured")
	}

	healthEventWithStatus.HealthEvent.Id = recordUUID

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

	status, processErr := w.processEventCallback(ctx, healthEventWithStatus)
	if withholdTransientRecoveryStatus(ctx, processErr) {
		// A partial status would remove this event from the next cold-start query,
		// permanently losing any rule action skipped by the transient failure.
		metrics.EventHandlingDuration.Observe(time.Since(startTime).Seconds())

		return false, processErr
	}

	if status != nil {
		if err := w.updateNodeQuarantineStatus(ctx, recordUUID, status); err != nil {
			metrics.ProcessingErrors.WithLabelValues("update_quarantine_status_error").Inc()
			slog.ErrorContext(ctx, "Failed to update node quarantine status", "error", err)

			return false, errors.Join(processErr, fmt.Errorf("failed to update node quarantine status: %w", err))
		}

		EmitNodeQuarantineDuration(status, healthEventWithStatus)

		if *status == model.UnQuarantined {
			w.emitRemediationDurationFromDocIDs(ctx, sourceDocIDs)
		}
	}

	duration := time.Since(startTime).Seconds()
	metrics.EventHandlingDuration.Observe(duration)

	return status != nil, processErr
}

func withholdTransientRecoveryStatus(ctx context.Context, err error) bool {
	return coldstart.IsRecoveryContext(ctx) && err != nil && !coldstart.IsPermanentError(err)
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

	uniqueIDs := make([]any, 0, len(docIDs))
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
			protos.RecommendedAction(doc.HealthEvent.RecommendedAction).String(),
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
		RecommendedAction  int32        `bson:"recommendedaction" json:"recommendedAction"`
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

func EmitRemediationDuration(nodeName, recommendedAction string, genTs time.Time, qft, dft *time.Time) {
	now := time.Now()

	if duration := now.Sub(genTs).Seconds(); duration > 0 {
		metrics.NodeRemediationDurationSeconds.WithLabelValues(recommendedAction).Observe(duration)
		slog.Info("Node remediation duration (end-to-end)",
			"node", nodeName, "recommended_action", recommendedAction, "duration_seconds", duration)
	}

	if qft != nil && dft != nil {
		drainDuration := dft.Sub(*qft).Seconds()
		endToEnd := now.Sub(genTs).Seconds()

		if durationExcludingDrain := endToEnd - drainDuration; durationExcludingDrain > 0 {
			metrics.NodeRemediationDurationExcludingDrainSeconds.
				WithLabelValues(recommendedAction).Observe(durationExcludingDrain)
			slog.Info("Node remediation duration (excluding drain)",
				"node", nodeName, "recommended_action", recommendedAction,
				"duration_seconds", durationExcludingDrain)
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

// quarantiningEvent is the part of a stored event that a cancellation reads.
type quarantiningEvent struct {
	ID          string    `bson:"_id" json:"_id"`
	CreatedAt   time.Time `bson:"createdAt" json:"createdAt"`
	HealthEvent struct {
		NodeName           string            `bson:"nodename" json:"nodeName"`
		GeneratedTimestamp *dbTimestamp      `bson:"generatedtimestamp" json:"generatedTimestamp"`
		Metadata           map[string]string `bson:"metadata" json:"metadata"`
		RecommendedAction  int32             `bson:"recommendedaction" json:"recommendedAction"`
	} `bson:"healthevent" json:"healthEvent"`
	HealthEventStatus struct {
		NodeQuarantined           string            `bson:"nodequarantined" json:"nodeQuarantined"`
		QuarantineFinishTimestamp *dbTimestamp      `bson:"quarantinefinishtimestamp,omitempty" json:"quarantineFinishTimestamp"` //nolint:lll
		DrainFinishTimestamp      *dbTimestamp      `bson:"drainfinishtimestamp,omitempty" json:"drainFinishTimestamp"`
		SpanIDs                   map[string]string `bson:"spanids"`
	} `bson:"healtheventstatus" json:"healthEventStatus"`
}

// sessionListBatchSize is the batch size of the store lists of a cancellation.
const sessionListBatchSize = 200

// cancelBatchSize bounds the IDs of one update, below the PostgreSQL limit on bind parameters.
const cancelBatchSize = 1000

// FindQuarantineSession returns the first event of the quarantine session that holds the events
// eventIDs, and whether that first event is still Quarantined. A session starts with a
// Quarantined event and holds the AlreadyQuarantined events after it. The first event stays
// Quarantined when a healthy event ends the session, and the healthy event is UnQuarantined. A
// cancelled session is Cancelled, and its first event is cancelled last. When the first event
// is among eventIDs, it is used as is. Otherwise the first event is the latest Quarantined or
// UnQuarantined event before the earliest of eventIDs in the order of the store.
// common.QuarantineCancellationLatestSession stands for the latest session of the node. The
// store calls retry a failure and keep the caller's deadline.
func (w *EventWatcher) FindQuarantineSession(
	ctx context.Context,
	nodeName string,
	eventIDs []string,
) (string, bool, error) {
	if len(eventIDs) == 1 && eventIDs[0] == common.QuarantineCancellationLatestSession {
		start, found, err := w.findSessionStart(ctx, nodeName, nil)
		if err != nil {
			return "", false, fmt.Errorf("error finding the latest quarantine session of node %s: %w", nodeName, err)
		}

		return start.id, found && start.status == model.Quarantined, nil
	}

	ids := make([]any, 0, len(eventIDs))
	for _, id := range eventIDs {
		ids = append(ids, id)
	}

	events, err := w.listSessionEvents(ctx, query.And(
		query.In("_id", ids),
		query.Eq("healthevent.nodename", nodeName),
	), nil)
	if err != nil {
		return "", false, fmt.Errorf("error finding events %v of node %s: %w", eventIDs, nodeName, err)
	}

	if len(events) == 0 {
		slog.WarnContext(ctx, "Events of an ended quarantine session not found", "node", nodeName, "eventIDs", eventIDs)
		return "", false, nil
	}

	for _, event := range events {
		if event.status == model.Quarantined {
			return event.id, true, nil
		}
	}

	return w.sessionOfLaterEvent(ctx, nodeName, events[0])
}

// sessionOfLaterEvent returns the first event of the session that holds a later event of the
// session, and whether that first event is Quarantined. The later event can be
// AlreadyQuarantined, already written by a cancellation, or still without a status.
func (w *EventWatcher) sessionOfLaterEvent(
	ctx context.Context,
	nodeName string,
	event sessionEvent,
) (string, bool, error) {
	start, found, err := w.findSessionStart(ctx, nodeName, &event)
	if err != nil {
		return "", false, fmt.Errorf("error finding the quarantine session of event %s: %w", event.id, err)
	}

	if !found {
		slog.WarnContext(ctx, "No first event found before an event of an ended quarantine session",
			"node", nodeName, "eventID", event.id)

		return "", false, nil
	}

	return start.id, start.status == model.Quarantined, nil
}

// findSessionStart returns the latest Quarantined or UnQuarantined event of a node, before the
// given event when one is set.
func (w *EventWatcher) findSessionStart(
	ctx context.Context,
	nodeName string,
	before *sessionEvent,
) (sessionEvent, bool, error) {
	conditions := []query.Condition{
		query.Eq("healthevent.nodename", nodeName),
		query.In("healtheventstatus.nodequarantined",
			[]any{string(model.Quarantined), string(model.UnQuarantined)}),
	}
	if before != nil {
		conditions = append(conditions, query.Lte("createdAt", before.createdAt))
	}

	starts, err := w.listSessionEvents(ctx, query.And(conditions...), nil)
	if err != nil {
		return sessionEvent{}, false, err
	}

	for i := len(starts) - 1; i >= 0; i-- {
		if before == nil || starts[i].before(*before) {
			return starts[i], true, nil
		}
	}

	return sessionEvent{}, false, nil
}

// CancelLatestQuarantiningEvents cancels the latest quarantine session of a node, unless a
// healthy event ended it.
func (w *EventWatcher) CancelLatestQuarantiningEvents(ctx context.Context, nodeName string, reason string) error {
	startID, quarantined, err := w.FindQuarantineSession(
		ctx, nodeName, []string{common.QuarantineCancellationLatestSession})
	if err != nil {
		return err
	}

	if !quarantined {
		slog.InfoContext(ctx, "Latest event is not Quarantined, no events to cancel", "node", nodeName)
		return nil
	}

	return w.CancelQuarantineSession(ctx, nodeName, startID, reason)
}

// CancelQuarantineSession marks Cancelled the Quarantined event startID and the
// AlreadyQuarantined events after it. It leaves the session alone when a healthy event ended it,
// and when the first event of a newer quarantine of the node is in the store by the time it
// reads, so it does not cancel the newer quarantine. The first event is cancelled last: a call
// that fails part of the way leaves it Quarantined, and a later call cancels the rest. The store
// calls retry a failure and keep the caller's deadline.
func (w *EventWatcher) CancelQuarantineSession(
	ctx context.Context,
	nodeName string,
	startID string,
	reason string,
) error {
	_, span := tracing.StartSpan(ctx, "fault_quarantine.cancel_latest_quarantining_events")
	defer span.End()

	span.SetAttributes(
		attribute.String("fault_quarantine.node_name", nodeName),
		attribute.String("fault_quarantine.cancel.reason", reason),
	)

	eventIDs, cancel, err := w.sessionToCancel(ctx, nodeName, startID)
	if err != nil {
		tracing.RecordError(span, err)
		return fmt.Errorf("error finding the events of the session of node %s: %w", nodeName, err)
	}

	if !cancel {
		return nil
	}

	start, _, err := w.findQuarantiningEvent(ctx, query.And(
		query.Eq("_id", startID),
		query.Eq("healthevent.nodename", nodeName),
	), nil)
	if err != nil {
		tracing.RecordError(span, err)
		return fmt.Errorf("error reading the first event %s of node %s: %w", startID, nodeName, err)
	}

	latestTraceID := tracing.TraceIDFromMetadata(start.HealthEvent.Metadata)
	platformConnectorSpanId := tracing.ParentSpanID(start.HealthEventStatus.SpanIDs, tracing.ServicePlatformConnector)

	_, eventSpan := tracing.StartSpanWithLinkFromTraceContext(
		context.Background(), latestTraceID, platformConnectorSpanId,
		"fault_quarantine.cancel_latest_quarantining_events")
	defer eventSpan.End()

	// The store calls keep the caller's deadline and cancellation.
	ctx = trace.ContextWithSpan(ctx, eventSpan)

	var cancelled int64

	for batch := range slices.Chunk(eventIDs, cancelBatchSize) {
		eventsResult, err := w.cancelEvents(ctx, query.And(
			query.In("_id", batch),
			query.Eq("healtheventstatus.nodequarantined", string(model.AlreadyQuarantined)),
		))
		if err != nil {
			tracing.RecordError(eventSpan, err)
			return fmt.Errorf("error cancelling the events of the session of node %s: %w", nodeName, err)
		}

		cancelled += eventsResult.ModifiedCount
	}

	updateResult, err := w.cancelEvents(ctx, query.And(
		query.Eq("_id", startID),
		query.Eq("healtheventstatus.nodequarantined", string(model.Quarantined)),
	))
	if err != nil {
		tracing.RecordError(eventSpan, err)
		return fmt.Errorf("error cancelling the first event of the session of node %s: %w", nodeName, err)
	}

	slog.InfoContext(ctx, "Updated quarantining events to cancelled status",
		"node", nodeName,
		"firstEventId", startID,
		"documentsUpdated", cancelled+updateResult.ModifiedCount)

	if updateResult.ModifiedCount > 0 {
		emitCancelledRemediationDuration(
			start.HealthEvent.NodeName,
			protos.RecommendedAction(start.HealthEvent.RecommendedAction).String(),
			start.HealthEvent.GeneratedTimestamp,
			start.HealthEventStatus.QuarantineFinishTimestamp,
			start.HealthEventStatus.DrainFinishTimestamp,
			nodeName,
		)
	}

	eventSpan.SetAttributes(
		attribute.String("fault_quarantine.event.node_quarantined", string(model.Cancelled)),
		attribute.String("fault_quarantine.event.reason", reason),
	)

	return nil
}

// sessionToCancel returns the IDs of the AlreadyQuarantined events of the session that starts
// with startID, and whether to cancel the session. It reads those events before it looks for a
// later Quarantined or UnQuarantined event. An event of a newer session that is AlreadyQuarantined
// by the first read has its Quarantined first event in the store by then, so the second read
// finds the newer session and nothing is cancelled. An event written after the first read is not
// in the list.
func (w *EventWatcher) sessionToCancel(ctx context.Context, nodeName, startID string) ([]any, bool, error) {
	start, found, err := w.findSessionEvent(ctx, nodeName, startID)
	if err != nil {
		return nil, false, err
	}

	if !found || start.status != model.Quarantined {
		slog.InfoContext(ctx, "First event of the session is not Quarantined, no events to cancel",
			"node", nodeName, "eventID", startID)

		return nil, false, nil
	}

	events, err := w.listSessionEvents(ctx, query.And(
		query.Eq("healthevent.nodename", nodeName),
		query.Eq("healtheventstatus.nodequarantined", string(model.AlreadyQuarantined)),
		query.Gte("createdAt", start.createdAt),
	), nil)
	if err != nil {
		return nil, false, err
	}

	next, found, err := w.findNextSessionStart(ctx, nodeName, start)
	if err != nil {
		return nil, false, err
	}

	if found {
		if next.status == model.UnQuarantined {
			slog.InfoContext(ctx, "A healthy event ended the session, no events to cancel",
				"node", nodeName, "eventID", startID)
		} else {
			slog.WarnContext(ctx, "A newer quarantine of the node started before the ended session was "+
				"cancelled; the earlier health events stay active", "node", nodeName, "eventID", startID)
		}

		return nil, false, nil
	}

	eventIDs := make([]any, 0, len(events))

	for _, event := range events {
		if start.before(event) {
			eventIDs = append(eventIDs, event.id)
		}
	}

	return eventIDs, true, nil
}

// findNextSessionStart returns the first Quarantined or UnQuarantined event of a node after
// start: the healthy event that ended its session, or the first event of a newer session.
func (w *EventWatcher) findNextSessionStart(
	ctx context.Context,
	nodeName string,
	start sessionEvent,
) (sessionEvent, bool, error) {
	later, err := w.listSessionEvents(ctx, query.And(
		query.Eq("healthevent.nodename", nodeName),
		query.In("healtheventstatus.nodequarantined",
			[]any{string(model.Quarantined), string(model.UnQuarantined)}),
		query.Gte("createdAt", start.createdAt),
	), start.before)
	if err != nil || len(later) == 0 || !start.before(later[len(later)-1]) {
		return sessionEvent{}, false, err
	}

	return later[len(later)-1], true, nil
}

// cancelEvents marks the events that match the filter Cancelled. A multi-document update is not
// a retryable write for the MongoDB driver, so a primary election fails it once. The filter
// takes only Quarantined or AlreadyQuarantined events, so running it again is safe.
func (w *EventWatcher) cancelEvents(ctx context.Context, filter query.Condition) (*client.UpdateResult, error) {
	update := map[string]any{
		"$set": map[string]any{
			"healtheventstatus.nodequarantined": string(model.Cancelled),
		},
	}

	var updateResult *client.UpdateResult

	err := retryLiveRecoveryStoreOperation(ctx, func() error {
		var updateErr error

		updateResult, updateErr = w.databaseClient.UpdateManyDocuments(ctx, query.New().Build(filter), update)

		return updateErr
	})

	return updateResult, err
}

// findQuarantiningEvent returns the first event that matches the filter, retrying a failed
// read. The MongoDB client reports a failed query from Decode, so the lookup and the decode
// retry together. Finding no event is an answer, so it returns found=false without a retry.
func (w *EventWatcher) findQuarantiningEvent(
	ctx context.Context,
	filter query.Condition,
	findOptions *client.FindOneOptions,
) (quarantiningEvent, bool, error) {
	var (
		event quarantiningEvent
		found bool
	)

	err := retryLiveRecoveryStoreOperation(ctx, func() error {
		event, found = quarantiningEvent{}, false

		result, err := w.databaseClient.FindOne(ctx, query.New().Build(filter), findOptions)
		if err == nil {
			err = result.Decode(&event)
		}

		switch {
		case err == nil:
			found = true

			return nil
		case errors.Is(err, client.ErrNoDocuments) || client.IsNoDocumentsError(err):
			return nil
		default:
			return err
		}
	})

	return event, found, err
}

// sessionEvent is a stored event of a quarantine session. createdAt is the creation time the
// store orders and filters by: the createdAt field in MongoDB and the created_at column in
// PostgreSQL. The PostgreSQL document holds an earlier createdAt from platform-connectors, and
// events of one MongoDB batch can share a millisecond, so events are ordered by createdAt and then
// by ID, as the store lists them.
type sessionEvent struct {
	id        string
	createdAt time.Time
	status    model.Status
}

// before reports whether the event comes before other in the order of the store.
func (e sessionEvent) before(other sessionEvent) bool {
	if !e.createdAt.Equal(other.createdAt) {
		return e.createdAt.Before(other.createdAt)
	}

	return e.id < other.id
}

// findSessionEvent returns the event eventID of a node.
func (w *EventWatcher) findSessionEvent(ctx context.Context, nodeName, eventID string) (sessionEvent, bool, error) {
	events, err := w.listSessionEvents(ctx, query.And(
		query.Eq("_id", eventID),
		query.Eq("healthevent.nodename", nodeName),
	), nil)
	if err != nil || len(events) == 0 {
		return sessionEvent{}, false, err
	}

	return events[0], true, nil
}

// listSessionEvents returns the events that match the filter in the order of the store, retrying
// a failed read. When stopAfter is set, the list ends with the first event it returns true for.
func (w *EventWatcher) listSessionEvents(
	ctx context.Context,
	filter query.Condition,
	stopAfter func(sessionEvent) bool,
) ([]sessionEvent, error) {
	if w.healthEventStore == nil {
		return nil, errors.New("health event store is not configured")
	}

	var events []sessionEvent

	errStop := errors.New("stop listing")

	err := retryLiveRecoveryStoreOperation(ctx, func() error {
		events = nil

		err := w.healthEventStore.FindHealthEventsByQueryBatched(ctx, query.New().Build(filter), sessionListBatchSize,
			func(records []datastore.HealthEventWithStatus) error {
				for _, record := range records {
					event, err := toSessionEvent(record)
					if err != nil {
						return err
					}

					events = append(events, event)

					if stopAfter != nil && stopAfter(event) {
						return errStop
					}
				}

				return nil
			})
		if errors.Is(err, errStop) {
			return nil
		}

		return err
	})

	return events, err
}

func toSessionEvent(record datastore.HealthEventWithStatus) (sessionEvent, error) {
	id, err := utils.ExtractDocumentID(record.RawEvent)
	if err != nil {
		return sessionEvent{}, fmt.Errorf("error reading the ID of a stored event: %w", err)
	}

	event := sessionEvent{id: id, createdAt: record.CreatedAt}
	if record.HealthEventStatus.NodeQuarantined != nil {
		event.status = model.Status(*record.HealthEventStatus.NodeQuarantined)
	}

	return event, nil
}

func emitCancelledRemediationDuration(
	nodeName, recommendedAction string,
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
		recommendedAction,
		genTs,
		protoTsToTimePtr(qfTS, nodeName),
		protoTsToTimePtr(dfTS, nodeName),
	)
}
