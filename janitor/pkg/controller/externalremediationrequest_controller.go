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
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/nvidia/nvsentinel/commons/pkg/managed"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	nvsentinelv1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/condition"
	"github.com/nvidia/nvsentinel/janitor/pkg/metrics"
)

const (
	// ExternalRemediationFinalizer guarantees node cleanup runs before the
	// ExtRR is deleted. Per ADR-040 this is the only mechanism by which
	// operators reclaim a node held by a stalled or failed external system.
	ExternalRemediationFinalizer = "nvsentinel.dgxc.nvidia.com/external-remediation-cleanup"

	ConditionNVSentinelOwnershipReleased = "NVSentinelOwnershipReleased"
	ConditionExternalRemediationComplete = "ExternalRemediationComplete"

	reasonInitializing           = "Initializing"
	reasonAwaitingExternalSystem = "AwaitingExternalSystem"

	// ReleaseTaintKey is the taint key the reconciler applies to release a
	// Node from NVSentinel ownership. The taint's value carries the owning
	// ExtRR's metadata.name so cleanup can find only its own taint and
	// `kubectl describe node` surfaces the ExtRR responsible.
	ReleaseTaintKey = "nvsentinel.dgxc.nvidia.com/external-remediation"

	ReasonReleaseTaintApplied = "ReleaseTaintApplied"
	ReasonReleaseTaintFailed  = "ReleaseTaintFailed"

	eventReasonReleaseTaintApplied   = "ReleaseTaintApplied"
	eventReasonReleaseTaintFailed    = "ReleaseTaintFailed"
	eventReasonReleaseTaintRemoved   = "ReleaseTaintRemoved"
	eventReasonOperatorDeleteRequest = "OperatorDeleteRequested"

	// Close-reason qualifiers appended to the ReleaseTaintRemoved event
	// message so the same event reason disambiguates which cleanup path
	// closed the ExtRR.
	closeReasonExternalRemediationCompleteTrue = "ExternalRemediationCompleteTrue"
	closeReasonOperatorInitiated               = "OperatorInitiated"

	// extrrNodeNameIndexKey is the field-indexer key for looking up ExtRRs by
	// their target node. Registered in SetupWithManager so the Node watch's
	// mapper does an O(1) indexed list instead of scanning every ExtRR on
	// every Node event.
	extrrNodeNameIndexKey = "spec.healthEvent.nodeName"
)

// ExternalRemediationRequestReconciler implements the six-branch ADR-040
// state machine driven by the deletion timestamp and the two status
// conditions on the ExtRR.
type ExternalRemediationRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder is auto-populated by SetupWithManager; tests may inject a
	// record.NewFakeRecorder before SetupWithManager runs.
	Recorder record.EventRecorder
}

// labelValueUnknown is the metric-label fallback when no action mapping is
// available. The validating webhook guarantees Spec.HealthEvent.NodeName is
// populated so the node label always has a concrete value; this fallback
// only kicks in for the recommendedAction label when model lookup yields "".
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

// emitEvent tolerates a nil Recorder so tests can construct the reconciler
// without one.
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

