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
	"fmt"
	"log/slog"
	"sync"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/evaluator"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/informers"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/metrics"
	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/queue"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/client"

	"k8s.io/client-go/kubernetes"
)

type Reconciler struct {
	Config              config.ReconcilerConfig
	NodeEvictionContext sync.Map
	DryRun              bool
	queueManager        queue.EventQueueManager
	informers           *informers.Informers
	evaluator           evaluator.DrainEvaluator
	kubernetesClient    kubernetes.Interface
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
	}

	queueManager.SetDatabaseEventProcessor(reconciler)

	return reconciler
}

func (r *Reconciler) GetQueueManager() queue.EventQueueManager {
	return r.queueManager
}

func (r *Reconciler) Shutdown() {
	r.queueManager.Shutdown()
}

// ProcessEventGeneric implements DatabaseEventProcessor interface for database-agnostic event processing
func (r *Reconciler) ProcessEventGeneric(ctx context.Context,
	event map[string]interface{}, database queue.DatabaseAPI, nodeName string) error {
	healthEventWithStatus := model.HealthEventWithStatus{}
	if err := r.unmarshalGenericEvent(event, &healthEventWithStatus); err != nil {
		return fmt.Errorf("failed to unmarshal health event: %w", err)
	}

	r.updateQuarantineMetrics(&healthEventWithStatus)

	metrics.TotalEventsReceived.Inc()

	// Use the new database-agnostic method
	actionResult, err := r.evaluator.EvaluateEventWithDatabase(ctx, healthEventWithStatus, database)

	if err != nil {
		return fmt.Errorf("failed to evaluate event: %w", err)
	}

	slog.Info("Evaluated action for node",
		"node", nodeName,
		"action", actionResult.Action.String())

	return r.executeActionGeneric(ctx, actionResult, healthEventWithStatus, event, database)
}

// ProcessEvent method has been removed - use ProcessEventGeneric instead

// unmarshalGenericEvent unmarshals a generic event map into a HealthEventWithStatus
func (r *Reconciler) unmarshalGenericEvent(event map[string]interface{}, target *model.HealthEventWithStatus) error {
	// Convert the generic event to the expected structure
	if fullDocument, ok := event["fullDocument"]; ok {
		// Marshal to JSON first, then unmarshal to the target struct
		jsonData, err := json.Marshal(fullDocument)
		if err != nil {
			return fmt.Errorf("failed to marshal event to JSON: %w", err)
		}

		if err := json.Unmarshal(jsonData, target); err != nil {
			return fmt.Errorf("failed to unmarshal JSON to HealthEventWithStatus: %w", err)
		}

		return nil
	}

	// If no fullDocument, try to unmarshal the event directly
	jsonData, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event to JSON: %w", err)
	}

	if err := json.Unmarshal(jsonData, target); err != nil {
		return fmt.Errorf("failed to unmarshal JSON to HealthEventWithStatus: %w", err)
	}

	return nil
}

// legacyCollectionWrapper has been removed - use DatabaseAPI directly

// executeActionGeneric executes actions using database-agnostic interfaces
func (r *Reconciler) executeActionGeneric(ctx context.Context, action *evaluator.DrainActionResult,
	healthEvent model.HealthEventWithStatus, event map[string]interface{}, database queue.DatabaseAPI) error {
	nodeName := healthEvent.HealthEvent.NodeName

	switch action.Action {
	case evaluator.ActionSkip:
		return r.executeSkipGeneric(ctx, nodeName, healthEvent, event, database)

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
		return r.executeMarkAlreadyDrainedGeneric(ctx, healthEvent, event, database)

	case evaluator.ActionUpdateStatus:
		return r.executeUpdateStatusGeneric(ctx, healthEvent, event, database)

	default:
		return fmt.Errorf("unknown action: %s", action.Action.String())
	}
}

// executeAction method has been removed - use executeActionGeneric instead

// executeSkip method has been removed - use executeSkipGeneric instead

