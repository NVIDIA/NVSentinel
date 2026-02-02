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
	"time"

	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/annotation"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/events"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/metrics"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/remediation"
	nvstoreclient "github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
)

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
}

// NewFaultRemediationReconciler creates a new FaultRemediationReconciler with the provided dependencies.
func NewFaultRemediationReconciler(
	ds datastore.DataStore,
	watcher datastore.ChangeStreamWatcher,
	healthEventStore datastore.HealthEventStore,
	config ReconcilerConfig,
	dryRun bool,
) FaultRemediationReconciler {
	return FaultRemediationReconciler{
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
) (ctrl.Result, error) {
	start := time.Now()

	slog.Info("Reconciling Event")

	defer func() {
		metrics.EventHandlingDuration.Observe(time.Since(start).Seconds())
	}()

	metrics.TotalEventsReceived.Inc()

	healthEventWithStatus, err := r.parseHealthEvent(*event, r.Watcher)
	if err != nil {
		return ctrl.Result{}, nil
	}

	// Safety checks for nil pointers
	if healthEventWithStatus.HealthEvent == nil {
		slog.Warn("HealthEvent is nil, skipping processing")
		return ctrl.Result{}, nil
	}

	nodeName := healthEventWithStatus.HealthEvent.NodeName
	nodeQuarantined := healthEventWithStatus.HealthEventStatus.NodeQuarantined

	if nodeQuarantined != nil {
		if *nodeQuarantined == model.UnQuarantined || *nodeQuarantined == model.Cancelled {
			return r.handleCancellationEvent(ctx, nodeName, *nodeQuarantined, r.Watcher, event.ResumeToken)
		}
	}

	return r.handleRemediationEvent(ctx, &healthEventWithStatus, *event, r.Watcher, r.healthEventStore)
}

func (r *FaultRemediationReconciler) shouldSkipEvent(ctx context.Context,
	healthEventWithStatus model.HealthEventWithStatus, groupConfig *common.EquivalenceGroupConfig) bool {
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

	if groupConfig != nil {
		return false
	}

	// Unsupported action detected
	slog.Info("Unsupported recommended action for node",
		"action", action.String(),
		"node", nodeName)
	metrics.TotalUnsupportedRemediationActions.WithLabelValues(action.String(), nodeName).Inc()

	_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		healthEventWithStatus.HealthEvent.NodeName,
		statemanager.RemediationFailedLabelValue, false)
	if err != nil {
		slog.Error("Error updating node label",
			"label", statemanager.RemediationFailedLabelValue,
			"error", err)
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

	slog.Info("Log collector feature enabled; running log collector for node",
		"node", healthEvent.NodeName)

	result, err := r.Config.RemediationClient.RunLogCollectorJob(ctx, healthEvent.NodeName, eventUID)
	if err != nil {
		slog.Error("Log collector job failed to launch for node",
			"node", healthEvent.NodeName,
			"error", err)

		return ctrl.Result{}, fmt.Errorf("failed to launch log collector on node: %w", err)
	}

	return result, nil
}

// performRemediation attempts to create maintenance resource with retries
func (r *FaultRemediationReconciler) performRemediation(ctx context.Context,
	healthEventWithStatus *events.HealthEventDoc, groupConfig *common.EquivalenceGroupConfig) (string, error) {
	nodeName := healthEventWithStatus.HealthEvent.NodeName

	// Update state to "remediating"
	_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		healthEventWithStatus.HealthEvent.NodeName,
		statemanager.RemediatingLabelValue, false)
	if err != nil {
		slog.Error("Error updating node label to remediating", "error", err)
		metrics.ProcessingErrors.WithLabelValues("label_update_error", nodeName).Inc()

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

		remediationLabelValue = statemanager.RemediationFailedLabelValue
		// don't throw error yet so we can update state
	}

	_, err = r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx,
		healthEventWithStatus.HealthEvent.NodeName,
		remediationLabelValue, false)
	if err != nil {
		slog.Error("Error updating node label",
			"label", remediationLabelValue,
			"error", err)
		metrics.ProcessingErrors.WithLabelValues("label_update_error", nodeName).Inc()

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
	slog.Info("Cancellation event received, clearing all remediation state",
		"node", nodeName,
		"status", status)

	if err := r.annotationManager.ClearRemediationState(ctx, nodeName); err != nil {
		slog.Error("Failed to clear remediation state for node",
			"node", nodeName,
			"error", err)

		return ctrl.Result{}, fmt.Errorf("failed to clear remediation state for node: %w", err)
	}

	if err := watcherInstance.MarkProcessed(context.Background(), resumeToken); err != nil {
		metrics.ProcessingErrors.WithLabelValues("mark_processed_error", nodeName).Inc()
		slog.Error("Error updating resume token", "error", err)

		return ctrl.Result{}, fmt.Errorf("failed to mark event as processed: %w", err)
	}

	return ctrl.Result{}, nil
}

