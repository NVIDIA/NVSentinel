// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/nvidia/nvsentinel/commons/pkg/condition"
	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	"github.com/nvidia/nvsentinel/commons/pkg/managed"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

const (
	mrFinalizerName = "nvsentinel.dgxc.nvidia.com/maintenance-request-cleanup"

	conditionHealthEventEmitted = "HealthEventEmitted"
	reasonEmitted               = "Emitted"
	reasonEmitFailed            = "EmitFailed"
	reasonBlocked               = "Blocked"
)

// MaintenanceRequestReconciler reconciles MaintenanceRequest objects.
type MaintenanceRequestReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Publisher *healthpub.Publisher
}

// SetupWithManager registers the reconciler with the manager.
func (r *MaintenanceRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.MaintenanceRequest{}).
		Named("maintenancerequest").
		Complete(r)
}

// Reconcile handles create, update, and delete of MaintenanceRequest objects.
func (r *MaintenanceRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := slog.With("maintenancerequest", req.NamespacedName)

	var mr v1alpha1.MaintenanceRequest
	if err := r.Get(ctx, req.NamespacedName, &mr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	if mr.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, log, &mr)
	}

	return r.handleCreateOrUpdate(ctx, log, &mr)
}

func (r *MaintenanceRequestReconciler) handleCreateOrUpdate(
	ctx context.Context, log *slog.Logger, mr *v1alpha1.MaintenanceRequest,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(mr, mrFinalizerName) {
		controllerutil.AddFinalizer(mr, mrFinalizerName)

		if err := r.Update(ctx, mr); err != nil {
			return ctrl.Result{}, err
		}
	}

	if mr.Spec == nil || mr.Spec.HealthEvent == nil {
		r.setCondition(mr, conditionHealthEventEmitted, "False", "InvalidSpec",
			"spec.healthEvent is required")

		if statusErr := r.Status().Update(ctx, mr); statusErr != nil {
			log.Error("Failed to update status for invalid spec", "error", statusErr)
		}

		return ctrl.Result{}, fmt.Errorf("spec.healthEvent is required")
	}

	nodeName := mr.Spec.HealthEvent.NodeName
	if nodeName == "" {
		r.setCondition(mr, conditionHealthEventEmitted, "False", "InvalidSpec",
			"spec.healthEvent.nodeName is required")

		if statusErr := r.Status().Update(ctx, mr); statusErr != nil {
			log.Error("Failed to update status for missing nodeName", "error", statusErr)
		}

		return ctrl.Result{}, fmt.Errorf("spec.healthEvent.nodeName is required")
	}

	if isConditionTrue(mr, conditionHealthEventEmitted) {
		return ctrl.Result{}, nil
	}

	return r.claimAndEmit(ctx, log, mr, nodeName)
}

