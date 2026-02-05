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

package healthevents

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
)

// QuarantineController reconciles HealthEvent resources to quarantine nodes.
// It watches for new fatal health events (phase=New, isFatal=true) and
// cordons the affected node to prevent new workloads from being scheduled.
type QuarantineController struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// MaxConcurrentReconciles is the maximum number of concurrent Reconciles.
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=nvsentinel.nvidia.com,resources=healthevents,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=nvsentinel.nvidia.com,resources=healthevents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles HealthEvent reconciliation.
// It processes events in phase=New with isFatal=true, cordons the node,
// and transitions the event to phase=Quarantined.
func (r *QuarantineController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := klog.FromContext(ctx).WithValues("healthevent", req.Name)
	log.V(1).Info("Reconciling HealthEvent")

	// Fetch the HealthEvent
	var healthEvent nvsentinelv1alpha1.HealthEvent
	if err := r.Get(ctx, req.NamespacedName, &healthEvent); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("HealthEvent not found, likely deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get HealthEvent")
		return ctrl.Result{}, err
	}

	// Skip if already processed or not in New phase
	// Note: Empty phase ("") is treated as New because status is a subresource
	// and isn't persisted during resource creation
	if healthEvent.Status.Phase != nvsentinelv1alpha1.PhaseNew && healthEvent.Status.Phase != "" {
		log.V(2).Info("Skipping HealthEvent not in New phase", "phase", healthEvent.Status.Phase)
		return ctrl.Result{}, nil
	}

	// Skip non-fatal events
	if !healthEvent.Spec.IsFatal {
		log.V(2).Info("Skipping non-fatal HealthEvent")
		// Mark as resolved since we won't process it further
		return r.markAsResolved(ctx, &healthEvent, "NonFatal", "Event is not fatal, no quarantine needed")
	}

	// Check if quarantine is skipped via overrides
	if healthEvent.Spec.Overrides != nil &&
		healthEvent.Spec.Overrides.Quarantine != nil &&
		healthEvent.Spec.Overrides.Quarantine.Skip {
		log.Info("Quarantine skipped via override", "nodeName", healthEvent.Spec.NodeName)
		return r.markAsCancelled(ctx, &healthEvent, "QuarantineSkipped", "Quarantine skipped via override")
	}

	// Get the node
	nodeName := healthEvent.Spec.NodeName
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			log.Error(err, "Node not found", "nodeName", nodeName)
			r.Recorder.Eventf(&healthEvent, corev1.EventTypeWarning, "NodeNotFound",
				"Node %s not found, cannot quarantine", nodeName)
			// Don't retry - node doesn't exist
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get node", "nodeName", nodeName)
		return ctrl.Result{}, err
	}

	// Check if node is already cordoned
	if node.Spec.Unschedulable {
		log.Info("Node already cordoned", "nodeName", nodeName)
		// Node is already cordoned, transition to Quarantined
		return r.transitionToQuarantined(ctx, &healthEvent, "NodeAlreadyCordoned",
			"Node was already cordoned")
	}

	// Cordon the node
	log.Info("Cordoning node", "nodeName", nodeName)
	if err := r.cordonNode(ctx, &node); err != nil {
		log.Error(err, "Failed to cordon node", "nodeName", nodeName)
		r.Recorder.Eventf(&healthEvent, corev1.EventTypeWarning, "CordonFailed",
			"Failed to cordon node %s: %v", nodeName, err)
		// Retry after backoff
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	// Record event
	r.Recorder.Eventf(&healthEvent, corev1.EventTypeNormal, "NodeCordoned",
		"Node %s cordoned due to fatal GPU error", nodeName)
	r.Recorder.Eventf(&node, corev1.EventTypeWarning, "Quarantined",
		"Node cordoned by NVSentinel due to HealthEvent %s", healthEvent.Name)

	// Transition to Quarantined
	return r.transitionToQuarantined(ctx, &healthEvent, "FatalError",
		fmt.Sprintf("Node cordoned due to fatal %s error", healthEvent.Spec.ComponentClass))
}

// cordonNode sets the node's Unschedulable field to true.
func (r *QuarantineController) cordonNode(ctx context.Context, node *corev1.Node) error {
	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = true

	// Add taint for immediate effect on pending pods
	quarantineTaint := corev1.Taint{
		Key:    "nvsentinel.nvidia.com/quarantine",
		Value:  "true",
		Effect: corev1.TaintEffectNoSchedule,
	}

	// Check if taint already exists
	hasTaint := false
	for _, t := range node.Spec.Taints {
		if t.Key == quarantineTaint.Key {
			hasTaint = true
			break
		}
	}
	if !hasTaint {
		node.Spec.Taints = append(node.Spec.Taints, quarantineTaint)
	}

	return r.Patch(ctx, node, patch)
}

