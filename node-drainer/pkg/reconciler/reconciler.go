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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"

	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/customdrain"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/evaluator"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/informers"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/metrics"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/queue"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
)

type eventStatusMap map[string]model.Status

type Reconciler struct {
	Config              config.ReconcilerConfig
	NodeEvictionContext sync.Map
	DryRun              bool
	queueManager        queue.EventQueueManager
	informers           *informers.Informers
	evaluator           evaluator.DrainEvaluator
	kubernetesClient    kubernetes.Interface
	databaseClient      queue.DataStore
	healthEventStore    datastore.HealthEventStore
	customDrainClient   *customdrain.Client
	nodeEventsMap       map[string]eventStatusMap
	cancelledNodes      map[string]struct{}
	nodeEventsMapMu     sync.Mutex
}

func NewReconciler(
	cfg config.ReconcilerConfig,
	dryRunEnabled bool,
	kubeClient kubernetes.Interface,
	informersInstance *informers.Informers,
	databaseClient queue.DataStore,
	healthEventStore datastore.HealthEventStore,
	dynamicClient dynamic.Interface,
	restMapper *restmapper.DeferredDiscoveryRESTMapper,
) (*Reconciler, error) {
	queueManager := queue.NewEventQueueManager()

	var customDrainClient *customdrain.Client

	if cfg.TomlConfig.CustomDrain.Enabled {
		var err error

		customDrainClient, err = customdrain.NewClient(cfg.TomlConfig.CustomDrain, dynamicClient, restMapper)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize custom drain client: %w", err)
		}
	}

	drainEvaluator := evaluator.NewNodeDrainEvaluator(cfg.TomlConfig, informersInstance, customDrainClient)

	reconciler := &Reconciler{
		Config:              cfg,
		NodeEvictionContext: sync.Map{},
		DryRun:              dryRunEnabled,
		queueManager:        queueManager,
		informers:           informersInstance,
		evaluator:           drainEvaluator,
		kubernetesClient:    kubeClient,
		databaseClient:      databaseClient,
		healthEventStore:    healthEventStore,
		customDrainClient:   customDrainClient,
		nodeEventsMap:       make(map[string]eventStatusMap),
		cancelledNodes:      make(map[string]struct{}),
	}

	queueManager.SetDataStoreEventProcessor(reconciler)

	return reconciler, nil
}

func (r *Reconciler) GetQueueManager() queue.EventQueueManager {
	return r.queueManager
}

func (r *Reconciler) GetCustomDrainClient() *customdrain.Client {
	return r.customDrainClient
}

func (r *Reconciler) Shutdown() {
	r.queueManager.Shutdown()
}

// PreprocessAndEnqueueEvent preprocesses an event from the change stream before enqueueing it.
// This function:
// 1. Extracts and unmarshals the health event
// 2. Skips events already in terminal status
// 3. Sets the initial status to InProgress (idempotent - only updates if not already set)
// 4. Enqueues the event to the processing queue
//
// This matches the behavior of the main branch's mongodb/event_watcher.go:preprocessAndEnqueueEvent
func (r *Reconciler) PreprocessAndEnqueueEvent(ctx context.Context, event client.Event) error {
	// Unmarshal the full document
	var document map[string]any
	if err := event.UnmarshalDocument(&document); err != nil {
		return fmt.Errorf("failed to unmarshal event document: %w", err)
	}

	// Extract health event with status
	healthEventWithStatus := model.HealthEventWithStatus{}
	if err := unmarshalGenericEvent(document, &healthEventWithStatus); err != nil {
		return fmt.Errorf("failed to extract health event with status: %w", err)
	}

	nodeName := healthEventWithStatus.HealthEvent.NodeName
	nodeQuarantined := healthEventWithStatus.HealthEventStatus.NodeQuarantined

	slog.Debug("Preprocessing event", "node", nodeName, "nodeQuarantined", nodeQuarantined)

	// Skip if already in terminal state
	if isTerminalStatus(healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus.Status) {
		slog.Debug("Skipping event - already in terminal state",
			"node", healthEventWithStatus.HealthEvent.NodeName,
			"status", healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus.Status)

		return nil
	}

	documentID, err := utils.ExtractDocumentIDNative(document)
	if err != nil {
		return fmt.Errorf("failed to extract document ID: %w", err)
	}

	// Handle cancellation logic (Cancelled/UnQuarantined events)
	if shouldSkip := r.handleEventCancellation(
		documentID, nodeName, healthEventWithStatus.HealthEventStatus.NodeQuarantined); shouldSkip {
		slog.Debug("Event skipped due to cancellation", "node", nodeName)

		return nil
	}

	// Set initial status to InProgress and enqueue
	return r.setInitialStatusAndEnqueue(ctx, document, documentID, nodeName)
}