func (r *MaintenanceRequestReconciler) claimAndEmit(
	ctx context.Context, log *slog.Logger,
	mr *v1alpha1.MaintenanceRequest, nodeName string,
) (ctrl.Result, error) {
	claimed, err := r.claimNode(ctx, log, mr, nodeName)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !claimed {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	r.autoPopulateEventFields(mr)
	r.stampTraceability(mr)

	if err := r.Update(ctx, mr); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.emitOpeningEvent(ctx, log, mr); err != nil {
		r.setCondition(mr, conditionHealthEventEmitted, "False", reasonEmitFailed,
			fmt.Sprintf("Failed to emit health event: %v", err))

		if statusErr := r.Status().Update(ctx, mr); statusErr != nil {
			log.Error("Failed to update status after emit failure", "error", statusErr)
		}

		return ctrl.Result{}, err
	}

	if err := r.persistEmittedCondition(ctx, mr); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Successfully emitted opening health event", "node", nodeName)

	return ctrl.Result{}, nil
}

func (r *MaintenanceRequestReconciler) persistEmittedCondition(
	ctx context.Context, mr *v1alpha1.MaintenanceRequest,
) error {
	r.setCondition(mr, conditionHealthEventEmitted, "True", reasonEmitted,
		"Submitted health event to platform-connector.")

	return r.Status().Update(ctx, mr)
}

// handleDeletion runs the cleanup path when DeletionTimestamp is set.
//
// Every step is idempotent so a crash at any point produces a clean
// retry:
//   - removeNodeAnnotation runs first so a new MR is not blocked while
//     the clearing event is retried. It is idempotent and ownership-
//     checked; repeated calls (e.g. across retries) are safe.
//   - emitClearingEvent may re-fire; platform-connector treats
//     duplicate isHealthy=true events as no-ops. Only fires if the
//     opening event was previously emitted (condition=True).
//   - The finalizer is removed only after all cleanup succeeds.
func (r *MaintenanceRequestReconciler) handleDeletion(
	ctx context.Context, log *slog.Logger, mr *v1alpha1.MaintenanceRequest,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(mr, mrFinalizerName) {
		return ctrl.Result{}, nil
	}

	nodeName := ""
	if mr.Spec != nil && mr.Spec.HealthEvent != nil {
		nodeName = mr.Spec.HealthEvent.NodeName
	}

	if nodeName != "" {
		// Release the node claim early so a new MR is not blocked
		// while the clearing event is retried. removeNodeAnnotation
		// is idempotent and ownership-checked, so calling it again
		// after the clearing event succeeds is safe.
		if err := r.removeNodeAnnotation(ctx, log, nodeName, mr.Name); err != nil {
			return ctrl.Result{}, err
		}

		// Only emit a clearing event if we previously emitted an
		// opening event. If emit never succeeded, there is nothing to
		// clear in the pipeline.
		if isConditionTrue(mr, conditionHealthEventEmitted) {
			if err := r.emitClearingEvent(ctx, log, mr); err != nil {
				return ctrl.Result{}, fmt.Errorf("emit clearing event: %w", err)
			}

			log.Info("Successfully emitted clearing health event", "node", nodeName)
		}
	}

	controllerutil.RemoveFinalizer(mr, mrFinalizerName)

	if err := r.Update(ctx, mr); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// claimNode atomically checks that no other MR owns the node and writes
// this MR's name into the node annotation in a single Update call.
// The Update uses the node's resourceVersion from the preceding Get, so
// concurrent claimNode calls for the same node will produce a conflict
// error on all but one, eliminating the TOCTOU window.
//
// Returns (true, nil) if the node is now claimed by this MR,
// (false, nil) if the node is blocked by a different MR, or
// (false, err) on transient errors (conflict included — caller requeues).
func (r *MaintenanceRequestReconciler) claimNode(
	ctx context.Context, log *slog.Logger,
	mr *v1alpha1.MaintenanceRequest, nodeName string,
) (bool, error) {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			// Node doesn't exist yet; nothing to block on and nothing
			// to annotate. Proceed — the event will reference the node
			// by name regardless of whether it's registered.
			return true, nil
		}

		return false, err
	}

	activeMR := node.Annotations[managed.AnnotationActiveMR]
	if activeMR != "" && activeMR != mr.Name {
		log.Info("Node already has an active MaintenanceRequest; blocking",
			"node", nodeName, "existingMR", activeMR)

		r.setCondition(mr, conditionHealthEventEmitted, "False", reasonBlocked,
			fmt.Sprintf("Node %s already has active MaintenanceRequest %s.", nodeName, activeMR))

		if statusErr := r.Status().Update(ctx, mr); statusErr != nil {
			log.Error("Failed to update blocked status", "error", statusErr)
		}

		return false, nil
	}

	if activeMR == mr.Name {
		return true, nil
	}

	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	node.Annotations[managed.AnnotationActiveMR] = mr.Name

	if err := r.Update(ctx, &node); err != nil {
		// Conflict means another MR claimed the node between our Get
		// and Update. Return the error so the caller requeues; the
		// next reconcile will re-read the annotation and either block
		// or succeed.
		return false, err
	}

	return true, nil
}

