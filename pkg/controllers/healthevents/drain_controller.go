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
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
)

// DrainController reconciles HealthEvent resources to drain pods from quarantined nodes.
// It watches for health events in Quarantined phase and evicts pods from the affected node.
type DrainController struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// MaxConcurrentReconciles is the maximum number of concurrent Reconciles.
	MaxConcurrentReconciles int

	// GracePeriodSeconds is the default grace period for pod eviction.
	// If not set, defaults to 30 seconds.
	GracePeriodSeconds int64

	// DeleteEmptyDirData allows eviction of pods with emptyDir volumes.
	DeleteEmptyDirData bool

	// IgnoreDaemonSets skips DaemonSet-managed pods during drain.
	IgnoreDaemonSets bool
}

// DrainResult tracks the outcome of a drain operation.
type DrainResult struct {
	PodsEvicted  int
	PodsFailed   int
	PodsSkipped  int
	ErrorMessage string
}

// +kubebuilder:rbac:groups=nvsentinel.nvidia.com,resources=healthevents,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=nvsentinel.nvidia.com,resources=healthevents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles HealthEvent reconciliation for draining.
// It processes events in phase=Quarantined, evicts pods from the node,
// and transitions the event to phase=Drained.
func (r *DrainController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := klog.FromContext(ctx).WithValues("healthevent", req.Name)
	log.V(1).Info("Reconciling HealthEvent for drain")

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

	// Skip if not in Quarantined phase
	if healthEvent.Status.Phase != nvsentinelv1alpha1.PhaseQuarantined {
		log.V(2).Info("Skipping HealthEvent not in Quarantined phase", "phase", healthEvent.Status.Phase)
		return ctrl.Result{}, nil
	}

	// Check if drain is skipped via overrides
	if healthEvent.Spec.Overrides != nil &&
		healthEvent.Spec.Overrides.Drain != nil &&
		healthEvent.Spec.Overrides.Drain.Skip {
		log.Info("Drain skipped via override", "nodeName", healthEvent.Spec.NodeName)
		return r.transitionToDrained(ctx, &healthEvent, "DrainSkipped", "Drain skipped via override", DrainResult{})
	}

	// Get pods on the node
	nodeName := healthEvent.Spec.NodeName
	pods, err := r.getPodsOnNode(ctx, nodeName)
	if err != nil {
		log.Error(err, "Failed to list pods on node", "nodeName", nodeName)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	// Filter pods to evict
	podsToEvict := r.filterPodsForEviction(pods)
	log.Info("Draining node", "nodeName", nodeName, "totalPods", len(pods), "podsToEvict", len(podsToEvict))

	// Update phase to Draining
	if err := r.updatePhaseToDraining(ctx, &healthEvent); err != nil {
		log.Error(err, "Failed to update phase to Draining")
		return ctrl.Result{}, err
	}

	// Evict pods
	result := r.evictPods(ctx, podsToEvict, nodeName)

	// Record events
	if result.PodsEvicted > 0 {
		r.Recorder.Eventf(&healthEvent, corev1.EventTypeNormal, "PodsEvicted",
			"Evicted %d pods from node %s", result.PodsEvicted, nodeName)
	}
	if result.PodsFailed > 0 {
		r.Recorder.Eventf(&healthEvent, corev1.EventTypeWarning, "EvictionFailed",
			"Failed to evict %d pods from node %s: %s", result.PodsFailed, nodeName, result.ErrorMessage)
	}

	// If there are failures, requeue to retry
	if result.PodsFailed > 0 {
		log.Info("Some pods failed to evict, will retry",
			"podsEvicted", result.PodsEvicted,
			"podsFailed", result.PodsFailed,
		)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// All pods evicted, transition to Drained
	return r.transitionToDrained(ctx, &healthEvent, "DrainComplete",
		fmt.Sprintf("Successfully evicted %d pods from node %s", result.PodsEvicted, nodeName),
		result)
}

// getPodsOnNode lists all pods running on the specified node.
func (r *DrainController) getPodsOnNode(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList, &client.ListOptions{
		FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": nodeName}),
	}); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	return podList.Items, nil
}

// filterPodsForEviction filters pods that should be evicted.
// It excludes mirror pods, completed pods, and optionally DaemonSet pods.
func (r *DrainController) filterPodsForEviction(pods []corev1.Pod) []corev1.Pod {
	var filtered []corev1.Pod

	for i := range pods {
		pod := &pods[i]

		// Skip mirror pods (static pods)
		if _, ok := pod.Annotations[corev1.MirrorPodAnnotationKey]; ok {
			continue
		}

		// Skip completed pods
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		// Skip DaemonSet pods if configured
		if r.IgnoreDaemonSets && isDaemonSetPod(pod) {
			continue
		}

		// Skip pods with local storage if not allowed
		if !r.DeleteEmptyDirData && hasLocalStorage(pod) {
			continue
		}

		filtered = append(filtered, pods[i])
	}

	return filtered
}

// isDaemonSetPod checks if a pod is managed by a DaemonSet.
func isDaemonSetPod(pod *corev1.Pod) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

// hasLocalStorage checks if a pod uses emptyDir volumes.
func hasLocalStorage(pod *corev1.Pod) bool {
	for _, vol := range pod.Spec.Volumes {
		if vol.EmptyDir != nil {
			return true
		}
	}
	return false
}

// evictPods evicts the specified pods.
func (r *DrainController) evictPods(ctx context.Context, pods []corev1.Pod, nodeName string) DrainResult {
	log := klog.FromContext(ctx)
	result := DrainResult{}

	gracePeriod := r.GracePeriodSeconds
	if gracePeriod == 0 {
		gracePeriod = 30
	}

	for i := range pods {
		pod := &pods[i]

		eviction := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
			DeleteOptions: &metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
			},
		}

		err := r.SubResource("eviction").Create(ctx, pod, eviction)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Pod already deleted
				result.PodsEvicted++
				continue
			}
			if apierrors.IsTooManyRequests(err) {
				// PDB violation - record but don't count as failure
				log.V(1).Info("Eviction blocked by PDB, will retry",
					"pod", pod.Name,
					"namespace", pod.Namespace,
				)
				result.PodsFailed++
				result.ErrorMessage = "blocked by PodDisruptionBudget"
				continue
			}

			log.Error(err, "Failed to evict pod",
				"pod", pod.Name,
				"namespace", pod.Namespace,
			)
			result.PodsFailed++
			result.ErrorMessage = err.Error()
			continue
		}

		log.V(1).Info("Evicted pod",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"nodeName", nodeName,
		)
		result.PodsEvicted++

		// Record metric
		drainActionsTotal.WithLabelValues(nodeName, "evicted").Inc()
	}

	return result
}