// handleEventCancellation handles Cancelled and UnQuarantined events
// Returns true if the event should be skipped (not enqueued)
func (r *Reconciler) handleEventCancellation(
	documentID interface{},
	nodeName string,
	statusPtr *model.Status,
) bool {
	if statusPtr == nil {
		return false
	}

	eventID := fmt.Sprintf("%v", documentID)

	slog.Debug("Processing cancellation", "node", nodeName, "eventID", eventID, "status", *statusPtr)

	// Handle Cancelled events - mark specific event as cancelled
	if *statusPtr == model.Cancelled {
		slog.Info("Detected Cancelled event, marking event as cancelled",
			"node", nodeName,
			"eventID", eventID)
		r.HandleCancellation(eventID, nodeName, model.Cancelled)

		return true
	}

	// Handle UnQuarantined events - mark all in-progress events for node as cancelled
	if *statusPtr == model.UnQuarantined {
		slog.Info("Detected UnQuarantined event, marking all in-progress events for node as cancelled",
			"node", nodeName,
			"eventID", eventID)
		r.HandleCancellation(eventID, nodeName, model.UnQuarantined)
	}

	return false
}

// setInitialStatusAndEnqueue sets the initial status to InProgress and enqueues the event
func (r *Reconciler) setInitialStatusAndEnqueue(ctx context.Context, document map[string]any,
	documentID any, nodeName string) error {
	// Set initial status to StatusInProgress (idempotent - only updates if not already set)
	filter := map[string]any{
		"_id": documentID,
		"healtheventstatus.userpodsevictionstatus.status": map[string]any{
			"$ne": string(model.StatusInProgress),
		},
	}

	update := map[string]any{
		"$set": map[string]any{
			"healtheventstatus.userpodsevictionstatus.status": string(model.StatusInProgress),
		},
	}

	result, err := r.databaseClient.UpdateDocument(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("failed to update initial status: %w", err)
	}

	r.logStatusUpdateResult(result, nodeName, documentID)

	// Enqueue to the queue manager
	return r.queueManager.EnqueueEventGeneric(ctx, nodeName, document, r.databaseClient, r.healthEventStore)
}

// logStatusUpdateResult logs the result of the status update
func (r *Reconciler) logStatusUpdateResult(result *client.UpdateResult, nodeName string, documentID any) {
	switch {
	case result.ModifiedCount > 0:
		slog.Info("Set initial eviction status to InProgress", "node", nodeName)
	case result.MatchedCount == 0:
		slog.Warn("No document matched for status update", "node", nodeName, "documentID", fmt.Sprintf("%v", documentID))
	default:
		slog.Debug("Status already set to InProgress", "node", nodeName, "documentID", fmt.Sprintf("%v", documentID))
	}
}

// isTerminalStatus checks if a status is terminal (processing should not continue)
func isTerminalStatus(status model.Status) bool {
	return status == model.StatusSucceeded ||
		status == model.StatusFailed ||
		status == model.Cancelled ||
		status == model.AlreadyDrained
}

// unmarshalGenericEvent converts a generic map to a specific struct using JSON marshaling
func unmarshalGenericEvent(event map[string]any, target any) error {
	jsonBytes, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event to JSON: %w", err)
	}

	if err := json.Unmarshal(jsonBytes, target); err != nil {
		return fmt.Errorf("failed to unmarshal JSON to target type: %w", err)
	}

	return nil
}

