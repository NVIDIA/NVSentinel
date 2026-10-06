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

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/nvidia/nvsentinel/commons/pkg/condition"
	"github.com/nvidia/nvsentinel/commons/pkg/distributedlock"
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
	reasonRejected              = "Rejected"

	claimLeasePrefix = "mr-claim."
)

// MaintenanceRequestReconciler reconciles MaintenanceRequest objects.
//
// NodeLock is the janitor node lock, shared with every janitor controller.
// NodeClaim allows one open MaintenanceRequest per node; build it with
// distributedlock.WithLeaseName(ClaimLeaseName).
type MaintenanceRequestReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Publisher *healthpub.Publisher
	NodeLock  distributedlock.NodeLock
	NodeClaim distributedlock.NodeLock
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
		return r.handleEmitted(ctx, log, mr, nodeName)
	}

	if hasConditionReason(mr, conditionHealthEventEmitted, reasonRejected) {
		return ctrl.Result{}, nil
	}

	return r.claimAndEmit(ctx, log, mr, nodeName)
}

// handleEmitted leaves an emitted request holding only its claim. A request
// emitted by an older version still holds the janitor node lock and has no
// claim, which blocks the janitor job that its own event triggers.
func (r *MaintenanceRequestReconciler) handleEmitted(
	ctx context.Context, log *slog.Logger,
	mr *v1alpha1.MaintenanceRequest, nodeName string,
) (ctrl.Result, error) {
	if !r.NodeClaim.LockNode(ctx, mr, nodeName) {
		// Another request can take the claim first only while requests from an
		// older version are open. Keeping the janitor lock makes that request
		// wait for this one, as the older version did.
		log.Warn("Emitted MaintenanceRequest does not hold the node claim; keeping the node lock",
			"node", nodeName)

		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if r.NodeLock.CheckUnlock(ctx, mr, nodeName) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// claimAndEmit sends the opening event. The claim is kept until deletion so
// that a node has at most one open MaintenanceRequest. The janitor node lock is
// held only around the emit, so the event cannot start maintenance while a
// janitor job runs; it must be released afterwards because the janitor job
// that this event triggers needs the same lock.
func (r *MaintenanceRequestReconciler) claimAndEmit(
	ctx context.Context, log *slog.Logger,
	mr *v1alpha1.MaintenanceRequest, nodeName string,
) (ctrl.Result, error) {
	if !r.NodeClaim.LockNode(ctx, mr, nodeName) {
		return r.handleClaimContention(ctx, log, mr, nodeName)
	}

	if !r.NodeLock.LockNode(ctx, mr, nodeName) {
		return r.setBlocked(ctx, log, mr, fmt.Sprintf(
			"Node %s is locked by another maintenance operation%s.",
			nodeName, r.lockHolderDescription(ctx, nodeName)))
	}

	emitErr := r.emitAndPersist(ctx, log, mr)
	retryUnlock := r.NodeLock.CheckUnlock(ctx, mr, nodeName)

	if emitErr != nil {
		return ctrl.Result{}, emitErr
	}

	log.Info("Successfully emitted opening health event", "node", nodeName)

	if retryUnlock {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// handleClaimContention rejects mr when another open MaintenanceRequest holds
// the node's claim. Any other holder means the claim is about to be released,
// so mr waits instead.
func (r *MaintenanceRequestReconciler) handleClaimContention(
	ctx context.Context, log *slog.Logger,
	mr *v1alpha1.MaintenanceRequest, nodeName string,
) (ctrl.Result, error) {
	// The holder is read from the cache that delivered mr's own event, so a
	// holder deleted before mr was created is already seen as being deleted.
	holder, active, err := ActiveClaimHolder(ctx, r.Client, r.NodeClaim, mr, nodeName)
	if err != nil {
		log.Warn("Unable to inspect node claim holder; will retry", "node", nodeName, "error", err)
	}

	if !active {
		return r.setBlocked(ctx, log, mr, fmt.Sprintf(
			"Waiting for the MaintenanceRequest claim on node %s to be released.", nodeName))
	}

	log.Info("Rejecting MaintenanceRequest: node already has an open MaintenanceRequest",
		"node", nodeName, "holder", holder.Name)

	r.setCondition(mr, conditionHealthEventEmitted, "False", reasonRejected, fmt.Sprintf(
		"Node %s already has an open MaintenanceRequest %s. This request will not be retried; delete it.",
		nodeName, holder.Name))

	if err := r.Status().Update(ctx, mr); err != nil {
		return ctrl.Result{}, fmt.Errorf("persist rejected status: %w", err)
	}

	return ctrl.Result{}, nil
}

// ActiveClaimHolder reports whether the claim on nodeName is held by another
// open MaintenanceRequest for the same node, which makes mr a duplicate. Every
// other holder (mr itself, a request that is being deleted or is gone, or an
// owner of another kind) is not a duplicate. A missing claim is not an error.
func ActiveClaimHolder(
	ctx context.Context, reader client.Reader, claim distributedlock.NodeLock,
	mr *v1alpha1.MaintenanceRequest, nodeName string,
) (*v1alpha1.MaintenanceRequest, bool, error) {
	owner, err := claim.GetHolder(ctx, nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("inspect node claim: %w", err)
	}

	if owner.Kind != managed.MRKind || owner.UID == mr.UID {
		return nil, false, nil
	}

	var holder v1alpha1.MaintenanceRequest
	if err := reader.Get(ctx, client.ObjectKey{Name: owner.Name}, &holder); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("get claim holder %q: %w", owner.Name, err)
	}

	if !isOpenRequestFor(&holder, owner.UID, nodeName) {
		return nil, false, nil
	}

	return &holder, true, nil
}

// isOpenRequestFor reports whether holder is the claim owner itself, rather
// than a recreated request with the same name, is not being deleted, and
// targets nodeName.
func isOpenRequestFor(holder *v1alpha1.MaintenanceRequest, ownerUID types.UID, nodeName string) bool {
	if holder.UID != ownerUID || holder.DeletionTimestamp != nil {
		return false
	}

	return holder.Spec != nil && holder.Spec.HealthEvent != nil && holder.Spec.HealthEvent.NodeName == nodeName
}

func (r *MaintenanceRequestReconciler) setBlocked(
	ctx context.Context, log *slog.Logger,
	mr *v1alpha1.MaintenanceRequest, message string,
) (ctrl.Result, error) {
	log.Info("MaintenanceRequest blocked; will retry", "reason", message)

	r.setCondition(mr, conditionHealthEventEmitted, "False", reasonBlocked, message)

	if statusErr := r.Status().Update(ctx, mr); statusErr != nil {
		log.Error("Failed to update blocked status", "error", statusErr)
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// lockHolderDescription names the janitor node lock holder for the Blocked
// message, or returns "" when the holder cannot be read.
func (r *MaintenanceRequestReconciler) lockHolderDescription(ctx context.Context, nodeName string) string {
	holder, err := r.NodeLock.GetHolder(ctx, nodeName)
	if err != nil || holder == nil {
		return ""
	}

	return fmt.Sprintf(" (%s/%s)", holder.Kind, holder.Name)
}

func (r *MaintenanceRequestReconciler) emitAndPersist(
	ctx context.Context, log *slog.Logger, mr *v1alpha1.MaintenanceRequest,
) error {
	if err := r.emitOpeningEvent(ctx, log, mr); err != nil {
		r.setCondition(mr, conditionHealthEventEmitted, "False", reasonEmitFailed,
			fmt.Sprintf("Failed to emit health event: %v", err))

		if statusErr := r.Status().Update(ctx, mr); statusErr != nil {
			log.Error("Failed to update status after emit failure", "error", statusErr)
		}

		return err
	}

	return r.persistEmittedCondition(ctx, mr)
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
//   - emitClearingEvent fires first (only if the opening event was
//     previously emitted). The claim is held during this step so no
//     other MaintenanceRequest can open on the node before it clears.
//   - CheckUnlock releases the janitor node lock, in case an interrupted
//     emit left it held, and then the claim. If the MR is force-deleted,
//     K8s GC cleans up both leases via their owner references.
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
		// Only emit a clearing event if we previously emitted an
		// opening event. If emit never succeeded, there is nothing to
		// clear in the pipeline.
		if isConditionTrue(mr, conditionHealthEventEmitted) {
			if err := r.emitClearingEvent(ctx, log, mr); err != nil {
				return ctrl.Result{}, fmt.Errorf("emit clearing event: %w", err)
			}

			log.Info("Successfully emitted clearing health event", "node", nodeName)
		}

		// CheckUnlock is idempotent: if the lease was already deleted
		// (e.g. by K8s GC via the owner reference), it returns false.
		if r.NodeLock.CheckUnlock(ctx, mr, nodeName) || r.NodeClaim.CheckUnlock(ctx, mr, nodeName) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}

	controllerutil.RemoveFinalizer(mr, mrFinalizerName)

	if err := r.Update(ctx, mr); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *MaintenanceRequestReconciler) emitOpeningEvent(
	ctx context.Context, log *slog.Logger, mr *v1alpha1.MaintenanceRequest,
) error {
	openingEvent := eventForPublishing(mr)
	events := &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{openingEvent},
	}

	log.Info("Emitting opening health event",
		"node", openingEvent.NodeName,
		"agent", openingEvent.Agent,
		"checkName", openingEvent.CheckName)

	return r.Publisher.Publish(ctx, events)
}

func (r *MaintenanceRequestReconciler) emitClearingEvent(
	ctx context.Context, log *slog.Logger, mr *v1alpha1.MaintenanceRequest,
) error {
	openingEvent := eventForPublishing(mr)

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

func eventForPublishing(mr *v1alpha1.MaintenanceRequest) *pb.HealthEvent {
	healthEvent := proto.Clone(mr.Spec.HealthEvent).(*pb.HealthEvent)

	// The webhook persists these fields for new production objects. Keep
	// deterministic fallbacks for legacy objects and direct controller tests.
	if healthEvent.Id == "" {
		healthEvent.Id = string(mr.UID)
	}

	if healthEvent.GeneratedTimestamp == nil {
		healthEvent.GeneratedTimestamp = timestamppb.New(mr.CreationTimestamp.Time)
	}

	if healthEvent.Metadata == nil {
		healthEvent.Metadata = make(map[string]string)
	}

	healthEvent.Metadata["maintenanceRequestName"] = mr.Name
	healthEvent.Metadata["maintenanceRequestUID"] = string(mr.UID)

	return healthEvent
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

func hasConditionReason(mr *v1alpha1.MaintenanceRequest, condType, reason string) bool {
	if mr.Status == nil {
		return false
	}

	cond := meta.FindStatusCondition(condition.ToMetav1Slice(mr.Status.Conditions), condType)

	return cond != nil && cond.Reason == reason
}

// ClaimLeaseName returns the name of the lease that holds a node's
// MaintenanceRequest claim. It must differ from the janitor node lock, which
// is named after the node. A name too long for a lease keeps its readable
// start and ends in a hash of the full name.
func ClaimLeaseName(nodeName string) string {
	return hashTruncateTrimmed(claimLeasePrefix+nodeName, validation.DNS1123SubdomainMaxLength, ".-")
}
