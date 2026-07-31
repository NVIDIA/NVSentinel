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
	oteltrace "go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	fqcommon "github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	fqannotation "github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/annotation"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/crstatus"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/events"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/metrics"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/remediation"
	nvstoreclient "github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	coldStartBatchSize = 1000

	// defaultInProgressRequeueDelay is how often an event held behind an equivalent
	// in-progress maintenance CR is re-evaluated for a terminal CR state.
	defaultInProgressRequeueDelay = 30 * time.Second
)

type ReconcilerConfig struct {
	DataStoreConfig    datastore.DataStoreConfig
	TokenConfig        nvstoreclient.TokenConfig
	Pipeline           datastore.Pipeline
	RemediationClient  remediation.FaultRemediationClientInterface
	StateManager       statemanager.StateManager
	NodeReader         client.Reader
	EnableLogCollector bool
	UpdateMaxRetries   int
	UpdateRetryDelay   time.Duration
	StartFresh         bool
	ColdStartAfterTime time.Time
	// InProgressRequeueDelay overrides how often events held behind an in-progress
	// maintenance CR are re-evaluated. Zero uses defaultInProgressRequeueDelay.
	InProgressRequeueDelay time.Duration
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
		return r.handleCancellationEvent(ctx, nodeName, model.Status(nodeQuarantined), r.Watcher, *event, r.healthEventStore)
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
	healthEventWithStatus model.HealthEventWithStatus,
	groupConfig *common.EquivalenceGroupConfig,
) (bool, error) {
	action := healthEventWithStatus.HealthEvent.RecommendedAction
	nodeName := healthEventWithStatus.HealthEvent.NodeName

	span := tracing.SpanFromContext(ctx)

	if action == protos.RecommendedAction_NONE {
		slog.InfoContext(ctx, "Skipping event for node: recommended action is NONE (no remediation needed)",
			"node", nodeName)

		span.SetAttributes(
			attribute.String("fault_remediation.skip_reason", "recommended_action_none"),
		)

		return true, nil
	}

	if healthEventWithStatus.HealthEventStatus != nil && healthEventWithStatus.HealthEventStatus.FaultRemediated != nil &&
		healthEventWithStatus.HealthEventStatus.FaultRemediated.GetValue() {
		span.SetAttributes(
			attribute.String("fault_remediation.skip_reason", "already_remediated"),
		)

		return true, nil
	}

	if groupConfig != nil {
		return false, nil
	}

	// Unsupported action detected
	actionName := model.GetEffectiveActionName(healthEventWithStatus.HealthEvent)
	slog.Info("Unsupported recommended action for node",
		"action", actionName,
		"node", nodeName)

	span.SetAttributes(
		attribute.String("fault_remediation.skip_reason", "unsupported_action"),
	)

	activeQuarantine, err := r.nodeHasActiveQuarantine(ctx, healthEventWithStatus.HealthEvent)
	if err != nil {
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "quarantine_state_check_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)

		return true, err
	}

	metrics.TotalUnsupportedRemediationActions.WithLabelValues(actionName, nodeName).Inc()

	if !activeQuarantine {
		slog.InfoContext(ctx, "Skipping remediation-failed label for node without an active quarantine",
			"action", actionName,
			"node", nodeName)
		span.SetAttributes(
			attribute.String("fault_remediation.skip_reason", "unsupported_action_stale_event"),
		)

		return true, nil
	}

	nodeModified, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		healthEventWithStatus.HealthEvent.NodeName,
		statemanager.RemediationFailedLabelValue, false)
	if err != nil {
		slog.ErrorContext(ctx, "Error updating node label",
			"label", statemanager.RemediationFailedLabelValue,
			"nodeModified", nodeModified,
			"error", err)
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "label_update_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)
		metrics.ProcessingErrors.WithLabelValues("label_update_error",
			healthEventWithStatus.HealthEvent.NodeName).Inc()

		if !nodeModified {
			return true, fmt.Errorf("failed to label unsupported remediation action for node %s: %w", nodeName, err)
		}
	}

	return true, nil
}

// nodeHasActiveQuarantine checks the current node rather than trusting persisted event status.
// It requires the same failure to remain in the live quarantine annotation so an event from an
// older quarantine session cannot affect a newer, unrelated session.
func (r *FaultRemediationReconciler) nodeHasActiveQuarantine(
	ctx context.Context,
	healthEvent *protos.HealthEvent,
) (bool, error) {
	if healthEvent == nil {
		return false, fmt.Errorf("health event is nil")
	}

	node, err := r.readLiveNode(ctx, healthEvent.NodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("failed to get live node state for %s: %w", healthEvent.NodeName, err)
	}

	active, err := activeQuarantineContainsEvent(node.GetAnnotations(), healthEvent)
	if err != nil {
		return false, fmt.Errorf(
			"failed to read active quarantine events for node %s: %w", healthEvent.NodeName, err)
	}

	return active, nil
}

// readLiveNode uses the API reader to bypass the informer cache.
func (r *FaultRemediationReconciler) readLiveNode(ctx context.Context, nodeName string) (*corev1.Node, error) {
	node := &corev1.Node{}
	if err := r.Config.NodeReader.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return nil, fmt.Errorf("failed to read node %s from the API: %w", nodeName, err)
	}

	return node, nil
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

