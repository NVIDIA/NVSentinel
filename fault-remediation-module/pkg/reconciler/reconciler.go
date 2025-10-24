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

package reconciler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/nvidia/nvsentinel/fault-remediation-module/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation-module/pkg/crstatus"
	storeconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	platformconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/protos"
	"github.com/nvidia/nvsentinel/statemanager"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"k8s.io/klog/v2"
)

type ReconcilerConfig struct {
	MongoConfig        storewatcher.MongoDBConfig
	TokenConfig        storewatcher.TokenConfig
	MongoPipeline      mongo.Pipeline
	RemediationClient  FaultRemediationClientInterface
	StateManager       statemanager.StateManager
	EnableLogCollector bool
	UpdateMaxRetries   int
	UpdateRetryDelay   time.Duration
}

type Reconciler struct {
	Config              ReconcilerConfig
	NodeEvictionContext sync.Map
	DryRun              bool
	annotationManager   NodeAnnotationManagerInterface
	remediationClient   FaultRemediationClientInterface
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
		remediationClient:   cfg.RemediationClient,
		annotationManager:   cfg.RemediationClient.GetAnnotationManager(),
	}
}

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
		klog.Fatalf("error initializing collection client with config %+v for mongodb: %+v",
			r.Config.MongoConfig, err)
	}

	watcher.Start(ctx)
	klog.Info("Listening for events on the channel...")

	for event := range watcher.Events() {
		klog.Infof("Event received: %+v", event)
		r.processEvent(ctx, event, watcher, collection)
	}
}

// processEvent handles a single event from the watcher
func (r *Reconciler) processEvent(ctx context.Context, event bson.M, watcher WatcherInterface,
	collection MongoInterface) {
	totalEventsReceived.Inc()

	healthEventWithStatus := HealthEventDoc{}
	if err := storewatcher.UnmarshalFullDocumentFromEvent(event, &healthEventWithStatus); err != nil {
		totalEventProcessingError.WithLabelValues("unmarshal_doc_error", "unknown").Inc()
		klog.Errorf("Failed to unmarshal event: %+v", err)

		if err := watcher.MarkProcessed(context.Background()); err != nil {
			totalEventProcessingError.WithLabelValues("mark_processed_error", "unknown").Inc()
			klog.Errorf("Error updating resume token: %v", err)
		}

		return
	}

	nodeName := healthEventWithStatus.HealthEventWithStatus.HealthEvent.NodeName
	nodeQuarantined := healthEventWithStatus.HealthEventWithStatus.HealthEventStatus.NodeQuarantined

	if nodeQuarantined != nil && *nodeQuarantined == storeconnector.UnQuarantined {
		r.handleUnquarantineEvent(ctx, nodeName, watcher)
		return
	}

	r.handleRemediationEvent(ctx, &healthEventWithStatus, event, watcher, collection)
}

func (r *Reconciler) shouldSkipEvent(ctx context.Context,
	healthEventWithStatus storeconnector.HealthEventWithStatus) bool {
	action := healthEventWithStatus.HealthEvent.RecommendedAction
	nodeName := healthEventWithStatus.HealthEvent.NodeName

	if action == platformconnector.RecommenedAction_NONE {
		klog.Infof("Skipping event for node: %s, recommended action is NONE (no remediation needed)", nodeName)
		return true
	}

	if common.GetRemediationGroupForAction(action) != "" {
		return false
	}

	klog.Infof("Unsupported recommended action %s for node %s.", action.String(), nodeName)
	totalUnsupportedRemediationActions.WithLabelValues(action.String(), nodeName).Inc()

	_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		healthEventWithStatus.HealthEvent.NodeName,
		statemanager.RemediationFailedLabelValue, false)
	if err != nil {
		klog.Errorf("Error updating node label to %s: %+v", statemanager.RemediationFailedLabelValue, err)
		totalEventProcessingError.WithLabelValues("label_update_error",
			healthEventWithStatus.HealthEvent.NodeName).Inc()
	}

	return true
}

// runLogCollector runs log collector for non-NONE actions if enabled
func (r *Reconciler) runLogCollector(ctx context.Context, healthEvent *platformconnector.HealthEvent) {
	if healthEvent.RecommendedAction == platformconnector.RecommenedAction_NONE ||
		!r.Config.EnableLogCollector {
		return
	}

	klog.Infof("Log collector feature enabled; running log collector for node %s", healthEvent.NodeName)

	if err := r.Config.RemediationClient.RunLogCollectorJob(ctx, healthEvent.NodeName); err != nil {
		klog.Errorf("Log collector job failed for node %s: %v", healthEvent.NodeName, err)
	}
}