func (r *MaintenanceRequestReconciler) autoPopulateEventFields(mr *v1alpha1.MaintenanceRequest) {
	he := mr.Spec.HealthEvent

	if he.Id == "" {
		he.Id = string(mr.UID)
	}

	if he.GeneratedTimestamp == nil {
		he.GeneratedTimestamp = timestamppb.New(mr.CreationTimestamp.Time)
	}
}

func (r *MaintenanceRequestReconciler) stampTraceability(mr *v1alpha1.MaintenanceRequest) {
	he := mr.Spec.HealthEvent
	if he.Metadata == nil {
		he.Metadata = make(map[string]string)
	}

	he.Metadata["maintenanceRequestName"] = mr.Name
	he.Metadata["maintenanceRequestUID"] = string(mr.UID)
}

func (r *MaintenanceRequestReconciler) emitOpeningEvent(
	ctx context.Context, log *slog.Logger, mr *v1alpha1.MaintenanceRequest,
) error {
	events := &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{mr.Spec.HealthEvent},
	}

	log.Info("Emitting opening health event",
		"node", mr.Spec.HealthEvent.NodeName,
		"agent", mr.Spec.HealthEvent.Agent,
		"checkName", mr.Spec.HealthEvent.CheckName)

	return r.Publisher.Publish(ctx, events)
}

func (r *MaintenanceRequestReconciler) emitClearingEvent(
	ctx context.Context, log *slog.Logger, mr *v1alpha1.MaintenanceRequest,
) error {
	openingEvent := mr.Spec.HealthEvent

	clearingEvent := &pb.HealthEvent{
		Version:            openingEvent.Version,
		Agent:              openingEvent.Agent,
		ComponentClass:     openingEvent.ComponentClass,
		CheckName:          openingEvent.CheckName,
		NodeName:           openingEvent.NodeName,
		IsHealthy:          true,
		IsFatal:            false,
		RecommendedAction:  pb.RecommendedAction_NONE,
		Message:            fmt.Sprintf("MaintenanceRequest %s cleared.", mr.Name),
		GeneratedTimestamp: timestamppb.Now(),
		Id:                 fmt.Sprintf("clear-%s", mr.UID),
		Metadata:           openingEvent.Metadata,
	}

	events := &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{clearingEvent},
	}

	log.Info("Emitting clearing health event",
		"node", openingEvent.NodeName,
		"agent", openingEvent.Agent,
		"checkName", openingEvent.CheckName)

	return r.Publisher.Publish(ctx, events)
}

func (r *MaintenanceRequestReconciler) removeNodeAnnotation(
	ctx context.Context, log *slog.Logger, nodeName, mrName string,
) error {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	currentMR, exists := node.Annotations[managed.AnnotationActiveMR]
	if !exists || currentMR != mrName {
		return nil
	}

	delete(node.Annotations, managed.AnnotationActiveMR)

	return r.Update(ctx, &node)
}

func (r *MaintenanceRequestReconciler) setCondition(
	mr *v1alpha1.MaintenanceRequest, condType, status, reason, message string,
) {
	if mr.Status == nil {
		mr.Status = &pb.MaintenanceRequestStatus{}
	}

	metav1Conds := condition.ToMetav1Slice(mr.Status.Conditions)
	meta.SetStatusCondition(&metav1Conds, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionStatus(status),
		ObservedGeneration: mr.Generation,
		Reason:             reason,
		Message:            message,
	})
	mr.Status.Conditions = condition.FromMetav1Slice(metav1Conds)
}

func isConditionTrue(mr *v1alpha1.MaintenanceRequest, condType string) bool {
	if mr.Status == nil {
		return false
	}

	return meta.IsStatusConditionTrue(
		condition.ToMetav1Slice(mr.Status.Conditions), condType,
	)
}
