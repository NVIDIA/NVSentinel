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

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

type EventWatcher struct {
	changeStreamWatcher  client.ChangeStreamWatcher
	databaseClient       client.DatabaseClient
	processEventCallback func(
		ctx context.Context,
		event *model.HealthEventWithStatus,
	) *model.Status
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
	CancelLatestQuarantiningEvents(ctx context.Context, nodeName string) error
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

func (w *EventWatcher) Start(ctx context.Context) error {
	slog.Info("Starting event watcher")

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
			slog.Error("Event watcher goroutine failed", "error", err)

			watchDoneCh <- err
		} else {
			slog.Error("Event watcher goroutine exited unexpectedly, event processing has stopped")

			watchDoneCh <- fmt.Errorf("event watcher channel closed unexpectedly")
		}
	}()

	var watchErr error

	select {
	case <-ctx.Done():
		slog.Info("Context cancelled, stopping event watcher")
	case err := <-watchDoneCh:
		slog.Error("Event watcher terminated unexpectedly, initiating shutdown", "error", err)
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
			slog.Error("Event processing failed, but still marking as processed to proceed ahead", "error", processErr)
		}

		// Extract the resume token from the event to avoid race condition
		// where the change stream cursor advances before we call MarkProcessed
		resumeToken := event.GetResumeToken()
		if err := w.changeStreamWatcher.MarkProcessed(ctx, resumeToken); err != nil {
			metrics.ProcessingErrors.WithLabelValues("mark_processed_error").Inc()
			slog.Error("Failed to mark event as processed", "error", err)

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

	slog.Debug("Processing event", "event", healthEventWithStatus)

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

	startTime := time.Now()

	status := w.processEventCallback(ctx, &healthEventWithStatus)
	if status != nil {
		if err := w.updateNodeQuarantineStatus(ctx, recordUUID, status); err != nil {
			metrics.ProcessingErrors.WithLabelValues("update_quarantine_status_error").Inc()
			slog.Error("Failed to update node quarantine status", "error", err)

			return fmt.Errorf("failed to update node quarantine status: %w", err)
		}
	}

	duration := time.Since(startTime).Seconds()
	metrics.EventHandlingDuration.Observe(duration)

	return nil
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
					slog.Debug("Failed to get unprocessed event count", "error", err)
					continue
				}

				metrics.EventBacklogSize.Set(float64(unprocessedCount))
				slog.Debug("Updated unprocessed events metric", "count", unprocessedCount, "afterObjectID", objID)
			} else {
				slog.Debug("Change stream watcher does not support metrics")
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
	err := client.UpdateHealthEventNodeQuarantineStatus(ctx, w.databaseClient, eventID, string(*nodeQuarantinedStatus))
	if err != nil {
		return fmt.Errorf("error updating node quarantine status: %w", err)
	}

	slog.Info("Document updated with status", "id", eventID, "status", *nodeQuarantinedStatus)

	return nil
}

func (w *EventWatcher) CancelLatestQuarantiningEvents(
	ctx context.Context,
	nodeName string,
) error {
	// Find the latest Quarantined or UnQuarantined event to check current state of node
	filter := map[string]interface{}{
		"healthevent.nodename": nodeName,
		"healtheventstatus.nodequarantined": map[string]interface{}{
			"$in": []interface{}{model.Quarantined, model.UnQuarantined},
		},
	}

	findOptions := &client.FindOneOptions{
		Sort: map[string]interface{}{"createdAt": -1},
	}

	var latestEvent struct {
		ID                string    `bson:"_id"`
		CreatedAt         time.Time `bson:"createdAt"`
		HealthEventStatus struct {
			NodeQuarantined *model.Status `bson:"nodequarantined"`
		} `bson:"healtheventstatus"`
	}

	result, err := w.databaseClient.FindOne(ctx, filter, findOptions)
	if err != nil {
		if errors.Is(err, client.ErrNoDocuments) {
			slog.Debug("No quarantining/unquarantining events found for node", "node", nodeName)

			return nil
		}

		slog.Error("Error finding latest quarantining event", "node", nodeName, "error", err)

		return fmt.Errorf("error finding latest quarantining event for node %s: %w", nodeName, err)
	}

	if err := result.Decode(&latestEvent); err != nil {
		slog.Error("Error decoding latest event", "node", nodeName, "error", err)

		return fmt.Errorf("error decoding latest quarantining event for node %s: %w", nodeName, err)
	}

	slog.Debug("Found latest event",
		"node", nodeName,
		"eventID", latestEvent.ID,
		"status", latestEvent.HealthEventStatus.NodeQuarantined)

	// Only cancel if latest status is Quarantined (not if already UnQuarantined by healthy event)
	if latestEvent.HealthEventStatus.NodeQuarantined == nil ||
		*latestEvent.HealthEventStatus.NodeQuarantined != model.Quarantined {
		slog.Debug("Latest event is not Quarantined, no events to cancel", "node", nodeName)

		return nil
	}

	// Update all events from the current quarantine session (Quarantined + AlreadyQuarantined)
	// This includes the first event and all subsequent events that occurred after it
	updateFilter := map[string]interface{}{
		"healthevent.nodename": nodeName,
		"createdAt":            map[string]interface{}{"$gte": latestEvent.CreatedAt},
		"healtheventstatus.nodequarantined": map[string]interface{}{
			"$in": []interface{}{model.Quarantined, model.AlreadyQuarantined},
		},
	}

	update := map[string]interface{}{
		"$set": map[string]interface{}{
			"healtheventstatus.nodequarantined": model.Cancelled,
		},
	}

	updateResult, err := w.databaseClient.UpdateManyDocuments(ctx, updateFilter, update)
	if err != nil {
		return fmt.Errorf("error cancelling quarantining events for node %s: %w", nodeName, err)
	}

	slog.Info("Updated quarantining events to cancelled status",
		"node", nodeName,
		"firstEventId", latestEvent.ID,
		"documentsUpdated", updateResult.ModifiedCount)

	return nil
}