// transitionToQuarantined updates the HealthEvent to Quarantined phase.
func (r *QuarantineController) transitionToQuarantined(
	ctx context.Context,
	healthEvent *nvsentinelv1alpha1.HealthEvent,
	reason, message string,
) (ctrl.Result, error) {
	log := klog.FromContext(ctx)

	patch := client.MergeFrom(healthEvent.DeepCopy())

	// Update phase
	healthEvent.Status.Phase = nvsentinelv1alpha1.PhaseQuarantined
	healthEvent.Status.ObservedGeneration = healthEvent.Generation

	// Update conditions
	now := metav1.Now()
	SetCondition(&healthEvent.Status.Conditions, nvsentinelv1alpha1.HealthEventCondition{
		Type:               nvsentinelv1alpha1.ConditionNodeQuarantined,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: healthEvent.Generation,
	})

	if err := r.Status().Patch(ctx, healthEvent, patch); err != nil {
		log.Error(err, "Failed to update HealthEvent status")
		return ctrl.Result{}, err
	}

	log.Info("HealthEvent transitioned to Quarantined",
		"healthEvent", healthEvent.Name,
		"nodeName", healthEvent.Spec.NodeName)

	// Increment metrics
	quarantineActionsTotal.WithLabelValues(healthEvent.Spec.NodeName, "success").Inc()

	return ctrl.Result{}, nil
}

// markAsResolved updates the HealthEvent to Resolved phase for non-fatal events.
func (r *QuarantineController) markAsResolved(
	ctx context.Context,
	healthEvent *nvsentinelv1alpha1.HealthEvent,
	reason, message string,
) (ctrl.Result, error) {
	log := klog.FromContext(ctx)

	patch := client.MergeFrom(healthEvent.DeepCopy())

	healthEvent.Status.Phase = nvsentinelv1alpha1.PhaseResolved
	healthEvent.Status.ObservedGeneration = healthEvent.Generation

	now := metav1.Now()
	healthEvent.Status.ResolvedAt = &now

	SetCondition(&healthEvent.Status.Conditions, nvsentinelv1alpha1.HealthEventCondition{
		Type:               nvsentinelv1alpha1.ConditionResolved,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: healthEvent.Generation,
	})

	if err := r.Status().Patch(ctx, healthEvent, patch); err != nil {
		log.Error(err, "Failed to update HealthEvent status")
		return ctrl.Result{}, err
	}

	log.V(1).Info("HealthEvent marked as Resolved", "healthEvent", healthEvent.Name)
	return ctrl.Result{}, nil
}

// markAsCancelled updates the HealthEvent to Cancelled phase.
func (r *QuarantineController) markAsCancelled(
	ctx context.Context,
	healthEvent *nvsentinelv1alpha1.HealthEvent,
	reason, message string,
) (ctrl.Result, error) {
	log := klog.FromContext(ctx)

	patch := client.MergeFrom(healthEvent.DeepCopy())

	healthEvent.Status.Phase = nvsentinelv1alpha1.PhaseCancelled
	healthEvent.Status.ObservedGeneration = healthEvent.Generation

	now := metav1.Now()
	healthEvent.Status.ResolvedAt = &now

	SetCondition(&healthEvent.Status.Conditions, nvsentinelv1alpha1.HealthEventCondition{
		Type:               nvsentinelv1alpha1.ConditionResolved,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: healthEvent.Generation,
	})

	if err := r.Status().Patch(ctx, healthEvent, patch); err != nil {
		log.Error(err, "Failed to update HealthEvent status")
		return ctrl.Result{}, err
	}

	log.Info("HealthEvent cancelled", "healthEvent", healthEvent.Name, "reason", reason)
	quarantineActionsTotal.WithLabelValues(healthEvent.Spec.NodeName, "skipped").Inc()

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
// Note: We intentionally do NOT use phase-based predicates because status subresource
// updates may not trigger watch events reliably. Instead, we filter by phase in Reconcile.
func (r *QuarantineController) SetupWithManager(mgr ctrl.Manager) error {
	// Initialize metrics
	registerMetrics()

	maxConcurrent := r.MaxConcurrentReconciles
	if maxConcurrent == 0 {
		maxConcurrent = 3 // Default
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&nvsentinelv1alpha1.HealthEvent{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: maxConcurrent,
		}).
		Named("quarantine").
		Complete(r)
}
