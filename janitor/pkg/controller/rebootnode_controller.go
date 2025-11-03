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

// nolint:wsl,lll,gocognit,cyclop,gocyclo,nestif // Business logic migrated from old code
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	janitordgxcnvidiacomv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
	"github.com/nvidia/nvsentinel/janitor/pkg/csp"
	"github.com/nvidia/nvsentinel/janitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/janitor/pkg/model"
)

const (
	// RebootNodeFinalizer is added to RebootNode objects to handle cleanup
	RebootNodeFinalizer = "janitor.dgxc.nvidia.com/rebootnode-finalizer"

	// CSPOperationTimeout is the maximum time allowed for a single CSP operation
	CSPOperationTimeout = 2 * time.Minute

	// MaxRebootRetries is the maximum number of retry attempts before giving up
	MaxRebootRetries = 20 // 10 minutes at 30s base intervals
)

// updateRebootNodeStatus is a helper function that handles status updates with proper error handling.
// It delegates to the generic updateNodeActionStatus function.
func (r *RebootNodeReconciler) updateRebootNodeStatus(
	ctx context.Context,
	req ctrl.Request,
	original *janitordgxcnvidiacomv1alpha1.RebootNode,
	updated *janitordgxcnvidiacomv1alpha1.RebootNode,
	result ctrl.Result,
) (ctrl.Result, error) {
	return updateNodeActionStatus(
		ctx,
		r.Status(),
		original,
		updated,
		&original.Status,
		&updated.Status,
		updated.Spec.NodeName,
		"rebootnode",
		result,
	)
}

// RebootNodeReconciler reconciles a RebootNode object
type RebootNodeReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Config    *config.RebootNodeControllerConfig
	CSPClient model.CSPClient
}

// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=rebootnodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=rebootnodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=rebootnodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *RebootNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get the RebootNode object
	var rebootNode janitordgxcnvidiacomv1alpha1.RebootNode
	if err := r.Get(ctx, req.NamespacedName, &rebootNode); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion with finalizer
	if !rebootNode.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&rebootNode, RebootNodeFinalizer) {
			logger.Info("rebootnode deletion requested, performing cleanup",
				"node", rebootNode.Spec.NodeName,
				"conditions", rebootNode.Status.Conditions,
				"cspRef", rebootNode.GetCSPReqRef())

			// Best effort: log the state for audit trail
			// Future enhancement: Could add CSP cancellation API call here if available

			controllerutil.RemoveFinalizer(&rebootNode, RebootNodeFinalizer)
			if err := r.Update(ctx, &rebootNode); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&rebootNode, RebootNodeFinalizer) {
		controllerutil.AddFinalizer(&rebootNode, RebootNodeFinalizer)
		if err := r.Update(ctx, &rebootNode); err != nil {
			return ctrl.Result{}, err
		}
	}

	if rebootNode.Status.CompletionTime != nil {
		logger.V(1).Info("rebootnode has completion time set, skipping reconcile",
			"node", rebootNode.Spec.NodeName)
		return ctrl.Result{}, nil
	}

	// Take a deep copy to compare against at the end
	originalRebootNode := rebootNode.DeepCopy()
	var result ctrl.Result

	// Initialize conditions if not already set
	rebootNode.SetInitialConditions()

	// Set the start time if it is not already set
	rebootNode.SetStartTime()

	// Check if max retries exceeded
	if rebootNode.Status.RetryCount >= MaxRebootRetries {
		logger.Info("max retries exceeded, marking as failed",
			"node", rebootNode.Spec.NodeName,
			"retries", int(rebootNode.Status.RetryCount),
			"maxRetries", MaxRebootRetries)

		rebootNode.SetCompletionTime()
		rebootNode.SetCondition(metav1.Condition{
			Type:   janitordgxcnvidiacomv1alpha1.RebootNodeConditionNodeReady,
			Status: metav1.ConditionFalse,
			Reason: "MaxRetriesExceeded",
			Message: fmt.Sprintf("Node failed to reach ready state after %d retries over %s",
				MaxRebootRetries, r.getRebootTimeout()),
			LastTransitionTime: metav1.Now(),
		})

		metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeReboot, metrics.StatusFailed, rebootNode.Spec.NodeName)

		result = ctrl.Result{} // Don't requeue

		// Update status and return
		return r.updateRebootNodeStatus(ctx, req, originalRebootNode, &rebootNode, result)
	}

	// Get the node to reboot
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: rebootNode.Spec.NodeName}, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if reboot has already started
	if rebootNode.IsRebootInProgress() {
		// Increment retry count for monitoring attempts
		rebootNode.Status.RetryCount++

		// Check if csp reports the node is ready
		cspReady := false
		var nodeReadyErr error
		if r.Config.ManualMode {
			cspReady = true
			nodeReadyErr = nil
		} else {
			// Add timeout to CSP operation to prevent queue blocking
			cspCtx, cancel := context.WithTimeout(ctx, CSPOperationTimeout)
			defer cancel()

			cspReady, nodeReadyErr = r.CSPClient.IsNodeReady(cspCtx, node, rebootNode.GetCSPReqRef())

			// Check for timeout specifically
			if errors.Is(nodeReadyErr, context.DeadlineExceeded) {
				logger.Info("CSP operation timed out, will retry",
					"node", node.Name,
					"operation", "IsNodeReady",
					"timeout", CSPOperationTimeout)

				rebootNode.Status.ConsecutiveFailures++
				delay := getNextRequeueDelay(rebootNode.Status.ConsecutiveFailures)

				result = ctrl.Result{RequeueAfter: delay}
				// Update status and return early
				return r.updateRebootNodeStatus(ctx, req, originalRebootNode, &rebootNode, result)
			}
		}

		// Check if kubernetes reports the node is ready.
		kubernetesReady := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				kubernetesReady = condition.Status == corev1.ConditionTrue
			}
		}

		// nolint:gocritic // Migrated business logic with if-else chain
		if nodeReadyErr != nil {
			logger.Error(nodeReadyErr, "node ready status check failed",
				"node", node.Name)

			rebootNode.Status.ConsecutiveFailures++

			rebootNode.SetCompletionTime()
			rebootNode.SetCondition(metav1.Condition{
				Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionNodeReady,
				Status:             metav1.ConditionFalse,
				Reason:             "Failed",
				Message:            fmt.Sprintf("Node status could not be checked from CSP: %s", nodeReadyErr),
				LastTransitionTime: metav1.Now(),
			})

			metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeReboot, metrics.StatusFailed, node.Name)
			result = ctrl.Result{} // Don't requeue on failure
		} else if cspReady && kubernetesReady {
			logger.Info("node reached ready state post-reboot",
				"node", node.Name,
				"duration", time.Since(rebootNode.Status.StartTime.Time))

			// Reset failure counters on success
			rebootNode.Status.ConsecutiveFailures = 0

			// Update status
			rebootNode.SetCompletionTime()
			rebootNode.SetCondition(metav1.Condition{
				Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionNodeReady,
				Status:             metav1.ConditionTrue,
				Reason:             "Succeeded",
				Message:            "Node reached ready state post-reboot",
				LastTransitionTime: metav1.Now(),
			})

			// Metrics and final result
			metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeReboot, metrics.StatusSucceeded, node.Name)
			metrics.GlobalMetrics.RecordActionMTTR(metrics.ActionTypeReboot, time.Since(rebootNode.Status.StartTime.Time))
			result = ctrl.Result{} // Don't requeue on success
		} else if time.Since(rebootNode.Status.StartTime.Time) > r.getRebootTimeout() {
			logger.Error(nil, "node reboot timed out",
				"node", node.Name,
				"timeout", r.getRebootTimeout(),
				"elapsed", time.Since(rebootNode.Status.StartTime.Time))

			// Update status
			rebootNode.SetCompletionTime()
			rebootNode.SetCondition(metav1.Condition{
				Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionNodeReady,
				Status:             metav1.ConditionFalse,
				Reason:             "Timeout",
				Message:            "Node failed to return to ready state after timeout duration",
				LastTransitionTime: metav1.Now(),
			})

			metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeReboot, metrics.StatusFailed, node.Name)
			result = ctrl.Result{} // Don't requeue on timeout
		} else {
			// Still waiting for reboot to complete
			// Use exponential backoff if there have been failures
			delay := getNextRequeueDelay(rebootNode.Status.ConsecutiveFailures)
			result = ctrl.Result{RequeueAfter: delay}
		}
	} else {
		// Check if signal was already sent (but reboot not in progress due to other issues)
		signalAlreadySent := false
		for _, condition := range rebootNode.Status.Conditions {
			if condition.Type == janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent && condition.Status == metav1.ConditionTrue {
				signalAlreadySent = true
				break
			}
		}

		if signalAlreadySent {
			// Signal was already sent, just continue monitoring
			logger.V(1).Info("reboot signal already sent, continuing monitoring",
				"node", node.Name)

			delay := getNextRequeueDelay(rebootNode.Status.ConsecutiveFailures)
			result = ctrl.Result{RequeueAfter: delay}
		} else {
			if r.Config.ManualMode {
				isManualModeConditionSet := false
				for _, condition := range rebootNode.Status.Conditions {
					if condition.Type == janitordgxcnvidiacomv1alpha1.ManualModeConditionType {
						isManualModeConditionSet = true
						break
					}
				}
				if !isManualModeConditionSet {
					now := metav1.Now()
					rebootNode.SetCondition(metav1.Condition{
						Type:               janitordgxcnvidiacomv1alpha1.ManualModeConditionType,
						Status:             metav1.ConditionTrue,
						Reason:             "OutsideActorRequired",
						Message:            "Janitor is in manual mode, outside actor required to send reboot signal",
						LastTransitionTime: now,
					})
					metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeReboot, metrics.StatusStarted, node.Name)
				}
				logger.Info("manual mode enabled, janitor will not send reboot signal",
					"node", node.Name)
				result = ctrl.Result{}
			} else {
				// Start the reboot process
				metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeReboot, metrics.StatusStarted, node.Name)
				logger.Info("sending reboot signal to node",
					"node", node.Name)

				// Add timeout to CSP operation
				cspCtx, cancel := context.WithTimeout(ctx, CSPOperationTimeout)
				defer cancel()

				reqRef, rebootErr := r.CSPClient.SendRebootSignal(cspCtx, node)

				// Check for timeout
				if errors.Is(rebootErr, context.DeadlineExceeded) {
					logger.Info("CSP operation timed out, will retry",
						"node", node.Name,
						"operation", "SendRebootSignal",
						"timeout", CSPOperationTimeout)

					rebootNode.Status.ConsecutiveFailures++
					delay := getNextRequeueDelay(rebootNode.Status.ConsecutiveFailures)

					result = ctrl.Result{RequeueAfter: delay}
					// Update status and return early
					return r.updateRebootNodeStatus(ctx, req, originalRebootNode, &rebootNode, result)
				}

				// Update status based on reboot result
				var signalSentCondition metav1.Condition
				if rebootErr == nil {
					// Reset consecutive failures on success
					rebootNode.Status.ConsecutiveFailures = 0

					signalSentCondition = metav1.Condition{
						Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent,
						Status:             metav1.ConditionTrue,
						Reason:             "Succeeded",
						Message:            string(reqRef),
						LastTransitionTime: metav1.Now(),
					}
					// Continue monitoring if signal was sent successfully
					result = ctrl.Result{RequeueAfter: 30 * time.Second}
				} else {
					rebootNode.Status.ConsecutiveFailures++

					signalSentCondition = metav1.Condition{
						Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent,
						Status:             metav1.ConditionFalse,
						Reason:             "Failed",
						Message:            rebootErr.Error(),
						LastTransitionTime: metav1.Now(),
					}
					rebootNode.SetCompletionTime()
					// Don't requeue on failure
					result = ctrl.Result{}
					metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeReboot, metrics.StatusFailed, node.Name)
				}
				rebootNode.SetCondition(signalSentCondition)
			}
		}
	}

	// Update status if changed and return
	return r.updateRebootNodeStatus(ctx, req, originalRebootNode, &rebootNode, result)
}

// SetupWithManager sets up the controller with the Manager.
func (r *RebootNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Use background context for client initialization during controller setup
	// This is synchronous and happens before the controller starts processing events
	ctx := context.Background()

	var err error

	r.CSPClient, err = csp.New(ctx)
	if err != nil {
		return fmt.Errorf("failed to create CSP client: %w", err)
	}

	// Note: We use RequeueAfter in the reconcile loop rather than the controller's
	// rate limiter because we need per-resource (per-node) backoff based on each
	// node's individual failure count, not per-controller rate limiting.
	// This allows nodes with consecutive failures to back off independently.
	return ctrl.NewControllerManagedBy(mgr).
		For(&janitordgxcnvidiacomv1alpha1.RebootNode{}).
		Named("rebootnode").
		Complete(r)
}

// getRebootTimeout returns the timeout for reboot operations
func (r *RebootNodeReconciler) getRebootTimeout() time.Duration {
	cfg := r.Config
	if cfg == nil || cfg.Timeout == 0 {
		return 30 * time.Minute // fallback default
	}

	return cfg.Timeout
}