// handleRemediationEvent processes remediation for quarantined nodes
// nolint: cyclop // todo
func (r *FaultRemediationReconciler) handleRemediationEvent(
	ctx context.Context,
	healthEventWithStatus *events.HealthEventDoc,
	eventWithToken datastore.EventWithToken,
	watcherInstance datastore.ChangeStreamWatcher,
	healthEventStore datastore.HealthEventStore,
) (ctrl.Result, error) {
	healthEvent := healthEventWithStatus.HealthEvent
	nodeName := healthEvent.NodeName

	groupConfig, err := common.GetGroupConfigForEvent(r.Config.RemediationClient.GetConfig().RemediationActions,
		healthEvent)
	if err != nil {
		// If we got an error, groupConfig will be nil which will result in shouldSkipEvent setting state label to
		// remediation-failed
		slog.Error("Got an error getting group config for event, skipping event and failing remediation",
			"error", err, "event", healthEventWithStatus.ID)
	}

	// Check if we should skip this event (NONE actions or unsupported actions)
	if r.shouldSkipEvent(ctx, healthEventWithStatus.HealthEventWithStatus, groupConfig) {
		if err := watcherInstance.MarkProcessed(ctx, eventWithToken.ResumeToken); err != nil {
			metrics.ProcessingErrors.WithLabelValues("mark_processed_error", nodeName).Inc()
			slog.Error("Error updating resume token", "error", err)

			return ctrl.Result{}, fmt.Errorf("error updating resume token: %w", err)
		}

		return ctrl.Result{}, nil
	}

	shouldCreateCR, existingCR, err := r.checkExistingCRStatus(ctx, healthEvent, groupConfig)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("cr_status_check_error", nodeName).Inc()
		slog.Error("Error checking existing CR status", "node", nodeName, "error", err)

		return ctrl.Result{}, fmt.Errorf("error checking existing CR status: %w", err)
	}

	if !shouldCreateCR {
		slog.Info("Skipping event for node due to existing CR",
			"node", nodeName,
			"existingCR", existingCR)

		metrics.EventsProcessed.WithLabelValues(metrics.CRStatusSkipped, nodeName).Inc()

		if err = watcherInstance.MarkProcessed(ctx, eventWithToken.ResumeToken); err != nil {
			metrics.ProcessingErrors.WithLabelValues("mark_processed_error", nodeName).Inc()
			slog.Error("Error updating resume token", "error", err)

			return ctrl.Result{}, fmt.Errorf("error updating resume token: %w", err)
		}

		return ctrl.Result{}, nil
	}

	// Run log collector only when we're about to create a new CR
	// This prevents duplicate log-collector jobs when multiple events arrive for the same node
	result, err := r.runLogCollector(ctx, healthEvent, healthEventWithStatus.ID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error running log collector: %w", err)
	}

	if !result.IsZero() {
		return result, nil
	}

	_, performRemediationErr := r.performRemediation(ctx, healthEventWithStatus, groupConfig)

	nodeRemediatedStatus := performRemediationErr == nil // success if no error thrown
	if err = r.updateNodeRemediatedStatus(ctx, healthEventStore, eventWithToken, nodeRemediatedStatus); err != nil {
		metrics.ProcessingErrors.WithLabelValues("update_status_error", nodeName).Inc()
		slog.Error("Error updating remediation status for node", "error", err)

		return ctrl.Result{}, errors.Join(performRemediationErr, err)
	}

	if performRemediationErr != nil {
		return ctrl.Result{}, performRemediationErr
	}

	metrics.EventsProcessed.WithLabelValues(metrics.CRStatusCreated, nodeName).Inc()

	if err = watcherInstance.MarkProcessed(ctx, eventWithToken.ResumeToken); err != nil {
		metrics.ProcessingErrors.WithLabelValues("mark_processed_error", nodeName).Inc()
		slog.Error("Error updating resume token", "error", err)

		return ctrl.Result{}, fmt.Errorf("error updating resume token: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *FaultRemediationReconciler) updateNodeRemediatedStatus(
	ctx context.Context,
	healthEventStore datastore.HealthEventStore,
	eventWithToken datastore.EventWithToken,
	nodeRemediatedStatus bool,
) error {
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
	slog.Info("Updating health event with ID", "id", documentID)

	err = healthEventStore.UpdateHealthEventStatus(ctx, documentID, status)
	if err != nil {
		return fmt.Errorf("error updating document with ID: %v, error: %w", documentID, err)
	}

	slog.Info("Health event has been updated with status",
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
		slog.Error("Error getting remediation state", "node", nodeName, "error", err)
		return true, "", fmt.Errorf("error getting remediation state: %w", err)
	}

	if state == nil {
		slog.Warn("Remediation state is nil for node, allowing CR creation",
			"node", nodeName)

		return true, "", nil
	}

	statusChecker := r.Config.RemediationClient.GetStatusChecker()
	if statusChecker == nil {
		slog.Warn("Status checker is not available, allowing creation")
		return true, "", nil
	}

	groupStates := common.FilterEquivalenceGroupStates(groupConfig, state)

	for groupName, groupState := range groupStates {
		shouldSkip := statusChecker.ShouldSkipCRCreation(ctx, groupState.ActionName, groupState.MaintenanceCR)
		if shouldSkip {
			slog.Info("CR exists and is in progress, skipping event", "node", nodeName, "crName", groupState.MaintenanceCR)
			return false, groupState.MaintenanceCR, nil
		}

		slog.Info("CR completed or failed, allowing retry", "node", nodeName, "crName", groupState.MaintenanceCR)

		if err := r.annotationManager.RemoveGroupFromState(ctx, nodeName, groupName); err != nil {
			slog.Error("Failed to remove CR from annotation", "error", err)
		}
	}

	return true, "", nil
}

// parseHealthEvent extracts and parses health event from change stream event
// The eventWithToken.Event is already the fullDocument extracted by the store-client
func (r *FaultRemediationReconciler) parseHealthEvent(eventWithToken datastore.EventWithToken,
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
		slog.Error("Error parsing health event", "error", err)

		if markErr := watcherInstance.MarkProcessed(context.Background(), eventWithToken.ResumeToken); markErr != nil {
			metrics.ProcessingErrors.WithLabelValues("mark_processed_error", "unknown").Inc()
			slog.Error("Error updating resume token", "error", markErr)
		}

		return result, fmt.Errorf("error updating resume token: %w", err)
	}

	// Extract document ID and wrap into HealthEventDoc
	documentID, err := utils.ExtractDocumentID(eventWithToken.Event)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("extract_id_error", "unknown").Inc()
		slog.Error("Error extracting document ID", "error", err)

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
		slog.Error("failed to close datastore", "error", err)
		errs = append(errs, err)
	}

	if err := r.Watcher.Close(ctx); err != nil {
		slog.Error("failed to close Watcher", "error", err)
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// SetupWithManager configures the reconciler for controller-runtime managed operation.
// It starts the watcher stream using the provided context and registers the reconciler
// with the manager using a typed channel source. This method should only be called
// when running under controller-runtime management.
func (r *FaultRemediationReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	r.Watcher.Start(ctx)

	reconciler := builder.TypedControllerManagedBy[*datastore.EventWithToken](mgr)
	typedCh := AdaptEvents(ctx, r.Watcher.Events())

	src := source.TypedChannel[*datastore.EventWithToken, *datastore.EventWithToken](
		typedCh,
		handler.TypedFuncs[*datastore.EventWithToken, *datastore.EventWithToken]{
			GenericFunc: func(
				ctx context.Context,
				e event.TypedGenericEvent[*datastore.EventWithToken],
				q workqueue.TypedRateLimitingInterface[*datastore.EventWithToken],
			) {
				q.Add(e.Object)
			},
		},
	)

	return reconciler.
		Named("fault-remediation-controller").
		WatchesRawSource(
			src,
		).
		Complete(r)
}

// AdaptEvents transforms a channel of EventWithToken into a channel of controller-runtime
// TypedGenericEvent. It spawns a goroutine that continuously reads from the input channel
// until either the context is cancelled or the input channel is closed.
func AdaptEvents(
	ctx context.Context,
	in <-chan datastore.EventWithToken,
) <-chan event.TypedGenericEvent[*datastore.EventWithToken] {
	out := make(chan event.TypedGenericEvent[*datastore.EventWithToken])

	go func() {
		defer close(out)

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

	return out
}