// performRemediation attempts to create maintenance resource with retries
func (r *Reconciler) performRemediation(ctx context.Context, healthEventWithStatus *HealthEventDoc) (bool, string) {
	// Update state to "remediating"
	_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		healthEventWithStatus.HealthEventWithStatus.HealthEvent.NodeName,
		statemanager.RemediatingLabelValue, false)
	if err != nil {
		klog.Errorf("Error updating node label to remediating: %+v", err)
		totalEventProcessingError.WithLabelValues("label_update_error",
			healthEventWithStatus.HealthEventWithStatus.HealthEvent.NodeName).Inc()
	}

	success := false
	crName := ""

	for i := 1; i <= r.Config.UpdateMaxRetries; i++ {
		klog.Infof("Attempt %d, handle event for node: %s", i,
			healthEventWithStatus.HealthEventWithStatus.HealthEvent.NodeName)

		success, crName = r.Config.RemediationClient.CreateMaintenanceResource(ctx, healthEventWithStatus)
		if success {
			break
		}

		if i < r.Config.UpdateMaxRetries {
			time.Sleep(r.Config.UpdateRetryDelay)
		}
	}

	// Update final state based on success/failure
	remediationLabelValue := statemanager.RemediationFailedLabelValue
	if success {
		remediationLabelValue = statemanager.RemediationSucceededLabelValue
	}

	_, err = r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		healthEventWithStatus.HealthEventWithStatus.HealthEvent.NodeName,
		remediationLabelValue, false)
	if err != nil {
		klog.Errorf("Error updating node label to %s: %+v", remediationLabelValue, err)
		totalEventProcessingError.WithLabelValues("label_update_error",
			healthEventWithStatus.HealthEventWithStatus.HealthEvent.NodeName).Inc()
	}

	return success, crName
}

// handleUnquarantineEvent handles node unquarantine events by clearing annotations
func (r *Reconciler) handleUnquarantineEvent(
	ctx context.Context,
	nodeName string,
	watcher WatcherInterface,
) {
	klog.Infof("Node %s unquarantined, clearing remediation state annotation", nodeName)

	if err := r.annotationManager.ClearRemediationState(ctx, nodeName); err != nil {
		klog.Errorf("Failed to clear remediation state for node %s: %v", nodeName, err)
	}

	if err := watcher.MarkProcessed(context.Background()); err != nil {
		totalEventProcessingError.WithLabelValues("mark_processed_error", nodeName).Inc()
		klog.Errorf("Error updating resume token: %v", err)
	}
}

// handleRemediationEvent processes remediation for quarantined nodes
func (r *Reconciler) handleRemediationEvent(
	ctx context.Context,
	healthEventWithStatus *HealthEventDoc,
	event bson.M,
	watcher WatcherInterface,
	collection MongoInterface,
) {
	healthEvent := healthEventWithStatus.HealthEventWithStatus.HealthEvent
	nodeName := healthEvent.NodeName

	r.runLogCollector(ctx, healthEvent)

	// Check if we should skip this event (NONE actions or unsupported actions)
	if r.shouldSkipEvent(ctx, healthEventWithStatus.HealthEventWithStatus) {
		if err := watcher.MarkProcessed(ctx); err != nil {
			totalEventProcessingError.WithLabelValues("mark_processed_error", nodeName).Inc()
			klog.Errorf("Error updating resume token: %v", err)
		}

		return
	}

	shouldCreateCR, existingCR, err := r.checkExistingCRStatus(ctx, healthEvent)
	if err != nil {
		totalEventProcessingError.WithLabelValues("cr_status_check_error", nodeName).Inc()
		klog.Errorf("Error checking existing CR status for node %s: %v", nodeName, err)
	}

	if !shouldCreateCR {
		klog.Infof("Skipping event for node %s due to existing CR %s", nodeName, existingCR)

		if err := watcher.MarkProcessed(ctx); err != nil {
			totalEventProcessingError.WithLabelValues("mark_processed_error", nodeName).Inc()
			klog.Errorf("Error updating resume token: %v", err)
		}

		return
	}

	nodeRemediatedStatus, _ := r.performRemediation(ctx, healthEventWithStatus)

	if err := r.updateNodeRemediatedStatus(ctx, collection, event, nodeRemediatedStatus); err != nil {
		totalEventProcessingError.WithLabelValues("update_status_error", nodeName).Inc()
		log.Printf("\nError updating remediation status for node: %+v\n", err)

		return
	}

	totalEventsSuccessfullyProcessed.Inc()

	if err := watcher.MarkProcessed(ctx); err != nil {
		totalEventProcessingError.WithLabelValues("mark_processed_error", nodeName).Inc()
		klog.Errorf("Error updating resume token: %v", err)
	}
}