// handleCancellationEvent handles node unquarantine and cancellation events by clearing
// annotations and writing a completion marker so the event is excluded from cold start
// queries on future restarts.
func (r *FaultRemediationReconciler) handleCancellationEvent(
	ctx context.Context,
	nodeName string,
	status model.Status,
	watcherInstance datastore.ChangeStreamWatcher,
	eventWithToken datastore.EventWithToken,
	healthEventStore datastore.HealthEventStore,
) (ctrl.Result, error) {
	ctx, span := tracing.StartSpan(ctx, "fault_remediation.cancellation_event")
	defer span.End()

	slog.InfoContext(ctx, "Cancellation event received, clearing all remediation state",
		"node", nodeName,
		"status", status)

	remediationState, _, err := r.annotationManager.GetRemediationState(ctx, nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			slog.WarnContext(ctx, "Node no longer exists, marking cancellation event as terminal", "node", nodeName)

			return r.markEventTerminalAndProcessed(ctx, healthEventStore, eventWithToken, watcherInstance, nodeName, true)
		}

		slog.ErrorContext(ctx, "Failed to get remediation state for node",
			"node", nodeName,
			"error", err)
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "get_remediation_state_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)

		return ctrl.Result{}, fmt.Errorf("failed to get remediation state for node: %w", err)
	}

	if err := r.closeStaleEquivalentEvents(ctx, nodeName, status, remediationState, healthEventStore); err != nil {
		return ctrl.Result{}, err
	}

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

	if err := r.clearFaultRemediationNodeLabel(ctx, nodeName); err != nil {
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "clear_remediation_label_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)

		return ctrl.Result{}, err
	}

	if err := r.updateNodeRemediatedStatus(ctx, healthEventStore, eventWithToken, true); err != nil {
		slog.ErrorContext(ctx, "Failed to write completion marker for cancellation event",
			"node", nodeName,
			"error", err)
		tracing.RecordError(span, err)

		return ctrl.Result{}, fmt.Errorf("failed to write completion marker for cancellation event: %w", err)
	}

	if err := safeMarkProcessed(ctx, watcherInstance, eventWithToken.ResumeToken, nodeName); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// clearFaultRemediationNodeLabel removes terminal or in-progress labels owned by this controller.
// It deliberately leaves quarantine and drain labels alone because those belong to other
// controllers and may represent a newer quarantine session.
func (r *FaultRemediationReconciler) clearFaultRemediationNodeLabel(
	ctx context.Context,
	nodeName string,
) error {
	_, err := r.Config.StateManager.RemoveNVSentinelStateNodeLabelIfMatch(
		ctx,
		nodeName,
		statemanager.RemediatingLabelValue,
		statemanager.RemediationSucceededLabelValue,
		statemanager.RemediationFailedLabelValue,
	)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to clear fault-remediation node label",
			"node", nodeName,
			"error", err)

		return fmt.Errorf("failed to clear fault-remediation label for node %s: %w", nodeName, err)
	}

	return nil
}

func (r *FaultRemediationReconciler) handlePartialRecoveryEvent(
	ctx context.Context,
	nodeName string,
	watcherInstance datastore.ChangeStreamWatcher,
	eventWithToken datastore.EventWithToken,
	healthEventStore datastore.HealthEventStore,
) (ctrl.Result, error) {
	ctx, span := tracing.StartSpan(ctx, "fault_remediation.partial_recovery_event")
	defer span.End()

	remediationState, node, err := r.annotationManager.GetRemediationState(ctx, nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			slog.WarnContext(ctx, "Node no longer exists, marking partial recovery event as terminal", "node", nodeName)

			return r.markEventTerminalAndProcessed(ctx, healthEventStore, eventWithToken, watcherInstance, nodeName, true)
		}

		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "get_remediation_state_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)

		return ctrl.Result{}, fmt.Errorf("failed to get remediation state for partial recovery: %w", err)
	}

	// Only terminal remediation labels can be left stale by a partial recovery, so recompute
	// those from the remaining active failures. Non-terminal (remediating) and pre-remediation
	// states are owned by the in-progress flow, so we just finalize the event.
	if isTerminalRemediationLabel(currentNodeStateLabel(node)) {
		if err := r.reconcilePartialRecoveryLabel(ctx, nodeName, node, remediationState, healthEventStore); err != nil {
			tracing.RecordError(span, err)

			return ctrl.Result{}, err
		}
	}

	if err := r.updateNodeRemediatedStatus(ctx, healthEventStore, eventWithToken, true); err != nil {
		tracing.RecordError(span, err)

		return ctrl.Result{}, fmt.Errorf("failed to write completion marker for partial recovery event: %w", err)
	}

	if err := safeMarkProcessed(ctx, watcherInstance, eventWithToken.ResumeToken, nodeName); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// currentNodeStateLabel returns the nvsentinel-state label value on the node, or "" if absent.
func currentNodeStateLabel(node *corev1.Node) string {
	if node == nil {
		return ""
	}

	return node.Labels[statemanager.NVSentinelStateLabelKey]
}

// isTerminalRemediationLabel reports whether the label is one fault-remediation writes as a
// terminal outcome. Either can be invalidated by a partial recovery and must be recomputed.
func isTerminalRemediationLabel(label string) bool {
	return label == string(statemanager.RemediationFailedLabelValue) ||
		label == string(statemanager.RemediationSucceededLabelValue)
}

