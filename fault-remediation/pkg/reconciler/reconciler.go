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
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/annotation"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/events"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/metrics"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/remediation"
	nvstoreclient "github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const coldStartBatchSize = 1000

type ReconcilerConfig struct {
	DataStoreConfig    datastore.DataStoreConfig
	TokenConfig        nvstoreclient.TokenConfig
	Pipeline           datastore.Pipeline
	RemediationClient  remediation.FaultRemediationClientInterface
	StateManager       statemanager.StateManager
	EnableLogCollector bool
	UpdateMaxRetries   int
	UpdateRetryDelay   time.Duration
}

// FaultRemediationReconciler reconciles health events from a datastore change stream
// and manages fault remediation lifecycle. It supports both standalone execution
// and controller-runtime managed operation via SetupWithManager.
type FaultRemediationReconciler struct {
	client.Client
	ds                datastore.DataStore
	Watcher           datastore.ChangeStreamWatcher
	healthEventStore  datastore.HealthEventStore
	Config            ReconcilerConfig
	annotationManager annotation.NodeAnnotationManagerInterface
	dryRun            bool
	coldStartCh       chan event.TypedGenericEvent[*datastore.EventWithToken]
	eventSessions     sync.Map
}

type eventTraceSession struct {
	span oteltrace.Span
}

// NewFaultRemediationReconciler creates a new FaultRemediationReconciler with the provided dependencies.
func NewFaultRemediationReconciler(
	ds datastore.DataStore,
	watcher datastore.ChangeStreamWatcher,
	healthEventStore datastore.HealthEventStore,
	config ReconcilerConfig,
	dryRun bool,
) *FaultRemediationReconciler {
	return &FaultRemediationReconciler{
		ds:                ds,
		Watcher:           watcher,
		healthEventStore:  healthEventStore,
		Config:            config,
		annotationManager: config.RemediationClient.GetAnnotationManager(),
		dryRun:            dryRun,
	}
}

// Reconcile processes a single health event from the datastore change stream.
// It parses the event, determines the appropriate action (cancellation or remediation),
// and returns a result instructing controller-runtime on requeue behavior.
func (r *FaultRemediationReconciler) Reconcile(
	ctx context.Context,
	event *datastore.EventWithToken,
) (result ctrl.Result, reconcileErr error) {
	start := time.Now()

	slog.InfoContext(ctx, "Reconciling Event")

	defer func() {
		metrics.EventHandlingDuration.Observe(time.Since(start).Seconds())
	}()

	metrics.TotalEventsReceived.Inc()

	healthEventWithStatus, err := r.parseHealthEvent(ctx, *event, r.Watcher)
	if err != nil {
		return ctrl.Result{}, nil
	}

	// Safety checks for nil pointers
	if healthEventWithStatus.HealthEvent == nil {
		slog.WarnContext(ctx, "HealthEvent is nil, skipping processing")
		return ctrl.Result{}, nil
	}

	nodeName := healthEventWithStatus.HealthEvent.NodeName
	traceID := tracing.TraceIDFromMetadata(healthEventWithStatus.HealthEvent.GetMetadata())
	parentSpanID := tracing.ParentSpanID(healthEventWithStatus.HealthEventStatus.SpanIds, tracing.ServiceNodeDrainer)
	sessionCtx, session := r.startOrReuseEventSession(ctx,
		traceID,
		parentSpanID,
		healthEventWithStatus.ID,
	)

	defer func() {
		r.completeEventSession(healthEventWithStatus.ID, session, result, reconcileErr)
	}()

	ctx, span := tracing.StartSpan(sessionCtx, "fault_remediation.reconcile")

	defer span.End()

	// Add health event attributes to span (nil-safe: span and optional status fields)
	tracing.AddHealthEventStatusAttributes(
		span, healthEventWithStatus.HealthEventStatus, healthEventWithStatus.ID)
	nodeQuarantined := healthEventWithStatus.HealthEventStatus.NodeQuarantined

	if nodeQuarantined == string(model.UnQuarantined) || nodeQuarantined == string(model.Cancelled) {
		return r.handleCancellationEvent(ctx, nodeName, model.Status(nodeQuarantined), r.Watcher, event.ResumeToken)
	}

	return r.handleRemediationEvent(ctx, &healthEventWithStatus, *event, r.Watcher, r.healthEventStore)
}

