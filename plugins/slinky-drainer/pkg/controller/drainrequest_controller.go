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
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	drainv1alpha1 "github.com/nvidia/nvsentinel/plugins/slinky-drainer/api/v1alpha1"
)

const (
	drainCompleteConditionType       = "DrainComplete"
	slurmNodeStateDrainConditionType = "SlurmNodeStateDrain"
	annotationKey                    = "nodeset.slinky.slurm.net/node-cordon-reason"
	annotationPrefix                 = "[J] [NVSentinel]"
	nvsentinelStateLabelKey          = "dgxc.nvidia.com/nvsentinel-state"
	drainRequestFinalizer            = "nvsentinel.nvidia.com/slinky-drainer"

	// Slurm base-state conditions that indicate the node still has running work.
	// A pod is only considered fully drained when SlurmNodeStateDrain is True
	// and none of these busy-state conditions are True.
	// Ref: https://github.com/SlinkyProject/slurm-operator/blob/main/pkg/conditions/conditions.go
	slurmNodeStateAllocatedConditionType  = "SlurmNodeStateAllocated"
	slurmNodeStateMixedConditionType      = "SlurmNodeStateMixed"
	slurmNodeStateCompletingConditionType = "SlurmNodeStateCompleting"
	slurmNodeStateUndrainConditionType    = "SlurmNodeStateUndrain"
)

type DrainRequestReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	PodCheckInterval time.Duration
	DrainTimeout     time.Duration
	SlinkyNamespace  string
}

func NewDrainRequestReconciler(
	mgr ctrl.Manager,
	podCheckInterval, drainTimeout time.Duration,
	slinkyNamespace string,
) *DrainRequestReconciler {
	return &DrainRequestReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		PodCheckInterval: podCheckInterval,
		DrainTimeout:     drainTimeout,
		SlinkyNamespace:  slinkyNamespace,
	}
}

