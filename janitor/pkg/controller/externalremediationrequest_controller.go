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

package controller

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"go.opentelemetry.io/otel/attribute"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/nvidia/nvsentinel/commons/pkg/condition"
	"github.com/nvidia/nvsentinel/commons/pkg/distributedlock"
	"github.com/nvidia/nvsentinel/commons/pkg/managed"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	nvsentinelv1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/metrics"
)

const (
	// ExternalRemediationFinalizer gates node cleanup before the ExtRR is GC'd.
	ExternalRemediationFinalizer = "nvsentinel.dgxc.nvidia.com/external-remediation-cleanup"

	ConditionNVSentinelOwnershipReleased = "NVSentinelOwnershipReleased"
	ConditionExternalRemediationComplete = "ExternalRemediationComplete"

	reasonInitializing           = "Initializing"
	reasonAwaitingExternalSystem = "AwaitingExternalSystem"

	// ReleaseTaintKey is applied to release a Node from NVSentinel ownership.
	// The value carries the owning ExtRR's metadata.name so cleanup is drift-safe.
	ReleaseTaintKey = "nvsentinel.dgxc.nvidia.com/external-remediation"

	ReasonReleaseTaintApplied = "ReleaseTaintApplied"

	ReasonNodeNotFound = "NodeNotFound"

	eventReasonReleaseTaintApplied   = "ReleaseTaintApplied"
	eventReasonReleaseTaintRemoved   = "ReleaseTaintRemoved"
	eventReasonOperatorDeleteRequest = "OperatorDeleteRequested"
	eventReasonCompletedDeleted      = "CompletedRequestDeleted"
	eventReasonOwnerNodeDeleted      = "OwnerNodeDeleted"
	eventReasonNodeNotFound          = "NodeNotFound"

	// closeReason* qualifiers disambiguate which cleanup path closed the ExtRR
	// in the ReleaseTaintRemoved event message.
	closeReasonExternalRemediationCompleteTrue = "ExternalRemediationCompleteTrue"
	closeReasonOperatorInitiated               = "OperatorInitiated"
)

// ExternalRemediationRequestReconciler implements the ADR-040 state machine.
type ExternalRemediationRequestReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Recorder      record.EventRecorder
	NodeLock      distributedlock.NodeLock
	LockNamespace string
}

const labelValueUnknown = "unknown"

func recommendedActionLabel(extrrObj *nvsentinelv1.ExternalRemediationRequest) string {
	if name := model.GetEffectiveActionName(extrrObj.Spec.HealthEvent); name != "" {
		return name
	}

	return labelValueUnknown
}

func extrrNodeLabel(extrrObj *nvsentinelv1.ExternalRemediationRequest) string {
	return extrrObj.Spec.HealthEvent.NodeName
}

func (r *ExternalRemediationRequestReconciler) emitEvent(
	extrrObj *nvsentinelv1.ExternalRemediationRequest, eventType, reason, message string,
) {
	if r.Recorder == nil {
		return
	}

	r.Recorder.Event(extrrObj, eventType, reason, message)
}

//nolint:lll // kubebuilder RBAC marker must stay on one line
// +kubebuilder:rbac:groups=nvsentinel.dgxc.nvidia.com,resources=externalremediationrequests,verbs=get;list;watch;update;patch;delete
//nolint:lll
// +kubebuilder:rbac:groups=nvsentinel.dgxc.nvidia.com,resources=externalremediationrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nvsentinel.dgxc.nvidia.com,resources=externalremediationrequests/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;delete

