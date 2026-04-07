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
	"errors"
	"fmt"
	"reflect"
	"time"

	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// cspProviderDialFunc creates a CSP provider client and returns a cleanup function.
type cspProviderDialFunc func(ctx context.Context) (cspv1alpha1.CSPProviderServiceClient, func(), error)

// RebootNodeReconciler reconciles a RebootNode object
type RebootNodeReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Config        *config.RebootNodeControllerConfig
	NodeLock      distributedlock.NodeLock
	LockNamespace string

	// dialProviderFunc overrides the default gRPC dial behavior.
	// Used in tests to inject a mock CSP client.
	dialProviderFunc cspProviderDialFunc
}

// errRebootNodeDeleted is returned when the RebootNode was deleted during status update (do not requeue).
var errRebootNodeDeleted = errors.New("rebootnode deleted during status update")

// requeueBackoffForTransientCSPError is the delay before retrying after a transient gRPC/CSP error.
const requeueBackoffForTransientCSPError = 15 * time.Second

//nolint:lll // kubebuilder RBAC marker must stay on one line
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
func (r *RebootNodeReconciler) reconcileHelper(
	ctx context.Context, rebootNode *janitordgxcnvidiacomv1alpha1.RebootNode,
) (ctrl.Result, error) {
	originalRebootNode := rebootNode.DeepCopy()

	rebootNode.SetInitialConditions()
	rebootNode.SetStartTime()

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: rebootNode.Spec.NodeName}, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Create a fresh gRPC connection per reconciliation so that rotated
	// CA bundles and SA tokens are picked up from disk automatically.
	cspClient, cleanup, err := r.dialProvider(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("dial csp-provider: %w", err)
	}
	defer cleanup()

	var result ctrl.Result

	if rebootNode.IsRebootInProgress() {
		result = r.handleRebootInProgress(ctx, cspClient, rebootNode, &node)
	} else {
		result = r.handleRebootNotStarted(ctx, cspClient, rebootNode, &node)
	}

	if err := r.updateRebootNodeStatusIfChanged(ctx, originalRebootNode, rebootNode); err != nil {
		if errors.Is(err, errRebootNodeDeleted) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	return result, nil
}

// updateRebootNodeStatusIfChanged refreshes and updates status when it has changed;
// returns errRebootNodeDeleted if the object was deleted.
func (r *RebootNodeReconciler) updateRebootNodeStatusIfChanged(
	ctx context.Context, original, rebootNode *janitordgxcnvidiacomv1alpha1.RebootNode,
) error {
	if reflect.DeepEqual(original.Status, rebootNode.Status) {
		return nil
	}

	var freshRebootNode janitordgxcnvidiacomv1alpha1.RebootNode

	objectKey := client.ObjectKey{Name: rebootNode.Name, Namespace: rebootNode.Namespace}
	if err := r.Get(ctx, objectKey, &freshRebootNode); err != nil {
		if apierrors.IsNotFound(err) {
			slog.Info("Post-reconciliation status update: not found, object assumed deleted", "node", rebootNode.Name)

			return errRebootNodeDeleted
		}

		slog.Error("failed to refresh RebootNode before status update", "error", err)

		return fmt.Errorf("refreshing RebootNode %q before status update: %w", rebootNode.Name, err)
	}

	freshRebootNode.Status = rebootNode.Status

	if err := r.Status().Update(ctx, &freshRebootNode); err != nil {
		slog.Error("failed to update RebootNode status", "error", err)

		return fmt.Errorf("updating RebootNode %q status: %w", rebootNode.Name, err)
	}

	slog.Info("RebootNode status updated", "node", rebootNode.Spec.NodeName)

	return nil
}

// --- Reboot in progress: node ready checks and outcome ---

// isTransientGRPCError returns true for gRPC/network errors worth retrying
// (e.g. Unavailable, DeadlineExceeded).
func isTransientGRPCError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	s, ok := status.FromError(err)
	if !ok {
		return false
	}

	c := s.Code()

	return c == codes.Unavailable || c == codes.DeadlineExceeded
}

// handleRebootInProgress evaluates node ready state and returns the appropriate requeue result.
func (r *RebootNodeReconciler) handleRebootInProgress(
	ctx context.Context, cspClient cspv1alpha1.CSPProviderServiceClient,
	rebootNode *janitordgxcnvidiacomv1alpha1.RebootNode, node *corev1.Node,
) ctrl.Result {
	cspReady, nodeReadyErr := r.checkNodeReadyFromCSP(ctx, cspClient, rebootNode, node.Name)
	kubernetesReady := isNodeKubernetesReady(node)

	if nodeReadyErr != nil {
		if isTransientGRPCError(nodeReadyErr) {
			slog.Warn("Transient CSP error during node ready check, will requeue",
				"node", node.Name, "error", nodeReadyErr, "requeueAfter", requeueBackoffForTransientCSPError)

			return ctrl.Result{RequeueAfter: requeueBackoffForTransientCSPError}
		}

		slog.Error("Node ready status check failed", "node", node.Name, "error", nodeReadyErr)

		return r.completeNodeReadyCheck(rebootNode, node, metav1.ConditionFalse, "Failed",
			fmt.Sprintf("Node status could not be checked from CSP: %s", nodeReadyErr), metrics.StatusFailed)
	}

	if cspReady && kubernetesReady {
		slog.Info("Node reached ready state post-reboot", "node", node.Name)
		metrics.GlobalMetrics.RecordActionMTTR(metrics.ActionTypeReboot, time.Since(rebootNode.CreationTimestamp.Time))

		return r.completeNodeReadyCheck(rebootNode, node, metav1.ConditionTrue, "Succeeded",
			"Node reached ready state post-reboot", metrics.StatusSucceeded)
	}

	if time.Since(rebootNode.Status.StartTime.Time) > r.getRebootTimeout() {
		slog.Error("Node reboot timed out", "node", node.Name, "timeout", r.getRebootTimeout())

		return r.completeNodeReadyCheck(rebootNode, node, metav1.ConditionFalse, "Timeout",
			"Node failed to return to ready state after timeout duration", metrics.StatusFailed)
	}

	return ctrl.Result{RequeueAfter: 60 * time.Second}
}

