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
	"os"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	janitordgxcnvidiacomv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
	"github.com/nvidia/nvsentinel/janitor/pkg/csp"
	"github.com/nvidia/nvsentinel/janitor/pkg/metrics"
)

// TerminateNodeReconciler manages the terminate node operation.
type TerminateNodeReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Config    *config.TerminateNodeControllerConfig
	CSPClient csp.Client
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

	if terminateNode.Status.CompletionTime != nil {
		logger.V(1).Info("TerminateNode has completion time set, skipping reconcile", "node", terminateNode.Spec.NodeName)
		return ctrl.Result{}, nil
	}

	// Take a deep copy to compare against at the end
	originalTerminateNode := terminateNode.DeepCopy()
	var result ctrl.Result

	// Initialize conditions if not already set
	terminateNode.SetInitialConditions()

	// Set the start time if it is not already set
	terminateNode.SetStartTime()

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
		switch {
		case !nodeExists:
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
			logger.V(0).Info("Node reached not ready state, deleting from cluster", "node", terminateNode.Spec.NodeName)
			if err := r.Delete(ctx, &node); err != nil {
				return ctrl.Result{}, err
			}

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
			logger.Error(nil, "Node terminate timed out",
				"node", node.Name,
				"timeout", r.getTerminateTimeout())

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
			result = ctrl.Result{RequeueAfter: 30 * time.Second}
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
			logger.V(1).Info("Terminate signal already sent, continuing monitoring", "node", node.Name)
			result = ctrl.Result{RequeueAfter: 30 * time.Second}
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
				logger.V(0).Info("Manual mode enabled, janitor will not send terminate signal", "node", node.Name)
				result = ctrl.Result{}
			} else {
				// Send terminate signal via CSP
				logger.V(0).Info("Sending terminate signal to node", "node", terminateNode.Spec.NodeName)
				metrics.IncActionCount(metrics.ActionTypeTerminate, metrics.StatusStarted, node.Name)
				_, terminateErr := r.CSPClient.SendTerminateSignal(ctx, node)

				// Update status based on terminate result
				var signalSentCondition metav1.Condition
				if terminateErr == nil {
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

	// Compare status to see if anything changed, and push updates if needed
	if !reflect.DeepEqual(originalTerminateNode.Status, terminateNode.Status) {
		// Refresh the object before updating to avoid precondition failures
		var freshTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
		if err := r.Get(ctx, req.NamespacedName, &freshTerminateNode); err != nil {
			if apierrors.IsNotFound(err) {
				logger.V(0).Info("Post-reconciliation status update: TerminateNode not found, object assumed deleted",
					"name", terminateNode.Name)
				return ctrl.Result{}, nil
			}
			logger.Error(err, "failed to refresh TerminateNode before status update")
			return ctrl.Result{}, err
		}

		// Apply status changes to the fresh object
		freshTerminateNode.Status = terminateNode.Status

		if err := r.Status().Update(ctx, &freshTerminateNode); err != nil {
			logger.Error(err, "failed to update TerminateNode status")
			return ctrl.Result{}, err
		}
		logger.V(0).Info("TerminateNode status updated", "node", terminateNode.Spec.NodeName)
	}

	return result, nil
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
	// Get CSP client from environment variable, default to "kind" for development
	cspType := os.Getenv("CSP")
	if cspType == "" {
		cspType = "kind" // Default to kind for local development
	}

	cspClient, err := csp.NewClient(cspType)
	if err != nil {
		return fmt.Errorf("failed to create CSP client: %w", err)
	}

	r.CSPClient = cspClient

	return ctrl.NewControllerManagedBy(mgr).
		For(&janitordgxcnvidiacomv1alpha1.TerminateNode{}).
		Named("terminatenode").
		Complete(r)
}