func (r *DrainRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	drainReq := &drainv1alpha1.DrainRequest{}
	if err := r.Get(ctx, req.NamespacedName, drainReq); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !drainReq.DeletionTimestamp.IsZero() || isDrainComplete(drainReq) {
		return r.reconcileCompleted(ctx, drainReq)
	}

	if err := r.ensureFinalizer(ctx, drainReq); err != nil {
		return ctrl.Result{},
			fmt.Errorf("failed to add finalizer to DrainRequest %s/%s: %w", drainReq.Namespace, drainReq.Name, err)
	}

	if err := r.setNodeAnnotation(ctx, drainReq); err != nil {
		slog.Error("Failed to set node annotation", "drainrequest", req.NamespacedName, "error", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	pods, err := r.getSlinkyPods(ctx, drainReq.Spec.NodeName)
	if err != nil {
		slog.Error("Failed to list Slinky pods", "drainrequest", req.NamespacedName, "error", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	if len(pods) == 0 {
		slog.Info("No Slinky pods found on node, marking complete", "drainrequest", req.NamespacedName)
		return r.markDrainComplete(ctx, drainReq, "NoPods", "No Slinky pods found on node")
	}

	allDrained, notReadyPods := r.checkPodsFullyDrained(pods)
	if !allDrained {
		slog.Info("Waiting for pods to be fully drained",
			"drainrequest", req.NamespacedName,
			"total", len(pods),
			"notReady", len(notReadyPods),
			"notReadyPods", notReadyPods)

		return ctrl.Result{RequeueAfter: r.PodCheckInterval}, nil
	}

	if err := r.deleteSlinkyPods(ctx, pods); err != nil {
		slog.Error("Failed to delete Slinky pods", "drainrequest", req.NamespacedName, "error", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	slog.Info("Successfully drained all Slinky pods", "drainrequest", req.NamespacedName, "count", len(pods))

	return r.markDrainComplete(ctx, drainReq, "DrainComplete", "All Slinky pods drained successfully")
}

func (r *DrainRequestReconciler) reconcileCompleted(
	ctx context.Context,
	drainReq *drainv1alpha1.DrainRequest,
) (ctrl.Result, error) {
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: drainReq.Spec.NodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return r.removeFinalizer(ctx, drainReq)
		}

		// Return nil error to use fixed requeue interval instead of exponential backoff.
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if !shouldRemoveAnnotation(node) {
		return ctrl.Result{}, nil
	}

	if err := r.removeNodeAnnotation(ctx, node); err != nil {
		slog.Error("Failed to remove node annotation", "node", node.Name, "error", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	return r.removeFinalizer(ctx, drainReq)
}

func (r *DrainRequestReconciler) ensureFinalizer(ctx context.Context, drainReq *drainv1alpha1.DrainRequest) error {
	if controllerutil.ContainsFinalizer(drainReq, drainRequestFinalizer) {
		return nil
	}

	controllerutil.AddFinalizer(drainReq, drainRequestFinalizer)

	return r.Update(ctx, drainReq)
}

func (r *DrainRequestReconciler) removeFinalizer(
	ctx context.Context,
	drainReq *drainv1alpha1.DrainRequest,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(drainReq, drainRequestFinalizer) {
		return ctrl.Result{}, nil
	}

	controllerutil.RemoveFinalizer(drainReq, drainRequestFinalizer)

	if err := r.Update(ctx, drainReq); err != nil {
		return ctrl.Result{},
			fmt.Errorf("failed to remove finalizer from DrainRequest %s/%s: %w", drainReq.Namespace, drainReq.Name, err)
	}

	return ctrl.Result{}, nil
}

func shouldRemoveAnnotation(node *corev1.Node) bool {
	_, exists := node.Labels[nvsentinelStateLabelKey]

	return !exists
}

func (r *DrainRequestReconciler) removeNodeAnnotation(ctx context.Context, node *corev1.Node) error {
	val, ok := node.Annotations[annotationKey]
	if !ok || !strings.HasPrefix(val, annotationPrefix) {
		return nil
	}

	slog.Info("Node healthy, removing cordon annotation", "node", node.Name)

	delete(node.Annotations, annotationKey)

	if err := r.Update(ctx, node); err != nil {
		return fmt.Errorf("failed to update node %s: %w", node.Name, err)
	}

	return nil
}

func (r *DrainRequestReconciler) setNodeAnnotation(
	ctx context.Context,
	drainReq *drainv1alpha1.DrainRequest,
) error {
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: drainReq.Spec.NodeName}, node); err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	if node.Annotations != nil {
		if _, ok := node.Annotations[annotationKey]; ok {
			slog.Info("Node already has annotation, skipping", "node", drainReq.Spec.NodeName)
			return nil
		}
	}

	reason := buildCordonReason(drainReq)

	slog.Info("Setting node annotation", "node", drainReq.Spec.NodeName, "reason", reason)

	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	node.Annotations[annotationKey] = reason

	if err := r.Update(ctx, node); err != nil {
		return fmt.Errorf("failed to update node %s annotations: %w", node.Name, err)
	}

	return nil
}

func buildCordonReason(dr *drainv1alpha1.DrainRequest) string {
	var parts []string

	parts = append(parts, annotationPrefix)

	if len(dr.Spec.ErrorCode) > 0 {
		parts = append(parts, strings.Join(dr.Spec.ErrorCode, ","))
	}

	entities := formatEntitiesImpacted(dr.Spec.EntitiesImpacted)
	if entities != "" {
		parts = append(parts, entities)
	}

	message := dr.Spec.Reason
	if message == "" {
		message = dr.Spec.CheckName
	}

	if message == "" {
		message = "Health check failed"
	}

	return fmt.Sprintf("%s - %s", strings.Join(parts, " "), message)
}

func formatEntitiesImpacted(entities []drainv1alpha1.EntityImpacted) string {
	if len(entities) == 0 {
		return ""
	}

	var formatted []string
	for _, entity := range entities {
		formatted = append(formatted, fmt.Sprintf("%s:%s", entity.Type, entity.Value))
	}

	if len(formatted) > 3 {
		return fmt.Sprintf("%s,+%d more", strings.Join(formatted[:3], ","), len(formatted)-3)
	}

	return strings.Join(formatted, ",")
}

func (r *DrainRequestReconciler) getSlinkyPods(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}

	opts := []client.ListOption{
		client.InNamespace(r.SlinkyNamespace),
		client.MatchingFields{"spec.nodeName": nodeName},
	}

	if err := r.List(ctx, podList, opts...); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	return podList.Items, nil
}

func (r *DrainRequestReconciler) checkPodsFullyDrained(pods []corev1.Pod) (bool, []string) {
	var notReady []string

	for _, pod := range pods {
		if !isPodReady(&pod) {
			continue
		}

		if !isPodDrained(&pod) {
			notReady = append(notReady, pod.Name)
		}
	}

	return len(notReady) == 0, notReady
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}

	return false
}

// isPodDrained returns true when the Slurm node behind this pod has the DRAIN
// flag set, the UNDRAIN flag is NOT set, and no work is running. This mirrors
// the Slinky operator's IsNodeDrained logic so we only delete pods after all
// Slurm jobs have finished.
func isPodDrained(pod *corev1.Pod) bool {
	return isNodeDrain(pod) && !isPodBusy(pod)
}

func isNodeDrain(pod *corev1.Pod) bool {
	return hasPodCondition(pod, slurmNodeStateDrainConditionType) &&
		!hasPodCondition(pod, slurmNodeStateUndrainConditionType)
}

func isPodBusy(pod *corev1.Pod) bool {
	return hasPodCondition(pod, slurmNodeStateAllocatedConditionType) ||
		hasPodCondition(pod, slurmNodeStateMixedConditionType) ||
		hasPodCondition(pod, slurmNodeStateCompletingConditionType)
}

func hasPodCondition(pod *corev1.Pod, condType corev1.PodConditionType) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == condType && cond.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

func (r *DrainRequestReconciler) deleteSlinkyPods(ctx context.Context, pods []corev1.Pod) error {
	gracePeriod := int64(30)

	for _, pod := range pods {
		if err := r.Delete(ctx, &pod, &client.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
		}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete pod %s: %w", pod.Name, err)
		}
	}

	return nil
}

func (r *DrainRequestReconciler) markDrainComplete(
	ctx context.Context,
	dr *drainv1alpha1.DrainRequest,
	reason, message string,
) (ctrl.Result, error) {
	condition := metav1.Condition{
		Type:               drainCompleteConditionType,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}

	meta.SetStatusCondition(&dr.Status.Conditions, condition)

	if err := r.Status().Update(ctx, dr); err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second},
			fmt.Errorf("failed to update status for DrainRequest %s/%s: %w", dr.Namespace, dr.Name, err)
	}

	return ctrl.Result{}, nil
}