func (r *Reconciler) ProcessEventGeneric(ctx context.Context,
	event datastore.Event, database queue.DataStore, healthEventStore datastore.HealthEventStore, nodeName string) error {
	start := time.Now()

	defer func() {
		metrics.EventHandlingDuration.Observe(time.Since(start).Seconds())
	}()

	healthEventWithStatus, err := r.parseHealthEventFromEvent(event, nodeName)
	if err != nil {
		return err
	}

	eventID := utils.ExtractEventID(event)

	slog.Debug("Processing event", "node", nodeName, "eventID", eventID)

	metrics.TotalEventsReceived.Inc()

	nodeQuarantinedStatus := healthEventWithStatus.HealthEventStatus.NodeQuarantined

	if r.isEventCancelled(eventID, nodeName, nodeQuarantinedStatus) {
		slog.Info("Event was cancelled, performing cleanup", "node", nodeName, "eventID", eventID)

		return r.handleCancelledEvent(ctx, nodeName, &healthEventWithStatus, event, database, eventID)
	}

	r.markEventInProgress(eventID, nodeName)

	actionResult, err := r.evaluator.EvaluateEventWithDatabase(ctx, healthEventWithStatus, database, healthEventStore)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("evaluate_event_error", nodeName).Inc()

		return fmt.Errorf("failed to evaluate event: %w", err)
	}

	slog.Info("Evaluated action for node",
		"node", nodeName,
		"action", actionResult.Action.String())

	return r.executeAction(ctx, actionResult, healthEventWithStatus, event, database, eventID)
}

//nolint:cyclop // executeAction is a clean dispatcher - complexity comes from number of actions, not nesting
func (r *Reconciler) executeAction(ctx context.Context, action *evaluator.DrainActionResult,
	healthEvent model.HealthEventWithStatus, event datastore.Event, database queue.DataStore, eventID string) error {
	nodeName := healthEvent.HealthEvent.NodeName

	switch action.Action {
	case evaluator.ActionSkip:
		r.clearEventStatus(eventID, nodeName)
		return r.executeSkip(ctx, nodeName, healthEvent, event, database)

	case evaluator.ActionWait:
		slog.Info("Waiting for node",
			"node", nodeName,
			"delay", action.WaitDelay)

		return fmt.Errorf("waiting for retry delay: %v", action.WaitDelay)

	case evaluator.ActionCreateCR:
		return r.executeCustomDrain(ctx, action, healthEvent, event, database, action.PartialDrainEntity)

	case evaluator.ActionEvictImmediate:
		r.updateNodeDrainStatus(ctx, nodeName, &healthEvent, true)
		return r.executeImmediateEviction(ctx, action, healthEvent, action.PartialDrainEntity)

	case evaluator.ActionEvictWithTimeout:
		r.updateNodeDrainStatus(ctx, nodeName, &healthEvent, true)
		return r.executeTimeoutEviction(ctx, action, healthEvent, eventID, action.PartialDrainEntity)

	case evaluator.ActionCheckCompletion:
		slog.Debug("Executing ActionCheckCompletion", "node", nodeName)
		r.updateNodeDrainStatus(ctx, nodeName, &healthEvent, true)

		return r.executeCheckCompletion(ctx, action, healthEvent, action.PartialDrainEntity)

	case evaluator.ActionMarkAlreadyDrained:
		r.clearEventStatus(eventID, nodeName)

		err := r.executeMarkAlreadyDrained(ctx, healthEvent, event, database, action.Status)
		if err == nil {
			r.deleteCustomDrainCRIfEnabled(ctx, nodeName, eventID)
		}

		return err

	case evaluator.ActionUpdateStatus:
		r.clearEventStatus(eventID, nodeName)
		return r.executeUpdateStatus(ctx, healthEvent, event, database, action.Status)

	default:
		return fmt.Errorf("unknown action: %s", action.Action.String())
	}
}

