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
	"log/slog"
	"sync"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation-module/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation-module/pkg/crstatus"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
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
	ID                          primitive.ObjectID `bson:"_id"`
	model.HealthEventWithStatus `bson:",inline"`
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

func (r *Reconciler) Start(ctx context.Context) error {
	watcher, err := storewatcher.NewChangeStreamWatcher(ctx, r.Config.MongoConfig, r.Config.TokenConfig,
		r.Config.MongoPipeline)
	if err != nil {
		return fmt.Errorf("error initializing change stream watcher: %w", err)
	}

	defer func() {
		if err := watcher.Close(ctx); err != nil {
			slog.Error("failed to close watcher", "error", err)
		}
	}()

	collection, err := storewatcher.GetCollectionClient(ctx, r.Config.MongoConfig)
	if err != nil {
		slog.Error("error initializing collection client for mongodb",
			"config", r.Config.MongoConfig,
			"error", err)

		return fmt.Errorf("error initializing collection client for mongodb: %w", err)
	}

	watcher.Start(ctx)
	slog.Info("Listening for events on the channel...")

	for event := range watcher.Events() {
		slog.Info("Event received", "event", event)
		r.processEvent(ctx, event, watcher, collection)
	}

	return nil
}

// processEvent handles a single event from the watcher
func (r *Reconciler) processEvent(ctx context.Context, event bson.M, watcher WatcherInterface,
	collection MongoInterface) {
	totalEventsReceived.Inc()

	healthEventWithStatus := HealthEventDoc{}
	if err := storewatcher.UnmarshalFullDocumentFromEvent(event, &healthEventWithStatus); err != nil {
		totalEventProcessingError.WithLabelValues("unmarshal_doc_error", "unknown").Inc()
		slog.Error("Failed to unmarshal event", "error", err)

		if err := watcher.MarkProcessed(context.Background()); err != nil {
			totalEventProcessingError.WithLabelValues("mark_processed_error", "unknown").Inc()
			slog.Error("Error updating resume token", "error", err)
		}

		return
	}

	nodeName := healthEventWithStatus.HealthEventWithStatus.HealthEvent.NodeName
	nodeQuarantined := healthEventWithStatus.HealthEventWithStatus.HealthEventStatus.NodeQuarantined

	if nodeQuarantined != nil && *nodeQuarantined == model.UnQuarantined {
		r.handleUnquarantineEvent(ctx, nodeName, watcher)
		return
	}

	r.handleRemediationEvent(ctx, &healthEventWithStatus, event, watcher, collection)
}

func (r *Reconciler) shouldSkipEvent(ctx context.Context,
	healthEventWithStatus model.HealthEventWithStatus) bool {
	action := healthEventWithStatus.HealthEvent.RecommendedAction
	nodeName := healthEventWithStatus.HealthEvent.NodeName

	if action == protos.RecommendedAction_NONE {
		slog.Info("Skipping event for node: recommended action is NONE (no remediation needed)",
			"node", nodeName)
		return true
	}

	if common.GetRemediationGroupForAction(action) != "" {
		return false
	}

	slog.Info("Unsupported recommended action for node",
		"action", action.String(),
		"node", nodeName)
	totalUnsupportedRemediationActions.WithLabelValues(action.String(), nodeName).Inc()

	_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		healthEventWithStatus.HealthEvent.NodeName,
		statemanager.RemediationFailedLabelValue, false)
	if err != nil {
		slog.Error("Error updating node label",
			"label", statemanager.RemediationFailedLabelValue,
			"error", err)
		totalEventProcessingError.WithLabelValues("label_update_error",
			healthEventWithStatus.HealthEvent.NodeName).Inc()
	}

	return true
}

// runLogCollector runs log collector for non-NONE actions if enabled
func (r *Reconciler) runLogCollector(ctx context.Context, healthEvent *protos.HealthEvent) {
	if healthEvent.RecommendedAction == protos.RecommendedAction_NONE ||
		!r.Config.EnableLogCollector {
		return
	}

	slog.Info("Log collector feature enabled; running log collector for node",
		"node", healthEvent.NodeName)

	if err := r.Config.RemediationClient.RunLogCollectorJob(ctx, healthEvent.NodeName); err != nil {
		slog.Error("Log collector job failed for node",
			"node", healthEvent.NodeName,
			"error", err)
	}
}