// Reconcile drives the ExtRR through its lifecycle. The OTEL span is linked to
// the upstream health-monitor trace via annotations stamped by fault-remediation.
func (r *ExternalRemediationRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var extrr nvsentinelv1.ExternalRemediationRequest
	if err := r.Get(ctx, req.NamespacedName, &extrr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	annotations := extrr.GetAnnotations()

	ctx, span := tracing.StartSpanWithLinkFromTraceContext(
		ctx,
		annotations[tracing.TraceIDAnnotationKey],
		annotations[tracing.SpanIDAnnotationKey],
		"janitor.externalremediationrequest.reconcile",
	)
	defer span.End()

	span.SetAttributes(
		attribute.String("extrr.name", extrr.Name),
		attribute.String("extrr.namespace", extrr.Namespace),
		attribute.String("extrr.node", extrrNodeLabel(&extrr)),
		attribute.String("extrr.recommended_action", recommendedActionLabel(&extrr)),
	)

	if r.needsInitialization(&extrr) {
		span.SetAttributes(attribute.String("extrr.branch", "init"))
		return r.reconcileInitialize(ctx, &extrr)
	}

	// Lock lifecycle matches the sibling janitor controllers: hold the node
	// lock for the entire pre-completion lifetime, drop it once CompletionTime
	// is set. Operator-delete still needs the lock (cleanup-on-deletion scrubs
	// the Node), so "still actively working on this CR" is "not completed OR
	// being deleted".
	nodeName := extrr.Spec.HealthEvent.NodeName
	completed := extrr.Status != nil && extrr.Status.CompletionTime != nil
	deleting := !extrr.DeletionTimestamp.IsZero()

	if completed && !deleting {
		if r.NodeLock.CheckUnlock(ctx, &extrr, nodeName) {
			return ctrl.Result{RequeueAfter: nodeLockRequeue}, nil
		}

		return ctrl.Result{}, nil
	}

	if !r.NodeLock.LockNode(ctx, &extrr, nodeName) {
		return ctrl.Result{RequeueAfter: nodeLockRequeue}, nil
	}

	result, dispatchErr := r.dispatch(ctx, &extrr)
	if dispatchErr != nil {
		tracing.RecordError(span, dispatchErr)
	}

	return result, dispatchErr
}

// needsInitialization is presence-only — values set by the external system
// (e.g. Complete=True) survive re-entry.
func (r *ExternalRemediationRequestReconciler) needsInitialization(
	extrrObj *nvsentinelv1.ExternalRemediationRequest,
) bool {
	if !controllerutil.ContainsFinalizer(extrrObj, ExternalRemediationFinalizer) {
		return true
	}

	conds := statusConditions(extrrObj)
	if meta.FindStatusCondition(conds, ConditionNVSentinelOwnershipReleased) == nil {
		return true
	}

	if meta.FindStatusCondition(conds, ConditionExternalRemediationComplete) == nil {
		return true
	}

	return false
}

// reconcileInitialize adds the finalizer and seeds initial Unknown conditions
// in a single pass. setInitialConditions re-fetches before patching, so it
// tolerates the rv bump from the finalizer Update.
func (r *ExternalRemediationRequestReconciler) reconcileInitialize(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(extrrObj, ExternalRemediationFinalizer) {
		updated := extrrObj.DeepCopy()
		controllerutil.AddFinalizer(updated, ExternalRemediationFinalizer)

		if err := r.Update(ctx, updated); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer to ExternalRemediationRequest %s: %w", extrrObj.Name, err)
		}

		slog.InfoContext(ctx, "Added cleanup finalizer to ExternalRemediationRequest", "name", extrrObj.Name)
	}

	changed, err := r.setInitialConditions(ctx, extrrObj)
	if err != nil {
		return ctrl.Result{}, err
	}

	if changed {
		// Fire created exactly once across re-reconciles.
		metrics.GlobalMetrics.IncExtRRTotal(metrics.ExtRRPhaseCreated, metrics.ExtRRResultNone)
	}

	return ctrl.Result{}, nil
}

