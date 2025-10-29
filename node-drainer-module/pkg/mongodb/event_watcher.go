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
	"fmt"
	"log/slog"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/metrics"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/queue"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

type EventWatcher struct {
	mongoConfig   storewatcher.MongoDBConfig
	tokenConfig   storewatcher.TokenConfig
	mongoPipeline mongo.Pipeline
	queueManager  queue.EventQueueManager
	collection    queue.MongoCollectionAPI
}

func NewEventWatcher(
	mongoConfig storewatcher.MongoDBConfig,
	tokenConfig storewatcher.TokenConfig,
	mongoPipeline mongo.Pipeline,
	queueManager queue.EventQueueManager,
	collection queue.MongoCollectionAPI,
) *EventWatcher {
	return &EventWatcher{
		mongoConfig:   mongoConfig,
		tokenConfig:   tokenConfig,
		mongoPipeline: mongoPipeline,
		queueManager:  queueManager,
		collection:    collection,
	}
}

func (w *EventWatcher) Start(ctx context.Context) error {
	slog.Info("Starting MongoDB event watcher")

	if err := w.handleColdStart(ctx); err != nil {
		slog.Error("Failed to handle cold start", "error", err)
		return fmt.Errorf("failed to cold start: %w", err)
	}

	watcher, err := storewatcher.NewChangeStreamWatcher(ctx, w.mongoConfig, w.tokenConfig, w.mongoPipeline)
	if err != nil {
		return fmt.Errorf("failed to create change stream watcher: %w", err)
	}
	defer watcher.Close(ctx)

	watcher.Start(ctx)
	slog.Info("MongoDB change stream watcher started successfully")

	for {
		select {
		case <-ctx.Done():
			slog.Info("Context cancelled, stopping MongoDB event watcher")
			return nil
		case event := <-watcher.Events():
			if err := w.preprocessAndEnqueueEvent(ctx, event); err != nil {
				slog.Error("Failed to preprocess and enqueue event", "error", err)
				return fmt.Errorf("failed to preprocess and enqueue event: %w", err)
			}

			if err := watcher.MarkProcessed(ctx); err != nil {
				slog.Error("Error updating resume token", "error", err)
				return fmt.Errorf("failed to update resume token: %w", err)
			}
		}
	}
}

func (w *EventWatcher) Stop() error {
	slog.Info("Stopping MongoDB event watcher")
	return nil
}

func (w *EventWatcher) handleColdStart(ctx context.Context) error {
	slog.Info("Handling cold start - processing existing in-progress events")

	inProgressEvents, err := w.getInProgressEvents(ctx)
	if err != nil {
		return fmt.Errorf("failed to get in-progress events: %w", err)
	}

	slog.Info("Found in-progress events to process", "count", len(inProgressEvents))

	for _, event := range inProgressEvents {
		// Wrap the event in the same format as change stream events
		wrappedEvent := bson.M{
			"fullDocument": event,
		}

		if err := w.preprocessAndEnqueueEvent(ctx, wrappedEvent); err != nil {
			slog.Error("Failed to enqueue cold start event", "error", err)
		} else {
			metrics.TotalEventsReplayed.Inc()
		}
	}

	slog.Info("Cold start processing completed")

	return nil
}

func (w *EventWatcher) getInProgressEvents(ctx context.Context) ([]bson.M, error) {
	filter := bson.M{
		"healtheventstatus.userpodsevictionstatus.status": model.StatusInProgress,
	}

	cursor, err := w.collection.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("failed to find in-progress events: %w", err)
	}
	defer cursor.Close(ctx)

	var events []bson.M
	if err := cursor.All(ctx, &events); err != nil {
		return nil, fmt.Errorf("failed to decode in-progress events: %w", err)
	}

	return events, nil
}

func (w *EventWatcher) preprocessAndEnqueueEvent(ctx context.Context, event bson.M) error {
	healthEventWithStatus := model.HealthEventWithStatus{}
	if err := storewatcher.UnmarshalFullDocumentFromEvent(event, &healthEventWithStatus); err != nil {
		return fmt.Errorf("failed to unmarshal health event: %w", err)
	}

	if isTerminalStatus(healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus.Status) {
		slog.Info("Skipping health event as it's already in terminal state",
			slog.Any("event", healthEventWithStatus.HealthEvent))
		return nil
	}

	slog.Info("Enqueuing",
		slog.Any("event", healthEventWithStatus.HealthEvent),
		slog.Any("userPodEvictingStatus", healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus))

	// Extract fullDocument to access the actual document _id
	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		return fmt.Errorf("error extracting fullDocument from event: %+v", event)
	}

	filter := bson.M{
		"_id": document["_id"],
		"healtheventstatus.userpodsevictionstatus.status": bson.M{"$ne": model.StatusInProgress},
	}
	update := bson.M{
		"$set": bson.M{
			"healtheventstatus.userpodsevictionstatus.status": model.StatusInProgress,
		},
	}

	result, err := w.collection.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("error updating initial status: %w", err)
	}

	if result.ModifiedCount > 0 {
		slog.Info("Set initial eviction status to InProgress for node",
			"node", healthEventWithStatus.HealthEvent.NodeName)
	}

	nodeName := healthEventWithStatus.HealthEvent.NodeName

	return w.queueManager.EnqueueEvent(ctx, nodeName, event, w.collection)
}

func isTerminalStatus(status model.Status) bool {
	return status == model.StatusSucceeded ||
		status == model.StatusFailed ||
		status == model.AlreadyDrained
}