// reconcilePartialRecoveryLabel recomputes the node state label from the remaining active
// quarantine events and applies it. It is only invoked when the node currently carries the
// stale remediation-failed label.
func (r *FaultRemediationReconciler) reconcilePartialRecoveryLabel(
	ctx context.Context,
	nodeName string,
	node *corev1.Node,
	remediationState *annotation.RemediationStateAnnotation,
	healthEventStore datastore.HealthEventStore,
) error {
	span := tracing.SpanFromContext(ctx)

	var annotations map[string]string
	if node != nil {
		annotations = node.GetAnnotations()
	}

	targetLabel, shouldUpdate, err := r.recomputePartialRecoveryNodeLabel(
		ctx, nodeName, annotations, remediationState, healthEventStore)
	if err != nil {
		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "partial_recovery_recompute_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)

		return fmt.Errorf("failed to recompute partial recovery label for node %s: %w", nodeName, err)
	}

	if !shouldUpdate {
		return nil
	}

	nodeModified, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx, nodeName, targetLabel, false)
	if err != nil {
		slog.WarnContext(ctx, "Partial recovery label recompute reported an error",
			"node", nodeName,
			"targetLabel", targetLabel,
			"nodeModified", nodeModified,
			"error", err)

		if !nodeModified {
			span.SetAttributes(
				attribute.String("fault_remediation.error.type", "partial_recovery_label_update_error"),
				attribute.String("fault_remediation.error.message", err.Error()),
			)

			return fmt.Errorf("failed to update partial recovery label: %w", err)
		}
	}

	return nil
}

func (r *FaultRemediationReconciler) recomputePartialRecoveryNodeLabel(
	ctx context.Context,
	nodeName string,
	annotations map[string]string,
	remediationState *annotation.RemediationStateAnnotation,
	healthEventStore datastore.HealthEventStore,
) (statemanager.NVSentinelStateLabelValue, bool, error) {
	activeEvents, err := activeQuarantineEvents(annotations)
	if err != nil {
		return "", false, err
	}

	if len(activeEvents) == 0 {
		return "", false, nil
	}

	sawRemediationSuccess := false

	for _, activeEvent := range activeEvents {
		failed, err := r.activeEventForcesRemediationFailed(
			ctx, nodeName, activeEvent, remediationState, healthEventStore, &sawRemediationSuccess)
		if err != nil {
			return "", false, err
		}

		if failed {
			return statemanager.RemediationFailedLabelValue, true, nil
		}
	}

	if sawRemediationSuccess {
		return statemanager.RemediationSucceededLabelValue, true, nil
	}

	// A remaining failure is supported but has no remediation outcome yet and no maintenance CR
	// covering it, so it has not been skipped and the normal remediation flow will create a CR and
	// set the correct label. Leave the current label untouched.
	return "", false, nil
}

// activeEventForcesRemediationFailed reports whether a still-active quarantine event requires the
// terminal remediation-failed label: either its action is unsupported, or its remediation was
// recorded as failed. Otherwise it records, via sawRemediationSuccess, whether the event has been
// remediated.
//
// Success is taken from the event's own FaultRemediated=true status when present. When it is nil,
// the event may still be waiting behind an equivalent maintenance CR (handleExistingCRInProgress
// requeues the event without writing FaultRemediated until the CR reaches a terminal state, so the
// flag can stay nil). In that case we consult the actual covering-CR status: an in-progress or
// succeeded CR means the node is being/has been remediated, matching the normal flow that sets
// remediation-succeeded on CR creation.
func (r *FaultRemediationReconciler) activeEventForcesRemediationFailed(
	ctx context.Context,
	nodeName string,
	activeEvent *protos.HealthEvent,
	remediationState *annotation.RemediationStateAnnotation,
	healthEventStore datastore.HealthEventStore,
	sawRemediationSuccess *bool,
) (bool, error) {
	status, err := findHealthEventStatusByID(ctx, healthEventStore, nodeName, activeEvent.Id)
	if err != nil {
		return false, fmt.Errorf("failed to evaluate active event %s: %w", activeEvent.Id, err)
	}

	groupConfig, supported := r.partialRecoveryGroupConfig(activeEvent)
	if !supported {
		return true, nil
	}

	if status != nil && status.FaultRemediated != nil {
		if !*status.FaultRemediated {
			return true, nil
		}

		*sawRemediationSuccess = true

		return false, nil
	}

	if r.coveredByActiveRemediationCR(ctx, groupConfig, remediationState) {
		*sawRemediationSuccess = true
	}

	return false, nil
}

// partialRecoveryGroupConfig returns the event's remediation group config and whether the action
// is supported. A nil config with a non-NONE action means the action is unsupported.
func (r *FaultRemediationReconciler) partialRecoveryGroupConfig(
	healthEvent *protos.HealthEvent,
) (*common.EquivalenceGroupConfig, bool) {
	groupConfig, lookupErr := common.GetGroupConfigForEvent(
		r.Config.RemediationClient.GetConfig().RemediationActions, healthEvent)
	if lookupErr != nil || unsupportedRemediationAction(healthEvent, groupConfig) {
		return nil, false
	}

	return groupConfig, true
}

// coveredByActiveRemediationCR reports whether an equivalent maintenance CR that is in progress or
// succeeded covers the event's remediation group. It reads the live CR status (not just annotation
// membership), so a recorded CR that has since failed or been deleted does not count as coverage.
func (r *FaultRemediationReconciler) coveredByActiveRemediationCR(
	ctx context.Context,
	groupConfig *common.EquivalenceGroupConfig,
	remediationState *annotation.RemediationStateAnnotation,
) bool {
	if groupConfig == nil || remediationState == nil {
		return false
	}

	statusChecker := r.Config.RemediationClient.GetStatusChecker()
	if statusChecker == nil {
		return false
	}

	for _, groupState := range common.FilterEquivalenceGroupStates(groupConfig, remediationState) {
		state := statusChecker.GetCRState(ctx, groupState.ActionName, groupState.MaintenanceCR)
		if state == crstatus.CRStateInProgress || state == crstatus.CRStateSucceeded {
			return true
		}
	}

	return false
}

