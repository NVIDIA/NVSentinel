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

package informer

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/breaker"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

var customBackoff = wait.Backoff{
	Steps:    10,
	Duration: 10 * time.Millisecond,
	Factor:   1.5,
	Jitter:   0.1,
}

type FaultQuarantineClient struct {
	Clientset                kubernetes.Interface
	DryRunMode               bool
	NodeInformer             *NodeInformer
	cordonedReasonLabelKey   string
	uncordonedReasonLabelKey string
	operationMutex           sync.Map // map[string]*sync.Mutex for per-node locking
}

func NewFaultQuarantineClient(kubeconfig string, dryRun bool,
	resyncPeriod time.Duration) (*FaultQuarantineClient, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("error creating Kubernetes config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating clientset: %w", err)
	}

	nodeInformer, err := NewNodeInformer(clientset, resyncPeriod)
	if err != nil {
		return nil, fmt.Errorf("error creating node informer: %w", err)
	}

	client := &FaultQuarantineClient{
		Clientset:    clientset,
		DryRunMode:   dryRun,
		NodeInformer: nodeInformer,
	}

	return client, nil
}

func (c *FaultQuarantineClient) EnsureCircuitBreakerConfigMap(ctx context.Context,
	name, namespace string, initialStatus breaker.State) error {
	slog.Info("Ensuring circuit breaker config map",
		"name", name, "namespace", namespace, "initialStatus", initialStatus)

	cmClient := c.Clientset.CoreV1().ConfigMaps(namespace)

	_, err := cmClient.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		slog.Info("Circuit breaker config map already exists", "name", name, "namespace", namespace)
		return nil
	}

	if !errors.IsNotFound(err) {
		slog.Error("Error getting circuit breaker config map", "name", name, "namespace", namespace, "error", err)
		return fmt.Errorf("failed to get config map %s in namespace %s: %w", name, namespace, err)
	}

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string]string{"status": string(initialStatus)},
	}

	_, err = cmClient.Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		slog.Error("Error creating circuit breaker config map", "name", name, "namespace", namespace, "error", err)
		return fmt.Errorf("failed to create config map %s in namespace %s: %w", name, namespace, err)
	}

	return nil
}

func (c *FaultQuarantineClient) GetTotalNodes(ctx context.Context) (int, error) {
	totalNodes, _, err := c.NodeInformer.GetNodeCounts()
	if err != nil {
		return 0, fmt.Errorf("failed to get node counts from informer: %w", err)
	}

	slog.Debug("Got total nodes from NodeInformer cache", "totalNodes", totalNodes)

	return totalNodes, nil
}

func (c *FaultQuarantineClient) SetLabelKeys(cordonedReasonKey, uncordonedReasonKey string) {
	c.cordonedReasonLabelKey = cordonedReasonKey
	c.uncordonedReasonLabelKey = uncordonedReasonKey
}

func (c *FaultQuarantineClient) UpdateNode(ctx context.Context, nodeName string, updateFn func(*v1.Node) error) error {
	mu, _ := c.operationMutex.LoadOrStore(nodeName, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()

	defer mu.(*sync.Mutex).Unlock()

	return retry.OnError(retry.DefaultBackoff, errors.IsConflict, func() error {
		node, err := c.Clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if err := updateFn(node); err != nil {
			return err
		}

		_, err = c.Clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		if err != nil {
			return err
		}

		slog.Debug("Updated node", "node", nodeName)

		return nil
	})
}

func (c *FaultQuarantineClient) ReadCircuitBreakerState(
	ctx context.Context, name, namespace string,
) (breaker.State, error) {
	slog.Info("Reading circuit breaker state from config map",
		"name", name, "namespace", namespace)

	cm, err := c.Clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get config map %s in namespace %s: %w", name, namespace, err)
	}

	if cm.Data == nil {
		return "", nil
	}

	return breaker.State(cm.Data["status"]), nil
}

