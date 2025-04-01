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

package reconciler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	storeconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/store"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/store-client-sdk/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"k8s.io/klog"
)

const (
	maxRetries = 5
	retryDelay = 10 * time.Second
)

type ReconcilerConfig struct {
	MongoConfig   storewatcher.MongoDBConfig
	TokenConfig   storewatcher.TokenConfig
	MongoPipeline mongo.Pipeline
	K8sClient     FaultRemediationClientInterface
}

type Reconciler struct {
	Config              ReconcilerConfig
	NodeEvictionContext sync.Map
	DryRun              bool
}

func NewReconciler(cfg ReconcilerConfig, dryRunEnabled bool) *Reconciler {
	return &Reconciler{Config: cfg, NodeEvictionContext: sync.Map{}, DryRun: dryRunEnabled}
}

func (r *Reconciler) Start(ctx context.Context) {

	watcher, err := storewatcher.NewChangeStreamWatcher(ctx, r.Config.MongoConfig, r.Config.TokenConfig, r.Config.MongoPipeline)
	if err != nil {
		log.Fatalf("failed to create change stream watcher: %+v", err)
	}

	defer func() {
		if err := watcher.Close(ctx); err != nil {
			klog.Errorf("failed to close watcher: %+v", err)
		}
	}()

	collection, err := storewatcher.GetCollectionClient(ctx, r.Config.MongoConfig)
	if err != nil {
		klog.Fatalf("error initializing collection client with config %+v for mongodb: %+v", r.Config.MongoConfig, err)
	}

	watcher.Start(ctx)

	klog.Info("Listening for events on the channel...")

	for event := range watcher.Events() {
		klog.Info("Event received....")
		totalEventsReceived.Inc()
		healthEventWithStatus := storeconnector.HealthEventWithStatus{}
		if err := storewatcher.UnmarshalFullDocumentFromEvent(
			event,
			&healthEventWithStatus,
		); err != nil {
			totalEventProcessingError.WithLabelValues("unmarshal_doc_error", healthEventWithStatus.HealthEvent.NodeName).Inc()
			klog.Errorf("Failed to unmarshal event: %+v", err)
			if err := watcher.MarkProcessed(ctx); err != nil {
				totalEventProcessingError.WithLabelValues("mark_processed_error", healthEventWithStatus.HealthEvent.NodeName).Inc()
				klog.Errorf("Error updating resume token: %+v", err)
			}
			continue
		}

		nodeRemediatedStatus := false
		for i := 1; i <= maxRetries; i++ {
			klog.Infof("Attempt %d, handle event for node: %s", i, healthEventWithStatus.HealthEvent.NodeName)
			if r.handleEvent(ctx, healthEventWithStatus.HealthEvent.NodeName) {
				nodeRemediatedStatus = true
				break
			}

			time.Sleep(retryDelay)
		}

		if err := r.updateNodeRemediatedStatus(ctx, collection, event, nodeRemediatedStatus); err != nil {
			totalEventProcessingError.WithLabelValues("update_status_error", healthEventWithStatus.HealthEvent.NodeName).Inc()
			log.Printf("\nError updating remediation status for node: %+v\n", err)
		} else {
			totalEventsSuccessfullyProcessed.Inc()
		}
	}
}

func (r *Reconciler) handleEvent(ctx context.Context, nodeName string) bool {
	return r.Config.K8sClient.CreateMaintenanceResource(ctx, nodeName)
}

func (r *Reconciler) updateNodeRemediatedStatus(ctx context.Context, collection MongoInterface, event bson.M, nodeRemediatedStatus bool) error {
	var err error
	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		return fmt.Errorf("error extracting fullDocument from event: %+v", event)
	}
	filter := bson.M{"_id": document["_id"]}
	update := bson.M{
		"$set": bson.M{
			"healtheventstatus.noderemediated": nodeRemediatedStatus,
		},
	}

	for i := 1; i <= maxRetries; i++ {
		klog.Infof("Attempt %d, updating health event with ID %v", i, document["_id"])
		_, err = collection.UpdateOne(ctx, filter, update)
		if err == nil {
			break
		}
		time.Sleep(retryDelay)
	}

	if err != nil {
		return fmt.Errorf("error updating document with ID: %v, error: %w", document["_id"], err)
	}

	klog.Infof("Health event with ID %v has been updated with status %+v", document["_id"], nodeRemediatedStatus)
	return nil
}
