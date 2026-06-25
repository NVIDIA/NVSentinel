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

	"github.com/nvidia/nvsentinel/commons/pkg/managed"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	nvsentinelv1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/condition"
	"github.com/nvidia/nvsentinel/janitor/pkg/distributedlock"
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

	eventReasonReleaseTaintApplied   = "ReleaseTaintApplied"
	eventReasonReleaseTaintRemoved   = "ReleaseTaintRemoved"
	eventReasonOperatorDeleteRequest = "OperatorDeleteRequested"
	eventReasonApplyAbandoned        = "ApplyAbandoned"

	// closeReason* qualifiers disambiguate which cleanup path closed the ExtRR
	// in the ReleaseTaintRemoved event message.
	closeReasonExternalRemediationCompleteTrue = "ExternalRemediationCompleteTrue"
	closeReasonOperatorInitiated               = "OperatorInitiated"

	// applyAbandonedAnnotation marks an ExtRR whose apply path gave up after
	// applyAbandonAfter elapsed with the target Node still missing. Subsequent
	// reconciles short-circuit; only operator delete drives cleanup from here.
	applyAbandonedAnnotation = "nvsentinel.dgxc.nvidia.com/apply-abandoned"
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
// +kubebuilder:rbac:groups=nvsentinel.dgxc.nvidia.com,resources=externalremediationrequests,verbs=get;list;watch;update;patch
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

// dispatch routes to one of the five non-init ADR-040 branches.
func (r *ExternalRemediationRequestReconciler) dispatch(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (ctrl.Result, error) {
	conds := statusConditions(extrrObj)

	switch {
	case !extrrObj.DeletionTimestamp.IsZero():
		return r.reconcileCleanupOnDeletion(ctx, extrrObj)

	case meta.IsStatusConditionPresentAndEqual(conds, ConditionNVSentinelOwnershipReleased, metav1.ConditionUnknown):
		return r.reconcileApply(ctx, extrrObj)

	case meta.IsStatusConditionTrue(conds, ConditionExternalRemediationComplete):
		return r.reconcileCleanupAfterComplete(ctx, extrrObj)

	case meta.IsStatusConditionFalse(conds, ConditionExternalRemediationComplete):
		return r.reconcileNoOpOnFalse(ctx, extrrObj)

	default:
		// Steady state: Released=True awaiting the external system.
		return ctrl.Result{}, nil
	}
}

// nodeMissingRequeue covers the cluster-autoscaler / kubelet-registration race.
const nodeMissingRequeue = 30 * time.Second

// nodeLockRequeue matches the sibling janitor controllers' lock-retry cadence.
const nodeLockRequeue = 2 * time.Second

// applyAbandonAfter bounds how long the apply path retries on a missing Node
// before giving up. var (not const) so envtests can override the threshold.
var applyAbandonAfter = 10 * time.Minute

// reconcileApply applies the release taint + managed=false in one PATCH then
// transitions NVSentinelOwnershipReleased=True. Node not found → transient
// requeue; already-applied → idempotent fast path. Other API errors propagate
// and controller-runtime requeues with backoff.
//
// Acquires the cross-controller node-level lock before any node mutation so a
// concurrent RebootNode / TerminateNode / GPUReset can't act on the same node.
// The lock is released on Complete=True cleanup (branch 4) and on operator
// delete (branch 2); the lease's ownerReference catches the failure case.
//
//nolint:cyclop // distinct apiserver failure modes; inline reads cleaner than a fan-out.
func (r *ExternalRemediationRequestReconciler) reconcileApply(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (ctrl.Result, error) {
	if extrrObj.GetAnnotations()[applyAbandonedAnnotation] == "true" {
		return ctrl.Result{}, nil
	}

	nodeName := extrrObj.Spec.HealthEvent.NodeName

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			if time.Since(extrrObj.CreationTimestamp.Time) > applyAbandonAfter {
				return ctrl.Result{}, r.abandonApply(ctx, extrrObj, nodeName)
			}

			slog.WarnContext(ctx, "target node not found; requeueing",
				"extrr", extrrObj.Name, "node", nodeName, "requeueAfter", nodeMissingRequeue)

			return ctrl.Result{RequeueAfter: nodeMissingRequeue}, nil
		}

		return ctrl.Result{}, fmt.Errorf("get node %q for ExtRR %q: %w", nodeName, extrrObj.Name, err)
	}

	if !r.NodeLock.LockNode(ctx, extrrObj, nodeName) {
		slog.InfoContext(ctx, "another maintenance resource holds the node lock; requeueing",
			"extrr", extrrObj.Name, "node", nodeName)

		return ctrl.Result{RequeueAfter: nodeLockRequeue}, nil
	}

	// We hold the lock, so any taint at our key is ours.
	if findTaintByKey(node.Spec.Taints, ReleaseTaintKey) != nil &&
		node.Labels[managed.ManagedLabelKey] == managed.ManagedLabelValueFalse {
		slog.InfoContext(ctx, "release taint and managed=false label already in place; transitioning condition",
			"extrr", extrrObj.Name, "node", nodeName)

		msg := fmt.Sprintf("release taint %s=%s and managed=false label already present on node %q",
			ReleaseTaintKey, extrrObj.Name, nodeName)

		return ctrl.Result{}, r.transitionToReleaseSuccess(ctx, extrrObj, msg)
	}

	nodeToUpdate := node.DeepCopy()

	if findTaintByKey(nodeToUpdate.Spec.Taints, ReleaseTaintKey) == nil {
		nodeToUpdate.Spec.Taints = append(nodeToUpdate.Spec.Taints, corev1.Taint{
			Key:    ReleaseTaintKey,
			Value:  extrrObj.Name,
			Effect: corev1.TaintEffectNoSchedule,
		})
	}

	if nodeToUpdate.Labels == nil {
		nodeToUpdate.Labels = map[string]string{}
	}

	nodeToUpdate.Labels[managed.ManagedLabelKey] = managed.ManagedLabelValueFalse

	if err := r.Patch(ctx, nodeToUpdate, client.StrategicMergeFrom(&node)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch node %q with release taint + managed=false: %w", nodeName, err)
	}

	slog.InfoContext(ctx, "applied release taint and managed=false label to node",
		"extrr", extrrObj.Name, "node", nodeName)

	msg := fmt.Sprintf("applied release taint %s=%s and managed=false label to node %q",
		ReleaseTaintKey, extrrObj.Name, nodeName)

	return ctrl.Result{}, r.transitionToReleaseSuccess(ctx, extrrObj, msg)
}

