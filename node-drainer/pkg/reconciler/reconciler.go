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

	"go.opentelemetry.io/otel/attribute"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"

	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
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
	"google.golang.org/protobuf/types/known/timestamppb"
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

	traceID := tracing.TraceIDFromMetadata(healthEventWithStatus.HealthEvent.GetMetadata())
	parentSpanID := tracing.ParentSpanID(
		healthEventWithStatus.HealthEventStatus.SpanIds, tracing.ServiceFaultQuarantine)

	if parentSpanID == "" {
		parentSpanID = tracing.ParentSpanID(healthEventWithStatus.HealthEventStatus.SpanIds, tracing.ServicePlatformConnector)
	}

	ctx, enqueueSpan := tracing.StartSpanWithLinkFromTraceContext(
		ctx,
		traceID,
		parentSpanID,
		"node_drainer.enqueue_event",
	)

	defer enqueueSpan.End()

	documentID, err := utils.ExtractDocumentIDNative(document)
	if err != nil {
		return fmt.Errorf("failed to extract document ID: %w", err)
	}

	tracing.AddHealthEventStatusAttributes(
		enqueueSpan,
		healthEventWithStatus.HealthEventStatus,
		fmt.Sprintf("%v", documentID),
	)

	slog.DebugContext(ctx, "Preprocessing event", "node", nodeName, "nodeQuarantined", nodeQuarantined)

	// Skip if already in terminal state
	if healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus != nil &&
		isTerminalStatus(model.Status(healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus.Status)) {
		slog.DebugContext(ctx, "Skipping event - already in terminal state",
			"node", healthEventWithStatus.HealthEvent.NodeName,
			"status", healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus.Status)

		enqueueSpan.SetAttributes(
			attribute.String("node_drainer.reconciler.status", "skipped"),
			attribute.String("node_drainer.reconciler.skip.reason", "already in terminal state"),
			attribute.String("node_drainer.drain.result", healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus.Status),
		)

		return nil
	}

	status := (&healthEventWithStatus.HealthEventStatus.NodeQuarantined)
	// Handle cancellation logic (Cancelled/UnQuarantined events)
	if shouldSkip := r.handleEventCancellation(
		ctx, documentID, nodeName, (*model.Status)(status)); shouldSkip {
		slog.DebugContext(ctx, "Event skipped due to cancellation", "node", nodeName)

		enqueueSpan.SetAttributes(
			attribute.String("node_drainer.reconciler.skip.type", *status),
			attribute.String("node_drainer.reconciler.skip.reason", "Event skipped due to cancellation"),
		)

		return nil
	}

	return r.setInitialStatusAndEnqueue(ctx, document, documentID, nodeName)
}

// handleEventCancellation handles Cancelled and UnQuarantined events
// Returns true if the event should be skipped (not enqueued)
func (r *Reconciler) handleEventCancellation(
	ctx context.Context,
	documentID interface{},
	nodeName string,
	statusPtr *model.Status,
) bool {
	if statusPtr == nil {
		return false
	}

	eventID := fmt.Sprintf("%v", documentID)

	slog.DebugContext(ctx, "Processing cancellation", "node", nodeName, "eventID", eventID, "status", *statusPtr)

	// Handle Cancelled events - mark specific event as cancelled
	if *statusPtr == model.Cancelled {
		slog.InfoContext(ctx, "Detected Cancelled event, marking event as cancelled",
			"node", nodeName,
			"eventID", eventID)
		r.HandleCancellation(ctx, eventID, nodeName, model.Cancelled)

		return true
	}

	// Handle UnQuarantined events - mark all in-progress events for node as cancelled
	if *statusPtr == model.UnQuarantined {
		slog.InfoContext(ctx, "Detected UnQuarantined event, marking all in-progress events for node as cancelled",
			"node", nodeName,
			"eventID", eventID)
		r.HandleCancellation(ctx, eventID, nodeName, model.UnQuarantined)
	}

	return false
}

// setInitialStatusAndEnqueue sets the initial status to InProgress and enqueues the event
func (r *Reconciler) setInitialStatusAndEnqueue(ctx context.Context, document map[string]any,
	documentID any, nodeName string) error {
	ctx, span := tracing.StartSpan(ctx, "node_drainer.set_initial_status_and_enqueue")
	defer span.End()

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
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "initial_status_update_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)

		return fmt.Errorf("failed to update initial status: %w", err)
	}

	r.logStatusUpdateResult(ctx, result, nodeName, documentID)

	// Enqueue to the queue manager
	if err := r.queueManager.EnqueueEventGeneric(
		ctx, nodeName, datastore.Event(document), r.databaseClient, r.healthEventStore, documentID,
	); err != nil {
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "enqueue_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)

		return err
	}

	return nil
}