// Reconcile drives the ExtRR through its lifecycle. The OTEL span links to
// the originating health-monitor trace via the trace-id / span-id annotations
// fault-remediation stamps on the ExtRR.
func (r *ExternalRemediationRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var extrr nvsentinelv1.ExternalRemediationRequest
	if err := r.Get(ctx, req.NamespacedName, &extrr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The validating webhook (failurePolicy=Fail) enforces that Spec and
	// Spec.HealthEvent are populated. This belt-and-suspenders check survives
	// webhook outages (cert rotation, deployment race) so a poison-pill object
	// can't crashloop the controller. An ExtRR with nil Spec also can't have
	// had a finalizer applied (reconcileInitialize would have crashed), so
	// the apiserver garbage-collects it without our help.
	if extrr.Spec == nil || extrr.Spec.HealthEvent == nil || extrr.Spec.HealthEvent.NodeName == "" {
		slog.ErrorContext(ctx, "ExternalRemediationRequest missing required spec fields; webhook bypass?",
			"name", extrr.Name, "namespace", extrr.Namespace)

		return ctrl.Result{}, nil
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

// needsInitialization returns true if the cleanup finalizer or either initial
// status condition is absent. Checks only presence, never values, so
// conditions set by the external system survive re-entry.
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

// reconcileInitialize attaches the cleanup finalizer (one API call) and seeds
// the initial Unknown conditions (a second, status-subresource API call). A
// partially-initialized ExtRR is recovered cleanly on re-reconcile because
// each step only writes what's missing.
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
		// controller-runtime's own-kind watch re-enqueues this object after
		// the metadata Update; the next reconcile will write initial conditions.
		return ctrl.Result{}, nil
	}

	changed, err := r.setInitialConditions(ctx, extrrObj)
	if err != nil {
		return ctrl.Result{}, err
	}

	if changed {
		// First time the initial conditions actually landed for this ExtRR —
		// count exactly once even if reconcileInitialize is invoked again
		// (idempotent setInitialConditions short-circuits on re-entry).
		metrics.GlobalMetrics.IncExtRRTotal(metrics.ExtRRPhaseCreated, metrics.ExtRRResultNone)
	}

	return ctrl.Result{}, nil
}

// setInitialConditions seeds the two initial Unknown conditions if absent;
// existing conditions are preserved. Returns (true, nil) when conditions
// were actually written.
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

// dispatch is the six-branch ADR-040 state machine. Branch 1 (init) runs
// before dispatch; branches map below.
func (r *ExternalRemediationRequestReconciler) dispatch(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (ctrl.Result, error) {
	conds := statusConditions(extrrObj)

	switch {
	case !extrrObj.DeletionTimestamp.IsZero():
		// Branch 2: deletion — cleanup, then drop the finalizer.
		return r.reconcileCleanupOnDeletion(ctx, extrrObj)

	case meta.IsStatusConditionPresentAndEqual(conds, ConditionNVSentinelOwnershipReleased, metav1.ConditionUnknown):
		// Branch 3: apply path — release taint + managed=false label.
		return r.reconcileApply(ctx, extrrObj)

	case meta.IsStatusConditionTrue(conds, ConditionExternalRemediationComplete):
		// Branch 4: external system reported success — cleanup; ExtRR stays as a historical record.
		return r.reconcileCleanupAfterComplete(ctx, extrrObj)

	case meta.IsStatusConditionFalse(conds, ConditionExternalRemediationComplete):
		// Branch 5: external system reported failure — asymmetric no-op per ADR-040.
		return r.reconcileNoOpOnFalse(ctx, extrrObj)

	default:
		// Branch 6: released, awaiting the external system.
		return ctrl.Result{}, nil
	}
}

// nodeMissingRequeue covers the cluster-autoscaler / kubelet-registration
// race where the target Node may show up shortly.
const nodeMissingRequeue = 30 * time.Second

// reconcileApply (branch 3) takes a fresh ExtRR to released state via a
// single strategic-merge PATCH on the target Node (release taint +
// managed=false label) then transitions NVSentinelOwnershipReleased=True.
//
// Failure modes per ADR-040: drift (taint at our key with a different value)
// / RBAC forbidden → persistent failure (transition to False). Node not
// found → transient, requeue. Taint already at our value → idempotent fast
// path. spec.healthEvent.nodeName is guaranteed non-empty by the validating
// webhook.
//
// Branches share node-and-extrr state, so the inline switch reads better than
// a fan-out into single-use helpers — hence the cyclop nolint below.
//
//nolint:cyclop // distinct apiserver failure modes; see doc above.
func (r *ExternalRemediationRequestReconciler) reconcileApply(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (ctrl.Result, error) {
	nodeName := extrrObj.Spec.HealthEvent.NodeName

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			slog.WarnContext(ctx, "target node not found; requeueing",
				"extrr", extrrObj.Name, "node", nodeName, "requeueAfter", nodeMissingRequeue)

			return ctrl.Result{RequeueAfter: nodeMissingRequeue}, nil
		}

		return ctrl.Result{}, fmt.Errorf("get node %q for ExtRR %q: %w", nodeName, extrrObj.Name, err)
	}

	if existing := findTaintByKey(node.Spec.Taints, ReleaseTaintKey); existing != nil {
		if existing.Value != extrrObj.Name {
			msg := fmt.Sprintf(
				"node %q already tainted by ExternalRemediationRequest %q; another ExtRR owns this node",
				nodeName, existing.Value)
			slog.WarnContext(ctx, "release taint drift detected",
				"extrr", extrrObj.Name, "node", nodeName, "existing_owner", existing.Value)

			return ctrl.Result{}, r.transitionToReleaseFailure(ctx, extrrObj, msg)
		}
		// Taint already in place with our name — verify the label is also present,
		// then transition the condition without issuing a redundant PATCH.
		if node.Labels[managed.ManagedLabelKey] == managed.ManagedLabelValueFalse {
			slog.InfoContext(ctx, "release taint and managed=false label already in place; transitioning condition",
				"extrr", extrrObj.Name, "node", nodeName)

			msg := fmt.Sprintf("release taint %s=%s and managed=false label already present on node %q",
				ReleaseTaintKey, extrrObj.Name, nodeName)

			return ctrl.Result{}, r.transitionToReleaseSuccess(ctx, extrrObj, msg)
		}
		// Taint is right but the label is missing — patch only the label below.
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
		if apierrors.IsForbidden(err) {
			msg := fmt.Sprintf("forbidden to patch node %q: %v", nodeName, err)
			slog.ErrorContext(ctx, "release taint apply forbidden by RBAC",
				"extrr", extrrObj.Name, "node", nodeName, "error", err)

			return ctrl.Result{}, r.transitionToReleaseFailure(ctx, extrrObj, msg)
		}

		return ctrl.Result{}, fmt.Errorf("patch node %q with release taint + managed=false: %w", nodeName, err)
	}

	slog.InfoContext(ctx, "applied release taint and managed=false label to node",
		"extrr", extrrObj.Name, "node", nodeName)

	msg := fmt.Sprintf("applied release taint %s=%s and managed=false label to node %q",
		ReleaseTaintKey, extrrObj.Name, nodeName)

	return ctrl.Result{}, r.transitionToReleaseSuccess(ctx, extrrObj, msg)
}

// transitionToReleaseSuccess marks the apply path complete. Metric / event
// emissions are gated on the status patch actually mutating state, so
// re-reconciles don't double-fire.
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

// transitionToReleaseFailure marks the apply path persistently failed (drift,
// forbidden, missing nodeName). Intentionally skips err_open — the ExtRR is
// terminal-failure, not in-flight; failure tracking is err_total{released,failure}.
func (r *ExternalRemediationRequestReconciler) transitionToReleaseFailure(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest, message string,
) error {
	changed, err := r.transitionReleased(ctx, extrrObj, metav1.ConditionFalse, ReasonReleaseTaintFailed, message)
	if err != nil {
		return err
	}

	if !changed {
		return nil
	}

	metrics.GlobalMetrics.IncExtRRTotal(metrics.ExtRRPhaseReleased, metrics.ExtRRResultFailure)
	r.emitEvent(extrrObj, corev1.EventTypeWarning, eventReasonReleaseTaintFailed, message)

	return nil
}

// reconcileCleanupAfterComplete (branch 4) removes the taint+label after the
// external system reports success. The ExtRR stays as a historical record
// (finalizer still attached); TTL or `kubectl delete extrr` removes it later.
// Observability fires only on the pass that actually mutates state.
func (r *ExternalRemediationRequestReconciler) reconcileCleanupAfterComplete(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (ctrl.Result, error) {
	changed, err := r.reconcileCleanup(ctx, extrrObj)
	if err != nil {
		return ctrl.Result{}, err
	}

	if changed {
		r.recordClose(extrrObj, metrics.ExtRRResultSuccess, closeReasonExternalRemediationCompleteTrue)
		// external_response{success} is co-emitted with closed{success} because
		// the True observation and our cleanup PATCH happen on the same reconcile
		// pass for the first time only. Drift cases (Released=False + Complete=True)
		// are deliberately uncounted here: cleanup is a no-op so `changed` is false,
		// and a "we observed success but couldn't act on it" event is more
		// noise than signal on the dashboards.
		metrics.GlobalMetrics.IncExtRRTotal(metrics.ExtRRPhaseExternalResponse, metrics.ExtRRResultSuccess)
	}

	return ctrl.Result{}, nil
}

// reconcileCleanupOnDeletion (branch 2) runs when `kubectl delete extrr` sets
// the DeletionTimestamp. Cleanup PATCH then finalizer removal so the
// apiserver can garbage-collect the ExtRR. Idempotent against branch-4
// post-True state — if cleanup already ran, we skip straight to the
// finalizer; the operator_deleted close counter only fires when this path
// actually performed the cleanup.
func (r *ExternalRemediationRequestReconciler) reconcileCleanupOnDeletion(
	ctx context.Context, extrrObj *nvsentinelv1.ExternalRemediationRequest,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(extrrObj, ExternalRemediationFinalizer) {
		// Finalizer already gone — nothing left for us to do.
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

// recordClose emits the metrics + event triple that fires on every ExtRR close.
// result is one of metrics.ExtRRResult{Success,OperatorDeleted}; closeReason
// disambiguates which path closed it in the human-readable event message.
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

// reconcileCleanup is the shared cleanup PATCH. Removes the release taint
// only if its value matches this ExtRR's name (drift-safe — another ExtRR's
// taint is left in place) and deletes the managed label entirely (per
// ADR-040, absence rather than "true" so there's no rotting hint). Returns
// (true, nil) when the PATCH actually mutated the Node; (false, nil) when
// there was nothing to clean (taint absent, label absent, or Node gone).
//
// Same dispatch-over-drift-cases shape as reconcileApply — inline branches
// read better than fanning out into single-use helpers.
//
//nolint:cyclop // dispatch over drift cases; see doc above.
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

	if existing := findTaintByKey(nodeToUpdate.Spec.Taints, ReleaseTaintKey); existing != nil {
		switch existing.Value {
		case extrrObj.Name:
			nodeToUpdate.Spec.Taints = removeTaintByKey(nodeToUpdate.Spec.Taints, ReleaseTaintKey)
			changed = true
		default:
			// Drift: another ExtRR claims the taint. Leave it alone — that ExtRR's
			// own cleanup path will remove it. Logging only; not an error.
			slog.WarnContext(ctx, "release taint owned by a different ExtRR; leaving in place during cleanup",
				"extrr", extrrObj.Name, "node", nodeName, "existing_owner", existing.Value)
		}
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

// reconcileNoOpOnFalse (branch 5) is the asymmetric half of ADR-040.
// ExternalRemediationComplete=False means the external system gave up
// without telling us what state the node is in (mid-RMA, partial repair,
// unvalidated swap, ...) — returning it to workloads would be unsafe.
// The taint+label STAY. The node remains released until the external system
// retries with True (→ branch 4) or an operator deletes the ExtRR (→
// branch 2). This function deliberately mutates nothing.
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

// removeTaintByKey returns a new slice with the matching key removed. Fresh
// backing array — callers can patch the result without aliasing the input.
func removeTaintByKey(taints []corev1.Taint, key string) []corev1.Taint {
	out := make([]corev1.Taint, 0, len(taints))

	for i := range taints {
		if taints[i].Key != key {
			out = append(out, taints[i])
		}
	}

	return out
}

// findTaintByKey returns a pointer into the input slice — callers must not
// mutate in place if the slice will be patched later.
func findTaintByKey(taints []corev1.Taint, key string) *corev1.Taint {
	for i := range taints {
		if taints[i].Key == key {
			return &taints[i]
		}
	}

	return nil
}

// transitionReleased sets NVSentinelOwnershipReleased via a status-subresource
// merge patch. Returns (true, nil) when the patch actually mutated state.
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

// statusConditions returns the conditions as []metav1.Condition, nil-safe on
// a freshly-created ExtRR with no status.
func statusConditions(extrrObj *nvsentinelv1.ExternalRemediationRequest) []metav1.Condition {
	if extrrObj.Status == nil {
		return nil
	}

	return condition.ToMetav1Slice(extrrObj.Status.Conditions)
}

// patchStatusConditions writes conditions via a status-subresource merge
// patch. Re-fetches first to narrow the conflict window with concurrent
// writers (e.g. an external system patching ExternalRemediationComplete).
// Returns (true, nil) when the patch actually mutated state.
//
// MergeFrom generates a JSON merge patch, which replaces the entire
// conditions array. A concurrent writer that flips ExternalRemediationComplete
// between our Get and our Patch will have that update clobbered; the next
// reconcile re-observes and converges. Server-side apply with a field manager
// would close this race entirely but isn't worth the migration cost given
// (a) the rarity of concurrent status writers and (b) the convergence guarantee.
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

// SetupWithManager wires the ExtRR controller and a secondary Node watch
// (mapped to ExtRRs by spec.healthEvent.nodeName) so taint or label drift on
// the released node re-enqueues the owning ExtRR.
func (r *ExternalRemediationRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		// nolint:staticcheck // SA1019: GetEventRecorderFor returns the core/v1
		// events recorder all sibling reconcilers in this package use; the
		// migration to events.k8s.io/v1 is a project-wide change.
		r.Recorder = mgr.GetEventRecorderFor("externalremediationrequest-controller")
	}

	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&nvsentinelv1.ExternalRemediationRequest{},
		extrrNodeNameIndexKey,
		indexExtRRByNodeName,
	); err != nil {
		return fmt.Errorf("registering ExtRR node-name field indexer: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&nvsentinelv1.ExternalRemediationRequest{}).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.mapNodeToExtRRs),
			builder.WithPredicates(nodeReleaseStateChangedPredicate()),
		).
		Named("externalremediationrequest").
		Complete(r)
}

// indexExtRRByNodeName extracts spec.healthEvent.nodeName for the field
// indexer. Returns nil for malformed ExtRRs (no Spec / HealthEvent) so the
// indexer doesn't crash if the webhook ever lets one through.
func indexExtRRByNodeName(o client.Object) []string {
	extrr, ok := o.(*nvsentinelv1.ExternalRemediationRequest)
	if !ok || extrr.Spec == nil || extrr.Spec.HealthEvent == nil || extrr.Spec.HealthEvent.NodeName == "" {
		return nil
	}

	return []string{extrr.Spec.HealthEvent.NodeName}
}

// nodeReleaseStateChangedPredicate drops Node events that don't touch the
// release-state surface (the managed label and the release taint). Nodes
// churn constantly via kubelet heartbeats and status updates; without this
// filter, every ExtRR re-reconciles on every kubelet sync.
func nodeReleaseStateChangedPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(_ event.CreateEvent) bool { return true },
		DeleteFunc:  func(_ event.DeleteEvent) bool { return true },
		GenericFunc: func(_ event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, ok := e.ObjectOld.(*corev1.Node)
			if !ok {
				return false
			}

			newNode, ok := e.ObjectNew.(*corev1.Node)
			if !ok {
				return false
			}

			if oldNode.Labels[managed.ManagedLabelKey] != newNode.Labels[managed.ManagedLabelKey] {
				return true
			}

			oldTaint := findTaintByKey(oldNode.Spec.Taints, ReleaseTaintKey)
			newTaint := findTaintByKey(newNode.Spec.Taints, ReleaseTaintKey)

			if (oldTaint == nil) != (newTaint == nil) {
				return true
			}

			if oldTaint != nil && newTaint != nil && oldTaint.Value != newTaint.Value {
				return true
			}

			return false
		},
	}
}

// mapNodeToExtRRs enqueues every ExtRR whose spec.healthEvent.nodeName matches
// the given Node. Uses the field-indexer registered in SetupWithManager so
// the lookup is bounded by the number of ExtRRs targeting this Node (typically
// 0 or 1), not the total ExtRR population.
func (r *ExternalRemediationRequestReconciler) mapNodeToExtRRs(ctx context.Context, obj client.Object) []ctrl.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}

	var extrrs nvsentinelv1.ExternalRemediationRequestList
	if err := r.List(ctx, &extrrs, client.MatchingFields{extrrNodeNameIndexKey: node.Name}); err != nil {
		slog.ErrorContext(ctx, "listing ExternalRemediationRequests by node-name index",
			"node", node.Name, "error", err)

		return nil
	}

	requests := make([]ctrl.Request, 0, len(extrrs.Items))
	for i := range extrrs.Items {
		requests = append(requests, ctrl.Request{
			NamespacedName: client.ObjectKey{
				Name:      extrrs.Items[i].Name,
				Namespace: extrrs.Items[i].Namespace,
			},
		})
	}

	return requests
}
