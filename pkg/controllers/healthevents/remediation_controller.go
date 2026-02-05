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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
)

// RemediationStrategy defines how to remediate a health event.
type RemediationStrategy string

const (
	// StrategyReboot reboots the node via a privileged Job.
	StrategyReboot RemediationStrategy = "reboot"

	// StrategyTerminate terminates the node (cloud provider replaces it).
	StrategyTerminate RemediationStrategy = "terminate"

	// StrategyGPUReset resets the GPU via NVML (for non-fatal issues).
	StrategyGPUReset RemediationStrategy = "gpu-reset"

	// StrategyManual requires manual intervention.
	StrategyManual RemediationStrategy = "manual"

	// StrategyNone skips remediation entirely.
	StrategyNone RemediationStrategy = "none"
)

// RemediationController reconciles HealthEvent resources in the Drained phase.
// It determines the appropriate remediation action and executes it.
type RemediationController struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// DefaultStrategy is the remediation strategy when none is specified.
	DefaultStrategy RemediationStrategy

	// RebootJobNamespace is the namespace where reboot Jobs are created.
	RebootJobNamespace string

	// RebootJobImage is the container image for reboot Jobs.
	RebootJobImage string

	// MaxConcurrentReconciles is the maximum number of concurrent Reconciles.
	MaxConcurrentReconciles int

	// RebootJobTTL is how long to keep completed reboot Jobs.
	RebootJobTTL time.Duration
}

// +kubebuilder:rbac:groups=nvsentinel.nvidia.com,resources=healthevents,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=nvsentinel.nvidia.com,resources=healthevents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles HealthEvent remediation.
// It watches for events in the Drained phase and triggers remediation.
func (r *RemediationController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := klog.FromContext(ctx).WithValues("healthevent", req.Name)
	log.V(2).Info("Reconciling HealthEvent for remediation")

	// Fetch the HealthEvent
	var healthEvent nvsentinelv1alpha1.HealthEvent
	if err := r.Get(ctx, req.NamespacedName, &healthEvent); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get HealthEvent")
		return ctrl.Result{}, err
	}

	// Only process events in Drained phase
	if healthEvent.Status.Phase != nvsentinelv1alpha1.PhaseDrained {
		log.V(2).Info("Skipping event not in Drained phase", "phase", healthEvent.Status.Phase)
		return ctrl.Result{}, nil
	}

	nodeName := healthEvent.Spec.NodeName
	log = log.WithValues("nodeName", nodeName)

	// Determine remediation strategy
	strategy := r.determineStrategy(&healthEvent)
	log.Info("Determined remediation strategy", "strategy", strategy)

	// Execute remediation based on strategy
	var err error
	var message string

	switch strategy {
	case StrategyReboot:
		err = r.executeReboot(ctx, &healthEvent, log)
		message = "Node reboot initiated"
	case StrategyTerminate:
		err = r.executeTerminate(ctx, &healthEvent, log)
		message = "Node termination initiated"
	case StrategyGPUReset:
		err = r.executeGPUReset(ctx, &healthEvent, log)
		message = "GPU reset initiated"
	case StrategyManual:
		message = "Manual remediation required"
		log.Info("Manual remediation required", "recommendedAction", healthEvent.Spec.RecommendedAction)
	case StrategyNone:
		message = "No remediation action taken"
		log.Info("Remediation skipped")
	default:
		err = fmt.Errorf("unknown remediation strategy: %s", strategy)
	}

	if err != nil {
		log.Error(err, "Remediation failed")
		remediationFailuresTotal.WithLabelValues(nodeName, string(strategy)).Inc()

		// Record event
		r.Recorder.Event(&healthEvent, corev1.EventTypeWarning, "RemediationFailed",
			fmt.Sprintf("Remediation failed: %v", err))

		// Set condition
		now := metav1.Now()
		SetCondition(&healthEvent.Status.Conditions, nvsentinelv1alpha1.HealthEventCondition{
			Type:               nvsentinelv1alpha1.ConditionRemediated,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: now,
			Reason:             "RemediationFailed",
			Message:            err.Error(),
			ObservedGeneration: healthEvent.Generation,
		})

		// Update status with error condition but don't change phase
		if updateErr := r.Status().Update(ctx, &healthEvent); updateErr != nil {
			log.Error(updateErr, "Failed to update HealthEvent status")
			return ctrl.Result{}, updateErr
		}

		// Requeue to retry
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Remediation succeeded - transition to Remediated phase
	if err := r.transitionToRemediated(ctx, &healthEvent, message, log); err != nil {
		return ctrl.Result{}, err
	}

	// Record metrics
	remediationActionsTotal.WithLabelValues(nodeName, string(strategy)).Inc()

	return ctrl.Result{}, nil
}

