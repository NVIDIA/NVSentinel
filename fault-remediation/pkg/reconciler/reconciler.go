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
	"strings"
	"sync"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/common"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
	"github.com/nvidia/nvsentinel/store-client/pkg/watcher"
)

type ReconcilerConfig struct {
	DataStoreConfig    datastore.DataStoreConfig
	TokenConfig        client.TokenConfig
	Pipeline           datastore.Pipeline
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
	ID                          string `json:"_id"`
	model.HealthEventWithStatus `json:",inline"`
}

// HealthEventData represents health event data with string ID for compatibility
type HealthEventData struct {
	ID                          string `bson:"_id,omitempty"`
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
	// Create datastore instance
	ds, err := datastore.NewDataStore(ctx, r.Config.DataStoreConfig)
	if err != nil {
		return fmt.Errorf("error initializing datastore: %w", err)
	}

	defer func() {
		if err := ds.Close(ctx); err != nil {
			slog.Error("failed to close datastore", "error", err)
		}
	}()

	// Create watcher using the factory pattern
	watcherConfig := watcher.WatcherConfig{
		Pipeline:       r.Config.Pipeline,
		CollectionName: "HealthEvents",
	}

	watcherInstance, err := watcher.CreateChangeStreamWatcher(ctx, ds, watcherConfig)
	if err != nil {
		return fmt.Errorf("error initializing change stream watcher: %w", err)
	}

	defer func() {
		if err := watcherInstance.Close(ctx); err != nil {
			slog.Error("failed to close watcher", "error", err)
		}
	}()

	// Get the HealthEventStore for document operations
	healthEventStore := ds.HealthEventStore()

	watcherInstance.Start(ctx)
	slog.Info("Listening for events on the channel...")

	for event := range watcherInstance.Events() {
		slog.Info("Event received", "event", event)
		r.processEvent(ctx, event, watcherInstance, healthEventStore)
	}

	return nil
}

// processEvent handles a single event from the watcher
func (r *Reconciler) processEvent(ctx context.Context, eventWithToken datastore.EventWithToken,
	watcherInstance datastore.ChangeStreamWatcher, healthEventStore datastore.HealthEventStore) {
	start := time.Now()

	defer func() {
		eventHandlingDuration.Observe(time.Since(start).Seconds())
	}()

	totalEventsReceived.Inc()

	healthEventWithStatus, err := r.parseHealthEvent(eventWithToken, watcherInstance)
	if err != nil {
		return
	}

	// Safety checks for nil pointers
	if healthEventWithStatus.HealthEvent == nil {
		slog.Warn("HealthEvent is nil, skipping processing")
		return
	}

	nodeName := healthEventWithStatus.HealthEvent.NodeName
	nodeQuarantined := healthEventWithStatus.HealthEventStatus.NodeQuarantined

	if nodeQuarantined != nil {
		if *nodeQuarantined == model.UnQuarantined || *nodeQuarantined == model.Cancelled {
			r.handleCancellationEvent(ctx, nodeName, *nodeQuarantined, watcherInstance, eventWithToken.ResumeToken)
			return
		}
	}

	r.handleRemediationEvent(ctx, &healthEventWithStatus, eventWithToken, watcherInstance, healthEventStore)
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

	if healthEventWithStatus.HealthEventStatus.FaultRemediated != nil &&
		*healthEventWithStatus.HealthEventStatus.FaultRemediated {
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
		processingErrors.WithLabelValues("label_update_error",
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
	nodeName := healthEventWithStatus.HealthEvent.NodeName

	// Update state to "remediating"
	_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		healthEventWithStatus.HealthEvent.NodeName,
		statemanager.RemediatingLabelValue, false)
	if err != nil {
		slog.Error("Error updating node label to remediating", "error", err)
		processingErrors.WithLabelValues("label_update_error", nodeName).Inc()
	}

	success := false
	crName := ""

	for i := 1; i <= r.Config.UpdateMaxRetries; i++ {
		slog.Info("Handle event for node",
			"attempt", i,
			"node", healthEventWithStatus.HealthEvent.NodeName)

		healthEventData := &HealthEventData{
			ID:                    healthEventWithStatus.ID,
			HealthEventWithStatus: healthEventWithStatus.HealthEventWithStatus,
		}

		success, crName = r.Config.RemediationClient.CreateMaintenanceResource(ctx, healthEventData)
		if success {
			break
		}

		if i < r.Config.UpdateMaxRetries {
			time.Sleep(r.Config.UpdateRetryDelay)
		}
	}

	if !success {
		processingErrors.WithLabelValues("cr_creation_failed", nodeName).Inc()
	}

	// Update final state based on success/failure
	remediationLabelValue := statemanager.RemediationFailedLabelValue
	if success {
		remediationLabelValue = statemanager.RemediationSucceededLabelValue
	}

	_, err = r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		healthEventWithStatus.HealthEvent.NodeName,
		remediationLabelValue, false)
	if err != nil {
		slog.Error("Error updating node label",
			"label", remediationLabelValue,
			"error", err)
		processingErrors.WithLabelValues("label_update_error", nodeName).Inc()
	}

	return success, crName
}