func (r *ExternalRemediationRequestReconciler) setInitialConditions(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (bool, error) {
	existing := statusConditions(extrrObj)
	conditions := append([]metav1.Condition(nil), existing...)
	changed := false

	if meta.FindStatusCondition(conditions, ConditionNVSentinelOwnershipReleased) == nil {
		meta.SetStatusCondition(&conditions, metav1.Condition{
			Type:    ConditionNVSentinelOwnershipReleased,
			Status:  metav1.ConditionUnknown,
			Reason:  reasonInitializing,
			Message: "Reconciler has not yet applied the release taint and managed=false label.",
		})

		changed = true
	}

	if meta.FindStatusCondition(conditions, ConditionExternalRemediationComplete) == nil {
		meta.SetStatusCondition(&conditions, metav1.Condition{
			Type:    ConditionExternalRemediationComplete,
			Status:  metav1.ConditionUnknown,
			Reason:  reasonAwaitingExternalSystem,
			Message: "External system has not yet reported completion.",
		})

		changed = true
	}

	if !changed {
		return false, nil
	}

	return r.patchStatusConditions(ctx, extrrObj, conditions)
}

// dispatch routes to the active ADR-040 branch given the current conditions.
// Callers (Reconcile) are responsible for the node-lock lifecycle — by the
// time we get here, the lock is held.
func (r *ExternalRemediationRequestReconciler) dispatch(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (ctrl.Result, error) {
	if !extrrObj.DeletionTimestamp.IsZero() {
		return r.reconcileCleanupOnDeletion(ctx, extrrObj)
	}

	conds := statusConditions(extrrObj)

	switch {
	case meta.IsStatusConditionPresentAndEqual(conds, ConditionNVSentinelOwnershipReleased, metav1.ConditionUnknown):
		return r.reconcileApply(ctx, extrrObj)

	case meta.IsStatusConditionTrue(conds, ConditionExternalRemediationComplete):
		return r.reconcileCleanupAfterComplete(ctx, extrrObj)

	case meta.IsStatusConditionFalse(conds, ConditionExternalRemediationComplete):
		markOpen(extrrObj)
		return r.reconcileNoOpOnFalse(ctx, extrrObj)

	default:
		// Steady state: Released=True awaiting the external system.
		markOpen(extrrObj)
		return ctrl.Result{}, nil
	}
}

// markOpen re-asserts the open gauge on every reconcile of an open ExtRR.
// After a janitor restart or leader change the in-memory gauge starts empty,
// and the initial resync reconciles every ExtRR, which rebuilds it.
func markOpen(extrrObj *nvsentinelv1.ExternalRemediationRequest) {
	metrics.GlobalMetrics.SetExtRROpen(extrrNodeLabel(extrrObj), recommendedActionLabel(extrrObj),
		metrics.ExtRROpenStateAwaiting, true)
}

// nodeLockRequeue matches the sibling janitor controllers' lock-retry cadence.
const nodeLockRequeue = 2 * time.Second

// reconcileApply applies the release taint + managed=false in one PATCH then
// transitions NVSentinelOwnershipReleased=True. A missing or replacement Node
// is terminal Released=False (no retry, see ADR-040); already-applied uses an
// idempotent fast path. Other API errors propagate and controller-runtime
// requeues with backoff.
//
// Assumes the caller (dispatch) already acquired the cross-controller
// node-level lock, so a release taint with another ExtRR's name is stale and
// is replaced. reconcileCleanup removes only a taint whose value is this
// ExtRR's name, so the taint value must always be ours after apply.
func (r *ExternalRemediationRequestReconciler) reconcileApply(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (ctrl.Result, error) {
	nodeName := extrrObj.Spec.HealthEvent.NodeName

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.transitionToReleaseFailure(ctx, extrrObj, nodeName,
				fmt.Sprintf("target node %q does not exist", nodeName))
		}

		return ctrl.Result{}, fmt.Errorf("get node %q for ExtRR %q: %w", nodeName, extrrObj.Name, err)
	}

	if !nodeMatchesOwner(extrrObj, &node) {
		return ctrl.Result{}, r.transitionToReleaseFailure(ctx, extrrObj, nodeName,
			fmt.Sprintf("target node %q was replaced; refusing to release the replacement node", nodeName))
	}

	taint := findTaintByKeyAndEffect(node.Spec.Taints, ReleaseTaintKey, corev1.TaintEffectNoSchedule)
	if taint != nil && taint.Value == extrrObj.Name &&
		node.Labels[managed.ManagedLabelKey] == managed.ManagedLabelValueFalse {
		slog.InfoContext(ctx, "release taint and managed=false label already in place; transitioning condition",
			"extrr", extrrObj.Name, "node", nodeName)

		msg := fmt.Sprintf("release taint %s=%s and managed=false label already present on node %q",
			ReleaseTaintKey, extrrObj.Name, nodeName)

		return ctrl.Result{}, r.transitionToReleaseSuccess(ctx, extrrObj, msg)
	}

	if taint != nil && taint.Value != extrrObj.Name {
		slog.WarnContext(ctx, "replacing stale release taint owned by another ExtRR",
			"extrr", extrrObj.Name, "node", nodeName, "staleOwner", taint.Value)
	}

	nodeToUpdate := withReleaseMarkers(&node, extrrObj.Name)

	if err := r.Patch(ctx, nodeToUpdate, client.StrategicMergeFrom(&node)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch node %q with release taint + managed=false: %w", nodeName, err)
	}

	slog.InfoContext(ctx, "applied release taint and managed=false label to node",
		"extrr", extrrObj.Name, "node", nodeName)

	msg := fmt.Sprintf("applied release taint %s=%s and managed=false label to node %q",
		ReleaseTaintKey, extrrObj.Name, nodeName)

	return ctrl.Result{}, r.transitionToReleaseSuccess(ctx, extrrObj, msg)
}

// withReleaseMarkers returns a copy of node with exactly one release taint,
// valued extrrName, and the managed=false label.
func withReleaseMarkers(node *corev1.Node, extrrName string) *corev1.Node {
	updated := node.DeepCopy()

	if taint := findTaintByKeyAndEffect(updated.Spec.Taints, ReleaseTaintKey,
		corev1.TaintEffectNoSchedule); taint == nil || taint.Value != extrrName {
		updated.Spec.Taints = append(removeTaintByKeyAndEffect(updated.Spec.Taints, ReleaseTaintKey,
			corev1.TaintEffectNoSchedule), corev1.Taint{
			Key:    ReleaseTaintKey,
			Value:  extrrName,
			Effect: corev1.TaintEffectNoSchedule,
		})
	}

	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}

	updated.Labels[managed.ManagedLabelKey] = managed.ManagedLabelValueFalse

	return updated
}

