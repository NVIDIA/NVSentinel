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
	"reflect"
	"time"

	"log/slog"

	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cspv1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/csp/v1alpha1"
	janitordgxcnvidiacomv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	grpcclient "github.com/nvidia/nvsentinel/janitor/pkg/client"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
	"github.com/nvidia/nvsentinel/janitor/pkg/distributedlock"
	"github.com/nvidia/nvsentinel/janitor/pkg/metrics"
)

// TerminateNodeReconciler manages the terminate node operation.
type TerminateNodeReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Config        *config.TerminateNodeControllerConfig
	NodeLock      distributedlock.NodeLock
	LockNamespace string

	// dialProviderFunc overrides the default gRPC dial behavior.
	// Used in tests to inject a mock CSP client.
	dialProviderFunc cspProviderDialFunc
}

// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=terminatenodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=terminatenodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=terminatenodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;delete

// Steps of the terminate node reconciliation loop:
// 1. Initialize conditions and start time.
// 2. Check if a terminate signal has already been sent.
// 3. Delete the node if it is not ready.
// 4. If node is ready, check for timeout.
// 5. If signal has not been sent, send it to the CSP instance.
// 6. Write status updates to the TerminateNode CR.
//
//nolint:dupl // Structural duplication with RebootNode is acceptable - different business logic
func (r *TerminateNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var terminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
	if err := r.Get(ctx, req.NamespacedName, &terminateNode); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if reconciliation is complete
	completedReconciling := terminateNode.Status.CompletionTime != nil
	if !completedReconciling {
		locked := r.NodeLock.LockNode(ctx, &terminateNode, terminateNode.Spec.NodeName)
		if !locked {
			return ctrl.Result{RequeueAfter: time.Second * 2}, nil
		}

		result, err := r.reconcileHelper(ctx, &terminateNode)
		// We will always re-queue the object and check if Unlock is needed on the next reconcile rather than
		// re-fetch the object or require reconcileHelper to specify it completed reconciling. If the controller
		// forces a re-queue by returning an error or setting a RequeueAfter, we will respect that re-queue behavior,
		// else we will manually set RequeueAfter. As a result, if the controller doesn't force a re-queue or forces
		// a re-queue by setting Requeue, we will set RequeueAfter. Setting Requeue to true is deprecated
		// and can lead to long reconcile times so we will enforce that RequeueAfter is used.
		if err != nil || result.RequeueAfter > 0 {
			return result, err
		}

		return ctrl.Result{RequeueAfter: time.Second * 2}, nil
	}

	retryUnlock := r.NodeLock.CheckUnlock(ctx, &terminateNode, terminateNode.Spec.NodeName)
	if retryUnlock {
		return ctrl.Result{RequeueAfter: time.Second * 2}, nil
	}

	return ctrl.Result{}, nil
}