func activeQuarantineEvents(annotations map[string]string) ([]*protos.HealthEvent, error) {
	if annotations == nil {
		return nil, nil
	}

	annotationValue := annotations[fqcommon.QuarantineHealthEventAnnotationKey]
	if annotationValue == "" {
		return nil, nil
	}

	healthEventsMap := fqannotation.NewHealthEventsAnnotationMap()
	if err := json.Unmarshal([]byte(annotationValue), healthEventsMap); err != nil {
		var singleEvent protos.HealthEvent
		if err2 := json.Unmarshal([]byte(annotationValue), &singleEvent); err2 != nil {
			return nil, fmt.Errorf("failed to parse quarantine annotation: %w", err)
		}

		return []*protos.HealthEvent{&singleEvent}, nil
	}

	seen := make(map[string]struct{}, len(healthEventsMap.Events))
	activeEvents := make([]*protos.HealthEvent, 0, len(healthEventsMap.Events))

	for key, event := range healthEventsMap.Events {
		if event == nil {
			continue
		}

		dedupeKey := event.Id
		if dedupeKey == "" {
			dedupeKey = fmt.Sprintf("%s/%s/%s/%s/%s/%d",
				key.Agent, key.ComponentClass, key.CheckName, key.EntityType, key.EntityValue, key.Version)
		}

		if _, ok := seen[dedupeKey]; ok {
			continue
		}

		seen[dedupeKey] = struct{}{}

		activeEvents = append(activeEvents, event)
	}

	return activeEvents, nil
}

// activeQuarantineContainsEvent reports whether the incoming event belongs to the current
// quarantine session. Event IDs distinguish repeated failures across sessions when available;
// legacy records without IDs fall back to fault-quarantine's normal event-key matching.
func activeQuarantineContainsEvent(
	annotations map[string]string,
	incomingEvent *protos.HealthEvent,
) (bool, error) {
	activeEvents, err := activeQuarantineEvents(annotations)
	if err != nil {
		return false, err
	}

	for _, activeEvent := range activeEvents {
		if activeEvent.Id != "" && incomingEvent.Id != "" {
			if activeEvent.Id == incomingEvent.Id {
				return true, nil
			}

			continue
		}

		activeEventMap := fqannotation.NewHealthEventsAnnotationMap()
		activeEventMap.AddOrUpdateEvent(activeEvent)

		if _, matches := activeEventMap.GetEvent(incomingEvent); matches {
			return true, nil
		}
	}

	return false, nil
}

// unsupportedRemediationAction reports whether an active event needs remediation but has no
// configured action. A nil groupConfig alone is ambiguous: GetGroupConfigForEvent also returns
// nil for RecommendedAction_NONE, which means "no remediation needed" rather than "unsupported".
// The NONE guard keeps those events from being misclassified as failures.
func unsupportedRemediationAction(
	healthEvent *protos.HealthEvent,
	groupConfig *common.EquivalenceGroupConfig,
) bool {
	return healthEvent.RecommendedAction != protos.RecommendedAction_NONE && groupConfig == nil
}

func isPartialRecoveryRemediationEvent(healthEventWithStatus model.HealthEventWithStatus) bool {
	if healthEventWithStatus.HealthEvent == nil || healthEventWithStatus.HealthEventStatus == nil {
		return false
	}

	status := healthEventWithStatus.HealthEventStatus.NodeQuarantined

	// A healthy event that leaves the node quarantined is a partial recovery: the recovered
	// failure cleared while other active failures keep the node quarantined. IsHealthy is the
	// discriminator from a normal (unhealthy) remediation event — a healthy event never needs
	// remediation regardless of its RecommendedAction, so we do not gate on the action.
	return healthEventWithStatus.HealthEvent.IsHealthy &&
		(status == string(model.Quarantined) || status == string(model.AlreadyQuarantined))
}

func findHealthEventStatusByID(
	ctx context.Context,
	healthEventStore datastore.HealthEventStore,
	nodeName string,
	eventID string,
) (*datastore.HealthEventStatus, error) {
	if healthEventStore == nil || eventID == "" {
		return nil, nil
	}

	q := query.New().Build(query.Eq("_id", eventID))

	events, err := healthEventStore.FindHealthEventsByQuery(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("failed to query active health event %s for node %s: %w", eventID, nodeName, err)
	}

	if len(events) == 0 {
		return nil, nil
	}

	if len(events) > 1 {
		return nil, fmt.Errorf("unexpected number of events for node %s and event ID %s: %d",
			nodeName, eventID, len(events))
	}

	return &events[0].HealthEventStatus, nil
}

