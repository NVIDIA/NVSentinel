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
	"reflect"
	"time"

	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	cspv1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/csp/v1alpha1"
	janitordgxcnvidiacomv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
	"github.com/nvidia/nvsentinel/janitor/pkg/distributedlock"
	"github.com/nvidia/nvsentinel/janitor/pkg/metrics"
)

// RebootNodeReconciler reconciles a RebootNode object
type RebootNodeReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Config        *config.RebootNodeControllerConfig
	CSPClient     cspv1alpha1.CSPProviderServiceClient
	NodeLock      distributedlock.NodeLock
	LockNamespace string
	grpcConn      *grpc.ClientConn
}

// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=rebootnodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=rebootnodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=janitor.dgxc.nvidia.com,resources=rebootnodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
//nolint:dupl // Structural duplication with TerminateNode is acceptable - different business logic
func (r *RebootNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Get the RebootNode object
	var rebootNode janitordgxcnvidiacomv1alpha1.RebootNode
	if err := r.Get(ctx, req.NamespacedName, &rebootNode); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	completedReconciling := rebootNode.Status.CompletionTime != nil
	if !completedReconciling {
		locked := r.NodeLock.LockNode(ctx, &rebootNode, rebootNode.Spec.NodeName)
		if !locked {
			return ctrl.Result{RequeueAfter: time.Second * 2}, nil
		}

		result, err := r.reconcileHelper(ctx, &rebootNode)
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

	retryUnlock := r.NodeLock.CheckUnlock(ctx, &rebootNode, rebootNode.Spec.NodeName)
	if retryUnlock {
		return ctrl.Result{RequeueAfter: time.Second * 2}, nil
	}

	return ctrl.Result{}, nil
}

// reconcileHelper contains the main reconciliation logic
func (r *RebootNodeReconciler) reconcileHelper(ctx context.Context, rebootNode *janitordgxcnvidiacomv1alpha1.RebootNode) (ctrl.Result, error) {
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
		var nodeReadyErr error

		cspReady := false
		if *r.Config.ManualMode {
			cspReady = true
		} else {
			var rsp *cspv1alpha1.IsNodeReadyResponse

			rsp, nodeReadyErr = r.CSPClient.IsNodeReady(ctx, &cspv1alpha1.IsNodeReadyRequest{
				NodeName:  node.Name,
				RequestId: rebootNode.GetCSPReqRef(),
			})
			if nodeReadyErr != nil {
				cspReady = false
			} else {
				cspReady = rsp.IsReady
			}
		}

		// Check if kubernetes reports the node is ready.
		kubernetesReady := false

		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				kubernetesReady = condition.Status == corev1.ConditionTrue
			}
		}

		// nolint:gocritic // the if/else chain is fine
		if nodeReadyErr != nil {
			slog.Error("Node ready status check failed", "node", node.Name, "error", nodeReadyErr)

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
			slog.Info("Node reached ready state post-reboot", "node", node.Name)

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
			metrics.GlobalMetrics.RecordActionMTTR(metrics.ActionTypeReboot, time.Since(rebootNode.CreationTimestamp.Time))

			result = ctrl.Result{} // Don't requeue on success
		} else if time.Since(rebootNode.Status.StartTime.Time) > r.getRebootTimeout() {
			slog.Error("Node reboot timed out", "node", node.Name, "timeout", r.getRebootTimeout())

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
			result = ctrl.Result{RequeueAfter: 60 * time.Second}
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
			slog.Debug("Reboot signal already sent for node, continuing monitoring", "node", node.Name)

			result = ctrl.Result{RequeueAfter: 30 * time.Second}
		} else {
			if *r.Config.ManualMode {
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

				slog.Info("Manual mode enabled, janitor will not send reboot signal for node", "node", node.Name)

				result = ctrl.Result{}
			} else {
				// Start the reboot process
				metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeReboot, metrics.StatusStarted, node.Name)
				slog.Info("Sending reboot signal to node", "node", node.Name)
				rsp, rebootErr := r.CSPClient.SendRebootSignal(ctx, &cspv1alpha1.SendRebootSignalRequest{
					NodeName: node.Name,
				})

				// Update status based on reboot result
				var signalSentCondition metav1.Condition
				if rebootErr == nil {
					signalSentCondition = metav1.Condition{
						Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent,
						Status:             metav1.ConditionTrue,
						Reason:             "Succeeded",
						Message:            rsp.RequestId,
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

		objectKey := client.ObjectKey{Name: rebootNode.Name, Namespace: rebootNode.Namespace}
		if err := r.Get(ctx, objectKey, &freshRebootNode); err != nil {
			if apierrors.IsNotFound(err) {
				slog.Info("Post-reconciliation status update: not found, object assumed deleted", "node", rebootNode.Name)
				return ctrl.Result{}, nil
			}

			slog.Error("failed to refresh RebootNode before status update", "error", err)

			return ctrl.Result{}, err
		}

		// Apply status changes to the fresh object
		freshRebootNode.Status = rebootNode.Status

		if err := r.Status().Update(ctx, &freshRebootNode); err != nil {
			slog.Error("failed to update RebootNode status", "error", err)
			return ctrl.Result{}, err
		}

		slog.Info("RebootNode status updated", "node", rebootNode.Spec.NodeName)
	}

	return result, nil
}

// SetupWithManager sets up the controller with the Manager.
//
//nolint:dupl // Structural duplication with TerminateNode is acceptable - same setup pattern
func (r *RebootNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	conn, err := grpc.NewClient(r.Config.CSPProviderHost, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to create CSP client: %w", err)
	}

	r.grpcConn = conn
	r.CSPClient = cspv1alpha1.NewCSPProviderServiceClient(r.grpcConn)

	// Initialize NodeLock for distributed locking across maintenance operations
	r.NodeLock = distributedlock.NewNodeLock(mgr.GetClient(), r.LockNamespace)

	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return r.grpcConn.Close()
	})); err != nil {
		return fmt.Errorf("failed to add grpc connection cleanup to manager: %w", err)
	}

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