// updatePhaseToDraining updates the HealthEvent to Draining phase.
func (r *DrainController) updatePhaseToDraining(ctx context.Context, healthEvent *nvsentinelv1alpha1.HealthEvent) error {
	// Only update if still in Quarantined phase
	if healthEvent.Status.Phase != nvsentinelv1alpha1.PhaseQuarantined {
		return nil
	}

	patch := client.MergeFrom(healthEvent.DeepCopy())
	healthEvent.Status.Phase = nvsentinelv1alpha1.PhaseDraining
	healthEvent.Status.ObservedGeneration = healthEvent.Generation

	return r.Status().Patch(ctx, healthEvent, patch)
}

// transitionToDrained updates the HealthEvent to Drained phase.
func (r *DrainController) transitionToDrained(
	ctx context.Context,
	healthEvent *nvsentinelv1alpha1.HealthEvent,
	reason, message string,
	result DrainResult,
) (ctrl.Result, error) {
	log := klog.FromContext(ctx)

	patch := client.MergeFrom(healthEvent.DeepCopy())

	// Update phase
	healthEvent.Status.Phase = nvsentinelv1alpha1.PhaseDrained
	healthEvent.Status.ObservedGeneration = healthEvent.Generation

	// Update conditions
	now := metav1.Now()
	SetCondition(&healthEvent.Status.Conditions, nvsentinelv1alpha1.HealthEventCondition{
		Type:               nvsentinelv1alpha1.ConditionPodsDrained,
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

	log.Info("HealthEvent transitioned to Drained",
		"healthEvent", healthEvent.Name,
		"nodeName", healthEvent.Spec.NodeName,
		"podsEvicted", result.PodsEvicted,
		"podsSkipped", result.PodsSkipped,
	)

	// Increment metrics
	drainActionsTotal.WithLabelValues(healthEvent.Spec.NodeName, "completed").Inc()

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
// Note: We intentionally do NOT use phase-based predicates because status subresource
// updates may not trigger watch events reliably. Instead, we filter by phase in Reconcile.
func (r *DrainController) SetupWithManager(mgr ctrl.Manager) error {
	// Register metrics
	registerDrainMetrics()

	maxConcurrent := r.MaxConcurrentReconciles
	if maxConcurrent == 0 {
		maxConcurrent = 2 // Default - lower than quarantine to avoid overwhelming API
	}

	// Set defaults
	if r.GracePeriodSeconds == 0 {
		r.GracePeriodSeconds = 30
	}
	if !r.DeleteEmptyDirData {
		r.DeleteEmptyDirData = true // Default to allowing emptyDir eviction
	}
	if !r.IgnoreDaemonSets {
		r.IgnoreDaemonSets = true // Default to ignoring DaemonSet pods
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&nvsentinelv1alpha1.HealthEvent{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: maxConcurrent,
		}).
		Named("drain").
		Complete(r)
}