// performRemediation attempts to create maintenance resource with retries
func (r *Reconciler) performRemediation(ctx context.Context, healthEventWithStatus *HealthEventDoc) (bool, string) {
	// Update state to "remediating"
	_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		healthEventWithStatus.HealthEventWithStatus.HealthEvent.NodeName,
		statemanager.RemediatingLabelValue, false)
	if err != nil {
		slog.Error("Error updating node label to remediating", "error", err)
		totalEventProcessingError.WithLabelValues("label_update_error",
			healthEventWithStatus.HealthEventWithStatus.HealthEvent.NodeName).Inc()
	}

	success := false
	crName := ""

	for i := 1; i <= r.Config.UpdateMaxRetries; i++ {
		slog.Info("Handle event for node",
			"attempt", i,
			"node", healthEventWithStatus.HealthEventWithStatus.HealthEvent.NodeName)

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
		slog.Error("Error updating node label",
			"label", remediationLabelValue,
			"error", err)
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
	slog.Info("Node unquarantined, clearing remediation state annotation",
		"node", nodeName)

	if err := r.annotationManager.ClearRemediationState(ctx, nodeName); err != nil {
		slog.Error("Failed to clear remediation state for node",
			"node", nodeName,
			"error", err)
	}

	if err := watcher.MarkProcessed(context.Background()); err != nil {
		totalEventProcessingError.WithLabelValues("mark_processed_error", nodeName).Inc()
		slog.Error("Error updating resume token", "error", err)
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
			slog.Error("Error updating resume token", "error", err)
		}

		return
	}

	shouldCreateCR, existingCR, err := r.checkExistingCRStatus(ctx, healthEvent)
	if err != nil {
		totalEventProcessingError.WithLabelValues("cr_status_check_error", nodeName).Inc()
		slog.Error("Error checking existing CR status", "node", nodeName, "error", err)
	}

	if !shouldCreateCR {
		slog.Info("Skipping event for node due to existing CR",
			"node", nodeName,
			"existingCR", existingCR)

		if err := watcher.MarkProcessed(ctx); err != nil {
			totalEventProcessingError.WithLabelValues("mark_processed_error", nodeName).Inc()
			slog.Error("Error updating resume token", "error", err)
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
		slog.Error("Error updating resume token", "error", err)
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
		slog.Info("Updating health event with ID",
			"attempt", i,
			"id", document["_id"])

		_, err = collection.UpdateOne(ctx, filter, update)
		if err == nil {
			break
		}

		time.Sleep(r.Config.UpdateRetryDelay)
	}

	if err != nil {
		return fmt.Errorf("error updating document with ID: %v, error: %w", document["_id"], err)
	}

	slog.Info("Health event with ID %v has been updated with status %+v", document["_id"], nodeRemediatedStatus)

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
		slog.Info("Skipping event for node - CR is in final state",
			"node", nodeName,
			"crName", crName,
			"status", fmt.Sprintf("%v", status))

		return false, crName
	case crstatus.CRStatusFailed:
		// Previous CR failed, remove it from annotation and allow retry
		slog.Info("Previous CR failed for node, allowing retry",
			"crName", crName,
			"node", nodeName)

		if err := r.annotationManager.RemoveGroupFromState(ctx, nodeName, group); err != nil {
			slog.Error("Failed to remove failed CR from annotation", "error", err)
		}

		return true, ""
	case crstatus.CRStatusNotFound:
		// CR doesn't exist anymore, clean up annotation
		slog.Info("CR not found for node, cleaning up annotation",
			"crName", crName,
			"node", nodeName)

		if err := r.annotationManager.RemoveGroupFromState(ctx, nodeName, group); err != nil {
			slog.Error("Failed to remove stale CR from annotation", "error", err)
		}

		return true, ""
	}

	return true, ""
}

// checkExistingCRStatus checks if there's an existing CR for the same equivalence group
// and determines whether to create a new CR based on its status
func (r *Reconciler) checkExistingCRStatus(
	ctx context.Context,
	healthEvent *protos.HealthEvent,
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
		slog.Error("Error getting remediation state", "node", nodeName, "error", err)
		// On error, allow creating CR
		return true, "", nil
	}

	if state == nil {
		slog.Warn("Remediation state is nil for node, allowing CR creation",
			"node", nodeName)
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
		slog.Error("Error getting status checker for action",
			"action", healthEvent.RecommendedAction.String(),
			"error", err)
		// On error, allow creating CR
		return true, "", nil
	}

	// Check the CR status
	status, err := statusChecker.GetCRStatus(ctx, groupState.MaintenanceCR)
	if err != nil {
		slog.Error("Error checking CR status", "crName", groupState.MaintenanceCR, "error", err)
		// On error checking status, assume NotFound
		status = crstatus.CRStatusNotFound
	}

	slog.Info("Found existing CR %s for node %s group %s with status %s",
		groupState.MaintenanceCR, nodeName, group, status)

	// Decide based on CR status
	shouldCreate, existingCRName := r.handleCRStatus(ctx, nodeName, group, groupState.MaintenanceCR, status)

	return shouldCreate, existingCRName, nil
}