func isDrainComplete(dr *drainv1alpha1.DrainRequest) bool {
	for _, cond := range dr.Status.Conditions {
		if cond.Type == drainCompleteConditionType && cond.Status == metav1.ConditionTrue {
			return true
		}
	}

	return false
}

// nodeToMatchingDrainRequests maps a Node event to DrainRequests targeting that node.
func (r *DrainRequestReconciler) nodeToMatchingDrainRequests(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	node := obj.(*corev1.Node)

	drainRequests := &drainv1alpha1.DrainRequestList{}
	if err := r.List(ctx, drainRequests); err != nil {
		slog.Error("Failed to list DrainRequests for node watch", "node", node.Name, "error", err)
		return nil
	}

	var requests []reconcile.Request

	for i := range drainRequests.Items {
		if drainRequests.Items[i].Spec.NodeName == node.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      drainRequests.Items[i].Name,
					Namespace: drainRequests.Items[i].Namespace,
				},
			})
		}
	}

	return requests
}

func (r *DrainRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&corev1.Pod{},
		"spec.nodeName",
		func(obj client.Object) []string {
			pod := obj.(*corev1.Pod)
			return []string{pod.Spec.NodeName}
		},
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&drainv1alpha1.DrainRequest{}).
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.nodeToMatchingDrainRequests),
			builder.WithPredicates(predicate.LabelChangedPredicate{}),
		).
		Complete(r)
}