// transitionToReleaseFailure terminates the apply path when the target Node
// instance is missing. This includes replacement by a new Node with the same
// name. It sets NVSentinelOwnershipReleased=False + Status.CompletionTime in
// two patches; the CompletionTime stamp short-circuits dispatch on subsequent
// reconciles. The external system can still drive close via Complete=True or
// operator delete. Waiting cannot bring the original Node instance back.
func (r *ExternalRemediationRequestReconciler) transitionToReleaseFailure(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest, nodeName, message string,
) error {
	changed, err := r.transitionReleased(ctx, extrrObj, metav1.ConditionFalse, ReasonNodeNotFound, message)
	if err != nil {
		return err
	}

	if !changed {
		return nil
	}

	slog.WarnContext(ctx, "target node unavailable; failing ExtRR apply",
		"extrr", extrrObj.Name, "node", nodeName)

	metrics.GlobalMetrics.IncExtRRTotal(metrics.ExtRRPhaseReleased, metrics.ExtRRResultNodeNotFound)
	r.emitEvent(extrrObj, corev1.EventTypeWarning, eventReasonNodeNotFound, message)

	if _, err := r.markCompletionTime(ctx, extrrObj); err != nil {
		return err
	}

	return nil
}

// transitionToReleaseSuccess gates metric/event emission on the status patch
// actually mutating state so re-reconciles don't double-fire.
func (r *ExternalRemediationRequestReconciler) transitionToReleaseSuccess(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest, message string,
) error {
	changed, err := r.transitionReleased(ctx, extrrObj, metav1.ConditionTrue, ReasonReleaseTaintApplied, message)
	if err != nil {
		return err
	}

	if !changed {
		return nil
	}

	metrics.GlobalMetrics.IncExtRRTotal(metrics.ExtRRPhaseReleased, metrics.ExtRRResultSuccess)
	markOpen(extrrObj)
	r.emitEvent(extrrObj, corev1.EventTypeNormal, eventReasonReleaseTaintApplied, message)

	return nil
}

