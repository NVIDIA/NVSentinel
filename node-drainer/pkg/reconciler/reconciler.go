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
	"log/slog"
	"sync"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/evaluator"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/informers"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/metrics"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/queue"
	"github.com/nvidia/nvsentinel/store-client/pkg/storewatcher"

	"go.mongodb.org/mongo-driver/bson"
	"k8s.io/client-go/kubernetes"
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
	nodeEventsMap       map[string]eventStatusMap // nodeName → eventStatusMap
	cancelledNodes      map[string]struct{}       // Node-level cancellation flags
	nodeEventsMapMu     sync.Mutex
}

func NewReconciler(cfg config.ReconcilerConfig,
	dryRunEnabled bool, kubeClient kubernetes.Interface, informersInstance *informers.Informers) *Reconciler {
	queueManager := queue.NewEventQueueManager()
	drainEvaluator := evaluator.NewNodeDrainEvaluator(cfg.TomlConfig, informersInstance)

	reconciler := &Reconciler{
		Config:              cfg,
		NodeEvictionContext: sync.Map{},
		DryRun:              dryRunEnabled,
		queueManager:        queueManager,
		informers:           informersInstance,
		evaluator:           drainEvaluator,
		kubernetesClient:    kubeClient,
		nodeEventsMap:       make(map[string]eventStatusMap),
		cancelledNodes:      make(map[string]struct{}),
	}

	queueManager.SetEventProcessor(reconciler)

	return reconciler
}

func (r *Reconciler) GetQueueManager() queue.EventQueueManager {
	return r.queueManager
}

func (r *Reconciler) Shutdown() {
	r.queueManager.Shutdown()
}

func (r *Reconciler) ProcessEvent(ctx context.Context,
	event bson.M, collection queue.MongoCollectionAPI, nodeName string) error {
	start := time.Now()

	defer func() {
		metrics.EventHandlingDuration.Observe(time.Since(start).Seconds())
	}()

	healthEventWithStatus := model.HealthEventWithStatus{}
	if err := storewatcher.UnmarshalFullDocumentFromEvent(event, &healthEventWithStatus); err != nil {
		metrics.ProcessingErrors.WithLabelValues("unmarshal_error", nodeName).Inc()
		return fmt.Errorf("failed to unmarshal health event: %w", err)
	}

	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		metrics.ProcessingErrors.WithLabelValues("extract_document_error", nodeName).Inc()
		return fmt.Errorf("error extracting fullDocument from event")
	}

	eventID := fmt.Sprintf("%v", document["_id"])

	metrics.TotalEventsReceived.Inc()

	nodeQuarantinedStatus := healthEventWithStatus.HealthEventStatus.NodeQuarantined

	if r.isEventCancelled(eventID, nodeName, nodeQuarantinedStatus) {
		slog.Info("Event was cancelled, performing cleanup", "node", nodeName, "eventID", eventID)
		return r.handleCancelledEvent(ctx, nodeName, &healthEventWithStatus, event, collection, eventID)
	}

	r.markEventInProgress(eventID, nodeName)

	actionResult, err := r.evaluator.EvaluateEvent(ctx, healthEventWithStatus, collection)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("evaluate_event_error", nodeName).Inc()
		return fmt.Errorf("failed to evaluate event: %w", err)
	}

	slog.Info("Evaluated action for node",
		"node", nodeName,
		"action", actionResult.Action.String())

	return r.executeAction(ctx, actionResult, healthEventWithStatus, event, collection, eventID)
}

func (r *Reconciler) executeAction(ctx context.Context,
	action *evaluator.DrainActionResult, healthEvent model.HealthEventWithStatus,
	event bson.M, collection queue.MongoCollectionAPI, eventID string) error {
	nodeName := healthEvent.HealthEvent.NodeName

	switch action.Action {
	case evaluator.ActionSkip:
		r.clearEventStatus(eventID, nodeName)
		return r.executeSkip(ctx, nodeName, healthEvent, event, collection)

	case evaluator.ActionWait:
		slog.Info("Waiting for node",
			"node", nodeName,
			"delay", action.WaitDelay)

		return fmt.Errorf("waiting for retry delay: %v", action.WaitDelay)

	case evaluator.ActionEvictImmediate:
		r.updateNodeDrainStatus(ctx, nodeName, &healthEvent, true)
		return r.executeImmediateEviction(ctx, action, healthEvent)

	case evaluator.ActionEvictWithTimeout:
		r.updateNodeDrainStatus(ctx, nodeName, &healthEvent, true)
		return r.executeTimeoutEviction(ctx, action, healthEvent)

	case evaluator.ActionCheckCompletion:
		r.updateNodeDrainStatus(ctx, nodeName, &healthEvent, true)
		return r.executeCheckCompletion(ctx, action, healthEvent)

	case evaluator.ActionMarkAlreadyDrained:
		r.clearEventStatus(eventID, nodeName)
		return r.executeMarkAlreadyDrained(ctx, healthEvent, event, collection)

	case evaluator.ActionUpdateStatus:
		r.clearEventStatus(eventID, nodeName)
		return r.executeUpdateStatus(ctx, healthEvent, event, collection)

	default:
		return fmt.Errorf("unknown action: %s", action.Action.String())
	}
}