// logStatusUpdateResult logs the result of the status update
func (r *Reconciler) logStatusUpdateResult(
	ctx context.Context, result *client.UpdateResult, nodeName string, documentID any,
) {
	docID := fmt.Sprintf("%v", documentID)

	switch {
	case result.ModifiedCount > 0:
		slog.InfoContext(ctx, "Set initial eviction status to InProgress", "node", nodeName)
	case result.MatchedCount == 0:
		slog.WarnContext(ctx, "No document matched for status update",
			"node", nodeName, "documentID", docID)
	default:
		slog.DebugContext(ctx, "Status already set to InProgress",
			"node", nodeName, "documentID", docID)
	}
}

// isTerminalStatus checks if a status is terminal (processing should not continue)
func isTerminalStatus(status model.Status) bool {
	return status == model.StatusSucceeded ||
		status == model.StatusFailed ||
		status == model.Cancelled ||
		status == model.AlreadyDrained
}

func isRequeueSignal(err error) bool {
	if err == nil {
		return false
	}

	message := err.Error()

	return strings.Contains(message, "requeu") || strings.HasPrefix(message, "waiting for")
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
		_, span := tracing.StartSpan(ctx, "node_drainer.process_event")
		defer span.End()

		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "parse_event_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)

		return err
	}

	eventID := utils.ExtractEventID(event)

	if healthEventWithStatus.HealthEvent != nil && healthEventWithStatus.HealthEvent.Id == "" {
		if docID, err := utils.ExtractDocumentID(event); err == nil {
			healthEventWithStatus.HealthEvent.Id = docID
		}
	}

	slog.DebugContext(ctx, "Processing event", "node", nodeName, "eventID", eventID)

	metrics.TotalEventsReceived.Inc()

	nodeQuarantinedStatus := healthEventWithStatus.HealthEventStatus.NodeQuarantined

	if r.isEventCancelled(eventID, nodeName, (*model.Status)(&nodeQuarantinedStatus)) {
		slog.InfoContext(ctx, "Event was cancelled, performing cleanup", "node", nodeName, "eventID", eventID)

		err := r.handleCancelledEvent(ctx, nodeName, &healthEventWithStatus, event, database, eventID)
		if err != nil {
			span := tracing.SpanFromContext(ctx)
			tracing.RecordError(span, err)
			span.SetAttributes(
				attribute.String("node_drainer.error.type", "handle_cancelled_event_error"),
				attribute.String("node_drainer.error.message", err.Error()),
			)
		}

		return err
	}

	r.markEventInProgress(ctx, eventID, nodeName)

	actionResult, err := r.evaluator.EvaluateEventWithDatabase(ctx, healthEventWithStatus, database, healthEventStore)
	if err != nil {
		_, span := tracing.StartSpan(ctx, "node_drainer.evaluate_event")
		defer span.End()

		metrics.ProcessingErrors.WithLabelValues("evaluate_event_error", nodeName).Inc()
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "evaluate_event_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)

		return fmt.Errorf("failed to evaluate event: %w", err)
	}

	slog.InfoContext(ctx, "Evaluated action for node",
		"node", nodeName,
		"action", actionResult.Action.String())

	err = r.executeAction(ctx, actionResult, healthEventWithStatus, event, database, eventID)
	if err != nil {
		if !isRequeueSignal(err) {
			_, span := tracing.StartSpan(ctx, "node_drainer.execute_action")
			defer span.End()

			tracing.RecordError(span, err)
			span.SetAttributes(
				attribute.String("node_drainer.error.type", "execute_action_error"),
				attribute.String("node_drainer.error.message", err.Error()),
			)
		}
	}

	return err
}