func (r *Reconciler) executeImmediateEviction(ctx context.Context,
	action *evaluator.DrainActionResult, healthEvent model.HealthEventWithStatus) error {
	nodeName := healthEvent.HealthEvent.NodeName
	for _, namespace := range action.Namespaces {
		if err := r.informers.EvictAllPodsInImmediateMode(ctx, namespace, nodeName, action.Timeout); err != nil {
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

// executeMarkAlreadyDrained and executeUpdateStatus methods have been removed - use Generic versions instead

func (r *Reconciler) updateNodeDrainStatus(ctx context.Context,
	nodeName string, healthEvent *model.HealthEventWithStatus, isDraining bool) {
	if healthEvent.HealthEventStatus.NodeQuarantined == nil {
		return
	}

	// Handle UnQuarantined events - remove draining label
	if *healthEvent.HealthEventStatus.NodeQuarantined == model.UnQuarantined {
		if _, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
			nodeName, statemanager.DrainingLabelValue, true); err != nil {
			slog.Error("Failed to remove draining label for node",
				"node", nodeName,
				"error", err)
		}

		metrics.NodeDrainStatus.WithLabelValues(nodeName).Set(0)

		return
	}

	// Handle Quarantined/AlreadyQuarantined events
	if isDraining {
		if _, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
			nodeName, statemanager.DrainingLabelValue, false); err != nil {
			slog.Error("Failed to update node label to draining",
				"node", nodeName,
				"error", err)
			metrics.TotalEventProcessingError.WithLabelValues("label_update_error").Inc()
		}

		metrics.NodeDrainStatus.WithLabelValues(nodeName).Set(1)
	} else {
		metrics.NodeDrainStatus.WithLabelValues(nodeName).Set(0)
	}
}

func (r *Reconciler) updateQuarantineMetrics(healthEventWithStatus *model.HealthEventWithStatus) {
	if healthEventWithStatus.HealthEventStatus.NodeQuarantined == nil {
		slog.Warn("NodeQuarantined is nil, skipping metrics update",
			"node", healthEventWithStatus.HealthEvent.NodeName)
		return
	}

	//nolint:exhaustive
	switch *healthEventWithStatus.HealthEventStatus.NodeQuarantined {
	case model.Quarantined:
		metrics.UnhealthyEvent.WithLabelValues(healthEventWithStatus.HealthEvent.NodeName,
			healthEventWithStatus.HealthEvent.CheckName).Inc()
	case model.UnQuarantined:
		metrics.HealthyEvent.WithLabelValues(healthEventWithStatus.HealthEvent.NodeName,
			healthEventWithStatus.HealthEvent.CheckName).Inc()
	case model.AlreadyQuarantined:
		slog.Info("Node already quarantined",
			"node", healthEventWithStatus.HealthEvent.NodeName)
		metrics.UnhealthyEvent.WithLabelValues(healthEventWithStatus.HealthEvent.NodeName,
			healthEventWithStatus.HealthEvent.CheckName).Inc()
	default:
		slog.Warn("Unknown NodeQuarantined status",
			"node", healthEventWithStatus.HealthEvent.NodeName,
			"status", *healthEventWithStatus.HealthEventStatus.NodeQuarantined)
	}
}

// updateNodeUserPodsEvictedStatus method has been removed - use updateNodeUserPodsEvictedStatusGeneric instead

// Generic versions of action execution methods for database-agnostic processing

func (r *Reconciler) executeSkipGeneric(ctx context.Context,
	nodeName string, healthEvent model.HealthEventWithStatus,
	event map[string]interface{}, database queue.DatabaseAPI) error {
	slog.Info("Skipping event for node", "node", nodeName)

	// Track if this is a healthy event that canceled draining
	if healthEvent.HealthEventStatus.NodeQuarantined != nil &&
		*healthEvent.HealthEventStatus.NodeQuarantined == model.UnQuarantined {
		metrics.HealthyEventWithContextCancellation.Inc()

		// Update database status to StatusSucceeded for healthy events that cancel draining
		podsEvictionStatus := &healthEvent.HealthEventStatus.UserPodsEvictionStatus
		podsEvictionStatus.Status = model.StatusSucceeded

		if err := r.updateNodeUserPodsEvictedStatusGeneric(ctx, database, event, podsEvictionStatus); err != nil {
			slog.Error("Failed to update database status for node",
				"node", nodeName,
				"error", err)

			return fmt.Errorf("failed to update database status for node %s: %w", nodeName, err)
		}

		slog.Info("Updated database status for node",
			"node", nodeName,
			"status", "succeeded")
	}

	r.updateNodeDrainStatus(ctx, nodeName, &healthEvent, false)

	return nil
}

func (r *Reconciler) executeMarkAlreadyDrainedGeneric(ctx context.Context,
	healthEvent model.HealthEventWithStatus, event map[string]interface{}, database queue.DatabaseAPI) error {
	podsEvictionStatus := &healthEvent.HealthEventStatus.UserPodsEvictionStatus
	podsEvictionStatus.Status = model.AlreadyDrained

	return r.updateNodeUserPodsEvictedStatusGeneric(ctx, database, event, podsEvictionStatus)
}

func (r *Reconciler) executeUpdateStatusGeneric(ctx context.Context,
	healthEvent model.HealthEventWithStatus, event map[string]interface{}, database queue.DatabaseAPI) error {
	nodeName := healthEvent.HealthEvent.NodeName
	podsEvictionStatus := &healthEvent.HealthEventStatus.UserPodsEvictionStatus
	podsEvictionStatus.Status = model.StatusSucceeded

	if _, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		nodeName, statemanager.DrainSucceededLabelValue, false); err != nil {
		slog.Error("Failed to update node label to drain-succeeded",
			"node", nodeName,
			"error", err)
		metrics.TotalEventProcessingError.WithLabelValues("label_update_error").Inc()
	}

	err := r.updateNodeUserPodsEvictedStatusGeneric(ctx, database, event, podsEvictionStatus)
	if err != nil {
		return fmt.Errorf("failed to update user pod eviction status: %w", err)
	}

	metrics.NodeDrainStatus.WithLabelValues(nodeName).Set(0)

	return nil
}

func (r *Reconciler) updateNodeUserPodsEvictedStatusGeneric(ctx context.Context, database queue.DatabaseAPI,
	event map[string]interface{}, userPodsEvictionStatus *model.OperationStatus) error {
	var documentID interface{}

	// Extract document ID from the event
	if fullDocument, ok := event["fullDocument"].(map[string]interface{}); ok {
		documentID = fullDocument["_id"]
	} else if id, ok := event["_id"]; ok {
		documentID = id
	} else {
		return fmt.Errorf("error extracting document ID from event: %+v", event)
	}

	filter := map[string]interface{}{"_id": documentID}
	// Use database-agnostic update building
	update := client.BuildSetUpdate(map[string]interface{}{
		"healtheventstatus.userpodsevictionstatus": *userPodsEvictionStatus,
	})

	_, err := database.UpdateDocument(ctx, filter, update)
	if err != nil {
		metrics.TotalEventProcessingError.WithLabelValues("update_status_error").Inc()
		return fmt.Errorf("error updating document with ID: %v, error: %w", documentID, err)
	}

	slog.Info("Health event status has been updated",
		"documentID", documentID,
		"evictionStatus", userPodsEvictionStatus.Status)
	metrics.TotalEventsSuccessfullyProcessed.Inc()

	return nil
}
