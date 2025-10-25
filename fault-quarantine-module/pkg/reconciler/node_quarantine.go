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

package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/config"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

// other modules may also update the node, so we need to make sure that we retry on conflict
var customBackoff = wait.Backoff{
	Steps:    10,
	Duration: 10 * time.Millisecond,
	Factor:   1.5,
	Jitter:   0.1,
}

type FaultQuarantineClient struct {
	// client is the Kubernetes client
	clientset    kubernetes.Interface
	dryRunMode   bool
	nodeInformer NodeInfoProvider
}

// NodeInfoProvider defines the interface for getting node counts efficiently
type NodeInfoProvider interface {
	GetGpuNodeCounts() (totalGpuNodes int, cordonedNodesMap map[string]bool, err error)
	HasSynced() bool
}

func NewFaultQuarantineClient(kubeconfig string, dryRun bool) (*FaultQuarantineClient, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		if kubeconfig == "" {
			return nil, fmt.Errorf("kubeconfig is not set")
		}

		// build config from kubeconfig file
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("error creating Kubernetes config from kubeconfig: %w", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating clientset: %w", err)
	}

	client := &FaultQuarantineClient{
		clientset:  clientset,
		dryRunMode: dryRun,
	}

	return client, nil
}

func (c *FaultQuarantineClient) GetK8sClient() kubernetes.Interface {
	return c.clientset
}

func (c *FaultQuarantineClient) EnsureCircuitBreakerConfigMap(ctx context.Context,
	name, namespace string, initialStatus string) error {
	slog.Info("Ensuring circuit breaker config map",
		"name", name,
		"namespace", namespace,
		"initialStatus", initialStatus)

	cmClient := c.clientset.CoreV1().ConfigMaps(namespace)

	_, err := cmClient.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		slog.Info("Circuit breaker config map already exists",
			"name", name,
			"namespace", namespace)

		return nil
	}

	if !errors.IsNotFound(err) {
		slog.Error("Error getting circuit breaker config map",
			"name", name,
			"namespace", namespace,
			"error", err)

		return fmt.Errorf("failed to get circuit breaker config map %s/%s: %w", namespace, name, err)
	}

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string]string{"status": initialStatus},
	}

	_, err = cmClient.Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		slog.Error("Error creating circuit breaker config map",
			"name", name,
			"namespace", namespace,
			"error", err)

		return fmt.Errorf("failed to create circuit breaker config map %s/%s: %w", namespace, name, err)
	}

	return nil
}

func (c *FaultQuarantineClient) GetTotalGpuNodes(ctx context.Context) (int, error) {
	// Use NodeInformer lister if available and synced (much more efficient)
	if c.nodeInformer.HasSynced() {
		totalNodes, _, err := c.nodeInformer.GetGpuNodeCounts()
		if err == nil {
			slog.Debug("Got total GPU nodes from NodeInformer lister", "totalNodes", totalNodes)
			return totalNodes, nil
		}

		slog.Debug("NodeInformer failed, falling back to API", "error", err)
	}

	nodes, err := c.clientset.CoreV1().Nodes().List(ctx,
		metav1.ListOptions{LabelSelector: "nvidia.com/gpu.present=true"})
	if err != nil {
		return 0, fmt.Errorf("failed to list GPU nodes: %w", err)
	}

	slog.Debug("Got total GPU nodes from K8s API", "totalNodes", len(nodes.Items))

	return len(nodes.Items), nil
}

func (c *FaultQuarantineClient) SetNodeInformer(nodeInformer NodeInfoProvider) {
	c.nodeInformer = nodeInformer
}

func (c *FaultQuarantineClient) ReadCircuitBreakerState(ctx context.Context, name, namespace string) (string, error) {
	slog.Info("Reading circuit breaker state from config map",
		"name", name,
		"namespace", namespace)

	cm, err := c.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get circuit breaker config map %s/%s: %w", namespace, name, err)
	}

	if cm.Data == nil {
		return "", nil
	}

	return cm.Data["status"], nil
}