func (r *Reconciler) executeSkip(ctx context.Context,
	nodeName string, healthEvent model.HealthEventWithStatus,
	event datastore.Event, database queue.DataStore) error {
	slog.Info("Skipping event for node", "node", nodeName)

	if statusPtr := healthEvent.HealthEventStatus.NodeQuarantined; statusPtr != nil &&
		*statusPtr == model.UnQuarantined {
		podsEvictionStatus := &healthEvent.HealthEventStatus.UserPodsEvictionStatus
		podsEvictionStatus.Status = model.StatusSucceeded

		if err := r.updateNodeUserPodsEvictedStatus(ctx, database, event, podsEvictionStatus, nodeName,
			metrics.DrainStatusCancelled); err != nil {
			slog.Error("Failed to update MongoDB status for unquarantined node",
				"node", nodeName,
				"error", err)

			return fmt.Errorf("failed to update MongoDB status for node %s: %w", nodeName, err)
		}

		slog.Info("Updated MongoDB status for unquarantined node",
			"node", nodeName,
			"status", "succeeded")
	}

	r.updateNodeDrainStatus(ctx, nodeName, &healthEvent, false)

	return nil
}

func (r *Reconciler) executeImmediateEviction(ctx context.Context, action *evaluator.DrainActionResult,
	healthEvent model.HealthEventWithStatus, partialDrainEntity *protos.Entity) error {
	nodeName := healthEvent.HealthEvent.NodeName
	for _, namespace := range action.Namespaces {
		if err := r.informers.EvictAllPodsInImmediateMode(ctx, namespace, nodeName, action.Timeout,
			partialDrainEntity); err != nil {
			metrics.ProcessingErrors.WithLabelValues("immediate_eviction_error", nodeName).Inc()
			return fmt.Errorf("failed immediate eviction for namespace %s on node %s: %w", namespace, nodeName, err)
		}
	}

	return fmt.Errorf("immediate eviction completed, requeuing for status verification")
}

func (r *Reconciler) executeTimeoutEviction(ctx context.Context, action *evaluator.DrainActionResult,
	healthEvent model.HealthEventWithStatus, eventID string, partialDrainEntity *protos.Entity) error {
	nodeName := healthEvent.HealthEvent.NodeName
	timeoutMinutes := int(action.Timeout.Minutes())

	// Check if this event has been cancelled before proceeding with timeout eviction
	// This prevents force-deleting pods when a manual uncordon has happened
	r.nodeEventsMapMu.Lock()
	eventStatus, eventExists := r.nodeEventsMap[nodeName][eventID]
	_, nodeCancelled := r.cancelledNodes[nodeName]
	r.nodeEventsMapMu.Unlock()

	slog.Debug("Checking cancellation status before timeout eviction", "node", nodeName, "eventID", eventID)

	if (eventExists && eventStatus == model.Cancelled) || nodeCancelled {
		slog.Info("Event cancelled, aborting timeout eviction",
			"node", nodeName,
			"eventID", eventID)

		// Return nil (success) to stop requeueing - the event was handled via cancellation
		return nil
	}

	if err := r.informers.DeletePodsAfterTimeout(ctx,
		nodeName, action.Namespaces, timeoutMinutes, &healthEvent, partialDrainEntity); err != nil {
		// Check again after the operation in case cancellation happened during execution
		r.nodeEventsMapMu.Lock()
		checkEventStatus, checkEventExists := r.nodeEventsMap[nodeName][eventID]
		_, checkNodeCancelled := r.cancelledNodes[nodeName]
		r.nodeEventsMapMu.Unlock()

		if (checkEventExists && checkEventStatus == model.Cancelled) || checkNodeCancelled {
			slog.Info("Event was cancelled during timeout eviction, stopping",
				"node", nodeName, "eventID", eventID)

			return nil
		}

		metrics.ProcessingErrors.WithLabelValues("timeout_eviction_error", nodeName).Inc()

		return fmt.Errorf("failed timeout eviction for node %s: %w", nodeName, err)
	}

	return fmt.Errorf("timeout eviction initiated, requeuing for status verification")
}