// reconcileHelper contains the main reconciliation logic
func (r *TerminateNodeReconciler) reconcileHelper(ctx context.Context, terminateNode *janitordgxcnvidiacomv1alpha1.TerminateNode) (ctrl.Result, error) {
	// Take a deep copy to compare against at the end
	originalTerminateNode := terminateNode.DeepCopy()

	var result ctrl.Result

	// Initialize conditions if not already set
	terminateNode.SetInitialConditions()

	// Set the start time if it is not already set
	terminateNode.SetStartTime()

	// Get the node to terminate
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: terminateNode.Spec.NodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			// Node is already deleted, which is the desired state. Do not return an error.
			node = nil
		} else {
			return ctrl.Result{}, err
		}
	}

	// Create a fresh gRPC connection per reconciliation so that rotated
	// CA bundles and SA tokens are picked up from disk automatically.
	cspClient, cleanup, err := r.dialProvider(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("dial csp-provider: %w", err)
	}
	defer cleanup()

	if terminateNode.IsTerminateInProgress() {
		// nolint:gocritic // the if/else chain is fine
		if node == nil {
			terminateNode.SetCompletionTime()
			terminateNode.SetCondition(metav1.Condition{
				Type:               janitordgxcnvidiacomv1alpha1.TerminateNodeConditionNodeTerminated,
				Status:             metav1.ConditionTrue,
				Reason:             "Succeeded",
				Message:            "CSP instance deleted and Kubernetes node removed.",
				LastTransitionTime: metav1.Now(),
			})

			// Record successful termination metrics
			metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeTerminate, metrics.StatusSucceeded, terminateNode.Spec.NodeName)
			metrics.GlobalMetrics.RecordActionMTTR(metrics.ActionTypeTerminate, time.Since(terminateNode.CreationTimestamp.Time))

			result = ctrl.Result{} // Don't requeue on success
		} else if isNodeNotReady(node) {
			slog.Info("Node reached not ready state, deleting from cluster", "node", terminateNode.Spec.NodeName)

			if err := r.Delete(ctx, node); err != nil {
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
			metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeTerminate, metrics.StatusSucceeded, node.Name)
			metrics.GlobalMetrics.RecordActionMTTR(metrics.ActionTypeTerminate, time.Since(terminateNode.CreationTimestamp.Time))

			result = ctrl.Result{} // Don't requeue on success
		} else if time.Since(terminateNode.Status.StartTime.Time) > r.getTimeout() {
			slog.Error("Node terminate timed out", "node", node.Name, "timeout", r.getTimeout())

			// Update status
			terminateNode.SetCompletionTime()
			terminateNode.SetCondition(metav1.Condition{
				Type:               janitordgxcnvidiacomv1alpha1.TerminateNodeConditionNodeTerminated,
				Status:             metav1.ConditionFalse,
				Reason:             "Timeout",
				Message:            "Node failed to transition to not ready state after timeout duration",
				LastTransitionTime: metav1.Now(),
			})

			metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeTerminate, metrics.StatusFailed, node.Name)

			result = ctrl.Result{} // Don't requeue on timeout
		} else {
			// Still waiting for terminate to complete
			result = ctrl.Result{RequeueAfter: 30 * time.Second}
		}
	} else {
		// If this case is hit it means that the node did not exist when
		// the CR was created. This case should be handled by the admission webhook.
		if node == nil {
			return ctrl.Result{}, errors.New("node not found and terminate not in progress")
		}

		// Check if signal was already sent (but terminate not in progress due to other issues)
		signalAlreadySent := false

		for _, condition := range terminateNode.Status.Conditions {
			if condition.Type == janitordgxcnvidiacomv1alpha1.TerminateNodeConditionSignalSent && condition.Status == metav1.ConditionTrue {
				signalAlreadySent = true
				break
			}
		}

		if signalAlreadySent {
			// Signal was already sent, just continue monitoring
			slog.Debug("Terminate signal already sent for node, continuing monitoring", "node", node.Name)

			result = ctrl.Result{RequeueAfter: 30 * time.Second}
		} else {
			if *r.Config.ManualMode {
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
					metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeTerminate, metrics.StatusStarted, node.Name)
				}

				slog.Info("Manual mode enabled, janitor will not send terminate signal for node", "node", node.Name)

				result = ctrl.Result{}
			} else {
				slog.Info("Sending terminate signal to node", "node", terminateNode.Spec.NodeName)
				metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeTerminate, metrics.StatusStarted, node.Name)
				_, terminateErr := cspClient.SendTerminateSignal(ctx, &cspv1alpha1.SendTerminateSignalRequest{
					NodeName: node.Name,
				})

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

					metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeTerminate, metrics.StatusFailed, node.Name)
				}

				terminateNode.SetCondition(signalSentCondition)
			}
		}
	}

	// Compare status to see if anything changed, and push updates if needed
	if !reflect.DeepEqual(originalTerminateNode.Status, terminateNode.Status) {
		// Refresh the object before updating to avoid precondition failures
		var freshTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode

		objectKey := client.ObjectKey{Name: terminateNode.Name, Namespace: terminateNode.Namespace}
		if err := r.Get(ctx, objectKey, &freshTerminateNode); err != nil {
			if apierrors.IsNotFound(err) {
				slog.Debug("Post-reconciliation status update: not found, object assumed deleted", "node", terminateNode.Name)

				return ctrl.Result{}, nil
			}

			slog.Error("failed to refresh TerminateNode before status update", "error", err)

			return ctrl.Result{}, err
		}

		// Apply status changes to the fresh object
		freshTerminateNode.Status = terminateNode.Status

		if err := r.Status().Update(ctx, &freshTerminateNode); err != nil {
			slog.Error("failed to update TerminateNode status", "error", err)
			return ctrl.Result{}, err
		}

		slog.Info("TerminateNode status updated", "node", terminateNode.Spec.NodeName)
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

// getTimeout returns the timeout for terminate operations
func (r *TerminateNodeReconciler) getTimeout() time.Duration {
	cfg := r.Config
	if cfg == nil || cfg.Timeout == 0 {
		return 30 * time.Minute // fallback default
	}

	return cfg.Timeout
}

// dialProvider creates a fresh gRPC connection to the CSP provider.
// A new connection is created per reconciliation so that rotated CA bundles
// and SA tokens are picked up from disk automatically without watchers.
//
//nolint:dupl // Structural duplication with RebootNode is acceptable - same dial pattern
func (r *TerminateNodeReconciler) dialProvider(ctx context.Context) (cspv1alpha1.CSPProviderServiceClient, func(), error) {
	if r.dialProviderFunc != nil {
		return r.dialProviderFunc(ctx)
	}

	dialOpts, err := grpcclient.NewCSPProviderDialOptions(r.Config.CSPProviderCAPath, r.Config.CSPProviderInsecure)
	if err != nil {
		return nil, nil, fmt.Errorf("create dial options: %w", err)
	}

	if !r.Config.CSPProviderInsecure {
		tokenPath := r.Config.CSPProviderTokenPath
		if tokenPath == "" {
			tokenPath = grpcclient.DefaultSATokenPath
		}

		dialOpts = append(dialOpts,
			grpc.WithUnaryInterceptor(grpcclient.TokenInterceptor(tokenPath)))
	}

	conn, err := grpc.NewClient(r.Config.CSPProviderHost, dialOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("dial csp-provider: %w", err)
	}

	return cspv1alpha1.NewCSPProviderServiceClient(conn), func() { conn.Close() }, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TerminateNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	slog.Info("Configuring CSP provider connection",
		"host", r.Config.CSPProviderHost,
		"insecure", r.Config.CSPProviderInsecure)

	if !r.Config.CSPProviderInsecure {
		tokenPath := r.Config.CSPProviderTokenPath
		if tokenPath == "" {
			tokenPath = grpcclient.DefaultSATokenPath
		}

		slog.Info("CSP provider gRPC auth enabled", "tokenPath", tokenPath)
	}

	// Initialize NodeLock for distributed locking across maintenance operations
	r.NodeLock = distributedlock.NewNodeLock(mgr.GetClient(), r.LockNamespace)

	return ctrl.NewControllerManagedBy(mgr).
		For(&janitordgxcnvidiacomv1alpha1.TerminateNode{}).
		Named("terminatenode").
		Complete(r)
}