func (c *FaultQuarantineClient) WriteCircuitBreakerState(ctx context.Context, name, namespace, status string) error {
	cmClient := c.clientset.CoreV1().ConfigMaps(namespace)

	return retry.OnError(customBackoff, errors.IsConflict, func() error {
		cm, err := cmClient.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			slog.Error("Error getting circuit breaker config map",
				"name", name,
				"namespace", namespace,
				"error", err)

			return fmt.Errorf("failed to get circuit breaker config map %s/%s: %w", namespace, name, err)
		}

		if cm.Data == nil {
			cm.Data = map[string]string{}
		}

		cm.Data["status"] = status

		_, err = cmClient.Update(ctx, cm, metav1.UpdateOptions{})
		if err != nil {
			slog.Error("Error updating circuit breaker config map",
				"name", name,
				"namespace", namespace,
				"error", err)

			return fmt.Errorf("failed to update circuit breaker config map %s/%s: %w", namespace, name, err)
		}

		return nil
	})
}

// nolint: cyclop,gocognit //fix this as part of NGCC-21793
func (c *FaultQuarantineClient) TaintAndCordonNodeAndSetAnnotations(
	ctx context.Context,
	nodename string,
	taints []config.Taint,
	isCordon bool,
	annotations map[string]string,
	labels map[string]string,
) error {
	return retry.OnError(customBackoff, errors.IsConflict, func() error {
		node, err := c.clientset.CoreV1().Nodes().Get(ctx, nodename, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get node: %w", err)
		}

		// Taints check
		if len(taints) > 0 {
			// map to track existing taints
			existingTaints := make(map[config.Taint]v1.Taint)
			for _, taint := range node.Spec.Taints {
				existingTaints[config.Taint{Key: taint.Key, Value: taint.Value, Effect: string(taint.Effect)}] = taint
			}

			for _, taintConfig := range taints {
				key := config.Taint{Key: taintConfig.Key, Value: taintConfig.Value, Effect: string(taintConfig.Effect)}

				// Check if the taint is already present, if not then add it
				if _, exists := existingTaints[key]; !exists {
					slog.Info("Tainting node with taint config",
						"node", nodename,
						"taintConfig", taintConfig)

					existingTaints[key] = v1.Taint{
						Key:    taintConfig.Key,
						Value:  taintConfig.Value,
						Effect: v1.TaintEffect(taintConfig.Effect),
					}
				}
			}

			node.Spec.Taints = []v1.Taint{}
			for _, taint := range existingTaints {
				node.Spec.Taints = append(node.Spec.Taints, taint)
			}
		}

		// Cordon check
		// nolint: cyclop, gocognit, nestif //fix this as part of NGCC-21793
		if isCordon {
			_, exist := node.Annotations[common.QuarantineHealthEventAnnotationKey]
			if node.Spec.Unschedulable {
				if exist {
					slog.Info("Node already cordoned by FQM; skipping taint/annotation updates",
						"node", nodename)
					return nil
				}

				slog.Info("Node is cordoned manually; applying FQM taints/annotations",
					"node", nodename)
			} else {
				// Cordoning the node since it is currently schedulable.
				slog.Info("Cordoning node", "node", nodename)

				if !c.dryRunMode {
					node.Spec.Unschedulable = true
				}
			}
		}

		// Annotation check
		if len(annotations) > 0 {
			if node.Annotations == nil {
				node.Annotations = make(map[string]string)
			}

			slog.Info("Setting annotations on node",
				"node", nodename,
				"annotations", annotations)
			// set annotations
			for annotationKey, annotationValue := range annotations {
				node.Annotations[annotationKey] = annotationValue
			}
		}

		// Labels check
		if len(labels) > 0 {
			slog.Info("Adding labels on node", "node", nodename)

			for k, v := range labels {
				node.Labels[k] = v
			}
		}

		_, err = c.clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})

		if err != nil {
			return fmt.Errorf("failed to taint node: %w", err)
		}

		return nil
	})
}