func (r *Reconciler) executeAction(ctx context.Context, action *evaluator.DrainActionResult,
	healthEvent model.HealthEventWithStatus, event datastore.Event, database queue.DataStore, eventID string) error {
	nodeName := healthEvent.HealthEvent.NodeName

	r.updateDrainSessionTracing(ctx, action, healthEvent)

	switch action.Action {
	case evaluator.ActionSkip:
		r.clearEventStatus(eventID, nodeName)
		return r.executeSkip(ctx, nodeName, healthEvent, event, database)

	case evaluator.ActionWait:
		slog.InfoContext(ctx, "Waiting for node",
			"node", nodeName,
			"delay", action.WaitDelay)

		return fmt.Errorf("waiting for retry delay: %v", action.WaitDelay)

	case evaluator.ActionCreateCR:
		r.updateNodeDrainStatus(ctx, nodeName, &healthEvent, true)
		return r.executeCustomDrain(ctx, action, healthEvent, event, database, action.PartialDrainEntity)

	case evaluator.ActionEvictImmediate:
		r.updateNodeDrainStatus(ctx, nodeName, &healthEvent, true)
		return r.executeImmediateEviction(ctx, action, healthEvent, action.PartialDrainEntity)

	case evaluator.ActionEvictWithTimeout:
		r.updateNodeDrainStatus(ctx, nodeName, &healthEvent, true)
		return r.executeTimeoutEviction(ctx, action, healthEvent, eventID, action.PartialDrainEntity)

	case evaluator.ActionCheckCompletion:
		slog.DebugContext(ctx, "Executing ActionCheckCompletion", "node", nodeName)
		r.updateNodeDrainStatus(ctx, nodeName, &healthEvent, true)

		return r.executeCheckCompletion(ctx, action, healthEvent, action.PartialDrainEntity)

	case evaluator.ActionMarkAlreadyDrained:
		return r.handleMarkAlreadyDrained(ctx, eventID, nodeName, healthEvent, event, database, action.Status)

	case evaluator.ActionUpdateStatus:
		r.clearEventStatus(eventID, nodeName)
		return r.executeUpdateStatus(ctx, healthEvent, event, database, action.Status)

	default:
		return fmt.Errorf("unknown action: %s", action.Action.String())
	}
}

func (r *Reconciler) updateDrainSessionTracing(
	ctx context.Context, action *evaluator.DrainActionResult, healthEvent model.HealthEventWithStatus,
) {
	ds := queue.DrainSessionFromContext(ctx)
	if ds == nil || ds.DrainSessionSpan == nil {
		return
	}

	r.setDrainScope(ds, action)
}

func (r *Reconciler) setDrainScope(ds *queue.DrainSession, action *evaluator.DrainActionResult) {
	if ds.ScopeSet {
		return
	}

	if action.PartialDrainEntity != nil {
		ds.DrainSessionSpan.SetAttributes(
			attribute.String("node_drainer.drain.scope", "partial"),
		)
	} else {
		ds.DrainSessionSpan.SetAttributes(
			attribute.String("node_drainer.drain.scope", "full"),
		)
	}

	ds.ScopeSet = true
}

func (r *Reconciler) handleMarkAlreadyDrained(ctx context.Context, eventID, nodeName string,
	healthEvent model.HealthEventWithStatus, event datastore.Event, database queue.DataStore,
	status model.Status) error {
	r.clearEventStatus(eventID, nodeName)

	err := r.executeMarkAlreadyDrained(ctx, healthEvent, event, database, status)
	if err == nil {
		r.deleteCustomDrainCRIfEnabled(ctx, nodeName, event)
	}

	return err
}

func (r *Reconciler) executeSkip(ctx context.Context,
	nodeName string, healthEvent model.HealthEventWithStatus,
	event datastore.Event, database queue.DataStore) error {
	ctx, span := tracing.StartSpan(ctx, "node_drainer.execute_skip")
	defer span.End()

	slog.InfoContext(ctx, "Skipping event for node", "node", nodeName)

	if healthEvent.HealthEventStatus.NodeQuarantined == string(model.UnQuarantined) {
		if healthEvent.HealthEventStatus.UserPodsEvictionStatus == nil {
			slog.ErrorContext(ctx, "HealthEventStatus is missing UserPodsEvictionStatus",
				"node", nodeName)

			return errors.New("missing UserPodsEvictionStatus")
		}

		podsEvictionStatus := healthEvent.HealthEventStatus.UserPodsEvictionStatus
		podsEvictionStatus.Status = string(model.StatusSucceeded)

		if err := r.updateNodeUserPodsEvictedStatus(ctx, database, event, podsEvictionStatus, nodeName,
			metrics.DrainStatusCancelled); err != nil {
			slog.ErrorContext(ctx, "Failed to update MongoDB status for unquarantined node",
				"node", nodeName,
				"error", err)
			tracing.RecordError(span, err)
			span.SetAttributes(
				attribute.String("node_drainer.error.type", "update_status_unquarantined_error"),
				attribute.String("node_drainer.error.message", err.Error()),
			)

			return fmt.Errorf("failed to update MongoDB status for node %s: %w", nodeName, err)
		}

		slog.InfoContext(ctx, "Updated MongoDB status for unquarantined node",
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

			span := tracing.SpanFromContext(ctx)
			tracing.RecordError(span, err)
			span.SetAttributes(
				attribute.String("node_drainer.error.type", "immediate_eviction_error"),
				attribute.String("node_drainer.error.message", err.Error()),
			)

			return fmt.Errorf("failed immediate eviction for namespace %s on node %s: %w", namespace, nodeName, err)
		}
	}

	return fmt.Errorf("immediate eviction completed, requeuing for status verification")
}