// handleCancellationEvent handles node unquarantine and cancellation events by clearing annotations
func (r *Reconciler) handleCancellationEvent(
	ctx context.Context,
	nodeName string,
	status model.Status,
	watcherInstance datastore.ChangeStreamWatcher,
	resumeToken []byte,
) {
	slog.Info("Cancellation event received, clearing all remediation state",
		"node", nodeName,
		"status", status)

	if err := r.annotationManager.ClearRemediationState(ctx, nodeName); err != nil {
		slog.Error("Failed to clear remediation state for node",
			"node", nodeName,
			"error", err)
	}

	if err := watcherInstance.MarkProcessed(context.Background(), resumeToken); err != nil {
		processingErrors.WithLabelValues("mark_processed_error", nodeName).Inc()
		slog.Error("Error updating resume token", "error", err)
	}
}

// handleRemediationEvent processes remediation for quarantined nodes
func (r *Reconciler) handleRemediationEvent(
	ctx context.Context,
	healthEventWithStatus *HealthEventDoc,
	eventWithToken datastore.EventWithToken,
	watcherInstance datastore.ChangeStreamWatcher,
	healthEventStore datastore.HealthEventStore,
) {
	healthEvent := healthEventWithStatus.HealthEvent
	nodeName := healthEvent.NodeName

	r.runLogCollector(ctx, healthEvent)

	// Check if we should skip this event (NONE actions or unsupported actions)
	if r.shouldSkipEvent(ctx, healthEventWithStatus.HealthEventWithStatus) {
		if err := watcherInstance.MarkProcessed(ctx, eventWithToken.ResumeToken); err != nil {
			processingErrors.WithLabelValues("mark_processed_error", nodeName).Inc()
			slog.Error("Error updating resume token", "error", err)
		}

		return
	}

	shouldCreateCR, existingCR, err := r.checkExistingCRStatus(ctx, healthEvent)
	if err != nil {
		processingErrors.WithLabelValues("cr_status_check_error", nodeName).Inc()
		slog.Error("Error checking existing CR status", "node", nodeName, "error", err)
	}

	if !shouldCreateCR {
		slog.Info("Skipping event for node due to existing CR",
			"node", nodeName,
			"existingCR", existingCR)

		eventsProcessed.WithLabelValues(CRStatusSkipped, nodeName).Inc()

		if err := watcherInstance.MarkProcessed(ctx, eventWithToken.ResumeToken); err != nil {
			processingErrors.WithLabelValues("mark_processed_error", nodeName).Inc()
			slog.Error("Error updating resume token", "error", err)
		}

		return
	}

	nodeRemediatedStatus, _ := r.performRemediation(ctx, healthEventWithStatus)

	if err := r.updateNodeRemediatedStatus(ctx, healthEventStore, eventWithToken, nodeRemediatedStatus); err != nil {
		processingErrors.WithLabelValues("update_status_error", nodeName).Inc()
		log.Printf("\nError updating remediation status for node: %+v\n", err)

		return
	}

	eventsProcessed.WithLabelValues(CRStatusCreated, nodeName).Inc()

	if err := watcherInstance.MarkProcessed(ctx, eventWithToken.ResumeToken); err != nil {
		processingErrors.WithLabelValues("mark_processed_error", nodeName).Inc()
		slog.Error("Error updating resume token", "error", err)
	}
}