func (r *RebootNodeReconciler) completeNodeReadyCheck(
	rebootNode *janitordgxcnvidiacomv1alpha1.RebootNode, node *corev1.Node,
	conditionStatus metav1.ConditionStatus, reason, message, metricsStatus string,
) ctrl.Result {
	rebootNode.SetCompletionTime()
	rebootNode.SetCondition(metav1.Condition{
		Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionNodeReady,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
	metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeReboot, metricsStatus, node.Name)

	return ctrl.Result{}
}

func (r *RebootNodeReconciler) checkNodeReadyFromCSP(
	ctx context.Context, cspClient cspv1alpha1.CSPProviderServiceClient,
	rebootNode *janitordgxcnvidiacomv1alpha1.RebootNode, nodeName string,
) (bool, error) {
	if *r.Config.ManualMode {
		return true, nil
	}

	rsp, err := cspClient.IsNodeReady(ctx, &cspv1alpha1.IsNodeReadyRequest{
		NodeName:  nodeName,
		RequestId: rebootNode.GetCSPReqRef(),
	})
	if err != nil {
		return false, err
	}

	return rsp.IsReady, nil
}

func isNodeKubernetesReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

// --- Reboot not started: signal sent / manual mode / send signal ---

// handleRebootNotStarted handles the case when reboot has not yet started (signal not sent or manual mode).
func (r *RebootNodeReconciler) handleRebootNotStarted(
	ctx context.Context, cspClient cspv1alpha1.CSPProviderServiceClient,
	rebootNode *janitordgxcnvidiacomv1alpha1.RebootNode, node *corev1.Node,
) ctrl.Result {
	if hasConditionTrue(rebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent) {
		slog.Debug("Reboot signal already sent for node, continuing monitoring", "node", node.Name)

		return ctrl.Result{RequeueAfter: 30 * time.Second}
	}

	if *r.Config.ManualMode {
		return r.handleManualMode(rebootNode, node)
	}

	return r.sendRebootSignalAndSetCondition(ctx, cspClient, rebootNode, node)
}

func hasConditionTrue(conditions []metav1.Condition, condType string) bool {
	for _, c := range conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}

	return false
}

func (r *RebootNodeReconciler) handleManualMode(
	rebootNode *janitordgxcnvidiacomv1alpha1.RebootNode, node *corev1.Node,
) ctrl.Result {
	if !hasConditionTrue(rebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.ManualModeConditionType) {
		rebootNode.SetCondition(metav1.Condition{
			Type:               janitordgxcnvidiacomv1alpha1.ManualModeConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             "OutsideActorRequired",
			Message:            "Janitor is in manual mode, outside actor required to send reboot signal",
			LastTransitionTime: metav1.Now(),
		})
		metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeReboot, metrics.StatusStarted, node.Name)
	}

	slog.Info("Manual mode enabled, janitor will not send reboot signal for node", "node", node.Name)

	return ctrl.Result{}
}

func (r *RebootNodeReconciler) sendRebootSignalAndSetCondition(
	ctx context.Context, cspClient cspv1alpha1.CSPProviderServiceClient,
	rebootNode *janitordgxcnvidiacomv1alpha1.RebootNode, node *corev1.Node,
) ctrl.Result {
	metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeReboot, metrics.StatusStarted, node.Name)
	slog.Info("Sending reboot signal to node", "node", node.Name)

	rsp, rebootErr := cspClient.SendRebootSignal(ctx, &cspv1alpha1.SendRebootSignalRequest{
		NodeName: node.Name,
	})
	if rebootErr == nil {
		rebootNode.SetCondition(metav1.Condition{
			Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent,
			Status:             metav1.ConditionTrue,
			Reason:             "Succeeded",
			Message:            rsp.RequestId,
			LastTransitionTime: metav1.Now(),
		})

		return ctrl.Result{RequeueAfter: 30 * time.Second}
	}

	if isTransientGRPCError(rebootErr) {
		slog.Warn("Transient CSP error sending reboot signal, will requeue",
			"node", node.Name, "error", rebootErr)

		return ctrl.Result{RequeueAfter: 30 * time.Second}
	}

	rebootNode.SetCompletionTime()
	rebootNode.SetCondition(metav1.Condition{
		Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent,
		Status:             metav1.ConditionFalse,
		Reason:             "Failed",
		Message:            rebootErr.Error(),
		LastTransitionTime: metav1.Now(),
	})
	metrics.GlobalMetrics.IncActionCount(metrics.ActionTypeReboot, metrics.StatusFailed, node.Name)

	return ctrl.Result{}
}

// dialProvider creates a fresh gRPC connection to the CSP provider.
// A new connection is created per reconciliation so that rotated CA bundles
// and SA tokens are picked up from disk automatically without watchers.
//
//nolint:dupl // Structural duplication with TerminateNode is acceptable - same dial pattern
func (r *RebootNodeReconciler) dialProvider(ctx context.Context) (cspv1alpha1.CSPProviderServiceClient, func(), error) {
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
func (r *RebootNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
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