// reconcileCleanupAfterComplete scrubs the Node when Complete=True; the ExtRR
// stays as a historical record until an operator deletes it. Business
// metrics (closed + external_response + ExtRROpen=0 + age) fire on first
// observation of Complete=True regardless of whether the Node still exists,
// so terminate-style remediations (the external system deleting the Node as
// part of the repair) stay observable. Gated on Status.CompletionTime == nil
// for idempotency across re-reconciles.
func (r *ExternalRemediationRequestReconciler) reconcileCleanupAfterComplete(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (ctrl.Result, error) {
	changed, err := r.reconcileCleanup(ctx, extrrObj)
	if err != nil {
		return ctrl.Result{}, err
	}

	if extrrObj.Status == nil || extrrObj.Status.CompletionTime == nil {
		r.recordCloseMetrics(extrrObj, metrics.ExtRRResultSuccess)
		metrics.GlobalMetrics.IncExtRRTotal(metrics.ExtRRPhaseExternalResponse, metrics.ExtRRResultSuccess)

		if _, err := r.markCompletionTime(ctx, extrrObj); err != nil {
			return ctrl.Result{}, err
		}
	}

	if changed {
		r.emitEvent(extrrObj, corev1.EventTypeNormal, eventReasonReleaseTaintRemoved,
			fmt.Sprintf("release taint and managed=false label removed (%s)",
				closeReasonExternalRemediationCompleteTrue))
	}

	// Lock release on the next reconcile is handled by the dispatch
	// short-circuit once CompletionTime is set.
	return ctrl.Result{}, nil
}

// reconcileCleanupOnDeletion runs the cleanup PATCH then drops the finalizer.
// The close metric fires on first invocation regardless of whether the cleanup
// PATCH actually mutated the Node — gated by Status.CompletionTime so a
// previous close (Complete=True or NodeNotFound) is not counted twice.
func (r *ExternalRemediationRequestReconciler) reconcileCleanupOnDeletion(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(extrrObj, ExternalRemediationFinalizer) {
		return ctrl.Result{}, nil
	}

	closeResult, err := r.announceDeletion(ctx, extrrObj)
	if err != nil {
		return ctrl.Result{}, err
	}

	changed, err := r.reconcileCleanup(ctx, extrrObj)
	if err != nil {
		return ctrl.Result{}, err
	}

	if closeResult != metrics.ExtRRResultNone {
		r.recordCloseMetrics(extrrObj, closeResult)

		if _, err := r.markCompletionTime(ctx, extrrObj); err != nil {
			return ctrl.Result{}, err
		}
	}

	if changed {
		r.emitEvent(extrrObj, corev1.EventTypeNormal, eventReasonReleaseTaintRemoved,
			fmt.Sprintf("release taint and managed=false label removed (%s)",
				closeReasonOperatorInitiated))
	}

	// Release the lock before removing the finalizer. Requeue on transient
	// unlock failure so we don't strand the lease — K8s GC via the lease's
	// ownerReference would eventually clean it up, but the explicit unlock
	// keeps us consistent with the sibling controllers and shortens the
	// window where the lease could outlive its owner.
	if r.NodeLock.CheckUnlock(ctx, extrrObj, extrrObj.Spec.HealthEvent.NodeName) {
		return ctrl.Result{RequeueAfter: nodeLockRequeue}, nil
	}

	updated := extrrObj.DeepCopy()
	controllerutil.RemoveFinalizer(updated, ExternalRemediationFinalizer)

	if err := r.Update(ctx, updated); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing cleanup finalizer from ExternalRemediationRequest %s: %w",
			extrrObj.Name, err)
	}

	slog.InfoContext(ctx, "removed cleanup finalizer; ExternalRemediationRequest will be garbage-collected",
		"extrr", extrrObj.Name)

	return ctrl.Result{}, nil
}

