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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	annotationKey           = "nodeset.slinky.slurm.net/node-cordon-reason"
	slurmDrainConditionType = "SlurmNodeStateDrain"
)

type NodeReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	SlinkyNamespace   string
	ReconcileInterval time.Duration
}

func NewNodeReconciler(c client.Client,
	scheme *runtime.Scheme, slinkyNamespace string, reconcileInterval time.Duration) *NodeReconciler {
	return &NodeReconciler{
		Client:            c,
		Scheme:            scheme,
		SlinkyNamespace:   slinkyNamespace,
		ReconcileInterval: reconcileInterval,
	}
}

func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	reason, hasAnnotation := node.Annotations[annotationKey]
	if !hasAnnotation {
		return ctrl.Result{}, nil
	}

	logger.Info("Found node with drain annotation",
		"node", node.Name,
		"reason", reason)

	pods, err := r.listSlinkyPodsOnNode(ctx, node.Name)
	if err != nil {
		logger.Error(err, "Failed to list pods on node")
		return ctrl.Result{RequeueAfter: r.ReconcileInterval}, err
	}

	if len(pods) == 0 {
		logger.Info("No slinky pods found on node", "node", node.Name)
		return ctrl.Result{}, nil
	}

	updatedCount := 0

	for i := range pods {
		pod := &pods[i]
		if hasSlurmDrainCondition(pod) {
			continue
		}

		if err := r.updatePodDrainCondition(ctx, pod, reason); err != nil {
			logger.Error(err, "Failed to update pod condition",
				"pod", pod.Name,
				"namespace", pod.Namespace)

			continue
		}

		updatedCount++
	}

	if updatedCount > 0 {
		logger.Info("Updated pod drain conditions",
			"node", node.Name,
			"totalPods", len(pods),
			"updated", updatedCount)
	}

	return ctrl.Result{}, nil
}

func (r *NodeReconciler) listSlinkyPodsOnNode(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	listOpts := &client.ListOptions{
		Namespace:     r.SlinkyNamespace,
		FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": nodeName}),
	}

	if err := r.List(ctx, podList, listOpts); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	return podList.Items, nil
}

func (r *NodeReconciler) updatePodDrainCondition(ctx context.Context, pod *corev1.Pod, reason string) error {
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:               slurmDrainConditionType,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "NodeCordonedBySlinkyDrainer",
		Message:            reason,
	})

	return r.Status().Update(ctx, pod)
}

func hasSlurmDrainCondition(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == slurmDrainConditionType && condition.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Complete(r)
}