func (r *FaultRemediationReconciler) startOrReuseEventSession(
	ctx context.Context,
	traceID, parentSpanID, eventID string,
) (context.Context, *eventTraceSession) {
	if val, ok := r.eventSessions.Load(eventID); ok {
		session := val.(*eventTraceSession)
		if session != nil && session.span != nil {
			return oteltrace.ContextWithSpan(ctx, session.span), session
		}
	}

	sessionCtx, sessionSpan := tracing.StartSpanWithLinkFromTraceContext(
		ctx, traceID, parentSpanID, "fault_remediation.event_received")
	session := &eventTraceSession{span: sessionSpan}

	r.eventSessions.Store(eventID, session)

	return sessionCtx, session
}

func (r *FaultRemediationReconciler) completeEventSession(
	eventID string,
	session *eventTraceSession,
	result ctrl.Result,
	reconcileErr error,
) {
	// Keep the lifecycle span open while controller-runtime is still retrying/requeueing.
	if reconcileErr != nil || result.RequeueAfter > 0 {
		return
	}

	val, ok := r.eventSessions.Load(eventID)
	if !ok {
		return
	}

	current := val.(*eventTraceSession)
	if current != session || current == nil || current.span == nil {
		return
	}

	current.span.End()
	r.eventSessions.Delete(eventID)
}

func (r *FaultRemediationReconciler) shouldSkipEvent(ctx context.Context,
	healthEventWithStatus model.HealthEventWithStatus, groupConfig *common.EquivalenceGroupConfig) bool {
	action := healthEventWithStatus.HealthEvent.RecommendedAction
	nodeName := healthEventWithStatus.HealthEvent.NodeName

	span := tracing.SpanFromContext(ctx)

	if action == protos.RecommendedAction_NONE {
		slog.InfoContext(ctx, "Skipping event for node: recommended action is NONE (no remediation needed)",
			"node", nodeName)

		span.SetAttributes(
			attribute.String("fault_remediation.skip_reason", "recommended_action_none"),
		)

		return true
	}

	if healthEventWithStatus.HealthEventStatus != nil && healthEventWithStatus.HealthEventStatus.FaultRemediated != nil &&
		healthEventWithStatus.HealthEventStatus.FaultRemediated.GetValue() {
		span.SetAttributes(
			attribute.String("fault_remediation.skip_reason", "already_remediated"),
		)

		return true
	}

	if groupConfig != nil {
		return false
	}

	// Unsupported action detected
	actionName := model.GetEffectiveActionName(healthEventWithStatus.HealthEvent)
	slog.Info("Unsupported recommended action for node",
		"action", actionName,
		"node", nodeName)
	metrics.TotalUnsupportedRemediationActions.WithLabelValues(actionName, nodeName).Inc()

	span.SetAttributes(
		attribute.String("fault_remediation.skip_reason", "unsupported_action"),
	)

	_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		healthEventWithStatus.HealthEvent.NodeName,
		statemanager.RemediationFailedLabelValue, false)
	if err != nil {
		slog.ErrorContext(ctx, "Error updating node label",
			"label", statemanager.RemediationFailedLabelValue,
			"error", err)
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "label_update_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)
		metrics.ProcessingErrors.WithLabelValues("label_update_error",
			healthEventWithStatus.HealthEvent.NodeName).Inc()
	}

	return true
}

// runLogCollector runs log collector for non-NONE actions if enabled
func (r *FaultRemediationReconciler) runLogCollector(
	ctx context.Context,
	healthEvent *protos.HealthEvent,
	eventUID string,
) (ctrl.Result, error) {
	if healthEvent.RecommendedAction == protos.RecommendedAction_NONE || !r.Config.EnableLogCollector {
		return ctrl.Result{}, nil
	}

	ctx, span := tracing.StartSpan(ctx, "fault_remediation.log_collector")
	defer span.End()

	slog.InfoContext(ctx, "Log collector feature enabled; running log collector for node",
		"node", healthEvent.NodeName)

	result, err := r.Config.RemediationClient.RunLogCollectorJob(ctx, healthEvent.NodeName, eventUID)
	if err != nil {
		slog.ErrorContext(ctx, "Log collector job failed to launch for node",
			"node", healthEvent.NodeName,
			"error", err)
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "log_collector_launch_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)

		return ctrl.Result{}, fmt.Errorf("failed to launch log collector on node: %w", err)
	}

	return result, nil
}