func (r *Reconciler) executeCheckCompletion(ctx context.Context, action *evaluator.DrainActionResult,
	healthEvent model.HealthEventWithStatus, partialDrainEntity *protos.Entity) error {
	nodeName := healthEvent.HealthEvent.NodeName

	allPodsComplete := true

	var remainingPods []string

	for _, namespace := range action.Namespaces {
		pods, err := r.informers.FindEvictablePodsInNamespaceAndNode(namespace, nodeName, partialDrainEntity)
		if err != nil {
			return fmt.Errorf("failed to check pods in namespace %s on node %s: %w", namespace, nodeName, err)
		}

		if len(pods) > 0 {
			allPodsComplete = false

			for _, pod := range pods {
				remainingPods = append(remainingPods, fmt.Sprintf("%s/%s", namespace, pod.Name))
			}
		}
	}

	if !allPodsComplete {
		sort.Strings(remainingPods)

		message := fmt.Sprintf("Waiting for following pods to finish: %v", remainingPods)
		reason := "AwaitingPodCompletion"

		if err := r.informers.UpdateNodeEvent(ctx, nodeName, reason, message); err != nil {
			// Don't fail the whole operation just because event update failed
			slog.Error("Failed to update node event",
				"node", nodeName,
				"error", err)
		}

		slog.Info("Pods still running on node, requeueing for later check",
			"node", nodeName,
			"remainingPods", remainingPods)

		return fmt.Errorf("waiting for pods to complete: %d pods remaining", len(remainingPods))
	}

	slog.Info("All pods completed on node", "node", nodeName)

	return fmt.Errorf("pod completion verified, requeuing for status update")
}

func (r *Reconciler) executeMarkAlreadyDrained(ctx context.Context,
	healthEvent model.HealthEventWithStatus, event datastore.Event, database queue.DataStore, status model.Status) error {
	nodeName := healthEvent.HealthEvent.NodeName
	podsEvictionStatus := &healthEvent.HealthEventStatus.UserPodsEvictionStatus
	podsEvictionStatus.Status = status // expect AlreadyDrained

	return r.updateNodeUserPodsEvictedStatus(ctx, database, event, podsEvictionStatus,
		nodeName, metrics.DrainStatusSkipped)
}

func (r *Reconciler) executeUpdateStatus(ctx context.Context, healthEvent model.HealthEventWithStatus,
	event datastore.Event, database queue.DataStore, status model.Status) error {
	nodeName := healthEvent.HealthEvent.NodeName
	podsEvictionStatus := &healthEvent.HealthEventStatus.UserPodsEvictionStatus
	podsEvictionStatus.Status = status // expect StatusSucceeded or StatusFailed

	nodeDrainLabelValue := statemanager.DrainSucceededLabelValue
	if status == model.StatusFailed {
		nodeDrainLabelValue = statemanager.DrainFailedLabelValue
	}

	if _, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		nodeName, nodeDrainLabelValue, false); err != nil {
		slog.Error("Failed to update node label",
			"label", nodeDrainLabelValue,
			"node", nodeName,
			"error", err)
		metrics.ProcessingErrors.WithLabelValues("label_update_error", nodeName).Inc()
	}

	err := r.updateNodeUserPodsEvictedStatus(ctx, database, event, podsEvictionStatus,
		nodeName, metrics.DrainStatusDrained)
	if err != nil {
		return fmt.Errorf("failed to update user pod eviction status: %w", err)
	}

	return nil
}

func (r *Reconciler) updateNodeDrainStatus(ctx context.Context,
	nodeName string, healthEvent *model.HealthEventWithStatus, isDraining bool) {
	if healthEvent.HealthEventStatus.NodeQuarantined == nil {
		return
	}

	if *healthEvent.HealthEventStatus.NodeQuarantined == model.UnQuarantined {
		if _, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
			nodeName, statemanager.DrainingLabelValue, true); err != nil {
			slog.Error("Failed to remove draining label for unquarantined node",
				"node", nodeName,
				"error", err)
		}

		return
	}

	if isDraining {
		if _, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
			nodeName, statemanager.DrainingLabelValue, false); err != nil {
			slog.Error("Failed to update node label to draining",
				"node", nodeName,
				"error", err)
			metrics.ProcessingErrors.WithLabelValues("label_update_error", nodeName).Inc()
		}
	}
}