func (c *FaultQuarantineClient) WriteCircuitBreakerState(
	ctx context.Context, name, namespace string, state breaker.State,
) error {
	cmClient := c.Clientset.CoreV1().ConfigMaps(namespace)

	return retry.OnError(customBackoff, errors.IsConflict, func() error {
		cm, err := cmClient.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			slog.Error("Error getting circuit breaker config map", "name", name, "namespace", namespace, "error", err)
			return err
		}

		if cm.Data == nil {
			cm.Data = map[string]string{}
		}

		cm.Data["status"] = string(state)

		_, err = cmClient.Update(ctx, cm, metav1.UpdateOptions{})
		if err != nil {
			slog.Error("Error updating circuit breaker config map", "name", name, "namespace", namespace, "error", err)
		}

		return err
	})
}

func (c *FaultQuarantineClient) QuarantineNodeAndSetAnnotations(
	ctx context.Context,
	nodename string,
	taints []config.Taint,
	isCordon bool,
	annotations map[string]string,
	labels map[string]string,
) error {
	updateFn := func(node *v1.Node) error {
		if len(taints) > 0 {
			if err := c.applyTaints(node, taints, nodename); err != nil {
				return fmt.Errorf("failed to apply taints to node %s: %w", nodename, err)
			}
		}

		if isCordon {
			if shouldSkip := c.handleCordon(node, nodename); shouldSkip {
				return nil
			}
		}

		if len(annotations) > 0 {
			c.applyAnnotations(node, annotations, nodename)
		}

		if len(labels) > 0 {
			c.applyLabels(node, labels, nodename)
		}

		return nil
	}

	return c.UpdateNode(ctx, nodename, updateFn)
}