func (r *Reconciler) executeTimeoutEviction(ctx context.Context, action *evaluator.DrainActionResult,
	healthEvent model.HealthEventWithStatus, eventID string, partialDrainEntity *protos.Entity) error {
	span := tracing.SpanFromContext(ctx)
	nodeName := healthEvent.HealthEvent.NodeName
	timeoutMinutes := int(action.Timeout.Minutes())

	span.SetAttributes(
		attribute.Int("node_drainer.timeout_minutes", timeoutMinutes),
	)

	slog.DebugContext(ctx, "Checking cancellation status before timeout eviction",
		"node", nodeName, "eventID", eventID)

	if r.isTimeoutEvictionCancelled(ctx, eventID, nodeName) {
		return nil
	}

	if err := r.informers.DeletePodsAfterTimeout(ctx,
		nodeName, action.Namespaces, timeoutMinutes, &healthEvent, partialDrainEntity); err != nil {
		if r.isTimeoutEvictionCancelled(ctx, eventID, nodeName) {
			return nil
		}

		metrics.ProcessingErrors.WithLabelValues("timeout_eviction_error", nodeName).Inc()
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "timeout_eviction_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)

		return fmt.Errorf("failed timeout eviction for node %s: %w", nodeName, err)
	}

	return fmt.Errorf("timeout eviction initiated, requeuing for status verification")
}

func (r *Reconciler) isTimeoutEvictionCancelled(ctx context.Context, eventID, nodeName string) bool {
	r.nodeEventsMapMu.Lock()
	eventStatus, eventExists := r.nodeEventsMap[nodeName][eventID]
	_, nodeCancelled := r.cancelledNodes[nodeName]
	r.nodeEventsMapMu.Unlock()

	if (eventExists && eventStatus == model.Cancelled) || nodeCancelled {
		slog.InfoContext(ctx, "Event cancelled, aborting timeout eviction",
			"node", nodeName, "eventID", eventID)

		return true
	}

	return false
}

func (r *Reconciler) executeCheckCompletion(ctx context.Context, action *evaluator.DrainActionResult,
	healthEvent model.HealthEventWithStatus, partialDrainEntity *protos.Entity) error {
	span := tracing.SpanFromContext(ctx)
	nodeName := healthEvent.HealthEvent.NodeName

	allPodsComplete := true

	var remainingPods []string

	for _, namespace := range action.Namespaces {
		pods, err := r.informers.FindEvictablePodsInNamespaceAndNode(namespace, nodeName, partialDrainEntity)
		if err != nil {
			tracing.RecordError(span, err)
			span.SetAttributes(
				attribute.String("node_drainer.error.type", "failed_to_check_pods"),
				attribute.String("node_drainer.error.message", err.Error()),
			)

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
			slog.ErrorContext(ctx, "Failed to update node event",
				"node", nodeName,
				"error", err)
			tracing.RecordError(span, err)
			span.SetAttributes(
				attribute.String("node_drainer.error.type", "failed_to_update_node_event"),
				attribute.String("node_drainer.error.message", err.Error()),
			)
		}

		slog.InfoContext(ctx, "Pods still running on node, requeuing for later check",
			"node", nodeName,
			"remainingPods", remainingPods)

		return fmt.Errorf("waiting for pods to complete: %d pods remaining", len(remainingPods))
	}

	slog.InfoContext(ctx, "All pods completed on node", "node", nodeName)

	return fmt.Errorf("pod completion verified, requeuing for status update")
}