// determineStrategy determines the remediation strategy for an event.
func (r *RemediationController) determineStrategy(event *nvsentinelv1alpha1.HealthEvent) RemediationStrategy {
	// Check for annotation override
	if strategy, ok := event.Annotations["nvsentinel.nvidia.com/remediation-strategy"]; ok {
		switch RemediationStrategy(strategy) {
		case StrategyReboot, StrategyTerminate, StrategyGPUReset, StrategyManual, StrategyNone:
			return RemediationStrategy(strategy)
		}
	}

	// Map recommended action to strategy
	switch event.Spec.RecommendedAction {
	case nvsentinelv1alpha1.ActionRestartVM, nvsentinelv1alpha1.ActionRestartBM:
		return StrategyReboot
	case nvsentinelv1alpha1.ActionReplaceVM:
		return StrategyTerminate
	case nvsentinelv1alpha1.ActionComponentReset:
		return StrategyGPUReset
	case nvsentinelv1alpha1.ActionNone:
		return StrategyNone
	case nvsentinelv1alpha1.ActionContactSupport, nvsentinelv1alpha1.ActionRunFieldDiag:
		return StrategyManual
	}

	// Use default strategy
	if r.DefaultStrategy != "" {
		return r.DefaultStrategy
	}

	// Final fallback
	return StrategyManual
}

// executeReboot creates a privileged Job to reboot the node.
func (r *RemediationController) executeReboot(ctx context.Context, event *nvsentinelv1alpha1.HealthEvent, log klog.Logger) error {
	nodeName := event.Spec.NodeName

	// Check if reboot job already exists
	jobName := fmt.Sprintf("reboot-%s", event.Name)
	var existingJob batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: r.RebootJobNamespace}, &existingJob)
	if err == nil {
		// Job exists - check status
		if existingJob.Status.Succeeded > 0 {
			log.Info("Reboot job already succeeded", "job", jobName)
			return nil
		}
		if existingJob.Status.Failed > 0 {
			log.Info("Reboot job failed, will be retried", "job", jobName)
			return fmt.Errorf("reboot job failed")
		}
		// Job still running
		log.Info("Reboot job still running", "job", jobName)
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check existing reboot job: %w", err)
	}

	// Determine image
	image := r.RebootJobImage
	if image == "" {
		image = "busybox:latest"
	}

	// Determine namespace
	namespace := r.RebootJobNamespace
	if namespace == "" {
		namespace = "nvsentinel-system"
	}

	// Calculate TTL
	ttl := int32(r.RebootJobTTL.Seconds())
	if ttl == 0 {
		ttl = 3600 // 1 hour default
	}

	// Create reboot Job
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "nvsentinel-reboot",
				"app.kubernetes.io/component":  "remediation",
				"app.kubernetes.io/managed-by": "nvsentinel",
				"nvsentinel.nvidia.com/event":  event.Name,
				"nvsentinel.nvidia.com/node":   nodeName,
			},
			Annotations: map[string]string{
				"nvsentinel.nvidia.com/health-event": event.Name,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			BackoffLimit:            ptr.To[int32](3),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeName:      nodeName,
					RestartPolicy: corev1.RestartPolicyNever,
					HostPID:       true,
					Tolerations: []corev1.Toleration{
						{
							Key:      "node.kubernetes.io/unschedulable",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "nvidia.com/gpu-unhealthy",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "reboot",
							Image: image,
							Command: []string{
								"/bin/sh",
								"-c",
								"echo 'Initiating reboot...' && nsenter -t 1 -m -u -i -n -- /sbin/reboot",
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
							},
						},
					},
				},
			},
		},
	}

	if err := r.Create(ctx, job); err != nil {
		return fmt.Errorf("failed to create reboot job: %w", err)
	}

	log.Info("Created reboot job", "job", jobName, "namespace", namespace)

	// Record event
	r.Recorder.Event(event, corev1.EventTypeNormal, "RebootJobCreated",
		fmt.Sprintf("Created reboot job %s/%s", namespace, jobName))

	return nil
}