func (c *FaultQuarantineClient) applyTaints(node *v1.Node, taints []config.Taint, nodename string) error {
	if c.DryRunMode {
		slog.Info("DryRun mode enabled, skipping taint application", "node", nodename)
		return nil
	}

	existingTaints := make(map[config.Taint]v1.Taint)
	for _, taint := range node.Spec.Taints {
		existingTaints[config.Taint{Key: taint.Key, Value: taint.Value, Effect: string(taint.Effect)}] = taint
	}

	for _, taintConfig := range taints {
		key := config.Taint{Key: taintConfig.Key, Value: taintConfig.Value, Effect: string(taintConfig.Effect)}

		if _, exists := existingTaints[key]; !exists {
			slog.Info("Tainting node", "node", nodename, "taintConfig", taintConfig)
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

	return nil
}

func (c *FaultQuarantineClient) handleCordon(node *v1.Node, nodename string) bool {
	_, exist := node.Annotations[common.QuarantineHealthEventAnnotationKey]

	if node.Spec.Unschedulable {
		if exist {
			slog.Info("Node already cordoned by FQM; skipping taint/annotation updates", "node", nodename)
			return true
		}

		slog.Info("Node is cordoned manually; applying FQM taints/annotations", "node", nodename)
	} else {
		slog.Info("Cordoning node", "node", nodename)

		if !c.DryRunMode {
			node.Spec.Unschedulable = true
		}
	}

	return false
}

func (c *FaultQuarantineClient) applyAnnotations(node *v1.Node, annotations map[string]string, nodename string) {
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	slog.Info("Setting annotations on node", "node", nodename, "annotations", annotations)

	for annotationKey, annotationValue := range annotations {
		node.Annotations[annotationKey] = annotationValue
	}
}

func (c *FaultQuarantineClient) applyLabels(node *v1.Node, labels map[string]string, nodename string) {
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}

	slog.Info("Adding labels on node", "node", nodename)

	for k, v := range labels {
		node.Labels[k] = v
	}
}

func (c *FaultQuarantineClient) UnQuarantineNodeAndRemoveAnnotations(
	ctx context.Context,
	nodename string,
	taints []config.Taint,
	annotationKeys []string,
	labelsToRemove []string,
	labels map[string]string,
) error {
	updateFn := func(node *v1.Node) error {
		if len(taints) > 0 {
			if shouldReturn := c.removeTaints(node, taints, nodename); shouldReturn {
				return nil
			}
		}

		c.handleUncordon(node, labels, nodename)

		if len(annotationKeys) > 0 {
			for _, annotationKey := range annotationKeys {
				slog.Info("Removing annotation key from node", "key", annotationKey, "node", nodename)
				delete(node.Annotations, annotationKey)
			}
		}

		if len(labelsToRemove) > 0 {
			for _, labelKey := range labelsToRemove {
				slog.Info("Removing label key from node", "key", labelKey, "node", nodename)
				delete(node.Labels, labelKey)
			}
		}

		return nil
	}

	return c.UpdateNode(ctx, nodename, updateFn)
}

func (c *FaultQuarantineClient) removeTaints(node *v1.Node, taints []config.Taint, nodename string) bool {
	if c.DryRunMode {
		slog.Info("DryRun mode enabled, skipping taint removal", "node", nodename)
		return false
	}

	taintsAlreadyPresentOnNodeMap := map[config.Taint]bool{}
	for _, taint := range node.Spec.Taints {
		taintsAlreadyPresentOnNodeMap[config.Taint{Key: taint.Key, Value: taint.Value, Effect: string(taint.Effect)}] = true
	}

	taintsToActuallyRemove := []config.Taint{}

	for _, taintConfig := range taints {
		key := config.Taint{
			Key:    taintConfig.Key,
			Value:  taintConfig.Value,
			Effect: taintConfig.Effect,
		}

		found := taintsAlreadyPresentOnNodeMap[key]
		if !found {
			slog.Info("Node already does not have the taint", "node", nodename, "taint", taintConfig)
		} else {
			taintsToActuallyRemove = append(taintsToActuallyRemove, taintConfig)
		}
	}

	if len(taintsToActuallyRemove) == 0 {
		return true
	}

	slog.Info("Untainting node", "node", nodename, "taints", taintsToActuallyRemove)

	c.removeNodeTaints(node, taintsToActuallyRemove)

	return false
}

func (c *FaultQuarantineClient) handleUncordon(
	node *v1.Node, labels map[string]string, nodename string,
) {
	slog.Info("Uncordoning node", "node", nodename)

	if !c.DryRunMode {
		node.Spec.Unschedulable = false
	}

	if len(labels) > 0 {
		c.applyLabels(node, labels, nodename)

		uncordonReason := node.Labels[c.cordonedReasonLabelKey]

		if uncordonReason != "" {
			if len(uncordonReason) > 55 {
				uncordonReason = uncordonReason[:55]
			}

			node.Labels[c.uncordonedReasonLabelKey] = uncordonReason + "-removed"
		}
	}
}

// HandleManualUncordonCleanup atomically removes FQ annotations/taints/labels and adds manual uncordon annotation
// This is used when a node is manually uncordoned while having FQ quarantine state
func (c *FaultQuarantineClient) HandleManualUncordonCleanup(
	ctx context.Context,
	nodename string,
	taintsToRemove []config.Taint,
	annotationsToRemove []string,
	annotationsToAdd map[string]string,
	labelsToRemove []string,
) error {
	updateFn := func(node *v1.Node) error {
		if len(taintsToRemove) > 0 {
			c.removeNodeTaints(node, taintsToRemove)
		}

		if len(annotationsToRemove) > 0 || len(annotationsToAdd) > 0 {
			c.updateNodeAnnotationsForManualUncordon(node, annotationsToRemove, annotationsToAdd)
		}

		if len(labelsToRemove) > 0 {
			for _, key := range labelsToRemove {
				delete(node.Labels, key)
			}
		}

		return nil
	}

	return c.UpdateNode(ctx, nodename, updateFn)
}

func (c *FaultQuarantineClient) removeNodeTaints(node *v1.Node, taintsToRemove []config.Taint) {
	if c.DryRunMode {
		slog.Info("DryRun mode enabled, skipping node taint removal")
		return
	}

	taintsToRemoveMap := make(map[config.Taint]bool, len(taintsToRemove))
	for _, taint := range taintsToRemove {
		taintsToRemoveMap[taint] = true
	}

	newTaints := make([]v1.Taint, 0, len(node.Spec.Taints))

	for _, taint := range node.Spec.Taints {
		if !taintsToRemoveMap[config.Taint{Key: taint.Key, Value: taint.Value, Effect: string(taint.Effect)}] {
			newTaints = append(newTaints, taint)
		}
	}

	node.Spec.Taints = newTaints
}

func (c *FaultQuarantineClient) updateNodeAnnotationsForManualUncordon(
	node *v1.Node,
	annotationsToRemove []string,
	annotationsToAdd map[string]string,
) {
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	for _, key := range annotationsToRemove {
		delete(node.Annotations, key)
	}

	for key, value := range annotationsToAdd {
		node.Annotations[key] = value
	}
}