func (r *Reconciler) updateNodeRemediatedStatus(ctx context.Context, healthEventStore datastore.HealthEventStore,
	eventWithToken datastore.EventWithToken, nodeRemediatedStatus bool) error {
	documentID, err := utils.ExtractDocumentID(eventWithToken.Event)
	if err != nil {
		return err
	}

	// Create status object for the update
	status := datastore.HealthEventStatus{}
	faultRemediated := nodeRemediatedStatus
	status.FaultRemediated = &faultRemediated

	// If remediation was successful, set the timestamp
	if nodeRemediatedStatus {
		now := time.Now().UTC()
		status.LastRemediationTimestamp = &now
	}

	// Use the healthEventStore to update the status with retries
	for i := 1; i <= r.Config.UpdateMaxRetries; i++ {
		slog.Info("Updating health event with ID",
			"attempt", i,
			"id", documentID)

		err = healthEventStore.UpdateHealthEventStatus(ctx, documentID, status)
		if err == nil {
			break
		}

		if i < r.Config.UpdateMaxRetries {
			time.Sleep(r.Config.UpdateRetryDelay)
		}
	}

	if err != nil {
		return fmt.Errorf("error updating document with ID: %v, error: %w", documentID, err)
	}

	slog.Info("Health event has been updated with status",
		"id", documentID,
		"status", nodeRemediatedStatus)

	return nil
}

func (r *Reconciler) checkExistingCRStatus(
	ctx context.Context,
	healthEvent *protos.HealthEvent,
) (bool, string, error) {
	nodeName := healthEvent.NodeName
	group := common.GetRemediationGroupForAction(healthEvent.RecommendedAction)

	if group == "" {
		return true, "", nil
	}

	state, err := r.annotationManager.GetRemediationState(ctx, nodeName)
	if err != nil {
		slog.Error("Error getting remediation state", "node", nodeName, "error", err)
		return true, "", nil
	}

	if state == nil {
		slog.Warn("Remediation state is nil for node, allowing CR creation",
			"node", nodeName)

		return true, "", nil
	}

	groupState, exists := state.EquivalenceGroups[group]
	if !exists {
		return true, "", nil
	}

	statusChecker := r.remediationClient.GetStatusChecker()
	if statusChecker == nil {
		slog.Warn("Status checker is not available, allowing creation")
		return true, "", nil
	}

	shouldSkip := statusChecker.ShouldSkipCRCreation(ctx, groupState.MaintenanceCR)
	if shouldSkip {
		slog.Info("CR exists and is in progress, skipping event", "node", nodeName, "crName", groupState.MaintenanceCR)
		return false, groupState.MaintenanceCR, nil
	}

	slog.Info("CR completed or failed, allowing retry", "node", nodeName, "crName", groupState.MaintenanceCR)

	if err := r.annotationManager.RemoveGroupFromState(ctx, nodeName, group); err != nil {
		slog.Error("Failed to remove CR from annotation", "error", err)
	}

	return true, "", nil
}

// parseHealthEvent extracts and parses health event from change stream event
// The eventWithToken.Event is already the fullDocument extracted by the store-client
func (r *Reconciler) parseHealthEvent(eventWithToken datastore.EventWithToken,
	watcherInstance datastore.ChangeStreamWatcher) (HealthEventDoc, error) {
	var result HealthEventDoc

	// Use the shared parsing utility
	healthEventWithStatus, err := eventutil.ParseHealthEventFromEvent(eventWithToken.Event)
	if err != nil {
		// Determine the appropriate error label based on the error message
		errorLabel := "parse_event_error"
		errMsg := err.Error()

		if strings.Contains(errMsg, "failed to marshal") {
			errorLabel = "marshal_error"
		} else if strings.Contains(errMsg, "failed to unmarshal") ||
			strings.Contains(errMsg, "health event is nil") ||
			strings.Contains(errMsg, "node quarantined status is nil") {
			// failed to unmarshal covers JSON unmarshal errors
			// nil checks cover struct validation errors after unmarshaling
			errorLabel = "unmarshal_doc_error"
		}

		processingErrors.WithLabelValues(errorLabel, "unknown").Inc()
		slog.Error("Error parsing health event", "error", err)

		if markErr := watcherInstance.MarkProcessed(context.Background(), eventWithToken.ResumeToken); markErr != nil {
			processingErrors.WithLabelValues("mark_processed_error", "unknown").Inc()
			slog.Error("Error updating resume token", "error", markErr)
		}

		return result, err
	}

	// Extract document ID and wrap into HealthEventDoc
	documentID, err := utils.ExtractDocumentID(eventWithToken.Event)
	if err != nil {
		processingErrors.WithLabelValues("extract_id_error", "unknown").Inc()
		slog.Error("Error extracting document ID", "error", err)

		if markErr := watcherInstance.MarkProcessed(context.Background(), eventWithToken.ResumeToken); markErr != nil {
			processingErrors.WithLabelValues("mark_processed_error", "unknown").Inc()
			slog.Error("Error updating resume token", "error", markErr)
		}

		return result, fmt.Errorf("error extracting document ID: %w", err)
	}

	result.ID = documentID
	result.HealthEventWithStatus = healthEventWithStatus

	return result, nil
}