// announceDeletion emits the event that explains why the ExtRR is being
// deleted and returns the closed{result} to record, or ExtRRResultNone when
// the ExtRR was already closed. Who issued the delete (operator, TTL, or
// garbage collector) is not recorded on the object, so the cause is inferred
// from the ExtRR's state and its target Node.
func (r *ExternalRemediationRequestReconciler) announceDeletion(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (string, error) {
	if extrrObj.Status != nil && extrrObj.Status.CompletionTime != nil {
		r.emitEvent(extrrObj, corev1.EventTypeNormal, eventReasonCompletedDeleted,
			"request already completed; releasing finalizer")

		return metrics.ExtRRResultNone, nil
	}

	nodeName := extrrObj.Spec.HealthEvent.NodeName

	nodeDeleted, err := r.ownerNodeDeleted(ctx, extrrObj)
	if err != nil {
		return metrics.ExtRRResultNone, err
	}

	if nodeDeleted {
		r.emitEvent(extrrObj, corev1.EventTypeWarning, eventReasonOwnerNodeDeleted,
			fmt.Sprintf("target node %q was deleted or replaced while the request was open", nodeName))

		return metrics.ExtRRResultNodeDeleted, nil
	}

	r.emitEvent(extrrObj, corev1.EventTypeNormal, eventReasonOperatorDeleteRequest,
		"deletion requested; running cleanup before releasing finalizer")

	return metrics.ExtRRResultOperatorDeleted, nil
}

// ownerNodeDeleted reports whether the target Node is gone, is terminating, or
// was replaced by a Node with the same name but a different UID than the
// ExtRR's Node owner reference. Without a Node owner reference only absence
// and termination are detected.
func (r *ExternalRemediationRequestReconciler) ownerNodeDeleted(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (bool, error) {
	nodeName := extrrObj.Spec.HealthEvent.NodeName

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}

		return false, fmt.Errorf("get node %q for ExtRR %q deletion: %w", nodeName, extrrObj.Name, err)
	}

	if !node.DeletionTimestamp.IsZero() {
		return true, nil
	}

	return !nodeMatchesOwner(extrrObj, &node), nil
}

// recordCloseMetrics fires the close counter, sets ExtRROpen to 0, and
// observes the age histogram. Idempotency is the caller's responsibility
// (via Status.CompletionTime). Setting 0 is safe even when the ExtRR never
// reached Released=True (for example, deleted before apply).
func (r *ExternalRemediationRequestReconciler) recordCloseMetrics(
	extrrObj *nvsentinelv1.ExternalRemediationRequest, result string,
) {
	node := extrrNodeLabel(extrrObj)
	action := recommendedActionLabel(extrrObj)

	metrics.GlobalMetrics.IncExtRRTotal(metrics.ExtRRPhaseClosed, result)
	metrics.GlobalMetrics.SetExtRROpen(node, action, metrics.ExtRROpenStateAwaiting, false)

	if !extrrObj.CreationTimestamp.IsZero() {
		metrics.GlobalMetrics.ObserveExtRRAge(action, result,
			time.Since(extrrObj.CreationTimestamp.Time).Seconds())
	}
}

// markCompletionTime stamps Status.CompletionTime via a status-subresource
// patch if not already set, matching the sibling janitor controllers'
// terminal-state signal. Idempotent: re-fetches latest, no-ops if CompletionTime
// was set in the gap. Reflects the new Status back onto extrrObj so the
// caller's view stays consistent.
func (r *ExternalRemediationRequestReconciler) markCompletionTime(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (bool, error) {
	if extrrObj.Status != nil && extrrObj.Status.CompletionTime != nil {
		return false, nil
	}

	var latest nvsentinelv1.ExternalRemediationRequest
	if err := r.Get(ctx, client.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}, &latest); err != nil {
		return false, fmt.Errorf("refreshing ExternalRemediationRequest %s before completion-time patch: %w",
			extrrObj.Name, err)
	}

	if latest.Status != nil && latest.Status.CompletionTime != nil {
		extrrObj.Status = latest.Status
		return false, nil
	}

	updated := latest.DeepCopy()
	updated.SetCompletionTime()

	if err := r.Status().Patch(ctx, updated, client.MergeFrom(&latest)); err != nil {
		return false, fmt.Errorf("patching CompletionTime on ExternalRemediationRequest %s: %w",
			extrrObj.Name, err)
	}

	// Write back both Status and ResourceVersion: the status patch bumps the
	// object's resourceVersion, and any subsequent Update on extrrObj (e.g.
	// the finalizer-removal Update in reconcileCleanupOnDeletion) must carry
	// the fresh rv to avoid a 409 Conflict.
	extrrObj.Status = updated.Status
	extrrObj.ResourceVersion = updated.ResourceVersion

	return true, nil
}

