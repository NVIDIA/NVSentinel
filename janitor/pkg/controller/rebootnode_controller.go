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

// RebootNodeReconciler reconciles a RebootNode object
type RebootNodeReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Config    *config.RebootNodeControllerConfig
	CSPClient csp.Client
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

	if rebootNode.Status.CompletionTime != nil {
		logger.V(1).Info("RebootNode has completion time set, skipping reconcile", "node", rebootNode.Spec.NodeName)
		return ctrl.Result{}, nil
	}

	// Take a deep copy to compare against at the end
	originalRebootNode := rebootNode.DeepCopy()
	var result ctrl.Result

	// Initialize conditions if not already set
	rebootNode.SetInitialConditions()

	// Set the start time if it is not already set
	rebootNode.SetStartTime()

	// Get the node to reboot
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: rebootNode.Spec.NodeName}, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if reboot has already started
	if rebootNode.IsRebootInProgress() {
		// Check if csp reports the node is ready
		cspReady := false
		var nodeReadyErr error
		if r.Config.ManualMode {
			cspReady = true
			nodeReadyErr = nil
		} else {
			cspReady, nodeReadyErr = r.CSPClient.IsNodeReady(ctx, node, rebootNode.GetCSPReqRef())
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
			logger.Error(nodeReadyErr, fmt.Sprintf("Node %s ready status check failed", node.Name))

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
			logger.V(0).Info(fmt.Sprintf("Node reached ready state post-reboot: %s", node.Name))

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
			logger.Error(nil, fmt.Sprintf("Node %s reboot timed out after %s", node.Name, r.getRebootTimeout()))

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
			result = ctrl.Result{RequeueAfter: 30 * time.Second}
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
			logger.V(1).Info(fmt.Sprintf("Reboot signal already sent for node %s, continuing monitoring", node.Name))
			result = ctrl.Result{RequeueAfter: 30 * time.Second}
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
				logger.V(0).Info(fmt.Sprintf("Manual mode enabled, janitor will not send reboot signal for node %s", node.Name))
				result = ctrl.Result{}
			} else {
				// Start the reboot process
				metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeReboot, metrics.StatusStarted, node.Name)
				logger.V(0).Info(fmt.Sprintf("Sending reboot signal to node %s", node.Name))
				reqRef, rebootErr := r.CSPClient.SendRebootSignal(ctx, node)

				// Update status based on reboot result
				var signalSentCondition metav1.Condition
				if rebootErr == nil {
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

	// Compare status to see if anything changed, and push updates if needed
	if !reflect.DeepEqual(originalRebootNode.Status, rebootNode.Status) {
		// Refresh the object before updating to avoid precondition failures
		var freshRebootNode janitordgxcnvidiacomv1alpha1.RebootNode
		if err := r.Get(ctx, req.NamespacedName, &freshRebootNode); err != nil {
			if apierrors.IsNotFound(err) {
				logger.V(0).Info("Post-reconciliation status update:", rebootNode.Name, "not found, object assumed deleted")
				return ctrl.Result{}, nil
			}
			logger.Error(err, "failed to refresh RebootNode before status update")
			return ctrl.Result{}, err
		}

		// Apply status changes to the fresh object
		freshRebootNode.Status = rebootNode.Status

		if err := r.Status().Update(ctx, &freshRebootNode); err != nil {
			logger.Error(err, "failed to update RebootNode status")
			return ctrl.Result{}, err
		}
		logger.V(0).Info("RebootNode status updated", "node", rebootNode.Spec.NodeName)
	}

	return result, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RebootNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
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