func (r *Reconciler) executeMarkAlreadyDrained(ctx context.Context,
	healthEvent model.HealthEventWithStatus, event datastore.Event, database queue.DataStore, status model.Status) error {
	ctx, span := tracing.StartSpan(ctx, "node_drainer.execute_mark_already_drained")
	defer span.End()

	nodeName := healthEvent.HealthEvent.NodeName

	if healthEvent.HealthEventStatus.UserPodsEvictionStatus == nil {
		slog.ErrorContext(ctx, "HealthEventStatus is missing UserPodsEvictionStatus",
			"node", nodeName)

		return errors.New("missing UserPodsEvictionStatus")
	}

	podsEvictionStatus := healthEvent.HealthEventStatus.UserPodsEvictionStatus
	podsEvictionStatus.Status = string(status)

	return r.updateNodeUserPodsEvictedStatus(ctx, database, event, podsEvictionStatus,
		nodeName, metrics.DrainStatusSkipped)
}

func (r *Reconciler) executeUpdateStatus(ctx context.Context, healthEvent model.HealthEventWithStatus,
	event datastore.Event, database queue.DataStore, status model.Status) error {
	ctx, span := tracing.StartSpan(ctx, "node_drainer.execute_update_status")
	defer span.End()

	nodeName := healthEvent.HealthEvent.NodeName

	if healthEvent.HealthEventStatus.UserPodsEvictionStatus == nil {
		slog.ErrorContext(ctx, "HealthEventStatus is missing UserPodsEvictionStatus",
			"node", nodeName)

		return errors.New("missing UserPodsEvictionStatus")
	}

	podsEvictionStatus := healthEvent.HealthEventStatus.UserPodsEvictionStatus
	podsEvictionStatus.Status = string(status) // expect StatusSucceeded or StatusFailed

	nodeDrainLabelValue := statemanager.DrainSucceededLabelValue
	if status == model.StatusFailed {
		nodeDrainLabelValue = statemanager.DrainFailedLabelValue
	}

	if _, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		nodeName, nodeDrainLabelValue, false); err != nil {
		slog.ErrorContext(ctx, "Failed to update node label",
			"label", nodeDrainLabelValue,
			"node", nodeName,
			"error", err)
		metrics.ProcessingErrors.WithLabelValues("label_update_error", nodeName).Inc()
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "label_update_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)
	}

	return r.updateNodeUserPodsEvictedStatus(ctx, database, event, podsEvictionStatus,
		nodeName, metrics.DrainStatusDrained)
}

func (r *Reconciler) updateNodeDrainStatus(ctx context.Context,
	nodeName string, healthEvent *model.HealthEventWithStatus, isDraining bool) {
	if healthEvent.HealthEventStatus.NodeQuarantined == string(model.UnQuarantined) {
		ctx, span := tracing.StartSpan(ctx, "node_drainer.update_node_drain_status")
		span.SetAttributes(attribute.String("node_drainer.k8s.action", "remove_draining_label"))

		_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
			nodeName, statemanager.DrainingLabelValue, true)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to remove draining label for unquarantined node",
				"node", nodeName,
				"error", err)
			tracing.RecordError(span, err)
			span.SetAttributes(
				attribute.String("node_drainer.error.type", "update_nvsentinel_state_label"),
				attribute.String("node_drainer.error.message", err.Error()),
			)
		}

		span.End()

		return
	}

	if isDraining {
		if _, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
			nodeName, statemanager.DrainingLabelValue, false); err != nil {
			_, span := tracing.StartSpan(ctx, "node_drainer.update_node_drain_status")
			slog.ErrorContext(ctx, "Failed to update node label to draining",
				"node", nodeName,
				"error", err)
			metrics.ProcessingErrors.WithLabelValues("label_update_error", nodeName).Inc()
			tracing.RecordError(span, err)
			span.SetAttributes(
				attribute.String("node_drainer.error.type", "update_nvsentinel_state_label"),
				attribute.String("node_drainer.error.message", err.Error()),
			)
			span.End()
		}
	}
}