// performRemediation attempts to create maintenance resource with retries
func (r *FaultRemediationReconciler) performRemediation(ctx context.Context,
	healthEventWithStatus *events.HealthEventDoc, groupConfig *common.EquivalenceGroupConfig) (string, error) {
	nodeName := healthEventWithStatus.HealthEvent.NodeName

	ctx, span := tracing.StartSpan(ctx, "fault_remediation.perform_remediation")
	defer span.End()

	// Update state to "remediating"
	_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		healthEventWithStatus.HealthEvent.NodeName,
		statemanager.RemediatingLabelValue, false)
	if err != nil {
		slog.ErrorContext(ctx, "Error updating node label to remediating", "error", err)
		metrics.ProcessingErrors.WithLabelValues("label_update_error", nodeName).Inc()

		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "label_update_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)

		return "", fmt.Errorf("error updating node label to remediating: %w", err)
	}

	healthEventData := &events.HealthEventData{
		ID:                    healthEventWithStatus.ID,
		HealthEventWithStatus: healthEventWithStatus.HealthEventWithStatus,
	}

	remediationLabelValue := statemanager.RemediationSucceededLabelValue

	crName, createMaintenanceResourceError := r.Config.RemediationClient.CreateMaintenanceResource(ctx,
		healthEventData, groupConfig)
	if createMaintenanceResourceError != nil {
		metrics.ProcessingErrors.WithLabelValues("cr_creation_failed", nodeName).Inc()
		tracing.RecordError(span, createMaintenanceResourceError)
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "cr_creation_failed"),
			attribute.String("fault_remediation.error.message", createMaintenanceResourceError.Error()),
		)

		remediationLabelValue = statemanager.RemediationFailedLabelValue
		// don't throw error yet so we can update state
	}

	_, err = r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		healthEventWithStatus.HealthEvent.NodeName,
		remediationLabelValue, false)
	if err != nil {
		slog.ErrorContext(ctx, "Error updating node label",
			"label", remediationLabelValue,
			"error", err)
		metrics.ProcessingErrors.WithLabelValues("label_update_error", nodeName).Inc()
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "label_update_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)

		return "", errors.Join(createMaintenanceResourceError, err)
	}

	if createMaintenanceResourceError != nil {
		return "", fmt.Errorf("error creating maintenance resource: %w", createMaintenanceResourceError)
	}

	return crName, nil
}

// handleCancellationEvent handles node unquarantine and cancellation events by clearing annotations
func (r *FaultRemediationReconciler) handleCancellationEvent(
	ctx context.Context,
	nodeName string,
	status model.Status,
	watcherInstance datastore.ChangeStreamWatcher,
	resumeToken []byte,
) (ctrl.Result, error) {
	ctx, span := tracing.StartSpan(ctx, "fault_remediation.cancellation_event")
	defer span.End()

	slog.InfoContext(ctx, "Cancellation event received, clearing all remediation state",
		"node", nodeName,
		"status", status)

	if err := r.annotationManager.ClearRemediationState(ctx, nodeName); err != nil {
		slog.ErrorContext(ctx, "Failed to clear remediation state for node",
			"node", nodeName,
			"error", err)
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "clear_remediation_state_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)

		return ctrl.Result{}, fmt.Errorf("failed to clear remediation state for node: %w", err)
	}

	if err := safeMarkProcessed(context.Background(), watcherInstance, resumeToken, nodeName); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// handleRemediationEvent processes remediation for quarantined nodes
func (r *FaultRemediationReconciler) handleRemediationEvent(
	ctx context.Context,
	healthEventWithStatus *events.HealthEventDoc,
	eventWithToken datastore.EventWithToken,
	watcherInstance datastore.ChangeStreamWatcher,
	healthEventStore datastore.HealthEventStore,
) (ctrl.Result, error) {
	ctx, span := tracing.StartSpan(ctx, "fault_remediation.handle_remediation_event")
	defer span.End()

	healthEvent := healthEventWithStatus.HealthEvent
	nodeName := healthEvent.NodeName

	groupConfig, err := common.GetGroupConfigForEvent(r.Config.RemediationClient.GetConfig().RemediationActions,
		healthEvent)
	if err != nil {
		// If we got an error, groupConfig will be nil which will result in shouldSkipEvent setting state label to
		// remediation-failed
		slog.ErrorContext(ctx, "Got an error getting group config for event, skipping event and failing remediation",
			"error", err, "event", healthEventWithStatus.ID)
	}

	res, err, done := r.trySkipEvent(ctx, healthEventWithStatus, groupConfig, eventWithToken, watcherInstance, nodeName)
	if done {
		span.SetAttributes(
			attribute.String("fault_remediation.status", "skipped"),
		)

		return res, err
	}

	shouldCreateCR, existingCR, err := r.checkExistingCRStatus(ctx, healthEvent, groupConfig)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("cr_status_check_error", nodeName).Inc()
		slog.ErrorContext(ctx, "Error checking existing CR status", "node", nodeName, "error", err)

		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "cr_status_check_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)
		tracing.RecordError(span, err)

		return ctrl.Result{}, fmt.Errorf("error checking existing CR status: %w", err)
	}

	if !shouldCreateCR {
		return r.handleExistingCRSkip(ctx, eventWithToken, watcherInstance, nodeName, existingCR)
	}

	result, err := r.runLogCollectorAndRemediate(ctx, healthEvent, healthEventWithStatus, eventWithToken,
		watcherInstance, healthEventStore, groupConfig, nodeName)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !result.IsZero() {
		return result, nil
	}

	metrics.EventsProcessed.WithLabelValues(metrics.CRStatusCreated, nodeName).Inc()

	return r.markProcessedOrError(ctx, watcherInstance, eventWithToken, nodeName)
}

