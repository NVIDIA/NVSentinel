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
	platformconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/statemanager"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/store-client-sdk/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"k8s.io/klog"
)

const (
	maxRetries = 5
	retryDelay = 10 * time.Second
)

type ReconcilerConfig struct {
	MongoConfig        storewatcher.MongoDBConfig
	TokenConfig        storewatcher.TokenConfig
	MongoPipeline      mongo.Pipeline
	K8sClient          FaultRemediationClientInterface
	StateManager       statemanager.StateManager
	EnableLogCollector bool
}

type Reconciler struct {
	Config              ReconcilerConfig
	NodeEvictionContext sync.Map
	DryRun              bool
	MaxRetries          int
	RetryDelay          time.Duration
}

type HealthEventDoc struct {
	ID                                   primitive.ObjectID `bson:"_id"`
	storeconnector.HealthEventWithStatus `bson:",inline"`
}

func NewReconciler(cfg ReconcilerConfig, dryRunEnabled bool) *Reconciler {
	return &Reconciler{
		Config:              cfg,
		NodeEvictionContext: sync.Map{},
		DryRun:              dryRunEnabled,
		MaxRetries:          maxRetries,
		RetryDelay:          retryDelay,
	}
}

func (r *Reconciler) shouldSkipEvent(healthEventWithStatus storeconnector.HealthEventWithStatus) bool {
	action := healthEventWithStatus.HealthEvent.RecommendedAction
	nodeName := healthEventWithStatus.HealthEvent.NodeName

	switch action { // nolint:exhaustive  // we need to trim down the number of recommended actions
	case platformconnector.RecommenedAction_NONE:
		// NONE means no remediation needed
		klog.Infof("Skipping event for node: %s, recommended action is NONE (no remediation needed)", nodeName)
		return true
	case platformconnector.RecommenedAction_NODE_REBOOT,
		platformconnector.RecommenedAction_COMPONENT_RESET,
		platformconnector.RecommenedAction_RESTART_VM,
		platformconnector.RecommenedAction_RESET_FABRIC,
		platformconnector.RecommenedAction_RESET_GPU,
		platformconnector.RecommenedAction_RESTART_BM:
		// need to reboot the node, hence process this event
		return false
	default:
		// All other actions are currently unsupported
		klog.Infof("Unsupported recommended action %s for node %s. Only NODE_REBOOT, COMPONENT_RESET, RESTART_VM,"+
			"RESET_FABRIC, RESET_GPU and RESTART_BM are supported",
			action.String(), nodeName)
		totalUnsupportedRemediationActions.WithLabelValues(action.String(), nodeName).Inc()

		return true
	}
}

// nolint: cyclop // todo
func (r *Reconciler) Start(ctx context.Context) {
	watcher, err := storewatcher.NewChangeStreamWatcher(ctx, r.Config.MongoConfig, r.Config.TokenConfig,
		r.Config.MongoPipeline)
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

		healthEventWithStatus := HealthEventDoc{}

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

		// Run log collector for all non-NONE actions if enabled
		if healthEventWithStatus.HealthEvent.RecommendedAction != platformconnector.RecommenedAction_NONE &&
			r.Config.EnableLogCollector {
			klog.Infof("Log collector feature enabled; running log collector for node %s",
				healthEventWithStatus.HealthEvent.NodeName)

			if err := r.Config.K8sClient.RunLogCollectorJob(ctx, healthEventWithStatus.HealthEvent.NodeName); err != nil {
				klog.Errorf("Log collector job failed for node %s: %v", healthEventWithStatus.HealthEvent.NodeName, err)
			}
		}

		eventSkipped, nodeRemediatedStatus := r.executeRemediation(ctx, healthEventWithStatus)
		if !eventSkipped {
			if err := r.updateNodeRemediatedStatus(ctx, collection, event, nodeRemediatedStatus); err != nil {
				totalEventProcessingError.WithLabelValues("update_status_error", healthEventWithStatus.HealthEvent.NodeName).Inc()
				log.Printf("\nError updating remediation status for node: %+v\n", err)
			} else {
				totalEventsSuccessfullyProcessed.Inc()
			}
		}
	}
}

func (r *Reconciler) executeRemediation(ctx context.Context, healthEventWithStatus HealthEventDoc) (bool, bool) {
	_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx, healthEventWithStatus.HealthEvent.NodeName,
		statemanager.RemediatingLabelValue, false)
	if err != nil {
		klog.Errorf("Error updating node label: %+v", err)
		totalEventProcessingError.WithLabelValues("label_update_error", healthEventWithStatus.HealthEvent.NodeName).Inc()
	}

	shouldSkipEvent := r.shouldSkipEvent(healthEventWithStatus.HealthEventWithStatus)

	nodeRemediatedStatus := false

	remediationLabelValue := statemanager.RemediationFailedLabelValue

	if !shouldSkipEvent {
		for i := 1; i <= r.MaxRetries; i++ {
			klog.Infof("Attempt %d, handle event for node: %s", i, healthEventWithStatus.HealthEvent.NodeName)

			if r.Config.K8sClient.CreateMaintenanceResource(ctx, &healthEventWithStatus) {
				nodeRemediatedStatus = true
				remediationLabelValue = statemanager.RemediationSucceededLabelValue

				break
			}

			if i < r.MaxRetries {
				time.Sleep(r.RetryDelay)
			}
		}
	}
	// If shouldSkipEvent is true or if the nodeRemediatedStatus is false, we will update the state to remediation-failed,
	// else we will update the state to remediation-succeeded.
	_, err = r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx, healthEventWithStatus.HealthEvent.NodeName,
		remediationLabelValue, false)
	if err != nil {
		klog.Errorf("Error updating node label: %+v", err)
		totalEventProcessingError.WithLabelValues("label_update_error", healthEventWithStatus.HealthEvent.NodeName).Inc()
	}

	return shouldSkipEvent, nodeRemediatedStatus
}

func (r *Reconciler) updateNodeRemediatedStatus(ctx context.Context, collection MongoInterface,
	event bson.M, nodeRemediatedStatus bool) error {
	var err error

	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		return fmt.Errorf("error extracting fullDocument from event: %+v", event)
	}

	filter := bson.M{"_id": document["_id"]}
	update := bson.M{
		"$set": bson.M{
			"healtheventstatus.faultremediated": nodeRemediatedStatus,
		},
	}

	for i := 1; i <= r.MaxRetries; i++ {
		klog.Infof("Attempt %d, updating health event with ID %v", i, document["_id"])

		_, err = collection.UpdateOne(ctx, filter, update)
		if err == nil {
			break
		}

		time.Sleep(r.RetryDelay)
	}

	if err != nil {
		return fmt.Errorf("error updating document with ID: %v, error: %w", document["_id"], err)
	}

	klog.Infof("Health event with ID %v has been updated with status %+v", document["_id"], nodeRemediatedStatus)

	return nil
}
