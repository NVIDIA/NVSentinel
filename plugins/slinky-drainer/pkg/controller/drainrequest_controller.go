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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	drainv1alpha1 "github.com/nvidia/nvsentinel/plugins/slinky-drainer/api/v1alpha1"
)

const (
	drainCompleteConditionType       = "DrainComplete"
	slurmNodeStateDrainConditionType = "SlurmNodeStateDrain"
	annotationKey                    = "nodeset.slinky.slurm.net/node-cordon-reason"
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
	logger := log.FromContext(ctx).WithValues("drainrequest", req.NamespacedName)

	drainReq := &drainv1alpha1.DrainRequest{}
	if err := r.Get(ctx, req.NamespacedName, drainReq); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if isDrainComplete(drainReq) {
		logger.Info("Drain already complete")
		return ctrl.Result{}, nil
	}

	if err := r.setNodeAnnotation(ctx, drainReq); err != nil {
		logger.Error(err, "Failed to set node annotation")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	pods, err := r.getSlinkyPods(ctx, drainReq.Spec.NodeName)
	if err != nil {
		logger.Error(err, "Failed to list Slinky pods")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	if len(pods) == 0 {
		logger.Info("No Slinky pods found on node, marking complete")
		return r.markDrainComplete(ctx, drainReq, "NoPods", "No Slinky pods found on node")
	}

	allReady, notReadyPods := r.checkPodsReadyForDrain(pods)
	if !allReady {
		logger.Info("Waiting for pods to be ready for drain",
			"total", len(pods),
			"notReady", len(notReadyPods),
			"notReadyPods", notReadyPods)

		return ctrl.Result{RequeueAfter: r.PodCheckInterval}, nil
	}

	if err := r.deleteSlinkyPods(ctx, pods); err != nil {
		logger.Error(err, "Failed to delete Slinky pods")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	logger.Info("Successfully drained all Slinky pods", "count", len(pods))

	return r.markDrainComplete(ctx, drainReq, "DrainComplete", "All Slinky pods drained successfully")
}

func (r *DrainRequestReconciler) setNodeAnnotation(
	ctx context.Context,
	drainReq *drainv1alpha1.DrainRequest,
) error {
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: drainReq.Spec.NodeName}, node); err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	reason := buildCordonReason(drainReq)

	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	if existing, ok := node.Annotations[annotationKey]; ok && existing == reason {
		return nil
	}

	node.Annotations[annotationKey] = reason

	return r.Update(ctx, node)
}

func buildCordonReason(dr *drainv1alpha1.DrainRequest) string {
	var parts []string

	parts = append(parts, "[J]")

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

func (r *DrainRequestReconciler) checkPodsReadyForDrain(pods []corev1.Pod) (bool, []string) {
	var notReady []string

	for _, pod := range pods {
		if !hasSlurmDrainCondition(&pod) {
			notReady = append(notReady, pod.Name)
		}
	}

	return len(notReady) == 0, notReady
}

func hasSlurmDrainCondition(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == slurmNodeStateDrainConditionType && cond.Status == corev1.ConditionTrue {
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
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
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
		Complete(r)
}