// eventResolvedInStore reports whether the event no longer needs remediation according to
// its current datastore record:
//   - faultremediated=true: the event was remediated elsewhere.
//   - The record's quarantine status is UnQuarantined/Cancelled: the session ended and the
//     cancellation flow owns the event's terminal status.
//   - faultremediated=false while the event is no longer part of the node's live quarantine
//     annotation: a durable terminal record written by cancellation cleanup
//     (closeStaleEquivalentEvents). With a still-active quarantine, false only means the
//     last remediation attempt failed and the event must remain retryable.
//
// A missing record is treated as unresolved so the normal flow decides what to do with it.
func (r *FaultRemediationReconciler) eventResolvedInStore(
	ctx context.Context,
	healthEventStore datastore.HealthEventStore,
	healthEvent *protos.HealthEvent,
	eventID string,
) (bool, error) {
	status, err := findHealthEventStatusByID(ctx, healthEventStore, healthEvent.NodeName, eventID)
	if err != nil {
		return false, err
	}

	if status == nil {
		return false, nil
	}

	if status.FaultRemediated != nil && *status.FaultRemediated {
		return true, nil
	}

	if status.NodeQuarantined != nil {
		quarantined := string(*status.NodeQuarantined)
		if quarantined == string(model.UnQuarantined) || quarantined == string(model.Cancelled) {
			return true, nil
		}
	}

	if status.FaultRemediated != nil {
		activeQuarantine, err := r.nodeHasActiveQuarantine(ctx, healthEvent)
		if err != nil {
			return false, err
		}

		return !activeQuarantine, nil
	}

	return false, nil
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

	if isPartialRecoveryRemediationEvent(healthEventWithStatus.HealthEventWithStatus) {
		return r.handlePartialRecoveryEvent(ctx, nodeName, watcherInstance, eventWithToken, healthEventStore)
	}

	groupConfig, err := common.GetGroupConfigForEvent(r.Config.RemediationClient.GetConfig().RemediationActions,
		healthEvent)
	if err != nil {
		// If we got an error, groupConfig will be nil which will result in shouldSkipEvent setting state label to
		// remediation-failed
		slog.ErrorContext(ctx, "Got an error getting group config for event, skipping event and failing remediation",
			"error", err, "event", healthEventWithStatus.ID)
	}

	res, err, done := r.trySkipEvent(ctx, healthEventWithStatus, groupConfig, eventWithToken, watcherInstance,
		healthEventStore, nodeName)
	if done {
		span.SetAttributes(
			attribute.String("fault_remediation.status", "skipped"),
		)

		return res, err
	}

	// The event carries the status snapshot captured by the change stream (or cold start).
	// By the time it is acted on — especially after waiting in the workqueue behind an
	// in-progress CR — another actor may have closed it: a cancellation closes covered
	// events via closeStaleEquivalentEvents, or the quarantine session may have ended.
	// Re-read the current status so a stale snapshot cannot start a remediation for a
	// session that is already over.
	res, err, done = r.trySkipResolvedEvent(ctx, healthEventStore, eventWithToken, watcherInstance,
		healthEvent, healthEventWithStatus.ID)
	if done {
		return res, err
	}

	decision, err := r.checkExistingCRStatus(ctx, healthEvent,
		healthEventWithStatus.CreatedAt, groupConfig)
	if err != nil {
		if apierrors.IsNotFound(err) {
			slog.WarnContext(ctx, "Node no longer exists, marking remediation event as stale", "node", nodeName)

			return r.markEventTerminalAndProcessed(ctx, healthEventStore, eventWithToken, watcherInstance, nodeName, false)
		}

		metrics.ProcessingErrors.WithLabelValues("cr_status_check_error", nodeName).Inc()
		slog.ErrorContext(ctx, "Error checking existing CR status", "node", nodeName, "error", err)

		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "cr_status_check_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)
		tracing.RecordError(span, err)

		return ctrl.Result{}, fmt.Errorf("error checking existing CR status: %w", err)
	}

	if !decision.shouldCreate {
		return r.handleEventCoveredByExistingCR(ctx, decision, eventWithToken, watcherInstance, healthEventStore,
			nodeName)
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
	healthEventStore datastore.HealthEventStore,
	nodeName string,
) (ctrl.Result, error, bool) {
	shouldSkip, err := r.shouldSkipEvent(ctx, healthEventWithStatus.HealthEventWithStatus, groupConfig)
	if err != nil {
		return ctrl.Result{}, err, true
	}

	if !shouldSkip {
		return ctrl.Result{}, nil, false
	}

	ctx, skipSpan := tracing.StartSpan(ctx, "fault_remediation.skip_event")
	defer skipSpan.End()

	if shouldMarkSkippedEventUnsupported(healthEventWithStatus.HealthEventWithStatus, groupConfig) {
		if err := r.updateNodeRemediatedStatus(ctx, healthEventStore, eventWithToken, false); err != nil {
			return ctrl.Result{}, err, true
		}
	}

	if err := safeMarkProcessed(ctx, watcherInstance, eventWithToken.ResumeToken, nodeName); err != nil {
		return ctrl.Result{}, err, true
	}

	return ctrl.Result{}, nil, true
}

// trySkipResolvedEvent returns (result, err, true) when the event's current datastore
// record shows it no longer needs remediation; otherwise (zero, nil, false).
func (r *FaultRemediationReconciler) trySkipResolvedEvent(
	ctx context.Context,
	healthEventStore datastore.HealthEventStore,
	eventWithToken datastore.EventWithToken,
	watcherInstance datastore.ChangeStreamWatcher,
	healthEvent *protos.HealthEvent,
	eventID string,
) (ctrl.Result, error, bool) {
	span := tracing.SpanFromContext(ctx)
	nodeName := healthEvent.NodeName

	resolved, err := r.eventResolvedInStore(ctx, healthEventStore, healthEvent, eventID)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("status_recheck_error", nodeName).Inc()
		slog.ErrorContext(ctx, "Error re-reading event status", "node", nodeName, "error", err)

		span.SetAttributes(
			attribute.String("fault_remediation.error.type", "status_recheck_error"),
			attribute.String("fault_remediation.error.message", err.Error()),
		)
		tracing.RecordError(span, err)

		return ctrl.Result{}, fmt.Errorf("error re-reading event status: %w", err), true
	}

	if !resolved {
		return ctrl.Result{}, nil, false
	}

	slog.InfoContext(ctx, "Event already resolved in the datastore, skipping",
		"node", nodeName,
		"eventID", eventID)

	span.SetAttributes(
		attribute.String("fault_remediation.skip_reason", "resolved_in_datastore"),
	)

	metrics.EventsProcessed.WithLabelValues(metrics.CRStatusSkipped, nodeName).Inc()

	result, err := r.markProcessedOrError(ctx, watcherInstance, eventWithToken, nodeName)

	return result, err, true
}