// reconcileCleanup removes the release taint and the managed label from the
// Node. Returns true when the PATCH mutated the Node. It acts only when the
// release taint value equals this ExtRR's name. Cleanup can run again after
// the node lock is released (for example, when a completed ExtRR is deleted),
// and by then the Node may carry another ExtRR's taint, an operator-set
// managed label, or be a replacement Node with the same name.
func (r *ExternalRemediationRequestReconciler) reconcileCleanup(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (bool, error) {
	nodeName := extrrObj.Spec.HealthEvent.NodeName

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			slog.InfoContext(ctx, "target Node already gone; nothing to clean up",
				"extrr", extrrObj.Name, "node", nodeName)

			return false, nil
		}

		return false, fmt.Errorf("get node %q for ExtRR %q cleanup: %w", nodeName, extrrObj.Name, err)
	}

	if !nodeMatchesOwner(extrrObj, &node) {
		slog.InfoContext(ctx, "target Node was replaced; leaving replacement node unchanged",
			"extrr", extrrObj.Name, "node", nodeName, "nodeUID", node.UID)

		return false, nil
	}

	taint := findTaintByKeyAndEffect(node.Spec.Taints, ReleaseTaintKey, corev1.TaintEffectNoSchedule)
	if taint == nil {
		slog.InfoContext(ctx, "release taint not present; leaving node unchanged",
			"extrr", extrrObj.Name, "node", nodeName)

		return false, nil
	}

	if taint.Value != extrrObj.Name {
		slog.InfoContext(ctx, "release taint belongs to another ExtRR; leaving node unchanged",
			"extrr", extrrObj.Name, "node", nodeName, "taintOwner", taint.Value)

		return false, nil
	}

	nodeToUpdate := node.DeepCopy()
	nodeToUpdate.Spec.Taints = removeTaintByKeyAndEffect(nodeToUpdate.Spec.Taints, ReleaseTaintKey,
		corev1.TaintEffectNoSchedule)
	delete(nodeToUpdate.Labels, managed.ManagedLabelKey)

	if err := r.Patch(ctx, nodeToUpdate, client.StrategicMergeFrom(&node)); err != nil {
		return false, fmt.Errorf("patch node %q for ExtRR %q cleanup: %w", nodeName, extrrObj.Name, err)
	}

	slog.InfoContext(ctx, "removed release taint and managed label from node",
		"extrr", extrrObj.Name, "node", nodeName)

	return true, nil
}

// nodeMatchesOwner returns false when the live Node is not the Node instance
// that owns the ExtRR. ExtRRs without a Node owner reference keep the
// name-based behavior for backward compatibility and manual integrations.
func nodeMatchesOwner(
	extrrObj *nvsentinelv1.ExternalRemediationRequest, node *corev1.Node,
) bool {
	for _, ref := range extrrObj.GetOwnerReferences() {
		if ref.APIVersion == "v1" && ref.Kind == "Node" && ref.Name == node.Name {
			return ref.UID == node.UID
		}
	}

	return true
}