func (r *Reconciler) updateNodeUserPodsEvictedStatus(ctx context.Context, database queue.DataStore,
	event datastore.Event, userPodsEvictionStatus *model.OperationStatus,
	nodeName string, drainStatus string) error {
	// Extract the document ID (preserving native type for MongoDB)
	documentID, err := utils.ExtractDocumentIDNative(event)
	if err != nil {
		return fmt.Errorf("failed to extract document ID: %w", err)
	}

	filter := map[string]any{"_id": documentID}
	update := map[string]any{
		"$set": map[string]any{
			"healtheventstatus.userpodsevictionstatus": *userPodsEvictionStatus,
		},
	}

	_, err = database.UpdateDocument(ctx, filter, update)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("update_status_error", nodeName).Inc()
		return fmt.Errorf("error updating document with ID: %v, error: %w", documentID, err)
	}

	slog.Info("Health event status has been updated",
		"documentID", documentID,
		"evictionStatus", userPodsEvictionStatus.Status)
	metrics.EventsProcessed.WithLabelValues(drainStatus, nodeName).Inc()

	return nil
}

// parseHealthEventFromEvent extracts and parses health event from a document
// The event parameter is already the fullDocument extracted from the change stream
func (r *Reconciler) parseHealthEventFromEvent(event datastore.Event,
	nodeName string) (model.HealthEventWithStatus, error) {
	// Use the shared parsing utility
	healthEventWithStatus, err := eventutil.ParseHealthEventFromEvent(event)
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
			errorLabel = "unmarshal_error"
		}

		metrics.ProcessingErrors.WithLabelValues(errorLabel, nodeName).Inc()

		return healthEventWithStatus, err
	}

	return healthEventWithStatus, nil
}

func (r *Reconciler) HandleCancellation(eventID string, nodeName string, status model.Status) {
	r.nodeEventsMapMu.Lock()
	defer r.nodeEventsMapMu.Unlock()

	slog.Debug("HandleCancellation called", "node", nodeName, "eventID", eventID, "status", status)

	//nolint:exhaustive // we don't need to handle other statuses
	switch status {
	case model.Cancelled:
		if r.nodeEventsMap[nodeName] == nil {
			r.nodeEventsMap[nodeName] = make(eventStatusMap)
		}

		r.nodeEventsMap[nodeName][eventID] = model.Cancelled
		slog.Info("Marked specific event as cancelled", "node", nodeName, "eventID", eventID)
	case model.UnQuarantined:
		// Set node-level cancellation flag. This ensures any events queued but not yet
		// processed will see the cancellation, even if they haven't been added to
		// nodeEventsMap yet (race condition protection).
		r.cancelledNodes[nodeName] = struct{}{}
		slog.Info("Marked node as cancelled", "node", nodeName)

		if eventsMap, exists := r.nodeEventsMap[nodeName]; exists {
			for evtID := range eventsMap {
				eventsMap[evtID] = model.Cancelled
				slog.Info("Marked event as cancelled for node", "node", nodeName, "eventID", evtID)
			}
		}
	}

	slog.Debug("Cancellation processed", "node", nodeName, "eventID", eventID)
}