// abandonApply gives up on the apply path after applyAbandonAfter elapsed with
// the Node still missing. Fires the released{apply_abandoned} counter, emits a
// Warning event, and stamps an annotation so subsequent reconciles short-circuit.
// The ExtRR's conditions are left as-is; only operator delete drives cleanup
// from here.
func (r *ExternalRemediationRequestReconciler) abandonApply(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest, nodeName string,
) error {
	slog.WarnContext(ctx, "abandoning ExtRR apply: target node missing past threshold",
		"extrr", extrrObj.Name, "node", nodeName,
		"age", time.Since(extrrObj.CreationTimestamp.Time), "threshold", applyAbandonAfter)

	metrics.GlobalMetrics.IncExtRRTotal(metrics.ExtRRPhaseReleased, metrics.ExtRRResultApplyAbandoned)
	r.emitEvent(extrrObj, corev1.EventTypeWarning, eventReasonApplyAbandoned,
		fmt.Sprintf("Node %q not found after %s; abandoning apply. Delete this ExtRR to clear.",
			nodeName, applyAbandonAfter))

	updated := extrrObj.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}

	updated.Annotations[applyAbandonedAnnotation] = "true"

	if err := r.Patch(ctx, updated, client.MergeFrom(extrrObj)); err != nil {
		return fmt.Errorf("stamping apply-abandoned annotation on ExtRR %s: %w", extrrObj.Name, err)
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
	metrics.GlobalMetrics.AdjustExtRROpen(extrrNodeLabel(extrrObj), recommendedActionLabel(extrrObj),
		metrics.ExtRROpenStateAwaiting, 1)
	r.emitEvent(extrrObj, corev1.EventTypeNormal, eventReasonReleaseTaintApplied, message)

	return nil
}

