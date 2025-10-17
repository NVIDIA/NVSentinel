// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/metrics"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/queue"
	storeconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"k8s.io/klog/v2"
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
	klog.Info("Starting MongoDB event watcher")

	// Cold start failure shouldn't prevent normal operation
	if err := w.handleColdStart(ctx); err != nil {
		klog.Errorf("Failed to handle cold start: %v", err)
	}

	watcher, err := storewatcher.NewChangeStreamWatcher(ctx, w.mongoConfig, w.tokenConfig, w.mongoPipeline)
	if err != nil {
		return fmt.Errorf("failed to create change stream watcher: %w", err)
	}
	defer watcher.Close(ctx)

	watcher.Start(ctx)
	klog.Info("MongoDB change stream watcher started successfully")

	for {
		select {
		case <-ctx.Done():
			klog.Info("Context cancelled, stopping MongoDB event watcher")
			return nil
		case event := <-watcher.Events():
			if err := w.preprocessAndEnqueueEvent(ctx, event); err != nil {
				klog.Errorf("Failed to preprocess and enqueue event: %v", err)
				continue
			}

			if err := watcher.MarkProcessed(ctx); err != nil {
				klog.Errorf("Error updating resume token: %v", err)
			}
		}
	}
}

func (w *EventWatcher) Stop() error {
	klog.Info("Stopping MongoDB event watcher")
	return nil
}

func (w *EventWatcher) handleColdStart(ctx context.Context) error {
	klog.Info("Handling cold start - processing existing in-progress events")

	inProgressEvents, err := w.getInProgressEvents(ctx)
	if err != nil {
		return fmt.Errorf("failed to get in-progress events: %w", err)
	}

	klog.Infof("Found %d in-progress events to process", len(inProgressEvents))

	for _, event := range inProgressEvents {
		// Wrap the event in the same format as change stream events
		wrappedEvent := bson.M{
			"fullDocument": event,
		}

		if err := w.preprocessAndEnqueueEvent(ctx, wrappedEvent); err != nil {
			klog.Errorf("Failed to enqueue cold start event: %v", err)
		} else {
			metrics.TotalEventsReplayed.Inc()
		}
	}

	klog.Info("Cold start processing completed")

	return nil
}

func (w *EventWatcher) getInProgressEvents(ctx context.Context) ([]bson.M, error) {
	filter := bson.M{
		"healtheventstatus.userpodsevictionstatus.status": storeconnector.StatusInProgress,
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
	healthEventWithStatus := storeconnector.HealthEventWithStatus{}
	if err := storewatcher.UnmarshalFullDocumentFromEvent(event, &healthEventWithStatus); err != nil {
		return fmt.Errorf("failed to unmarshal health event: %w", err)
	}

	if isTerminalStatus(healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus.Status) {
		klog.Infof("Skipping health event as it's already in terminal state: %+v",
			healthEventWithStatus.HealthEvent)
		return nil
	}

	klog.Infof("Enqueuing event: %+v", healthEventWithStatus.HealthEvent)
	klog.Infof("Current UserPodsEvictionStatus: %+v", healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus)

	// Extract fullDocument to access the actual document _id
	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		return fmt.Errorf("error extracting fullDocument from event: %+v", event)
	}

	filter := bson.M{
		"_id": document["_id"],
		"healtheventstatus.userpodsevictionstatus.status": bson.M{"$ne": storeconnector.StatusInProgress},
	}
	update := bson.M{
		"$set": bson.M{
			"healtheventstatus.userpodsevictionstatus.status": storeconnector.StatusInProgress,
		},
	}

	result, err := w.collection.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("error updating initial status: %w", err)
	}

	if result.ModifiedCount > 0 {
		klog.Infof("Set initial eviction status to InProgress for node %s", healthEventWithStatus.HealthEvent.NodeName)
	}

	nodeName := healthEventWithStatus.HealthEvent.NodeName

	return w.queueManager.EnqueueEvent(ctx, nodeName, event, w.collection)
}

func isTerminalStatus(status storeconnector.Status) bool {
	return status == storeconnector.StatusSucceeded ||
		status == storeconnector.StatusFailed ||
		status == storeconnector.AlreadyDrained
}