// handleEventCoveredByExistingCR routes a shouldCreate=false decision: an event behind a
// still-in-progress CR is requeued, an event covered by a terminal CR is finalized.
func (r *FaultRemediationReconciler) handleEventCoveredByExistingCR(
	ctx context.Context,
	decision existingCRDecision,
	eventWithToken datastore.EventWithToken,
	watcherInstance datastore.ChangeStreamWatcher,
	healthEventStore datastore.HealthEventStore,
	nodeName string,
) (ctrl.Result, error) {
	if decision.inProgress {
		return r.handleExistingCRInProgress(ctx, nodeName, decision.crName)
	}

	return r.handleExistingCRSkip(ctx, eventWithToken, watcherInstance, healthEventStore, nodeName, decision.crName,
		decision.remediated)
}

func shouldMarkSkippedEventUnsupported(healthEventWithStatus model.HealthEventWithStatus,
	groupConfig *common.EquivalenceGroupConfig) bool {
	if healthEventWithStatus.HealthEvent == nil || groupConfig != nil {
		return false
	}

	if healthEventWithStatus.HealthEvent.RecommendedAction == protos.RecommendedAction_NONE {
		return false
	}

	if healthEventWithStatus.HealthEventStatus != nil && healthEventWithStatus.HealthEventStatus.FaultRemediated != nil &&
		healthEventWithStatus.HealthEventStatus.FaultRemediated.GetValue() {
		return false
	}

	return true
}

// closeStaleEquivalentEvents closes unresolved remediation-ready events that were
// covered by the remediation groups recorded on the node annotation.
//
// This runs when FQ sends UnQuarantined/Cancelled. At that point the quarantine
// session is over, so events waiting behind an equivalent in-progress CR must
// become durable terminal records; otherwise FR cold start can replay them and
// create duplicate maintenance CRs after the node is schedulable again. Live
// requeued events observe these terminal records through the pre-remediation
// datastore re-check (eventResolvedInStore) and stop requeueing.
//
// The cleanup is equivalence-group scoped. It only closes events whose computed
// remediation group, or a configured superseding group, matches a group from the
// remediation annotation snapshot.
func (r *FaultRemediationReconciler) closeStaleEquivalentEvents(
	ctx context.Context,
	nodeName string,
	status model.Status,
	remediationState *annotation.RemediationStateAnnotation,
	healthEventStore datastore.HealthEventStore,
) error {
	if healthEventStore == nil || remediationState == nil || len(remediationState.EquivalenceGroups) == 0 {
		return nil
	}

	coveredGroups := make(map[string]struct{}, len(remediationState.EquivalenceGroups))
	for groupName := range remediationState.EquivalenceGroups {
		coveredGroups[groupName] = struct{}{}
	}

	remediated := status == model.UnQuarantined
	q := unresolvedRemediationReadyEventsQuery(nodeName)

	now := time.Now().UTC()

	closeBatch := func(batch []datastore.HealthEventWithStatus) error {
		return r.closeStaleEquivalentEventBatch(ctx, healthEventStore, nodeName, coveredGroups, remediated, now, batch)
	}

	return healthEventStore.FindHealthEventsByQueryBatched(ctx, q, coldStartBatchSize, closeBatch)
}

// closeStaleEquivalentEventBatch evaluates one datastore batch from
// closeStaleEquivalentEvents and writes the terminal remediation status for
// matching events.
func (r *FaultRemediationReconciler) closeStaleEquivalentEventBatch(
	ctx context.Context,
	healthEventStore datastore.HealthEventStore,
	nodeName string,
	coveredGroups map[string]struct{},
	remediated bool,
	now time.Time,
	batch []datastore.HealthEventWithStatus,
) error {
	for _, event := range batch {
		parsedEvent, err := eventutil.ParseHealthEventFromEvent(event.RawEvent)
		if err != nil {
			slog.WarnContext(ctx, "Skipping stale event cleanup for unparsable health event",
				"node", nodeName,
				"error", err)

			continue
		}

		groupConfig, err := common.GetGroupConfigForEvent(
			r.Config.RemediationClient.GetConfig().RemediationActions,
			parsedEvent.HealthEvent,
		)
		if err != nil || groupConfig == nil || !groupConfigMatchesAny(groupConfig, coveredGroups) {
			continue
		}

		documentID, err := utils.ExtractDocumentID(event.RawEvent)
		if err != nil {
			slog.WarnContext(ctx, "Skipping stale event cleanup for health event without document ID",
				"node", nodeName,
				"error", err)

			continue
		}

		statusUpdate := datastore.HealthEventStatus{
			FaultRemediated: &remediated,
		}
		if remediated {
			statusUpdate.LastRemediationTimestamp = timestamppb.New(now)
		}

		if err := healthEventStore.UpdateHealthEventStatus(ctx, documentID, statusUpdate); err != nil {
			return fmt.Errorf("failed to close stale equivalent event %s for node %s: %w",
				documentID, nodeName, err)
		}

		slog.InfoContext(ctx, "Closed stale equivalent remediation event",
			"node", nodeName,
			"eventID", documentID,
			"remediated", remediated)
	}

	return nil
}