// reconcileNoOpOnFalse is the asymmetric half of ADR-040: Complete=False
// leaves the node released because the external system didn't tell us what
// state it left the node in. Only a True retry or operator delete closes it.
func (r *ExternalRemediationRequestReconciler) reconcileNoOpOnFalse(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (ctrl.Result, error) {
	complete := meta.FindStatusCondition(statusConditions(extrrObj), ConditionExternalRemediationComplete)

	var reason, message string
	if complete != nil {
		reason = complete.Reason
		message = complete.Message
	}

	slog.InfoContext(ctx,
		"external system reported failure; node remains released until operator deletes ExtRR or external system retries",
		"extrr", extrrObj.Name,
		"node", extrrObj.Spec.HealthEvent.NodeName,
		"external_reason", reason,
		"external_message", message,
	)

	return ctrl.Result{}, nil
}

// removeTaintByKeyAndEffect allocates a fresh slice — safe to patch without
// aliasing. A taint is identified by both key and effect.
func removeTaintByKeyAndEffect(
	taints []corev1.Taint, key string, effect corev1.TaintEffect,
) []corev1.Taint {
	out := make([]corev1.Taint, 0, len(taints))

	for i := range taints {
		if taints[i].Key != key || taints[i].Effect != effect {
			out = append(out, taints[i])
		}
	}

	return out
}

// findTaintByKeyAndEffect returns a pointer into the input slice. Do not mutate
// it in place if the slice will be patched later.
func findTaintByKeyAndEffect(
	taints []corev1.Taint, key string, effect corev1.TaintEffect,
) *corev1.Taint {
	for i := range taints {
		if taints[i].Key == key && taints[i].Effect == effect {
			return &taints[i]
		}
	}

	return nil
}

func (r *ExternalRemediationRequestReconciler) transitionReleased(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
	status metav1.ConditionStatus, reason, message string,
) (bool, error) {
	existing := statusConditions(extrrObj)
	conditions := append([]metav1.Condition(nil), existing...)

	meta.SetStatusCondition(&conditions, metav1.Condition{
		Type:    ConditionNVSentinelOwnershipReleased,
		Status:  status,
		Reason:  reason,
		Message: message,
	})

	return r.patchStatusConditions(ctx, extrrObj, conditions)
}

// statusConditions is nil-safe for a freshly-created ExtRR with no Status yet.
func statusConditions(extrrObj *nvsentinelv1.ExternalRemediationRequest) []metav1.Condition {
	if extrrObj.Status == nil {
		return nil
	}

	return condition.ToMetav1Slice(extrrObj.Status.Conditions)
}

// patchStatusConditions re-fetches before patching to narrow the conflict
// window with the external system writing ExternalRemediationComplete. The
// MergeFrom patch replaces the whole conditions array — a concurrent write
// in the gap gets clobbered, but the next reconcile re-observes and
// converges. SSA would close this race but isn't worth the migration.
func (r *ExternalRemediationRequestReconciler) patchStatusConditions(
	ctx context.Context,
	extrrObj *nvsentinelv1.ExternalRemediationRequest,
	conditions []metav1.Condition,
) (bool, error) {
	var latest nvsentinelv1.ExternalRemediationRequest
	if err := r.Get(ctx, client.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}, &latest); err != nil {
		return false, fmt.Errorf("refreshing ExternalRemediationRequest %s before status patch: %w", extrrObj.Name, err)
	}

	updated := latest.DeepCopy()
	if updated.Status == nil {
		updated.Status = &protos.ExternalRemediationRequestStatus{}
	}

	updated.Status.Conditions = condition.FromMetav1Slice(conditions)

	if reflect.DeepEqual(latest.Status, updated.Status) {
		return false, nil
	}

	if err := r.Status().Patch(ctx, updated, client.MergeFrom(&latest)); err != nil {
		return false, fmt.Errorf("patching ExternalRemediationRequest %s status: %w", extrrObj.Name, err)
	}

	slog.InfoContext(ctx, "ExternalRemediationRequest status conditions updated", "name", extrrObj.Name)

	return true, nil
}

func (r *ExternalRemediationRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		// nolint:staticcheck // SA1019: project-wide events.k8s.io migration pending.
		r.Recorder = mgr.GetEventRecorderFor("externalremediationrequest-controller")
	}

	if r.NodeLock == nil {
		r.NodeLock = distributedlock.NewNodeLock(
			mgr.GetClient(), mgr.GetScheme(), r.LockNamespace, metrics.JanitorLockMetrics{},
		)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&nvsentinelv1.ExternalRemediationRequest{}).
		Named("externalremediationrequest").
		Complete(r)
}
