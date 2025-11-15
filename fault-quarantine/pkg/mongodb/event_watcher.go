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

package mongodb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type MongoCollectionInterface interface {
	UpdateOne(ctx context.Context, filter interface{}, update interface{},
		opts ...*options.UpdateOptions) (*mongo.UpdateResult, error)
	UpdateMany(ctx context.Context, filter interface{}, update interface{},
		opts ...*options.UpdateOptions) (*mongo.UpdateResult, error)
	FindOne(ctx context.Context, filter interface{}, opts ...*options.FindOneOptions) *mongo.SingleResult
}

type EventWatcher struct {
	mongoConfig          storewatcher.MongoDBConfig
	tokenConfig          storewatcher.TokenConfig
	mongoPipeline        mongo.Pipeline
	collection           MongoCollectionInterface
	watcher              *storewatcher.ChangeStreamWatcher
	processEventCallback func(
		ctx context.Context,
		event *model.HealthEventWithStatus,
	) *model.Status
	unprocessedEventsMetricUpdateInterval time.Duration
	lastProcessedObjectID                 LastProcessedObjectIDStore
}

type LastProcessedObjectIDStore interface {
	StoreLastProcessedObjectID(objID primitive.ObjectID)
	LoadLastProcessedObjectID() (primitive.ObjectID, bool)
}

type EventWatcherInterface interface {
	Start(ctx context.Context) error
	SetProcessEventCallback(
		callback func(
			ctx context.Context,
			event *model.HealthEventWithStatus,
		) *model.Status,
	)
	CancelLatestQuarantiningEvents(ctx context.Context, nodeName string) error
}

func NewEventWatcher(
	mongoConfig storewatcher.MongoDBConfig,
	tokenConfig storewatcher.TokenConfig,
	mongoPipeline mongo.Pipeline,
	collection MongoCollectionInterface,
	lastProcessedObjectID LastProcessedObjectIDStore,
) *EventWatcher {
	return &EventWatcher{
		mongoConfig:           mongoConfig,
		tokenConfig:           tokenConfig,
		mongoPipeline:         mongoPipeline,
		collection:            collection,
		lastProcessedObjectID: lastProcessedObjectID,

		unprocessedEventsMetricUpdateInterval: time.Second *
			time.Duration(mongoConfig.UnprocessedEventsMetricUpdateIntervalSeconds),
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
	slog.Info("Starting MongoDB event watcher")

	watcher, err := storewatcher.NewChangeStreamWatcher(ctx, w.mongoConfig, w.tokenConfig, w.mongoPipeline)
	if err != nil {
		return fmt.Errorf("failed to create change stream watcher: %w", err)
	}

	w.watcher = watcher

	watcher.Start(ctx)
	slog.Info("MongoDB change stream watcher started successfully")

	go w.updateUnprocessedEventsMetric(ctx, watcher)

	watchDoneCh := make(chan error, 1)

	go func() {
		err := w.watchEvents(ctx, watcher)
		if err != nil {
			slog.Error("MongoDB event watcher goroutine failed", "error", err)

			watchDoneCh <- err
		} else {
			slog.Error("MongoDB event watcher goroutine exited unexpectedly, event processing has stopped")

			watchDoneCh <- fmt.Errorf("event watcher channel closed unexpectedly")
		}
	}()

	var watchErr error

	select {
	case <-ctx.Done():
		slog.Info("Context cancelled, stopping MongoDB event watcher")
	case err := <-watchDoneCh:
		slog.Error("Event watcher terminated unexpectedly, initiating shutdown", "error", err)
		watchErr = fmt.Errorf("event watcher terminated: %w", err)
	}

	watcher.Close(ctx)

	return watchErr
}

func (w *EventWatcher) watchEvents(ctx context.Context, watcher *storewatcher.ChangeStreamWatcher) error {
	for event := range watcher.Events() {
		metrics.TotalEventsReceived.Inc()

		if processErr := w.processEvent(ctx, event); processErr != nil {
			slog.Error("Event processing failed, but still marking as processed to proceed ahead", "error", processErr)
		}

		if err := w.watcher.MarkProcessed(ctx); err != nil {
			metrics.ProcessingErrors.WithLabelValues("mark_processed_error").Inc()
			slog.Error("Error updating resume token", "error", err)

			return fmt.Errorf("failed to mark event as processed: %w", err)
		}
	}

	return nil
}

func (w *EventWatcher) processEvent(ctx context.Context, event bson.M) error {
	healthEventWithStatus := model.HealthEventWithStatus{}

	err := storewatcher.UnmarshalFullDocumentFromEvent(
		event,
		&healthEventWithStatus,
	)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("unmarshal_error").Inc()

		return fmt.Errorf("failed to unmarshal event: %w", err)
	}

	slog.Debug("Processing event", "event", healthEventWithStatus)
	w.storeEventObjectID(event)

	startTime := time.Now()
	status := w.processEventCallback(ctx, &healthEventWithStatus)

	if status != nil {
		if err := w.updateNodeQuarantineStatus(ctx, event, status); err != nil {
			metrics.ProcessingErrors.WithLabelValues("update_quarantine_status_error").Inc()
			return fmt.Errorf("failed to update node quarantine status: %w", err)
		}
	}

	duration := time.Since(startTime).Seconds()
	metrics.EventHandlingDuration.Observe(duration)

	return nil
}