// groupConfigMatchesAny reports whether the event's effective remediation group
// is covered by one of the groups already remediated for this quarantine session.
// Superseding groups count as coverage; for example, a node restart can cover a
// GPU reset event when reset declares restart as a superseding equivalence group.
func groupConfigMatchesAny(groupConfig *common.EquivalenceGroupConfig, groups map[string]struct{}) bool {
	if _, ok := groups[groupConfig.EffectiveEquivalenceGroup]; ok {
		return true
	}

	for _, groupName := range groupConfig.SupersedingEquivalenceGroups {
		if _, ok := groups[groupName]; ok {
			return true
		}
	}

	return false
}

// unresolvedRemediationReadyEventsQuery returns the cold-start-style query for
// events that are drained/quarantined and still lack a fault-remediation terminal
// status. When nodeName is non-empty, it scopes the query to that node.
func unresolvedRemediationReadyEventsQuery(nodeName string) datastore.QueryBuilder {
	return query.New().Build(unresolvedRemediationReadyEventsCondition(nodeName))
}

// unresolvedRemediationReadyEventsCondition is the reusable condition form of
// unresolvedRemediationReadyEventsQuery, used when composing larger queries.
func unresolvedRemediationReadyEventsCondition(nodeName string) query.Condition {
	conditions := []query.Condition{
		query.In("healtheventstatus.nodequarantined",
			[]interface{}{string(model.Quarantined), string(model.AlreadyQuarantined)}),
		query.In("healtheventstatus.userpodsevictionstatus.status",
			[]interface{}{string(model.StatusSucceeded), string(model.AlreadyDrained)}),
		query.Eq("healtheventstatus.faultremediated", nil),
	}

	if nodeName != "" {
		conditions = append([]query.Condition{query.Eq("healthevent.nodename", nodeName)}, conditions...)
	}

	return query.And(conditions...)
}