func (r *Reconciler) updateNodeUserPodsEvictedStatus(ctx context.Context, database queue.DataStore,
	event datastore.Event, userPodsEvictionStatus *protos.OperationStatus,
	nodeName string, drainStatus string) error {
	ctx, span := tracing.StartSpan(ctx, "node_drainer.update_user_pods_eviction_status")
	defer span.End()
	// Extract the document ID (preserving native type for MongoDB)
	documentID, err := utils.ExtractDocumentIDNative(event)
	if err != nil {
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "extract_document_id_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)

		return fmt.Errorf("failed to extract document ID: %w", err)
	}

	// Explicitly construct the eviction status map, always setting all fields
	evictionStatusMap := map[string]interface{}{
		"status":  userPodsEvictionStatus.GetStatus(),
		"message": userPodsEvictionStatus.GetMessage(),
	}

	updateFields := map[string]any{
		"healtheventstatus.userpodsevictionstatus":                evictionStatusMap,
		"healtheventstatus.spanids." + tracing.ServiceNodeDrainer: tracing.SpanIDFromSpan(tracing.SpanFromContext(ctx)),
	}

	// Set DrainFinishTimestamp when drain completes successfully
	if userPodsEvictionStatus.Status == string(model.StatusSucceeded) ||
		userPodsEvictionStatus.Status == string(model.AlreadyDrained) {
		updateFields["healtheventstatus.drainfinishtimestamp"] = timestamppb.Now()
	}

	filter := map[string]any{"_id": documentID}
	update := map[string]any{"$set": updateFields}

	_, err = database.UpdateDocument(ctx, filter, update)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("update_status_error", nodeName).Inc()
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "update_status_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)

		return fmt.Errorf("error updating document with ID: %v, error: %w", documentID, err)
	}

	r.observeEvictionDurationIfSucceeded(ctx, event, userPodsEvictionStatus)

	span.SetAttributes(
		attribute.String("node_drainer.user_pods_eviction_status", string(userPodsEvictionStatus.Status)),
	)

	slog.InfoContext(ctx, "Health event status has been updated",
		"documentID", documentID,
		"evictionStatus", userPodsEvictionStatus.Status)
	metrics.EventsProcessed.WithLabelValues(drainStatus, nodeName).Inc()

	return nil
}