func (r *Reconciler) updateNodeRemediatedStatus(ctx context.Context, collection MongoInterface,
	event bson.M, nodeRemediatedStatus bool) error {
	var err error

	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		return fmt.Errorf("error extracting fullDocument from event: %+v", event)
	}

	filter := bson.M{"_id": document["_id"]}

	updateFields := bson.M{
		"healtheventstatus.faultremediated": nodeRemediatedStatus,
	}

	// If remediation was successful, set the timestamp
	if nodeRemediatedStatus {
		updateFields["healtheventstatus.lastremediationtimestamp"] = time.Now().UTC()
	}

	update := bson.M{
		"$set": updateFields,
	}

	for i := 1; i <= r.Config.UpdateMaxRetries; i++ {
		klog.Infof("Attempt %d, updating health event with ID %v", i, document["_id"])

		_, err = collection.UpdateOne(ctx, filter, update)
		if err == nil {
			break
		}

		time.Sleep(r.Config.UpdateRetryDelay)
	}

	if err != nil {
		return fmt.Errorf("error updating document with ID: %v, error: %w", document["_id"], err)
	}

	klog.Infof("Health event with ID %v has been updated with status %+v", document["_id"], nodeRemediatedStatus)

	return nil
}

// handleCRStatus processes the CR status and determines if a new CR should be created
func (r *Reconciler) handleCRStatus(
	ctx context.Context,
	nodeName, group string,
	crName string,
	status crstatus.CRStatus,
) (shouldCreateCR bool, existingCR string) {
	switch status {
	case crstatus.CRStatusSucceeded, crstatus.CRStatusInProgress:
		// Don't create new CR, remediation is complete or in progress
		klog.Infof("Skipping event for node %s - CR %s is %s", nodeName, crName, status)

		return false, crName
	case crstatus.CRStatusFailed:
		// Previous CR failed, remove it from annotation and allow retry
		klog.Infof("Previous CR %s failed for node %s, allowing retry", crName, nodeName)

		if err := r.annotationManager.RemoveGroupFromState(ctx, nodeName, group); err != nil {
			klog.Errorf("Failed to remove failed CR from annotation: %v", err)
		}

		return true, ""
	case crstatus.CRStatusNotFound:
		// CR doesn't exist anymore, clean up annotation
		klog.Infof("CR %s not found for node %s, cleaning up annotation", crName, nodeName)

		if err := r.annotationManager.RemoveGroupFromState(ctx, nodeName, group); err != nil {
			klog.Errorf("Failed to remove stale CR from annotation: %v", err)
		}

		return true, ""
	}

	return true, ""
}

// checkExistingCRStatus checks if there's an existing CR for the same equivalence group
// and determines whether to create a new CR based on its status
func (r *Reconciler) checkExistingCRStatus(
	ctx context.Context,
	healthEvent *platformconnector.HealthEvent,
) (bool, string, error) {
	nodeName := healthEvent.NodeName
	group := common.GetRemediationGroupForAction(healthEvent.RecommendedAction)

	if group == "" {
		// Action is not part of any remediation group, allow creating CR
		return true, "", nil
	}

	// Get current remediation state from node annotation
	state, err := r.annotationManager.GetRemediationState(ctx, nodeName)
	if err != nil {
		klog.Errorf("Error getting remediation state for node %s: %v", nodeName, err)
		// On error, allow creating CR
		return true, "", nil
	}

	if state == nil {
		klog.Warningf("Remediation state is nil for node %s, allowing CR creation", nodeName)
		return true, "", nil
	}

	// Check if there's an existing CR for this group
	groupState, exists := state.EquivalenceGroups[group]
	if !exists {
		// No existing CR for this group, allow creating new one
		return true, "", nil
	}

	// Get the appropriate status checker for this action
	statusChecker, err := r.remediationClient.GetStatusCheckerForAction(healthEvent.RecommendedAction)
	if err != nil {
		klog.Errorf("Error getting status checker for action %s: %v", healthEvent.RecommendedAction, err)
		// On error, allow creating CR
		return true, "", nil
	}

	// Check the CR status
	status, err := statusChecker.GetCRStatus(ctx, groupState.MaintenanceCR)
	if err != nil {
		klog.Errorf("Error checking CR status for %s: %v", groupState.MaintenanceCR, err)
		// On error checking status, assume NotFound
		status = crstatus.CRStatusNotFound
	}

	klog.Infof("Found existing CR %s for node %s group %s with status %s",
		groupState.MaintenanceCR, nodeName, group, status)

	// Decide based on CR status
	shouldCreate, existingCRName := r.handleCRStatus(ctx, nodeName, group, groupState.MaintenanceCR, status)

	return shouldCreate, existingCRName, nil
}