// handleExistingCRSkip finalizes an event covered by an equivalent maintenance CR that
// reached a terminal state: logs, records metrics, writes the remediated status when the
// completed CR covers the event, marks the event processed, and returns.
func (r *FaultRemediationReconciler) handleExistingCRSkip(
	ctx context.Context,
	eventWithToken datastore.EventWithToken,
	watcherInstance datastore.ChangeStreamWatcher,
	healthEventStore datastore.HealthEventStore,
	nodeName, existingCR string,
	existingCRRemediated bool,
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

	if existingCRRemediated {
		if err := r.updateNodeRemediatedStatus(ctx, healthEventStore, eventWithToken, true); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := safeMarkProcessed(ctx, watcherInstance, eventWithToken.ResumeToken, nodeName); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// handleExistingCRInProgress requeues an event whose equivalence group is covered by a
// maintenance CR that has not reached a terminal state yet. The event is deliberately
// neither checkpointed nor finalized: a post-reboot event can arrive while the covering
// CR is still InProgress, and dropping it here would strand the node with an actionable
// fault but no remediation (issue #1536). Requeueing re-evaluates the CR until it becomes
// terminal; evaluateExistingCR then either marks the event remediated (event covered by
// the completed remediation session) or allows a new CR (post-session event or failed CR).
func (r *FaultRemediationReconciler) handleExistingCRInProgress(
	ctx context.Context,
	nodeName, existingCR string,
) (ctrl.Result, error) {
	requeueDelay := r.inProgressRequeueDelay()

	span := tracing.SpanFromContext(ctx)
	slog.InfoContext(ctx, "Existing CR is still in progress, requeueing event until the CR reaches a terminal state",
		"node", nodeName,
		"existingCR", existingCR,
		"requeueAfter", requeueDelay)

	span.SetAttributes(
		attribute.String("fault_remediation.skip_reason", "existing_cr_in_progress"),
		attribute.String("fault_remediation.existing_cr.name", existingCR),
	)

	metrics.EventsProcessed.WithLabelValues(metrics.CRStatusWaiting, nodeName).Inc()

	return ctrl.Result{RequeueAfter: requeueDelay}, nil
}

func (r *FaultRemediationReconciler) inProgressRequeueDelay() time.Duration {
	if r.Config.InProgressRequeueDelay > 0 {
		return r.Config.InProgressRequeueDelay
	}

	return defaultInProgressRequeueDelay
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

// markEventTerminalAndProcessed writes faultRemediated and advances the change-stream token.
// Used when the target Node no longer exists so the event is not retried.
func (r *FaultRemediationReconciler) markEventTerminalAndProcessed(
	ctx context.Context,
	healthEventStore datastore.HealthEventStore,
	eventWithToken datastore.EventWithToken,
	watcherInstance datastore.ChangeStreamWatcher,
	nodeName string,
	remediated bool,
) (ctrl.Result, error) {
	if err := r.updateNodeRemediatedStatus(ctx, healthEventStore, eventWithToken, remediated); err != nil {
		return ctrl.Result{}, err
	}

	return r.markProcessedOrError(ctx, watcherInstance, eventWithToken, nodeName)
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
	eventCreatedAt time.Time, groupConfig *common.EquivalenceGroupConfig) (existingCRDecision, error) {
	nodeName := healthEvent.NodeName
	allowCreate := existingCRDecision{shouldCreate: true}

	if groupConfig == nil {
		return allowCreate, nil
	}

	state, _, err := r.annotationManager.GetRemediationState(ctx, nodeName)
	if err != nil {
		slog.ErrorContext(ctx, "Error getting remediation state", "node", nodeName, "error", err)
		return allowCreate, fmt.Errorf("error getting remediation state: %w", err)
	}

	if state == nil {
		slog.WarnContext(ctx, "Remediation state is nil for node, allowing CR creation",
			"node", nodeName)

		return allowCreate, nil
	}

	statusChecker := r.Config.RemediationClient.GetStatusChecker()
	if statusChecker == nil {
		slog.WarnContext(ctx, "Status checker is not available, allowing creation")
		return allowCreate, nil
	}

	groupStates := sortedEquivalenceGroupStates(common.FilterEquivalenceGroupStates(groupConfig, state))

	var groupsToRemove []string

	for _, groupState := range groupStates {
		decision := r.evaluateExistingCR(ctx, statusChecker, groupState, eventCreatedAt, nodeName)
		if !decision.shouldCreate {
			return decision, nil
		}

		if decision.removeGroup {
			groupsToRemove = append(groupsToRemove, groupState.name)
			continue
		}
	}

	if len(groupsToRemove) > 0 {
		if err := r.annotationManager.RemoveGroupsFromState(ctx, nodeName, groupsToRemove); err != nil {
			return allowCreate, fmt.Errorf("failed to remove groups from annotation: %w", err)
		}
	}

	return allowCreate, nil
}

type existingCRDecision struct {
	shouldCreate bool
	crName       string
	remediated   bool
	removeGroup  bool
	// inProgress marks a shouldCreate=false decision caused by a covering CR that has
	// not reached a terminal state yet. Such events must be retried, not finalized.
	inProgress bool
}

func (r *FaultRemediationReconciler) evaluateExistingCR(
	ctx context.Context,
	statusChecker crstatus.CRStatusCheckerInterface,
	groupState namedEquivalenceGroupState,
	eventCreatedAt time.Time,
	nodeName string,
) existingCRDecision {
	crName := groupState.state.MaintenanceCR
	crState := statusChecker.GetCRState(ctx, groupState.state.ActionName, crName)

	switch crState {
	case crstatus.CRStateInProgress:
		slog.InfoContext(ctx, "CR exists and is in progress, waiting for terminal state",
			"node", nodeName, "crName", crName)

		return existingCRDecision{shouldCreate: false, crName: crName, inProgress: true}
	case crstatus.CRStateSucceeded:
		return r.evaluateSucceededCR(ctx, groupState, eventCreatedAt, nodeName)
	case crstatus.CRStateNotFound, crstatus.CRStateFailed:
		slog.InfoContext(ctx, "CR completed or failed, allowing retry", "node", nodeName, "crName", crName)

		return existingCRDecision{shouldCreate: true, removeGroup: true}
	default:
		slog.WarnContext(ctx, "Unknown CR state, allowing retry", "node", nodeName, "crName", crName, "state", crState)

		return existingCRDecision{shouldCreate: true, removeGroup: true}
	}
}

func (r *FaultRemediationReconciler) evaluateSucceededCR(
	ctx context.Context,
	groupState namedEquivalenceGroupState,
	eventCreatedAt time.Time,
	nodeName string,
) existingCRDecision {
	crName := groupState.state.MaintenanceCR
	if eventCoveredByRemediationSession(eventCreatedAt, groupState.state.CreatedAt) {
		slog.InfoContext(ctx, "CR completed successfully, marking same-session equivalent event remediated",
			"node", nodeName, "crName", crName)

		return existingCRDecision{shouldCreate: false, crName: crName, remediated: true}
	}

	slog.InfoContext(ctx, "CR completed successfully but event is outside remediation session, allowing new CR",
		"node", nodeName, "crName", crName)

	return existingCRDecision{shouldCreate: true, removeGroup: true}
}

func eventCoveredByRemediationSession(eventCreatedAt, remediationCreatedAt time.Time) bool {
	if eventCreatedAt.IsZero() || remediationCreatedAt.IsZero() {
		return false
	}

	return !eventCreatedAt.After(remediationCreatedAt)
}

type namedEquivalenceGroupState struct {
	name  string
	state annotation.EquivalenceGroupState
}

func sortedEquivalenceGroupStates(
	groupStates map[string]annotation.EquivalenceGroupState,
) []namedEquivalenceGroupState {
	sortedStates := make([]namedEquivalenceGroupState, 0, len(groupStates))
	for groupName, groupState := range groupStates {
		sortedStates = append(sortedStates, namedEquivalenceGroupState{name: groupName, state: groupState})
	}

	sort.Slice(sortedStates, func(i, j int) bool {
		return sortedStates[i].state.CreatedAt.After(sortedStates[j].state.CreatedAt)
	})

	return sortedStates
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

		_ = safeMarkProcessed(context.Background(), watcherInstance, eventWithToken.ResumeToken, "unknown")

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
	if r.Config.StartFresh {
		slog.Info("Skipping cold start because resume-control CREATE was consumed")
		return
	}

	slog.Info("Handling cold start: checking for unremediated events")

	condition := query.Or(
		// Quarantined + drained but not yet remediated
		unresolvedRemediationReadyEventsCondition(""),
		// Cancelled/unquarantined events that haven't been marked complete
		query.And(
			query.In("healtheventstatus.nodequarantined",
				[]interface{}{string(model.UnQuarantined), string(model.Cancelled)}),
			query.Eq("healtheventstatus.faultremediated", nil),
		),
	)
	if !r.Config.ColdStartAfterTime.IsZero() {
		condition = query.And(
			query.Gt("createdAt", r.Config.ColdStartAfterTime),
			condition,
		)
	}

	q := query.New().Build(condition)

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