// reconcileCleanupAfterComplete scrubs the Node when Complete=True; the ExtRR
// stays as a historical record until an operator deletes it.
func (r *ExternalRemediationRequestReconciler) reconcileCleanupAfterComplete(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (ctrl.Result, error) {
	changed, err := r.reconcileCleanup(ctx, extrrObj)
	if err != nil {
		return ctrl.Result{}, err
	}

	if changed {
		r.recordClose(extrrObj, metrics.ExtRRResultSuccess, closeReasonExternalRemediationCompleteTrue)
		// external_response co-emits with closed only on the pass that
		// actually scrubbed; drift cases are deliberately uncounted.
		metrics.GlobalMetrics.IncExtRRTotal(metrics.ExtRRPhaseExternalResponse, metrics.ExtRRResultSuccess)
	}

	// Release the node-level lock — the external system has finished and the
	// node is back in NVSentinel's hands. CheckUnlock is idempotent.
	r.NodeLock.CheckUnlock(ctx, extrrObj, extrrObj.Spec.HealthEvent.NodeName)

	return ctrl.Result{}, nil
}

// reconcileCleanupOnDeletion runs the cleanup PATCH then drops the finalizer.
// Idempotent against post-Complete=True state: operator_deleted only fires
// when this path actually performed the cleanup PATCH.
func (r *ExternalRemediationRequestReconciler) reconcileCleanupOnDeletion(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(extrrObj, ExternalRemediationFinalizer) {
		return ctrl.Result{}, nil
	}

	r.emitEvent(extrrObj, corev1.EventTypeNormal, eventReasonOperatorDeleteRequest,
		"deletion requested; running cleanup before releasing finalizer")

	changed, err := r.reconcileCleanup(ctx, extrrObj)
	if err != nil {
		return ctrl.Result{}, err
	}

	if changed {
		r.recordClose(extrrObj, metrics.ExtRRResultOperatorDeleted, closeReasonOperatorInitiated)
	}

	// Release the lock before removing the finalizer. K8s GC via the lease's
	// ownerReference covers the failure case, but the explicit unlock keeps
	// us consistent with the sibling controllers and shortens the window
	// where the lease could outlive its owner.
	r.NodeLock.CheckUnlock(ctx, extrrObj, extrrObj.Spec.HealthEvent.NodeName)

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

// recordClose emits the close metric, decrements ExtRROpen, observes age,
// and emits the ReleaseTaintRemoved event.
func (r *ExternalRemediationRequestReconciler) recordClose(
	extrrObj *nvsentinelv1.ExternalRemediationRequest, result, closeReason string,
) {
	node := extrrNodeLabel(extrrObj)
	action := recommendedActionLabel(extrrObj)

	metrics.GlobalMetrics.IncExtRRTotal(metrics.ExtRRPhaseClosed, result)
	metrics.GlobalMetrics.AdjustExtRROpen(node, action, metrics.ExtRROpenStateAwaiting, -1)

	if !extrrObj.CreationTimestamp.IsZero() {
		metrics.GlobalMetrics.ObserveExtRRAge(action, result,
			time.Since(extrrObj.CreationTimestamp.Time).Seconds())
	}

	r.emitEvent(extrrObj, corev1.EventTypeNormal, eventReasonReleaseTaintRemoved,
		fmt.Sprintf("release taint and managed=false label removed (%s)", closeReason))
}

// reconcileCleanup removes the release taint and the managed label from the
// Node. Returns true when the PATCH mutated the Node. The node-level lock
// guarantees we own the taint at our key, so no value-match check is needed.
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

	nodeToUpdate := node.DeepCopy()
	changed := false

	if findTaintByKey(nodeToUpdate.Spec.Taints, ReleaseTaintKey) != nil {
		nodeToUpdate.Spec.Taints = removeTaintByKey(nodeToUpdate.Spec.Taints, ReleaseTaintKey)
		changed = true
	}

	if _, ok := nodeToUpdate.Labels[managed.ManagedLabelKey]; ok {
		delete(nodeToUpdate.Labels, managed.ManagedLabelKey)

		changed = true
	}

	if !changed {
		return false, nil
	}

	if err := r.Patch(ctx, nodeToUpdate, client.StrategicMergeFrom(&node)); err != nil {
		return false, fmt.Errorf("patch node %q for ExtRR %q cleanup: %w", nodeName, extrrObj.Name, err)
	}

	slog.InfoContext(ctx, "removed release taint and managed label from node",
		"extrr", extrrObj.Name, "node", nodeName)

	return true, nil
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

// removeTaintByKey allocates a fresh slice — safe to patch without aliasing.
func removeTaintByKey(taints []corev1.Taint, key string) []corev1.Taint {
	out := make([]corev1.Taint, 0, len(taints))

	for i := range taints {
		if taints[i].Key != key {
			out = append(out, taints[i])
		}
	}

	return out
}

// findTaintByKey returns a pointer into the input slice — don't mutate in place
// if the slice will be patched later.
func findTaintByKey(taints []corev1.Taint, key string) *corev1.Taint {
	for i := range taints {
		if taints[i].Key == key {
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
		r.NodeLock = distributedlock.NewNodeLock(mgr.GetClient(), r.LockNamespace)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&nvsentinelv1.ExternalRemediationRequest{}).
		Named("externalremediationrequest").
		Complete(r)
}