func (w *EventWatcher) storeEventObjectID(eventBson bson.M) {
	if fullDoc, ok := eventBson["fullDocument"].(bson.M); ok {
		if objID, ok := fullDoc["_id"].(primitive.ObjectID); ok {
			w.lastProcessedObjectID.StoreLastProcessedObjectID(objID)
		}
	}
}

func (w *EventWatcher) updateUnprocessedEventsMetric(ctx context.Context,
	watcher *storewatcher.ChangeStreamWatcher) {
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

			unprocessedCount, err := watcher.GetUnprocessedEventCount(ctx, objID)
			if err != nil {
				slog.Debug("Failed to get unprocessed event count", "error", err)
				continue
			}

			metrics.EventBacklogSize.Set(float64(unprocessedCount))
			slog.Debug("Updated unprocessed events metric", "count", unprocessedCount, "afterObjectID", objID.Hex())
		}
	}
}

func (w *EventWatcher) updateNodeQuarantineStatus(
	ctx context.Context,
	event bson.M,
	nodeQuarantinedStatus *model.Status,
) error {
	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		return fmt.Errorf("error extracting fullDocument from event")
	}

	filter := bson.M{"_id": document["_id"]}

	update := bson.M{
		"$set": bson.M{
			"healtheventstatus.nodequarantined": *nodeQuarantinedStatus,
		},
	}

	if _, err := w.collection.UpdateOne(ctx, filter, update); err != nil {
		return fmt.Errorf("error updating document with _id: %v, error: %w", document["_id"], err)
	}

	slog.Info("Document updated with status", "id", document["_id"], "status", *nodeQuarantinedStatus)

	return nil
}

func (w *EventWatcher) CancelLatestQuarantiningEvents(
	ctx context.Context,
	nodeName string,
) error {
	// Find the latest Quarantined or UnQuarantined event to check current state of node
	filter := bson.M{
		"healthevent.nodename": nodeName,
		"healtheventstatus.nodequarantined": bson.M{
			"$in": []model.Status{model.Quarantined, model.UnQuarantined},
		},
	}

	findOptions := options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}})

	var latestEvent struct {
		ID                primitive.ObjectID `bson:"_id"`
		CreatedAt         time.Time          `bson:"createdAt"`
		HealthEventStatus struct {
			NodeQuarantined *model.Status `bson:"nodequarantined"`
		} `bson:"healtheventstatus"`
	}

	err := w.collection.FindOne(ctx, filter, findOptions).Decode(&latestEvent)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			slog.Warn("No quarantining/unquarantining events found for node", "node", nodeName)
			return nil
		}

		return fmt.Errorf("error finding latest quarantining event for node %s: %w", nodeName, err)
	}

	// Only cancel if latest status is Quarantined (not if already UnQuarantined by healthy event)
	if *latestEvent.HealthEventStatus.NodeQuarantined != model.Quarantined {
		slog.Info("No latest quarantining event found for node, no events to cancel", "node", nodeName)
		return nil
	}

	// Update all events from the current quarantine session (Quarantined + AlreadyQuarantined)
	// This includes the first event and all subsequent events that occurred after it
	updateFilter := bson.M{
		"healthevent.nodename": nodeName,
		"createdAt":            bson.M{"$gte": latestEvent.CreatedAt},
		"healtheventstatus.nodequarantined": bson.M{
			"$in": []model.Status{model.Quarantined, model.AlreadyQuarantined},
		},
	}

	update := bson.M{
		"$set": bson.M{
			"healtheventstatus.nodequarantined": model.Cancelled,
		},
	}

	result, err := w.collection.UpdateMany(ctx, updateFilter, update)
	if err != nil {
		return fmt.Errorf("error cancelling quarantining events for node %s: %w", nodeName, err)
	}

	slog.Info("Updated quarantining events to cancelled status",
		"node", nodeName,
		"firstEventId", latestEvent.ID.Hex(),
		"documentsUpdated", result.ModifiedCount)

	return nil
}
