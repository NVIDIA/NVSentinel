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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// TerminateNodeFinalizer is added to TerminateNode objects to handle cleanup
	TerminateNodeFinalizer = "janitor.dgxc.nvidia.com/terminatenode-finalizer"

	// MaxTerminateRetries is the maximum number of retry attempts before giving up
	MaxTerminateRetries = 20 // 10 minutes at 30s base intervals
)

// TerminateNodeReconciler manages the terminate node operation.
type TerminateNodeReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Config    *config.TerminateNodeControllerConfig
	CSPClient model.CSPClient
}

// updateTerminateNodeStatus is a helper function that handles status updates with proper error handling.
// It delegates to the generic updateNodeActionStatus function.
func (r *TerminateNodeReconciler) updateTerminateNodeStatus(
	ctx context.Context,
	req ctrl.Request,
	original *janitordgxcnvidiacomv1alpha1.TerminateNode,
	updated *janitordgxcnvidiacomv1alpha1.TerminateNode,
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
		"terminatenode",
		result,
	)
}

// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=terminatenodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=terminatenodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=terminatenodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *TerminateNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get the TerminateNode object
	var terminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
	if err := r.Get(ctx, req.NamespacedName, &terminateNode); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion with finalizer
	if !terminateNode.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&terminateNode, TerminateNodeFinalizer) {
			logger.Info("terminatenode deletion requested, performing cleanup",
				"node", terminateNode.Spec.NodeName,
				"conditions", terminateNode.Status.Conditions)

			// Best effort: log the state for audit trail
			// Future enhancement: Could add CSP cancellation API call here if available

			controllerutil.RemoveFinalizer(&terminateNode, TerminateNodeFinalizer)
			if err := r.Update(ctx, &terminateNode); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&terminateNode, TerminateNodeFinalizer) {
		controllerutil.AddFinalizer(&terminateNode, TerminateNodeFinalizer)
		if err := r.Update(ctx, &terminateNode); err != nil {
			return ctrl.Result{}, err
		}
	}

	if terminateNode.Status.CompletionTime != nil {
		logger.V(1).Info("terminatenode has completion time set, skipping reconcile",
			"node", terminateNode.Spec.NodeName)
		return ctrl.Result{}, nil
	}

	// Take a deep copy to compare against at the end
	originalTerminateNode := terminateNode.DeepCopy()
	var result ctrl.Result

	// Initialize conditions if not already set
	terminateNode.SetInitialConditions()

	// Set the start time if it is not already set
	terminateNode.SetStartTime()

	// Check if max retries exceeded
	if terminateNode.Status.RetryCount >= MaxTerminateRetries {
		logger.Info("max retries exceeded, marking as failed",
			"node", terminateNode.Spec.NodeName,
			"retries", int(terminateNode.Status.RetryCount),
			"maxRetries", MaxTerminateRetries)

		terminateNode.SetCompletionTime()
		terminateNode.SetCondition(metav1.Condition{
			Type:   janitordgxcnvidiacomv1alpha1.TerminateNodeConditionNodeTerminated,
			Status: metav1.ConditionFalse,
			Reason: "MaxRetriesExceeded",
			Message: fmt.Sprintf("Node failed to terminate after %d retries over %s",
				MaxTerminateRetries, r.getTerminateTimeout()),
			LastTransitionTime: metav1.Now(),
		})

		metrics.IncActionCount(metrics.ActionTypeTerminate, metrics.StatusFailed, terminateNode.Spec.NodeName)

		result = ctrl.Result{} // Don't requeue

		// Update status and return
		return r.updateTerminateNodeStatus(ctx, req, originalTerminateNode, &terminateNode, result)
	}

	// Get the node to terminate
	var node corev1.Node
	nodeExists := true
	if err := r.Get(ctx, client.ObjectKey{Name: terminateNode.Spec.NodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			// Node is already deleted, which is the desired state
			nodeExists = false
		} else {
			return ctrl.Result{}, err
		}
	}

	// Check if terminate is in progress
	if terminateNode.IsTerminateInProgress() {
		// Increment retry count for monitoring attempts
		terminateNode.Status.RetryCount++

		switch {
		case !nodeExists:
			logger.Info("node terminated successfully",
				"node", terminateNode.Spec.NodeName,
				"duration", time.Since(terminateNode.Status.StartTime.Time))

			// Reset failure counters on success
			terminateNode.Status.ConsecutiveFailures = 0

			// Node is gone, termination succeeded
			terminateNode.SetCompletionTime()
			terminateNode.SetCondition(metav1.Condition{
				Type:               janitordgxcnvidiacomv1alpha1.TerminateNodeConditionNodeTerminated,
				Status:             metav1.ConditionTrue,
				Reason:             "Succeeded",
				Message:            "CSP instance deleted and Kubernetes node removed.",
				LastTransitionTime: metav1.Now(),
			})

			// Record successful termination metrics
			metrics.IncActionCount(metrics.ActionTypeTerminate, metrics.StatusSucceeded, terminateNode.Spec.NodeName)
			metrics.RecordActionMTTR(metrics.ActionTypeTerminate, time.Since(terminateNode.Status.StartTime.Time))

			result = ctrl.Result{} // Don't requeue on success
		case isNodeNotReady(&node):
			// Node is not ready, delete it from Kubernetes
			logger.Info("node reached not ready state, deleting from cluster",
				"node", terminateNode.Spec.NodeName)

			if err := r.Delete(ctx, &node); err != nil {
				logger.Error(err, "failed to delete node from kubernetes",
					"node", node.Name)
				return ctrl.Result{}, err
			}

			// Reset failure counters on success
			terminateNode.Status.ConsecutiveFailures = 0

			// Update status
			terminateNode.SetCompletionTime()
			terminateNode.SetCondition(metav1.Condition{
				Type:               janitordgxcnvidiacomv1alpha1.TerminateNodeConditionNodeTerminated,
				Status:             metav1.ConditionTrue,
				Reason:             "Succeeded",
				Message:            "CSP instance deleted and Kubernetes node removed.",
				LastTransitionTime: metav1.Now(),
			})

			// Record successful termination metrics
			metrics.IncActionCount(metrics.ActionTypeTerminate, metrics.StatusSucceeded, node.Name)
			metrics.RecordActionMTTR(metrics.ActionTypeTerminate, time.Since(terminateNode.Status.StartTime.Time))

			result = ctrl.Result{} // Don't requeue on success
		case time.Since(terminateNode.Status.StartTime.Time) > r.getTerminateTimeout():
			// Timeout exceeded
			logger.Error(nil, "node terminate timed out",
				"node", node.Name,
				"timeout", r.getTerminateTimeout(),
				"elapsed", time.Since(terminateNode.Status.StartTime.Time))

			// Update status
			terminateNode.SetCompletionTime()
			terminateNode.SetCondition(metav1.Condition{
				Type:               janitordgxcnvidiacomv1alpha1.TerminateNodeConditionNodeTerminated,
				Status:             metav1.ConditionFalse,
				Reason:             "Timeout",
				Message:            "Node failed to transition to not ready state after timeout duration",
				LastTransitionTime: metav1.Now(),
			})

			metrics.IncActionCount(metrics.ActionTypeTerminate, metrics.StatusFailed, node.Name)
			result = ctrl.Result{} // Don't requeue on timeout
		default:
			// Still waiting for terminate to complete
			// Use exponential backoff if there have been failures
			delay := getNextRequeueDelay(terminateNode.Status.ConsecutiveFailures)
			result = ctrl.Result{RequeueAfter: delay}
		}
	} else {
		// Terminate not in progress yet, need to send signal

		// If node doesn't exist, this is an error (should have been caught by webhook)
		if !nodeExists {
			return ctrl.Result{}, errors.New("node not found and terminate not in progress")
		}

		// Check if signal was already sent (but terminate not in progress due to other issues)
		signalAlreadySent := false
		for _, condition := range terminateNode.Status.Conditions {
			if condition.Type == janitordgxcnvidiacomv1alpha1.TerminateNodeConditionSignalSent &&
				condition.Status == metav1.ConditionTrue {
				signalAlreadySent = true
				break
			}
		}

		if signalAlreadySent {
			// Signal was already sent, just continue monitoring
			logger.V(1).Info("terminate signal already sent, continuing monitoring",
				"node", node.Name)

			delay := getNextRequeueDelay(terminateNode.Status.ConsecutiveFailures)
			result = ctrl.Result{RequeueAfter: delay}
		} else {
			// Need to send terminate signal
			if r.Config.ManualMode {
				// Check if manual mode condition is already set
				isManualModeConditionSet := false
				for _, condition := range terminateNode.Status.Conditions {
					if condition.Type == janitordgxcnvidiacomv1alpha1.ManualModeConditionType {
						isManualModeConditionSet = true
						break
					}
				}
				if !isManualModeConditionSet {
					now := metav1.Now()
					terminateNode.SetCondition(metav1.Condition{
						Type:               janitordgxcnvidiacomv1alpha1.ManualModeConditionType,
						Status:             metav1.ConditionTrue,
						Reason:             "OutsideActorRequired",
						Message:            "Janitor is in manual mode, outside actor required to send terminate signal",
						LastTransitionTime: now,
					})
					metrics.IncActionCount(metrics.ActionTypeTerminate, metrics.StatusStarted, node.Name)
				}
				logger.Info("manual mode enabled, janitor will not send terminate signal",
					"node", node.Name)
				result = ctrl.Result{}
			} else {
				// Send terminate signal via CSP
				logger.Info("sending terminate signal to node",
					"node", terminateNode.Spec.NodeName)

				metrics.IncActionCount(metrics.ActionTypeTerminate, metrics.StatusStarted, node.Name)

				// Add timeout to CSP operation
				cspCtx, cancel := context.WithTimeout(ctx, CSPOperationTimeout)
				defer cancel()

				_, terminateErr := r.CSPClient.SendTerminateSignal(cspCtx, node)

				// Check for timeout
				if errors.Is(terminateErr, context.DeadlineExceeded) {
					logger.Info("CSP operation timed out, will retry",
						"node", node.Name,
						"operation", "SendTerminateSignal",
						"timeout", CSPOperationTimeout)

					terminateNode.Status.ConsecutiveFailures++
					delay := getNextRequeueDelay(terminateNode.Status.ConsecutiveFailures)

					result = ctrl.Result{RequeueAfter: delay}
					// Update status and return early
					return r.updateTerminateNodeStatus(ctx, req, originalTerminateNode, &terminateNode, result)
				}

				// Update status based on terminate result
				var signalSentCondition metav1.Condition
				if terminateErr == nil {
					// Reset consecutive failures on success
					terminateNode.Status.ConsecutiveFailures = 0

					signalSentCondition = metav1.Condition{
						Type:               janitordgxcnvidiacomv1alpha1.TerminateNodeConditionSignalSent,
						Status:             metav1.ConditionTrue,
						Reason:             "Succeeded",
						Message:            "Terminate signal sent to CSP",
						LastTransitionTime: metav1.Now(),
					}
					// Continue monitoring if signal was sent successfully
					result = ctrl.Result{RequeueAfter: 30 * time.Second}
				} else {
					terminateNode.Status.ConsecutiveFailures++

					signalSentCondition = metav1.Condition{
						Type:               janitordgxcnvidiacomv1alpha1.TerminateNodeConditionSignalSent,
						Status:             metav1.ConditionFalse,
						Reason:             "Failed",
						Message:            terminateErr.Error(),
						LastTransitionTime: metav1.Now(),
					}
					terminateNode.SetCompletionTime()
					// Don't requeue on failure
					result = ctrl.Result{}
					metrics.IncActionCount(metrics.ActionTypeTerminate, metrics.StatusFailed, node.Name)
				}
				terminateNode.SetCondition(signalSentCondition)
			}
		}
	}

	// Update status at the end of reconciliation
	return r.updateTerminateNodeStatus(ctx, req, originalTerminateNode, &terminateNode, result)
}

// isNodeNotReady returns true if the node is not in Ready state
func isNodeNotReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status != corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// getTerminateTimeout returns the timeout for terminate operations
func (r *TerminateNodeReconciler) getTerminateTimeout() time.Duration {
	cfg := r.Config
	if cfg == nil || cfg.Timeout == 0 {
		return 30 * time.Minute // fallback default
	}

	return cfg.Timeout
}

// SetupWithManager sets up the controller with the Manager.
func (r *TerminateNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
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
		For(&janitordgxcnvidiacomv1alpha1.TerminateNode{}).
		Named("terminatenode").
		Complete(r)
}