func (r *Reconciler) executeSkip(ctx context.Context,
	nodeName string, healthEvent model.HealthEventWithStatus,
	event bson.M, collection queue.MongoCollectionAPI) error {
	slog.Info("Skipping event for node", "node", nodeName)

	if statusPtr := healthEvent.HealthEventStatus.NodeQuarantined; statusPtr != nil &&
		*statusPtr == model.UnQuarantined {
		podsEvictionStatus := &healthEvent.HealthEventStatus.UserPodsEvictionStatus
		podsEvictionStatus.Status = model.StatusSucceeded

		if err := r.updateNodeUserPodsEvictedStatus(ctx, collection, event, podsEvictionStatus, nodeName,
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

func (r *Reconciler) executeImmediateEviction(ctx context.Context,
	action *evaluator.DrainActionResult, healthEvent model.HealthEventWithStatus) error {
	nodeName := healthEvent.HealthEvent.NodeName
	for _, namespace := range action.Namespaces {
		if err := r.informers.EvictAllPodsInImmediateMode(ctx, namespace, nodeName, action.Timeout); err != nil {
			metrics.ProcessingErrors.WithLabelValues("immediate_eviction_error", nodeName).Inc()
			return fmt.Errorf("failed immediate eviction for namespace %s on node %s: %w", namespace, nodeName, err)
		}
	}

	return fmt.Errorf("immediate eviction completed, requeuing for status verification")
}

func (r *Reconciler) executeTimeoutEviction(ctx context.Context,
	action *evaluator.DrainActionResult, healthEvent model.HealthEventWithStatus) error {
	nodeName := healthEvent.HealthEvent.NodeName
	timeoutMinutes := int(action.Timeout.Minutes())

	if err := r.informers.DeletePodsAfterTimeout(ctx,
		nodeName, action.Namespaces, timeoutMinutes, &healthEvent); err != nil {
		metrics.ProcessingErrors.WithLabelValues("timeout_eviction_error", nodeName).Inc()
		return fmt.Errorf("failed timeout eviction for node %s: %w", nodeName, err)
	}

	return fmt.Errorf("timeout eviction initiated, requeuing for status verification")
}

func (r *Reconciler) executeCheckCompletion(ctx context.Context,
	action *evaluator.DrainActionResult, healthEvent model.HealthEventWithStatus) error {
	nodeName := healthEvent.HealthEvent.NodeName
	allPodsComplete := true

	var remainingPods []string

	for _, namespace := range action.Namespaces {
		pods, err := r.informers.FindEvictablePodsInNamespaceAndNode(namespace, nodeName)
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
	healthEvent model.HealthEventWithStatus, event bson.M, collection queue.MongoCollectionAPI) error {
	nodeName := healthEvent.HealthEvent.NodeName
	podsEvictionStatus := &healthEvent.HealthEventStatus.UserPodsEvictionStatus
	podsEvictionStatus.Status = model.AlreadyDrained

	return r.updateNodeUserPodsEvictedStatus(ctx, collection, event, podsEvictionStatus,
		nodeName, metrics.DrainStatusSkipped)
}

func (r *Reconciler) executeUpdateStatus(ctx context.Context,
	healthEvent model.HealthEventWithStatus, event bson.M, collection queue.MongoCollectionAPI) error {
	nodeName := healthEvent.HealthEvent.NodeName
	podsEvictionStatus := &healthEvent.HealthEventStatus.UserPodsEvictionStatus
	podsEvictionStatus.Status = model.StatusSucceeded

	if _, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		nodeName, statemanager.DrainSucceededLabelValue, false); err != nil {
		slog.Error("Failed to update node label to drain-succeeded",
			"node", nodeName,
			"error", err)
		metrics.ProcessingErrors.WithLabelValues("label_update_error", nodeName).Inc()
	}

	err := r.updateNodeUserPodsEvictedStatus(ctx, collection, event, podsEvictionStatus,
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

func (r *Reconciler) updateNodeUserPodsEvictedStatus(ctx context.Context, collection queue.MongoCollectionAPI,
	event bson.M, userPodsEvictionStatus *model.OperationStatus, nodeName string, drainStatus string) error {
	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		return fmt.Errorf("error extracting fullDocument from event: %+v", event)
	}

	filter := bson.M{"_id": document["_id"]}
	update := bson.M{
		"$set": bson.M{
			"healtheventstatus.userpodsevictionstatus": *userPodsEvictionStatus,
		},
	}

	_, err := collection.UpdateOne(ctx, filter, update)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("update_status_error", nodeName).Inc()
		return fmt.Errorf("error updating document with ID: %v, error: %w", document["_id"], err)
	}

	slog.Info("Health event status has been updated",
		"documentID", document["_id"],
		"evictionStatus", userPodsEvictionStatus.Status)
	metrics.EventsProcessed.WithLabelValues(drainStatus, nodeName).Inc()

	return nil
}

func (r *Reconciler) HandleCancellation(eventID string, nodeName string, status model.Status) {
	r.nodeEventsMapMu.Lock()
	defer r.nodeEventsMapMu.Unlock()

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
		delete(r.cancelledNodes, nodeName)
	}

	r.nodeEventsMap[nodeName][eventID] = model.StatusInProgress
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
	healthEvent *model.HealthEventWithStatus, event bson.M, collection queue.MongoCollectionAPI,
	eventID string) error {
	r.clearEventStatus(eventID, nodeName)

	podsEvictionStatus := &healthEvent.HealthEventStatus.UserPodsEvictionStatus
	podsEvictionStatus.Status = model.Cancelled

	if err := r.updateNodeUserPodsEvictedStatus(ctx, collection, event, podsEvictionStatus, nodeName,
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
