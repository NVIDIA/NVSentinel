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
	"fmt"
	"log/slog"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/client"
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
	SetProcessEventCallback(
		callback func(
			ctx context.Context,
			event *model.HealthEventWithStatus,
		) *model.Status,
	)
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

func (w *EventWatcher) SetProcessEventCallback(
	callback func(
		ctx context.Context,
		event *model.HealthEventWithStatus,
	) *model.Status,
) {
	w.processEventCallback = callback
}

func (w *EventWatcher) Start(ctx context.Context) error {
	slog.Info("Starting event watcher")

	w.changeStreamWatcher.Start(ctx)
	slog.Info("Change stream watcher started")

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

	w.changeStreamWatcher.Close(ctx)

	return watchErr
}

func (w *EventWatcher) watchEvents(ctx context.Context) error {
	for event := range w.changeStreamWatcher.Events() {
		metrics.TotalEventsReceived.Inc()

		if processErr := w.processEvent(ctx, event); processErr != nil {
			slog.Error("Event processing failed, but still marking as processed to proceed ahead", "error", processErr)
		}

		if err := w.changeStreamWatcher.MarkProcessed(ctx); err != nil {
			metrics.ProcessingErrors.WithLabelValues("mark_processed_error").Inc()
			slog.Error("Error updating resume token", "error", err)

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

	w.lastProcessedObjectID.StoreLastProcessedObjectID(eventID)

	startTime := time.Now()
	status := w.processEventCallback(ctx, &healthEventWithStatus)

	if status != nil {
		if err := w.updateNodeQuarantineStatus(ctx, eventID, status); err != nil {
			metrics.ProcessingErrors.WithLabelValues("update_quarantine_status_error").Inc()
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
				// If metrics are not supported, set metric to 0 or skip
				slog.Debug("Change stream watcher does not support metrics")
				metrics.EventBacklogSize.Set(0)
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
