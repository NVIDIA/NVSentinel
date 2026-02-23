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

// Package controller provides controllers for managing preflight resources.
package controller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nvidia/nvsentinel/preflight/pkg/config"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang/types"
	"github.com/nvidia/nvsentinel/preflight/pkg/webhook"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// GangController reconciles pods to update gang ConfigMaps with peer information.
type GangController struct {
	client.Client
	cfg         *config.Config
	coordinator *gang.Coordinator
	discoverer  gang.GangDiscoverer
}

// NewGangController creates a new gang controller.
func NewGangController(
	cfg *config.Config,
	client client.Client,
	coordinator *gang.Coordinator,
	discoverer gang.GangDiscoverer,
) *GangController {
	return &GangController{
		Client:      client,
		cfg:         cfg,
		coordinator: coordinator,
		discoverer:  discoverer,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (c *GangController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(c.podIPChangedPredicate()).
		Complete(c)
}

// podIPChangedPredicate returns a predicate that filters for gang pods with IP changes.
func (c *GangController) podIPChangedPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			pod, ok := e.Object.(*corev1.Pod)
			if !ok {
				return false
			}

			// Only process gang pods (injected by webhook) with an IP
			return hasGangConfigVolume(pod) && pod.Status.PodIP != ""
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, ok := e.ObjectOld.(*corev1.Pod)
			if !ok {
				return false
			}

			newPod, ok := e.ObjectNew.(*corev1.Pod)
			if !ok {
				return false
			}

			return hasGangConfigVolume(newPod) &&
				oldPod.Status.PodIP != newPod.Status.PodIP &&
				newPod.Status.PodIP != ""
		},
		DeleteFunc: func(_ event.DeleteEvent) bool {
			return false
		},
		GenericFunc: func(_ event.GenericEvent) bool {
			return false
		},
	}
}

// hasGangConfigVolume checks if the pod was injected by the webhook for gang coordination.
func hasGangConfigVolume(pod *corev1.Pod) bool {
	for _, vol := range pod.Spec.Volumes {
		if vol.Name == types.GangConfigVolumeName {
			return true
		}
	}

	return false
}

// Reconcile handles pod events to register gang peers.
func (c *GangController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pod corev1.Pod
	if err := c.Get(ctx, req.NamespacedName, &pod); err != nil {
		slog.Error("Pod deleted or not found", "error", err)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Skip if pod is terminating
	if pod.DeletionTimestamp != nil {
		slog.Info("Pod is terminating", "pod", pod.Name, "namespace", pod.Namespace)
		return ctrl.Result{}, nil
	}

	// Check if this pod belongs to a gang
	if c.discoverer == nil || !c.discoverer.CanHandle(&pod) {
		slog.Info("Pod does not belong to a gang", "pod", pod.Name, "namespace", pod.Namespace)
		return ctrl.Result{}, nil
	}

	gangID := c.discoverer.ExtractGangID(&pod)
	if gangID == "" {
		slog.Info("Pod does not have a gang ID", "pod", pod.Name, "namespace", pod.Namespace)
		return ctrl.Result{}, nil
	}

	gangInfo, err := c.discoverer.DiscoverPeers(ctx, &pod)
	if err != nil {
		slog.Error("Failed to discover gang peers",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"gangID", gangID,
			"error", err)

		return ctrl.Result{}, fmt.Errorf("failed to discover gang peers: %w", err)
	}

	if gangInfo == nil {
		slog.Info("No gang info found", "pod", pod.Name, "namespace", pod.Namespace)
		return ctrl.Result{}, nil
	}

	peer := gang.PeerInfo{
		PodName:   pod.Name,
		PodIP:     pod.Status.PodIP,
		NodeName:  pod.Spec.NodeName,
		Namespace: pod.Namespace,
	}

	if err := c.coordinator.RegisterPeer(ctx, pod.Namespace, gangInfo, peer); err != nil {
		slog.Error("Failed to register peer",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"gangID", gangID,
			"error", err)

		return ctrl.Result{}, fmt.Errorf("failed to register peer: %w", err)
	}

	slog.Info("Registered gang peer",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"gangID", gangID,
		"podIP", pod.Status.PodIP)

	return ctrl.Result{}, nil
}

// RegisterPod is called by the webhook when a pod is admitted that belongs to a gang.
// It creates the ConfigMap immediately so schedulers (like KAI) that validate
// ConfigMap existence before scheduling won't block.
func (c *GangController) RegisterPod(ctx context.Context, reg webhook.GangRegistration) {
	if reg.GangID == "" {
		slog.Info("Gang ID is empty", "namespace", reg.Namespace, "pod", reg.PodName)
		return
	}

	// Create ConfigMap immediately (with empty peer list).
	// Peer IPs will be added later when pods get scheduled and receive IPs.
	// This is needed as one of the schedulers (KAI) that we were targeting
	// validates the configmap before scheduling even for optional configmap volumes.
	// https://github.com/NVIDIA/KAI-Scheduler/issues/988
	if err := c.coordinator.EnsureConfigMap(ctx, reg.Namespace, reg.GangID, 0); err != nil {
		slog.Error("Failed to ensure gang ConfigMap",
			"namespace", reg.Namespace,
			"gangID", reg.GangID,
			"configMap", reg.ConfigMapName,
			"error", err)
	}

	// Create NCCL topology ConfigMap in the pod's namespace if topo data
	// is configured (e.g. Azure IB with ncclTopoShape). The topo XML is
	// loaded from the webhook's config and written to a ConfigMap that the
	// init container mounts at /etc/nccl/topo.xml.
	c.ensureNCCLTopoConfigMap(ctx, reg.Namespace)
}

func (c *GangController) ensureNCCLTopoConfigMap(ctx context.Context, namespace string) {
	gcfg := c.cfg.GangCoordination
	if gcfg.NCCLTopoConfigMap == "" || gcfg.NCCLTopoData == "" {
		return
	}

	existing := &corev1.ConfigMap{}

	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: gcfg.NCCLTopoConfigMap}, existing)
	if err == nil {
		return // already exists
	}

	if !errors.IsNotFound(err) {
		slog.Error("Failed to check NCCL topo ConfigMap",
			"namespace", namespace,
			"configMap", gcfg.NCCLTopoConfigMap,
			"error", err)

		return
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gcfg.NCCLTopoConfigMap,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/component": "nccl-topo",
				"app.kubernetes.io/name":      "nvsentinel",
			},
		},
		Data: map[string]string{
			"topo.xml": gcfg.NCCLTopoData,
		},
	}

	if err := c.Create(ctx, cm); err != nil && !errors.IsAlreadyExists(err) {
		slog.Error("Failed to create NCCL topo ConfigMap",
			"namespace", namespace,
			"configMap", gcfg.NCCLTopoConfigMap,
			"error", err)

		return
	}

	slog.Info("Created NCCL topo ConfigMap",
		"namespace", namespace,
		"configMap", gcfg.NCCLTopoConfigMap)
}