func (r *Reconciler) isEventCancelled(eventID string, nodeName string, nodeQuarantinedStatus *model.Status) bool {
	r.nodeEventsMapMu.Lock()
	defer r.nodeEventsMapMu.Unlock()

	// Don't apply node-level cancellation to UnQuarantined events themselves.
	// UnQuarantined events must process normally to set userpodsevictionstatus=Succeeded
	// so that FR can process them and clear remediation annotations.
	isUnQuarantinedEvent := nodeQuarantinedStatus != nil && *nodeQuarantinedStatus == model.UnQuarantined
	_, nodeCancelled := r.cancelledNodes[nodeName]

	// Check node-level cancellation flag for non-UnQuarantined events
	// (handles race condition where UnQuarantined arrives before Quarantined events are processed)
	if !isUnQuarantinedEvent && nodeCancelled {
		// Ensure the event is tracked so clearEventStatus can clean up the flag
		eventsMap, exists := r.nodeEventsMap[nodeName]
		if !exists {
			eventsMap = make(eventStatusMap)
			r.nodeEventsMap[nodeName] = eventsMap
		}

		if _, ok := eventsMap[eventID]; !ok {
			eventsMap[eventID] = model.Cancelled
		}

		return true
	}

	// Check if this specific event is marked as cancelled
	eventsMap, exists := r.nodeEventsMap[nodeName]
	if !exists {
		return false
	}

	status, eventExists := eventsMap[eventID]

	return eventExists && status == model.Cancelled
}

func (r *Reconciler) markEventInProgress(eventID string, nodeName string) {
	r.nodeEventsMapMu.Lock()
	defer r.nodeEventsMapMu.Unlock()

	if r.nodeEventsMap[nodeName] == nil {
		r.nodeEventsMap[nodeName] = make(eventStatusMap)
		// Clear node-level cancellation flag when starting fresh drain
		// (re-arm the node for new quarantine session)
		_, wasNodeCancelled := r.cancelledNodes[nodeName]
		if wasNodeCancelled {
			slog.Info("Clearing node-level cancellation flag when starting new drain session", "node", nodeName)
		}

		delete(r.cancelledNodes, nodeName)
	}

	r.nodeEventsMap[nodeName][eventID] = model.StatusInProgress

	slog.Debug("Event marked as in progress", "node", nodeName, "eventID", eventID)
}

func (r *Reconciler) clearEventStatus(eventID string, nodeName string) {
	r.nodeEventsMapMu.Lock()
	defer r.nodeEventsMapMu.Unlock()

	eventsMap, exists := r.nodeEventsMap[nodeName]
	if !exists {
		return
	}

	delete(eventsMap, eventID)

	// Clean up the node entry when no events remain.
	// This also clears the node-level cancellation flag since all queued events
	// have been processed and handled the cancellation.
	if len(eventsMap) == 0 {
		delete(r.nodeEventsMap, nodeName)
		delete(r.cancelledNodes, nodeName)
	}
}

func (r *Reconciler) handleCancelledEvent(ctx context.Context, nodeName string,
	healthEvent *model.HealthEventWithStatus, event datastore.Event, database queue.DataStore,
	eventID string) error {
	r.clearEventStatus(eventID, nodeName)

	podsEvictionStatus := &healthEvent.HealthEventStatus.UserPodsEvictionStatus
	podsEvictionStatus.Status = model.Cancelled

	if err := r.updateNodeUserPodsEvictedStatus(ctx, database, event, podsEvictionStatus, nodeName,
		metrics.DrainStatusCancelled); err != nil {
		slog.Error("Failed to update MongoDB status for cancelled event",
			"node", nodeName,
			"error", err)

		return fmt.Errorf("failed to update MongoDB status for cancelled event on node %s: %w", nodeName, err)
	}

	if _, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		nodeName, statemanager.DrainingLabelValue, true); err != nil {
		slog.Error("Failed to remove draining label for cancelled event",
			"node", nodeName,
			"error", err)
	}

	metrics.CancelledEvent.WithLabelValues(nodeName, healthEvent.HealthEvent.CheckName).Inc()
	slog.Info("Successfully cleaned up cancelled event", "node", nodeName, "eventID", eventID)

	return nil
}