// observeEvictionDurationIfSucceeded observes eviction duration metric if status is succeeded and timestamp is present
func (r *Reconciler) observeEvictionDurationIfSucceeded(ctx context.Context, event datastore.Event,
	userPodsEvictionStatus *protos.OperationStatus) {
	if userPodsEvictionStatus.Status != string(model.StatusSucceeded) {
		return
	}

	healthEvent, parseErr := eventutil.ParseHealthEventFromEvent(event)
	if parseErr != nil || healthEvent.HealthEventStatus.QuarantineFinishTimestamp == nil {
		return
	}

	evictionDuration := time.Since(healthEvent.HealthEventStatus.QuarantineFinishTimestamp.AsTime()).Seconds()
	slog.InfoContext(ctx, "Node drainer evictionDuration is", "evictionDuration", evictionDuration)

	if evictionDuration > 0 {
		metrics.PodEvictionDuration.Observe(evictionDuration)
	}
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

// HandleCancellation processes a cancellation status for a specific event or node.
// For Cancelled status, it marks the specific event as cancelled.
// For UnQuarantined status, it sets a node-level cancellation flag affecting all events.
// Other known statuses are logged and ignored; unknown statuses trigger a warning.
func (r *Reconciler) HandleCancellation(ctx context.Context, eventID string, nodeName string, status model.Status) {
	r.nodeEventsMapMu.Lock()
	defer r.nodeEventsMapMu.Unlock()

	slog.DebugContext(ctx, "HandleCancellation called", "node", nodeName, "eventID", eventID, "status", status)

	switch status {
	case model.Cancelled:
		if r.nodeEventsMap[nodeName] == nil {
			r.nodeEventsMap[nodeName] = make(eventStatusMap)
		}

		r.nodeEventsMap[nodeName][eventID] = model.Cancelled
		slog.InfoContext(ctx, "Marked specific event as cancelled", "node", nodeName, "eventID", eventID)
	case model.UnQuarantined:
		// Set node-level cancellation flag. This ensures any events queued but not yet
		// processed will see the cancellation, even if they haven't been added to
		// nodeEventsMap yet (race condition protection).
		r.cancelledNodes[nodeName] = struct{}{}
		slog.InfoContext(ctx, "Marked node as cancelled", "node", nodeName)

		if eventsMap, exists := r.nodeEventsMap[nodeName]; exists {
			for evtID := range eventsMap {
				eventsMap[evtID] = model.Cancelled
				slog.InfoContext(ctx, "Marked event as cancelled for node", "node", nodeName, "eventID", evtID)
			}
		}
	case model.StatusNotStarted, model.StatusInProgress, model.StatusFailed,
		model.StatusSucceeded, model.AlreadyDrained, model.Quarantined,
		model.AlreadyQuarantined:
		slog.DebugContext(ctx, "No cancellation action required for status", "node", nodeName, "status", status)
	default:
		slog.WarnContext(ctx, "Unknown cancellation status", "node", nodeName, "eventID", eventID, "status", status)
	}

	slog.DebugContext(ctx, "Cancellation processed", "node", nodeName, "eventID", eventID)
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

func (r *Reconciler) markEventInProgress(ctx context.Context, eventID string, nodeName string) {
	r.nodeEventsMapMu.Lock()
	defer r.nodeEventsMapMu.Unlock()

	if r.nodeEventsMap[nodeName] == nil {
		r.nodeEventsMap[nodeName] = make(eventStatusMap)
		// Clear node-level cancellation flag when starting fresh drain
		// (re-arm the node for new quarantine session)
		_, wasNodeCancelled := r.cancelledNodes[nodeName]
		if wasNodeCancelled {
			slog.InfoContext(ctx, "Clearing node-level cancellation flag when starting new drain session", "node", nodeName)
		}

		delete(r.cancelledNodes, nodeName)
	}

	r.nodeEventsMap[nodeName][eventID] = model.StatusInProgress

	slog.DebugContext(ctx, "Event marked as in progress", "node", nodeName, "eventID", eventID)
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
	ctx, span := tracing.StartSpan(ctx, "node_drainer.handle_cancelled_event")
	defer span.End()

	r.clearEventStatus(eventID, nodeName)

	if healthEvent.HealthEventStatus.UserPodsEvictionStatus == nil {
		slog.ErrorContext(ctx, "HealthEventStatus is missing UserPodsEvictionStatus",
			"node", nodeName)

		return errors.New("missing UserPodsEvictionStatus")
	}

	podsEvictionStatus := healthEvent.HealthEventStatus.UserPodsEvictionStatus
	podsEvictionStatus.Status = string(model.Cancelled)

	if err := r.updateNodeUserPodsEvictedStatus(ctx, database, event, podsEvictionStatus, nodeName,
		metrics.DrainStatusCancelled); err != nil {
		slog.ErrorContext(ctx, "Failed to update MongoDB status for cancelled event",
			"node", nodeName,
			"error", err)
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "update_cancelled_status_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)

		return fmt.Errorf("failed to update MongoDB status for cancelled event on node %s: %w", nodeName, err)
	}

	r.deleteCustomDrainCRIfEnabled(ctx, nodeName, event)

	if _, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		nodeName, statemanager.DrainingLabelValue, true); err != nil {
		slog.ErrorContext(ctx, "Failed to remove draining label for cancelled event",
			"node", nodeName,
			"error", err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "remove_draining_label_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)
		tracing.RecordError(span, err)
	}

	metrics.CancelledEvent.WithLabelValues(nodeName, healthEvent.HealthEvent.CheckName).Inc()
	slog.InfoContext(ctx, "Successfully cleaned up cancelled event", "node", nodeName, "eventID", eventID)

	return nil
}

func (r *Reconciler) executeCustomDrain(ctx context.Context, action *evaluator.DrainActionResult,
	healthEvent model.HealthEventWithStatus, event datastore.Event, database queue.DataStore,
	partialDrainEntity *protos.Entity) error {
	ctx, span := tracing.StartSpan(ctx, "node_drainer.execute_custom_drain")
	defer span.End()

	nodeName := healthEvent.HealthEvent.NodeName

	eventID, err := utils.ExtractDocumentID(event)
	if err != nil {
		return fmt.Errorf("failed to extract document ID for custom drain: %w", err)
	}

	podsToDrain := make(map[string][]string)

	for _, ns := range action.Namespaces {
		pods, err := r.informers.FindEvictablePodsInNamespaceAndNode(ns, nodeName, partialDrainEntity)
		if err != nil {
			slog.WarnContext(ctx, "Failed to find evictable pods",
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
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "custom_drain_cr_creation_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)

		return r.handleCustomDrainCRCreationError(ctx, err, nodeName, healthEvent, event, database)
	}

	slog.InfoContext(ctx, "Created custom drain CR",
		"node", nodeName,
		"crName", crName)

	span.SetAttributes(
		attribute.String("node_drainer.custom_cr.name", crName),
		attribute.Bool("node_drainer.custom_cr.created", true),
	)

	return fmt.Errorf("waiting for custom drain CR to complete: %s", crName)
}

func (r *Reconciler) deleteCustomDrainCRIfEnabled(ctx context.Context, nodeName string, event datastore.Event) {
	if !r.Config.TomlConfig.CustomDrain.Enabled {
		return
	}

	ctx, span := tracing.StartSpan(ctx, "node_drainer.delete_custom_drain_cr")
	defer span.End()

	eventID, err := utils.ExtractDocumentID(event)
	if err != nil {
		slog.WarnContext(ctx, "Failed to extract document ID for custom drain CR deletion",
			"node", nodeName,
			"error", err)
		tracing.RecordError(span, err)

		return
	}

	crName := customdrain.GenerateCRName(nodeName, eventID)
	span.SetAttributes(attribute.String("node_drainer.custom_cr.name", crName))

	if err := r.customDrainClient.DeleteDrainCR(ctx, crName); err != nil {
		slog.WarnContext(ctx, "Failed to delete custom drain CR",
			"node", nodeName,
			"crName", crName,
			"error", err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "custom_drain_cr_deletion_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)
	} else {
		slog.InfoContext(ctx, "Deleted custom drain CR",
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
	ctx, span := tracing.StartSpan(ctx, "node_drainer.handle_custom_drain_cr_creation_error")
	defer span.End()

	var noMatchErr *meta.NoKindMatchError
	if !errors.As(err, &noMatchErr) {
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "custom_drain_cr_creation_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)

		return fmt.Errorf("failed to create custom drain CR for node %s: %w", nodeName, err)
	}

	slog.ErrorContext(ctx, "Custom drain CRD not found - marking drain as failed",
		"node", nodeName,
		"apiGroup", r.Config.TomlConfig.CustomDrain.ApiGroup,
		"kind", r.Config.TomlConfig.CustomDrain.Kind,
		"error", err)

	metrics.CustomDrainCRDNotFound.WithLabelValues(nodeName).Inc()

	if err := r.setDrainFailedStatus(ctx, healthEvent, event, database,
		fmt.Sprintf("Custom drain CRD not found: %v", err)); err != nil {
		slog.ErrorContext(ctx, "Failed to update drain failed status",
			"node", nodeName,
			"error", err)
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "set_drain_failed_status_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)

		return fmt.Errorf("failed to update drain failed status: %w", err)
	}

	if _, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		nodeName, statemanager.DrainFailedLabelValue, false); err != nil {
		slog.ErrorContext(ctx, "Failed to update node label to drain-failed",
			"node", nodeName,
			"error", err)
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "update_nvsentinel_state_label"),
			attribute.String("node_drainer.error.message", err.Error()),
		)

		return fmt.Errorf("failed to update node label to drain-failed: %w", err)
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
	ctx, span := tracing.StartSpan(ctx, "node_drainer.set_drain_failed_status")
	defer span.End()

	span.SetAttributes(
		attribute.String("node_drainer.fail_reason", reason),
	)

	documentID, err := utils.ExtractDocumentIDNative(event)
	if err != nil {
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "extract_document_id_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)

		return fmt.Errorf("failed to extract document ID: %w", err)
	}

	filter := map[string]any{"_id": documentID}

	update := map[string]any{
		"$set": map[string]any{
			"healtheventstatus.userpodsevictionstatus": protos.OperationStatus{
				Status:  string(model.StatusFailed),
				Message: reason,
			},
			"healtheventstatus.spanids." + tracing.ServiceNodeDrainer: tracing.SpanIDFromSpan(tracing.SpanFromContext(ctx)),
		},
	}

	_, err = database.UpdateDocument(ctx, filter, update)
	if err != nil {
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("node_drainer.error.type", "update_drain_failed_status_error"),
			attribute.String("node_drainer.error.message", err.Error()),
		)

		return fmt.Errorf("failed to update MongoDB drain failed status: %w", err)
	}

	slog.InfoContext(ctx, "Updated drain status to failed",
		"node", healthEvent.HealthEvent.NodeName,
		"reason", reason)

	return nil
}