// trySkipEvent returns (result, err, true) when the event should be skipped; otherwise (zero, nil, false).
func (r *FaultRemediationReconciler) trySkipEvent(
	ctx context.Context,
	healthEventWithStatus *events.HealthEventDoc,
	groupConfig *common.EquivalenceGroupConfig,
	eventWithToken datastore.EventWithToken,
	watcherInstance datastore.ChangeStreamWatcher,
	nodeName string,
) (ctrl.Result, error, bool) {
	if !r.shouldSkipEvent(ctx, healthEventWithStatus.HealthEventWithStatus, groupConfig) {
		return ctrl.Result{}, nil, false
	}

	ctx, skipSpan := tracing.StartSpan(ctx, "fault_remediation.skip_event")
	defer skipSpan.End()

	if err := safeMarkProcessed(ctx, watcherInstance, eventWithToken.ResumeToken, nodeName); err != nil {
		return ctrl.Result{}, err, true
	}

	return ctrl.Result{}, nil, true
}

// handleExistingCRSkip logs, records metrics, marks the event processed, and returns.
func (r *FaultRemediationReconciler) handleExistingCRSkip(
	ctx context.Context,
	eventWithToken datastore.EventWithToken,
	watcherInstance datastore.ChangeStreamWatcher,
	nodeName, existingCR string,
) (ctrl.Result, error) {
	span := tracing.SpanFromContext(ctx)
	slog.InfoContext(ctx, "Skipping event for node due to existing CR",
		"node", nodeName,
		"existingCR", existingCR)

	span.SetAttributes(
		attribute.String("fault_remediation.skip_reason", "existing_cr_found"),
		attribute.String("fault_remediation.existing_cr.name", existingCR),
	)

	metrics.EventsProcessed.WithLabelValues(metrics.CRStatusSkipped, nodeName).Inc()

	if err := safeMarkProcessed(ctx, watcherInstance, eventWithToken.ResumeToken, nodeName); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// runLogCollectorAndRemediate runs the log collector, then performs remediation and updates status.
// Returns a non-zero ctrl.Result if the log collector requested a requeue; otherwise Result{}, and any error.
func (r *FaultRemediationReconciler) runLogCollectorAndRemediate(
	ctx context.Context,
	healthEvent *protos.HealthEvent,
	healthEventWithStatus *events.HealthEventDoc,
	eventWithToken datastore.EventWithToken,
	_ datastore.ChangeStreamWatcher,
	healthEventStore datastore.HealthEventStore,
	groupConfig *common.EquivalenceGroupConfig,
	nodeName string,
) (ctrl.Result, error) {
	span := tracing.SpanFromContext(ctx)

	result, err := r.runLogCollector(ctx, healthEvent, healthEventWithStatus.ID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error running log collector: %w", err)
	}

	if !result.IsZero() {
		return result, nil
	}

	_, performRemediationErr := r.performRemediation(ctx, healthEventWithStatus, groupConfig)

	nodeRemediatedStatus := performRemediationErr == nil
	if performRemediationErr != nil {
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "perform_remediation_error"),
			attribute.String("fault_remediation.error.message", performRemediationErr.Error()),
		)
		tracing.RecordError(span, performRemediationErr)
	}

	if err = r.updateNodeRemediatedStatus(ctx, healthEventStore, eventWithToken, nodeRemediatedStatus); err != nil {
		metrics.ProcessingErrors.WithLabelValues("update_status_error", nodeName).Inc()
		slog.ErrorContext(ctx, "Error updating remediation status for node", "error", err)
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "update_status_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)

		return ctrl.Result{}, errors.Join(performRemediationErr, err)
	}

	if performRemediationErr != nil {
		return ctrl.Result{}, performRemediationErr
	}

	return ctrl.Result{}, nil
}