func (r *Reconciler) executeCustomDrain(ctx context.Context, action *evaluator.DrainActionResult,
	healthEvent model.HealthEventWithStatus, event datastore.Event, database queue.DataStore,
	partialDrainEntity *protos.Entity) error {
	nodeName := healthEvent.HealthEvent.NodeName

	eventID, err := utils.ExtractDocumentID(event)
	if err != nil {
		return fmt.Errorf("failed to extract document ID for custom drain: %w", err)
	}

	podsToDrain := make(map[string][]string)

	for _, ns := range action.Namespaces {
		pods, err := r.informers.FindEvictablePodsInNamespaceAndNode(ns, nodeName, partialDrainEntity)
		if err != nil {
			slog.Warn("Failed to find evictable pods",
				"namespace", ns,
				"node", nodeName,
				"error", err)

			continue
		}

		if len(pods) > 0 {
			podNames := make([]string, 0, len(pods))

			for _, pod := range pods {
				podNames = append(podNames, pod.Name)
			}

			podsToDrain[ns] = podNames
		}
	}

	templateData := customdrain.TemplateData{
		HealthEvent: healthEvent.HealthEvent,
		EventID:     eventID,
		PodsToDrain: podsToDrain,
	}

	crName, err := r.customDrainClient.CreateDrainCR(ctx, templateData)
	if err != nil {
		return r.handleCustomDrainCRCreationError(ctx, err, nodeName, healthEvent, event, database)
	}

	slog.Info("Created custom drain CR",
		"node", nodeName,
		"crName", crName)

	return fmt.Errorf("waiting for custom drain CR to complete: %s", crName)
}

func (r *Reconciler) deleteCustomDrainCRIfEnabled(ctx context.Context, nodeName, eventID string) {
	if !r.Config.TomlConfig.CustomDrain.Enabled {
		return
	}

	crName := customdrain.GenerateCRName(nodeName, eventID)
	if err := r.customDrainClient.DeleteDrainCR(ctx, crName); err != nil {
		slog.Warn("Failed to delete custom drain CR",
			"node", nodeName,
			"crName", crName,
			"error", err)
	} else {
		slog.Info("Deleted custom drain CR",
			"node", nodeName,
			"crName", crName)
	}
}

func (r *Reconciler) handleCustomDrainCRCreationError(
	ctx context.Context,
	err error,
	nodeName string,
	healthEvent model.HealthEventWithStatus,
	event datastore.Event,
	database queue.DataStore,
) error {
	var noMatchErr *meta.NoKindMatchError
	if !errors.As(err, &noMatchErr) {
		return fmt.Errorf("failed to create custom drain CR for node %s: %w", nodeName, err)
	}

	slog.Error("Custom drain CRD not found - marking drain as failed",
		"node", nodeName,
		"apiGroup", r.Config.TomlConfig.CustomDrain.ApiGroup,
		"kind", r.Config.TomlConfig.CustomDrain.Kind,
		"error", err)

	metrics.CustomDrainCRDNotFound.WithLabelValues(nodeName).Inc()

	if err := r.setDrainFailedStatus(ctx, healthEvent, event, database,
		fmt.Sprintf("Custom drain CRD not found: %v", err)); err != nil {
		slog.Error("Failed to update drain failed status",
			"node", nodeName,
			"error", err)

		return fmt.Errorf("failed to update drain failed status: %w", err)
	}

	if _, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		nodeName, statemanager.DrainFailedLabelValue, false); err != nil {
		slog.Error("Failed to update node label to drain-failed",
			"node", nodeName,
			"error", err)
	}

	return nil
}

func (r *Reconciler) setDrainFailedStatus(
	ctx context.Context,
	healthEvent model.HealthEventWithStatus,
	event datastore.Event,
	database queue.DataStore,
	reason string,
) error {
	documentID, err := utils.ExtractDocumentIDNative(event)
	if err != nil {
		return fmt.Errorf("failed to extract document ID: %w", err)
	}

	filter := map[string]any{"_id": documentID}

	update := map[string]any{
		"$set": map[string]any{
			"healtheventstatus.userpodsevictionstatus": model.OperationStatus{
				Status:  model.StatusFailed,
				Message: reason,
			},
		},
	}

	_, err = database.UpdateDocument(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("failed to update MongoDB drain failed status: %w", err)
	}

	slog.Info("Updated drain status to failed",
		"node", healthEvent.HealthEvent.NodeName,
		"reason", reason)

	return nil
}