// nolint: cyclop,gocognit //fix this as part of NGCC-21793
func (c *FaultQuarantineClient) UnTaintAndUnCordonNodeAndRemoveAnnotations(
	ctx context.Context,
	nodename string,
	taints []config.Taint,
	isUnCordon bool,
	annotationKeys []string,
	labelsToRemove []string,
	labels map[string]string,
) error {
	return retry.OnError(customBackoff, errors.IsConflict, func() error {
		node, err := c.clientset.CoreV1().Nodes().Get(ctx, nodename, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get node: %w", err)
		}

		// untaint check
		if len(taints) > 0 {
			taintsAlreadyPresentOnNodeMap := map[config.Taint]bool{}
			for _, taint := range node.Spec.Taints {
				taintsAlreadyPresentOnNodeMap[config.Taint{Key: taint.Key, Value: taint.Value, Effect: string(taint.Effect)}] = true
			}

			// Check if the taints are present
			toRemove := map[config.Taint]bool{}

			for _, taintConfig := range taints {
				key := config.Taint{
					Key:    taintConfig.Key,
					Value:  taintConfig.Value,
					Effect: taintConfig.Effect,
				}

				found := taintsAlreadyPresentOnNodeMap[key]
				if !found {
					slog.Info("Node already does not have the taint",
						"node", nodename,
						"taintConfig", taintConfig)
				} else {
					toRemove[taintConfig] = true
				}
			}

			if len(toRemove) == 0 {
				return nil
			}

			slog.Info("Untainting node with taint config",
				"node", nodename,
				"toRemove", toRemove)

			newTaints := []v1.Taint{}

			for _, taint := range node.Spec.Taints {
				if toRemove[config.Taint{Key: taint.Key, Value: taint.Value, Effect: string(taint.Effect)}] {
					// Skip taints that need to be removed
					continue
				}

				newTaints = append(newTaints, taint)
			}

			node.Spec.Taints = newTaints
		}

		// uncordon check
		if isUnCordon {
			slog.Info("Uncordoning node", "node", nodename)

			if !c.dryRunMode {
				node.Spec.Unschedulable = false
			}

			// Only add labels if labels map is provided (non-nil and non-empty)
			if len(labels) > 0 {
				slog.Info("Adding labels on node", "node", nodename)

				for k, v := range labels {
					node.Labels[k] = v
				}

				uncordonReason := node.Labels[cordonedReasonLabelKey]

				if len(uncordonReason) > 55 {
					uncordonReason = uncordonReason[:55]
				}

				node.Labels[uncordonedReasonLabelkey] = uncordonReason + "-removed"
			}
		}

		// Annotation check
		if len(annotationKeys) > 0 && node.Annotations != nil {
			// remove annotations
			for _, annotationKey := range annotationKeys {
				slog.Info("Removing annotation key from node",
					"annotationKey", annotationKey,
					"node", nodename)
				delete(node.Annotations, annotationKey)
			}
		}

		// Label check
		if len(labelsToRemove) > 0 {
			for _, labelKey := range labelsToRemove {
				slog.Info("Removing label key from node",
					"labelKey", labelKey,
					"node", nodename)
				delete(node.Labels, labelKey)
			}
		}

		_, err = c.clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to remove taint from node: %w", err)
		}

		return nil
	})
}

func (c *FaultQuarantineClient) GetNodeAnnotations(ctx context.Context, nodename string) (map[string]string, error) {
	node, err := c.clientset.CoreV1().Nodes().Get(ctx, nodename, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get node: %w", err)
	}

	if node.Annotations == nil {
		return map[string]string{}, nil
	}

	// return a copy of the annotations map to prevent unintended modifications
	annotations := make(map[string]string)
	for key, value := range node.Annotations {
		annotations[key] = value
	}

	return annotations, nil
}

func (c *FaultQuarantineClient) GetNodesWithAnnotation(ctx context.Context, annotationKey string) ([]string, error) {
	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	var nodesWithAnnotation []string

	for _, node := range nodes.Items {
		annotationValue, exists := node.Annotations[annotationKey]
		if exists && annotationValue != "" {
			nodesWithAnnotation = append(nodesWithAnnotation, node.Name)
		}
	}

	return nodesWithAnnotation, nil
}

// UpdateNodeAnnotations updates only the specified annotations on a node without affecting other properties
func (c *FaultQuarantineClient) UpdateNodeAnnotations(
	ctx context.Context,
	nodename string,
	annotations map[string]string,
) error {
	return retry.OnError(customBackoff, errors.IsConflict, func() error {
		node, err := c.clientset.CoreV1().Nodes().Get(ctx, nodename, metav1.GetOptions{})
		if err != nil {
			return err
		}

		// Update annotations
		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		for key, value := range annotations {
			node.Annotations[key] = value
		}

		updateOptions := metav1.UpdateOptions{}
		if c.dryRunMode {
			updateOptions.DryRun = []string{metav1.DryRunAll}
		}

		_, err = c.clientset.CoreV1().Nodes().Update(ctx, node, updateOptions)
		if err != nil {
			return fmt.Errorf("failed to update node %s annotations: %w", nodename, err)
		}

		slog.Info("Successfully updated annotations for node", "node", nodename)

		return nil
	})
}