// executeTerminate deletes the Node object to trigger cloud provider replacement.
func (r *RemediationController) executeTerminate(ctx context.Context, event *nvsentinelv1alpha1.HealthEvent, log klog.Logger) error {
	nodeName := event.Spec.NodeName

	// Get the node
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Node already deleted/terminated", "node", nodeName)
			return nil
		}
		return fmt.Errorf("failed to get node: %w", err)
	}

	// Check if node has cloud provider info
	if node.Spec.ProviderID == "" {
		log.Info("Node has no provider ID, cannot terminate via cloud provider", "node", nodeName)
		return fmt.Errorf("node %s has no provider ID - cannot use terminate strategy", nodeName)
	}

	// Delete the node - cloud controller manager will handle instance termination
	log.Info("Deleting node to trigger cloud provider termination",
		"node", nodeName,
		"providerID", node.Spec.ProviderID,
	)

	if err := r.Delete(ctx, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete node: %w", err)
	}

	// Record event
	r.Recorder.Event(event, corev1.EventTypeNormal, "NodeTerminated",
		fmt.Sprintf("Deleted node %s to trigger cloud provider replacement", nodeName))

	return nil
}

// executeGPUReset initiates a GPU reset.
// This requires the NVML provider to be running on the node.
func (r *RemediationController) executeGPUReset(ctx context.Context, event *nvsentinelv1alpha1.HealthEvent, log klog.Logger) error {
	nodeName := event.Spec.NodeName

	// GPU reset needs to be handled by the device-api-server on the node.
	// We signal this by setting an annotation that the server watches.
	if event.Annotations == nil {
		event.Annotations = make(map[string]string)
	}
	event.Annotations["nvsentinel.nvidia.com/gpu-reset-requested"] = "true"
	event.Annotations["nvsentinel.nvidia.com/gpu-reset-requested-at"] = time.Now().UTC().Format(time.RFC3339)

	if err := r.Update(ctx, event); err != nil {
		return fmt.Errorf("failed to update event with gpu-reset annotation: %w", err)
	}

	log.Info("GPU reset requested via annotation", "node", nodeName)

	// Record event
	r.Recorder.Event(event, corev1.EventTypeNormal, "GPUResetRequested",
		fmt.Sprintf("GPU reset requested on node %s", nodeName))

	return nil
}

// transitionToRemediated updates the HealthEvent status to Remediated phase.
func (r *RemediationController) transitionToRemediated(ctx context.Context, event *nvsentinelv1alpha1.HealthEvent, message string, log klog.Logger) error {
	now := metav1.Now()

	// Update status
	event.Status.Phase = nvsentinelv1alpha1.PhaseRemediated
	event.Status.LastRemediationTime = &now
	event.Status.ObservedGeneration = event.Generation

	// Set condition
	SetCondition(&event.Status.Conditions, nvsentinelv1alpha1.HealthEventCondition{
		Type:               nvsentinelv1alpha1.ConditionRemediated,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "RemediationComplete",
		Message:            message,
		ObservedGeneration: event.Generation,
	})

	if err := r.Status().Update(ctx, event); err != nil {
		log.Error(err, "Failed to update HealthEvent status")
		return err
	}

	log.Info("HealthEvent transitioned to Remediated",
		"healthEvent", event.Name,
		"nodeName", event.Spec.NodeName,
		"message", message,
	)

	// Record event
	r.Recorder.Event(event, corev1.EventTypeNormal, "Remediated", message)

	return nil
}

// SetupWithManager sets up the controller with the Manager.
// Note: We intentionally do NOT use phase-based predicates because status subresource
// updates may not trigger watch events reliably. Instead, we filter by phase in Reconcile.
func (r *RemediationController) SetupWithManager(mgr ctrl.Manager) error {
	// Register metrics
	registerRemediationMetrics()

	maxConcurrent := r.MaxConcurrentReconciles
	if maxConcurrent == 0 {
		maxConcurrent = 2
	}

	if r.DefaultStrategy == "" {
		r.DefaultStrategy = StrategyManual
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&nvsentinelv1alpha1.HealthEvent{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: maxConcurrent,
		}).
		Named("remediation").
		Complete(r)
}