// safeMarkProcessed advances the resume token for live stream events.
// Cold-start events carry an empty ResumeToken; calling MarkProcessed
// with an empty token would incorrectly advance the checkpoint to the
// current change-stream cursor position, potentially skipping live
// events that haven't been processed yet.
func safeMarkProcessed(ctx context.Context, w datastore.ChangeStreamWatcher, token []byte, node string) error {
	if len(token) == 0 {
		return nil
	}

	ctx, span := tracing.StartSpan(ctx, "fault_remediation.mark_processed")
	defer span.End()

	if err := w.MarkProcessed(ctx, token); err != nil {
		metrics.ProcessingErrors.WithLabelValues("mark_processed_error", node).Inc()
		slog.ErrorContext(ctx, "Error updating resume token", "error", err)

		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "mark_processed_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)

		return fmt.Errorf("failed to mark event as processed: %w", err)
	}

	return nil
}

// markProcessedOrError marks the event processed and returns (Result{}, nil) or (zero, err).
func (r *FaultRemediationReconciler) markProcessedOrError(
	ctx context.Context,
	watcherInstance datastore.ChangeStreamWatcher,
	eventWithToken datastore.EventWithToken,
	nodeName string,
) (ctrl.Result, error) {
	if err := safeMarkProcessed(ctx, watcherInstance, eventWithToken.ResumeToken, nodeName); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *FaultRemediationReconciler) updateNodeRemediatedStatus(
	ctx context.Context,
	healthEventStore datastore.HealthEventStore,
	eventWithToken datastore.EventWithToken,
	nodeRemediatedStatus bool,
) error {
	ctx, statusSpan := tracing.StartSpan(ctx, "fault_remediation.remediation_status_updated")
	defer statusSpan.End()

	statusSpan.SetAttributes(
		attribute.Bool("fault_remediation.status", nodeRemediatedStatus),
	)

	documentID, err := utils.ExtractDocumentID(eventWithToken.Event)
	if err != nil {
		tracing.RecordError(statusSpan, err)
		statusSpan.SetAttributes(
			attribute.String("fault_remediation.error.type", "extract_document_id_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)

		return err
	}

	// Create status object for the update
	status := datastore.HealthEventStatus{}
	faultRemediated := nodeRemediatedStatus
	status.FaultRemediated = &faultRemediated

	// If remediation was successful, set the timestamp
	if nodeRemediatedStatus {
		now := time.Now().UTC()
		status.LastRemediationTimestamp = timestamppb.New(now)
	}

	// Use the healthEventStore to update the status with retries
	slog.InfoContext(ctx, "Updating health event with ID", "id", documentID)

	err = healthEventStore.UpdateHealthEventStatus(ctx, documentID, status)
	if err != nil {
		tracing.RecordError(statusSpan, err)
		statusSpan.SetAttributes(
			attribute.String("fault_remediation.error.type", "update_health_event_status_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)

		return fmt.Errorf("error updating document with ID: %v, error: %w", documentID, err)
	}

	if spanID := tracing.SpanIDFromSpan(statusSpan); spanID != "" {
		if updateErr := healthEventStore.UpdateSpanID(ctx, documentID,
			tracing.ServiceFaultRemediation, spanID); updateErr != nil {
			slog.WarnContext(ctx, "Failed to write fault_remediation span ID to document", "id", documentID, "error", updateErr)
		}
	}

	slog.InfoContext(ctx, "Health event has been updated with status",
		"id", documentID,
		"status", nodeRemediatedStatus)

	return nil
}

func (r *FaultRemediationReconciler) checkExistingCRStatus(ctx context.Context, healthEvent *protos.HealthEvent,
	groupConfig *common.EquivalenceGroupConfig) (bool, string, error) {
	nodeName := healthEvent.NodeName

	if groupConfig == nil {
		return true, "", nil
	}

	state, _, err := r.annotationManager.GetRemediationState(ctx, nodeName)
	if err != nil {
		slog.ErrorContext(ctx, "Error getting remediation state", "node", nodeName, "error", err)
		return true, "", fmt.Errorf("error getting remediation state: %w", err)
	}

	if state == nil {
		slog.WarnContext(ctx, "Remediation state is nil for node, allowing CR creation",
			"node", nodeName)

		return true, "", nil
	}

	statusChecker := r.Config.RemediationClient.GetStatusChecker()
	if statusChecker == nil {
		slog.WarnContext(ctx, "Status checker is not available, allowing creation")
		return true, "", nil
	}

	groupStates := common.FilterEquivalenceGroupStates(groupConfig, state)

	var groupsToRemove []string

	for groupName, groupState := range groupStates {
		shouldSkip := statusChecker.ShouldSkipCRCreation(ctx, groupState.ActionName, groupState.MaintenanceCR)
		if shouldSkip {
			slog.InfoContext(ctx, "CR exists and is in progress, skipping event",
				"node", nodeName, "crName", groupState.MaintenanceCR)

			return false, groupState.MaintenanceCR, nil
		}

		slog.InfoContext(ctx, "CR completed or failed, allowing retry", "node", nodeName, "crName", groupState.MaintenanceCR)

		groupsToRemove = append(groupsToRemove, groupName)
	}

	if len(groupsToRemove) > 0 {
		if err := r.annotationManager.RemoveGroupsFromState(ctx, nodeName, groupsToRemove); err != nil {
			return true, "", fmt.Errorf("failed to remove groups from annotation: %w", err)
		}
	}

	return true, "", nil
}

// parseHealthEvent extracts and parses health event from change stream event
// The eventWithToken.Event is already the fullDocument extracted by the store-client
func (r *FaultRemediationReconciler) parseHealthEvent(ctx context.Context, eventWithToken datastore.EventWithToken,
	watcherInstance datastore.ChangeStreamWatcher) (events.HealthEventDoc, error) {
	var result events.HealthEventDoc

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

		metrics.ProcessingErrors.WithLabelValues(errorLabel, "unknown").Inc()
		slog.ErrorContext(ctx, "Error parsing health event", "error", err)

		_ = safeMarkProcessed(context.Background(), watcherInstance, eventWithToken.ResumeToken, "unknown")

		return result, fmt.Errorf("error parsing health event: %w", err)
	}

	// Extract document ID and wrap into HealthEventDoc
	documentID, err := utils.ExtractDocumentID(eventWithToken.Event)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("extract_id_error", "unknown").Inc()
		slog.ErrorContext(ctx, "Error extracting document ID", "error", err)

		if markErr := watcherInstance.MarkProcessed(context.Background(), eventWithToken.ResumeToken); markErr != nil {
			metrics.ProcessingErrors.WithLabelValues("mark_processed_error", "unknown").Inc()
			slog.Error("Error updating resume token", "error", markErr)
		}

		return result, fmt.Errorf("error extracting document ID: %w", err)
	}

	result.ID = documentID
	result.HealthEventWithStatus = healthEventWithStatus

	return result, nil
}

// StartWatcherStream starts the watcher stream for non-controller-runtime managed mode.
// This method should not be used when running under controller-runtime management;
// use SetupWithManager instead for ctrl-runtime integration.
func (r *FaultRemediationReconciler) StartWatcherStream(ctx context.Context) {
	r.Watcher.Start(ctx)
}

// CloseAll closes all resources (datastore and watcher) and aggregates any errors.
// It attempts to close all resources even if individual Close operations fail.
func (r *FaultRemediationReconciler) CloseAll(ctx context.Context) error {
	var errs []error

	if err := r.ds.Close(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to close datastore", "error", err)
		errs = append(errs, err)
	}

	if err := r.Watcher.Close(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to close Watcher", "error", err)
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// SetupWithManager configures the reconciler for controller-runtime managed operation.
// It starts the watcher stream and returns a done channel that is closed when the event
// adapter goroutine exits. Callers should monitor this channel: if it closes while the
// context is still active, the change stream died unexpectedly and the pod should exit.
func (r *FaultRemediationReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) (<-chan struct{}, error) {
	r.Watcher.Start(ctx)

	typedCh, watcherDone := AdaptEvents(ctx, r.Watcher.Events())

	r.coldStartCh = make(chan event.TypedGenericEvent[*datastore.EventWithToken], coldStartBatchSize)

	enqueueHandler := handler.TypedFuncs[*datastore.EventWithToken, *datastore.EventWithToken]{
		GenericFunc: func(
			ctx context.Context,
			e event.TypedGenericEvent[*datastore.EventWithToken],
			q workqueue.TypedRateLimitingInterface[*datastore.EventWithToken],
		) {
			q.Add(e.Object)
		},
	}

	err := builder.TypedControllerManagedBy[*datastore.EventWithToken](mgr).
		Named("fault-remediation-controller").
		WatchesRawSource(source.TypedChannel(typedCh, enqueueHandler)).
		WatchesRawSource(source.TypedChannel(r.coldStartCh, enqueueHandler)).
		Complete(r)

	return watcherDone, err
}

// HandleColdStart queries for health events that need remediation or cancellation
// cleanup after a restart. Events are enqueued into the controller-runtime workqueue
// via the cold start channel so they get full requeue/retry semantics — the same
// processing path as live change stream events.
func (r *FaultRemediationReconciler) HandleColdStart(ctx context.Context) {
	slog.Info("Handling cold start: checking for unremediated events")

	q := query.New().Build(
		query.Or(
			// Quarantined + drained but not yet remediated
			query.And(
				query.In("healtheventstatus.nodequarantined",
					[]interface{}{string(model.Quarantined), string(model.AlreadyQuarantined)}),
				query.In("healtheventstatus.userpodsevictionstatus.status",
					[]interface{}{string(model.StatusSucceeded), string(model.AlreadyDrained)}),
				query.Eq("healtheventstatus.faultremediated", nil),
			),
			// Cancelled/unquarantined events (need cleanup)
			query.In("healtheventstatus.nodequarantined",
				[]interface{}{string(model.UnQuarantined), string(model.Cancelled)}),
		),
	)

	enqueued := 0

	err := r.healthEventStore.FindHealthEventsByQueryBatched(ctx, q, coldStartBatchSize,
		func(batch []datastore.HealthEventWithStatus) error {
			for _, he := range batch {
				if len(he.RawEvent) == 0 {
					continue
				}

				evt := datastore.EventWithToken{Event: he.RawEvent}

				select {
				case r.coldStartCh <- event.TypedGenericEvent[*datastore.EventWithToken]{Object: &evt}:
					enqueued++
				case <-ctx.Done():
					return ctx.Err()
				}
			}

			return nil
		})
	if err != nil {
		slog.Error("Cold start query failed", "error", err)
		return
	}

	slog.Info("Cold start: enqueued events for processing", "count", enqueued)
}

// AdaptEvents transforms a channel of EventWithToken into a channel of controller-runtime
// TypedGenericEvent. It spawns a goroutine that continuously reads from the input channel
// until either the context is cancelled or the input channel is closed.
// The returned done channel is closed when the adapter goroutine exits. If the input channel
// closed while the context was still active, this indicates the change stream died unexpectedly.
func AdaptEvents(
	ctx context.Context,
	in <-chan datastore.EventWithToken,
) (<-chan event.TypedGenericEvent[*datastore.EventWithToken], <-chan struct{}) {
	out := make(chan event.TypedGenericEvent[*datastore.EventWithToken])
	done := make(chan struct{})

	go func() {
		defer close(out)
		defer close(done)

		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-in:
				if !ok {
					return
				}

				eventOut := e
				out <- event.TypedGenericEvent[*datastore.EventWithToken]{Object: &eventOut}
			}
		}
	}()

	return out, done
}
