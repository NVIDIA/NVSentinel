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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"

	annotationutil "github.com/nvidia/nvsentinel/commons/pkg/annotation"
	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/breaker"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
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
	resyncPeriod time.Duration, gpuNodeLabelKey, gpuNodeLabelValue string) (*FaultQuarantineClient, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("error creating Kubernetes config: %w", err)
	}

	config.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return auditlogger.NewAuditingRoundTripper(rt)
	})

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating clientset: %w", err)
	}

	nodeInformer, err := NewNodeInformer(clientset, resyncPeriod, gpuNodeLabelKey, gpuNodeLabelValue)
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
	slog.InfoContext(ctx, "Ensuring circuit breaker config map",
		"name", name, "namespace", namespace, "initialStatus", initialStatus)

	cmClient := c.Clientset.CoreV1().ConfigMaps(namespace)

	_, err := cmClient.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		slog.InfoContext(ctx, "Circuit breaker config map already exists", "name", name, "namespace", namespace)
		return nil
	}

	if !errors.IsNotFound(err) {
		slog.ErrorContext(ctx, "Error getting circuit breaker config map", "name", name, "namespace", namespace, "error", err)
		return fmt.Errorf("failed to get config map %s in namespace %s: %w", name, namespace, err)
	}

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data: map[string]string{
			"status": string(initialStatus),
			"cursor": string(breaker.CursorModeResume),
		},
	}

	_, err = cmClient.Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		slog.ErrorContext(ctx, "Error creating circuit breaker config map",
			"name", name, "namespace", namespace, "error", err)

		return fmt.Errorf("failed to create config map %s in namespace %s: %w", name, namespace, err)
	}

	return nil
}

func (c *FaultQuarantineClient) GetTotalNodes(ctx context.Context) (int, error) {
	totalNodes, _, err := c.NodeInformer.GetNodeCounts()
	if err != nil {
		return 0, fmt.Errorf("failed to get node counts from informer: %w", err)
	}

	slog.DebugContext(ctx, "Got total nodes from NodeInformer cache", "totalNodes", totalNodes)

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

	// Increased retry attempts to handle node update conflicts when multiple modules
	// attempt concurrent updates, preventing nodes from remaining cordoned with stale annotations.
	backoff := wait.Backoff{
		Steps:    10,                    // Increased from default 5
		Duration: 20 * time.Millisecond, // Increased from default 10ms
		Factor:   2.0,
		Jitter:   0.1,
	}

	return retry.OnError(backoff, isRetryableError, func() error {
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

func isRetryableError(err error) bool {
	if errors.IsConflict(err) {
		return true
	}

	if errors.IsServerTimeout(err) || errors.IsTooManyRequests(err) {
		return true
	}

	if errors.IsTimeout(err) || errors.IsServiceUnavailable(err) {
		return true
	}

	return false
}

func (c *FaultQuarantineClient) ReadCircuitBreakerState(
	ctx context.Context, name, namespace string,
) (breaker.State, error) {
	slog.InfoContext(ctx, "Reading circuit breaker state from config map",
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

func (c *FaultQuarantineClient) ReadCursorMode(
	ctx context.Context, name, namespace string,
) (breaker.CursorMode, error) {
	cm, err := c.Clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return breaker.CursorModeResume, fmt.Errorf("failed to get config map %s in namespace %s: %w", name, namespace, err)
	}

	if cm.Data == nil {
		return breaker.CursorModeResume, nil
	}

	cursor := cm.Data["cursor"]
	if cursor == "" {
		return breaker.CursorModeResume, nil
	}

	return breaker.CursorMode(cursor), nil
}

func (c *FaultQuarantineClient) WriteCursorMode(
	ctx context.Context, name, namespace string, mode breaker.CursorMode,
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

		cm.Data["cursor"] = string(mode)

		_, err = cmClient.Update(ctx, cm, metav1.UpdateOptions{})
		if err != nil {
			slog.Error("Error updating circuit breaker config map cursor", "name", name, "namespace", namespace, "error", err)
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
) (bool, error) {
	alreadyQuarantined := false

	updateFn := func(node *v1.Node) error {
		alreadyQuarantined = hasNonEmptyQuarantineHealthEvent(node)

		if len(taints) > 0 {
			if err := c.applyTaints(ctx, node, taints, nodename); err != nil {
				return fmt.Errorf("failed to apply taints to node %s: %w", nodename, err)
			}
		}

		if isCordon {
			c.handleCordon(ctx, node, nodename)
		}

		if len(annotations) > 0 {
			if err := c.applyAnnotations(ctx, node, annotations, nodename); err != nil {
				return err
			}
		}

		if len(labels) > 0 {
			labelsToApply, err := labelsWithSessionWinners(node, labels)
			if err != nil {
				return fmt.Errorf("failed to get merged applied labels on node %s: %w", nodename, err)
			}

			c.applyLabels(ctx, node, labelsToApply, nodename)
		}

		return nil
	}

	err := c.UpdateNode(ctx, nodename, updateFn)

	return alreadyQuarantined, err
}

func labelsWithSessionWinners(node *v1.Node, labels map[string]string) (map[string]string, error) {
	labelsToApply := make(map[string]string, len(labels))
	for key, value := range labels {
		labelsToApply[key] = value
	}

	appliedLabelsJSON := node.Annotations[common.QuarantineHealthEventAppliedLabelsAnnotationKey]
	if annotationutil.IsEmptyValue(appliedLabelsJSON) {
		return labelsToApply, nil
	}

	appliedLabels, err := parseAppliedLabelsAnnotation(appliedLabelsJSON)
	if err != nil {
		return nil, err
	}

	for _, label := range appliedLabels {
		labelsToApply[label.Key] = label.Value
	}

	return labelsToApply, nil
}

func (c *FaultQuarantineClient) applyTaints(
	ctx context.Context, node *v1.Node, taints []config.Taint, nodename string,
) error {
	if c.DryRunMode {
		slog.InfoContext(ctx, "DryRun mode enabled, skipping taint application", "node", nodename)
		return nil
	}

	existingTaints := make(map[config.Taint]v1.Taint)
	for _, taint := range node.Spec.Taints {
		existingTaints[config.Taint{Key: taint.Key, Value: taint.Value, Effect: string(taint.Effect)}] = taint
	}

	for _, taintConfig := range taints {
		key := config.Taint{Key: taintConfig.Key, Value: taintConfig.Value, Effect: string(taintConfig.Effect)}

		if _, exists := existingTaints[key]; !exists {
			slog.InfoContext(ctx, "Tainting node", "node", nodename, "taintConfig", taintConfig)
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

func (c *FaultQuarantineClient) handleCordon(ctx context.Context, node *v1.Node, nodename string) {
	_, exist := node.Annotations[common.QuarantineHealthEventAnnotationKey]

	if node.Spec.Unschedulable {
		if exist {
			slog.InfoContext(ctx, "Node already cordoned by FQM; preserving cordon while applying updates", "node", nodename)
			return
		}

		slog.InfoContext(ctx, "Node is cordoned manually; applying FQM taints/annotations", "node", nodename)
	} else {
		slog.InfoContext(ctx, "Cordoning node", "node", nodename)

		if !c.DryRunMode {
			node.Spec.Unschedulable = true
		}
	}
}

func (c *FaultQuarantineClient) applyAnnotations(
	ctx context.Context, node *v1.Node, annotations map[string]string, nodename string,
) error {
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	slog.InfoContext(ctx, "Setting annotations on node", "node", nodename, "annotations", annotations)

	for annotationKey, annotationValue := range annotations {
		if annotationKey == common.QuarantineHealthEventAnnotationKey {
			mergedValue, err := mergeQuarantineHealthEventAnnotation(node.Annotations[annotationKey], annotationValue)
			if err != nil {
				return fmt.Errorf("failed to merge annotation %q on node %s: %w", annotationKey, nodename, err)
			}

			annotationValue = mergedValue
		}

		if annotationKey == common.QuarantineHealthEventAppliedTaintsAnnotationKey {
			mergedValue, err := mergeAppliedTaintsAnnotation(node.Annotations[annotationKey], annotationValue)
			if err != nil {
				return fmt.Errorf("failed to merge annotation %q on node %s: %w", annotationKey, nodename, err)
			}

			annotationValue = mergedValue
		}

		if annotationKey == common.QuarantineHealthEventAppliedLabelsAnnotationKey {
			mergedValue, err := mergeAppliedLabelsAnnotation(node.Annotations[annotationKey], annotationValue)
			if err != nil {
				return fmt.Errorf("failed to merge annotation %q on node %s: %w", annotationKey, nodename, err)
			}

			annotationValue = mergedValue
		}

		node.Annotations[annotationKey] = annotationValue
	}

	return nil
}

func hasNonEmptyQuarantineHealthEvent(node *v1.Node) bool {
	if node.Annotations == nil {
		return false
	}

	return !annotationutil.IsEmptyValue(node.Annotations[common.QuarantineHealthEventAnnotationKey])
}

func mergeQuarantineHealthEventAnnotation(existingValue, incomingValue string) (string, error) {
	return mergeAnnotation(
		existingValue,
		incomingValue,
		"quarantine health event",
		parseHealthEventsAnnotation,
		mergeHealthEventsAnnotations,
	)
}

func mergeAppliedTaintsAnnotation(existingValue, incomingValue string) (string, error) {
	return mergeAnnotation(
		existingValue,
		incomingValue,
		"applied taints",
		parseAppliedTaintsAnnotation,
		mergeAppliedTaints,
	)
}

func mergeAppliedLabelsAnnotation(existingValue, incomingValue string) (string, error) {
	return mergeAnnotation(
		existingValue,
		incomingValue,
		"applied labels",
		parseAppliedLabelsAnnotation,
		mergeAppliedLabels,
	)
}

func mergeAnnotation[T any](
	existingValue string,
	incomingValue string,
	annotationName string,
	parse func(string) (T, error),
	merge func(T, T) T,
) (string, error) {
	if annotationutil.IsEmptyValue(existingValue) {
		return incomingValue, nil
	}

	if annotationutil.IsEmptyValue(incomingValue) {
		return existingValue, nil
	}

	existing, err := parse(existingValue)
	if err != nil {
		return "", fmt.Errorf("failed to parse existing %s annotation: %w", annotationName, err)
	}

	incoming, err := parse(incomingValue)
	if err != nil {
		return "", fmt.Errorf("failed to parse incoming %s annotation: %w", annotationName, err)
	}

	annotationBytes, err := json.Marshal(merge(existing, incoming))
	if err != nil {
		return "", fmt.Errorf("failed to marshal merged %s annotation: %w", annotationName, err)
	}

	return string(annotationBytes), nil
}

func mergeHealthEventsAnnotations(
	existing *healthEventsAnnotation.HealthEventsAnnotationMap,
	incoming *healthEventsAnnotation.HealthEventsAnnotationMap,
) *healthEventsAnnotation.HealthEventsAnnotationMap {
	for _, event := range incoming.Events {
		existing.AddOrUpdateEvent(event)
	}

	return existing
}

func parseAppliedTaintsAnnotation(value string) ([]config.Taint, error) {
	var taints []config.Taint
	if err := json.Unmarshal([]byte(value), &taints); err != nil {
		return nil, err
	}

	return taints, nil
}

func mergeAppliedTaints(existingTaints, incomingTaints []config.Taint) []config.Taint {
	mergedByKey := make(map[config.Taint]config.Taint, len(existingTaints)+len(incomingTaints))
	for _, taints := range [][]config.Taint{existingTaints, incomingTaints} {
		for _, taint := range taints {
			key := config.Taint{Key: taint.Key, Value: taint.Value, Effect: taint.Effect}
			if existing, ok := mergedByKey[key]; ok {
				taint.PreExisting = existing.PreExisting || taint.PreExisting
			}

			mergedByKey[key] = taint
		}
	}

	mergedTaints := make([]config.Taint, 0, len(mergedByKey))
	for _, taint := range mergedByKey {
		mergedTaints = append(mergedTaints, taint)
	}

	return mergedTaints
}

func parseAppliedLabelsAnnotation(value string) ([]config.AppliedLabel, error) {
	var labels []config.AppliedLabel
	if err := json.Unmarshal([]byte(value), &labels); err != nil {
		return nil, err
	}

	return labels, nil
}

func mergeAppliedLabels(existingLabels, incomingLabels []config.AppliedLabel) []config.AppliedLabel {
	mergedByKey := make(map[string]config.AppliedLabel, len(existingLabels)+len(incomingLabels))
	for _, label := range existingLabels {
		mergedByKey[label.Key] = label
	}

	for _, incoming := range incomingLabels {
		existing, ok := mergedByKey[incoming.Key]
		if ok && (existing.Priority > incoming.Priority ||
			(existing.Priority == incoming.Priority && existing.Order > incoming.Order)) {
			continue
		}

		mergedByKey[incoming.Key] = incoming
	}

	keys := make([]string, 0, len(mergedByKey))
	for key := range mergedByKey {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	mergedLabels := make([]config.AppliedLabel, 0, len(keys))
	for _, key := range keys {
		mergedLabels = append(mergedLabels, mergedByKey[key])
	}

	return mergedLabels
}

func parseHealthEventsAnnotation(value string) (*healthEventsAnnotation.HealthEventsAnnotationMap, error) {
	healthEventsMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	if err := json.Unmarshal([]byte(value), healthEventsMap); err == nil {
		return healthEventsMap, nil
	}

	var singleEvent protos.HealthEvent
	if err := json.Unmarshal([]byte(value), &singleEvent); err != nil {
		return nil, err
	}

	healthEventsMap.AddOrUpdateEvent(&singleEvent)

	return healthEventsMap, nil
}

func (c *FaultQuarantineClient) applyLabels(
	ctx context.Context, node *v1.Node, labels map[string]string, nodename string,
) {
	if c.DryRunMode {
		slog.InfoContext(ctx, "DryRun mode enabled, skipping label application", "node", nodename, "labels", labels)
		return
	}

	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}

	slog.InfoContext(ctx, "Adding labels on node", "node", nodename)

	for k, v := range labels {
		node.Labels[k] = v
	}
}

func (c *FaultQuarantineClient) UnQuarantineNodeAndRemoveAnnotations(
	ctx context.Context,
	nodename string,
	taints []config.Taint,
	shouldUncordon bool,
	annotationKeys []string,
	labelsToRemove []string,
	labels map[string]string,
) error {
	updateFn := func(node *v1.Node) error {
		if len(taints) > 0 {
			if shouldReturn := c.removeTaints(ctx, node, taints, nodename); shouldReturn {
				return nil
			}
		}

		if shouldUncordon {
			c.handleUncordon(ctx, node, labels, nodename)
		}

		if len(annotationKeys) > 0 {
			for _, annotationKey := range annotationKeys {
				slog.InfoContext(ctx, "Removing annotation key from node", "key", annotationKey, "node", nodename)
				delete(node.Annotations, annotationKey)
			}
		}

		c.removeLabels(ctx, node, labelsToRemove, nodename)

		return nil
	}

	return c.UpdateNode(ctx, nodename, updateFn)
}

func (c *FaultQuarantineClient) removeLabels(
	ctx context.Context, node *v1.Node, labelKeys []string, nodename string,
) {
	if len(labelKeys) == 0 {
		return
	}

	if c.DryRunMode {
		slog.InfoContext(ctx, "DryRun mode enabled, skipping label removal", "node", nodename, "labels", labelKeys)
		return
	}

	for _, labelKey := range labelKeys {
		slog.InfoContext(ctx, "Removing label key from node", "key", labelKey, "node", nodename)
		delete(node.Labels, labelKey)
	}
}

func (c *FaultQuarantineClient) removeTaints(
	ctx context.Context, node *v1.Node, taints []config.Taint, nodename string,
) bool {
	if c.DryRunMode {
		slog.InfoContext(ctx, "DryRun mode enabled, skipping taint removal", "node", nodename)
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
			slog.InfoContext(ctx, "Node already does not have the taint", "node", nodename, "taint", taintConfig)
		} else {
			taintsToActuallyRemove = append(taintsToActuallyRemove, taintConfig)
		}
	}

	if len(taintsToActuallyRemove) == 0 {
		return true
	}

	slog.InfoContext(ctx, "Untainting node", "node", nodename, "taints", taintsToActuallyRemove)

	c.removeNodeTaints(ctx, node, taintsToActuallyRemove)

	return false
}

func (c *FaultQuarantineClient) handleUncordon(
	ctx context.Context, node *v1.Node, labels map[string]string, nodename string,
) {
	slog.InfoContext(ctx, "Uncordoning node", "node", nodename)

	if !c.DryRunMode {
		node.Spec.Unschedulable = false
	}

	if len(labels) > 0 {
		c.applyLabels(ctx, node, labels, nodename)

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
	annotationsToRemove []string,
	annotationsToAdd map[string]string,
	labelsToRemove []string,
) error {
	updateFn := func(node *v1.Node) error {
		if len(annotationsToRemove) > 0 || len(annotationsToAdd) > 0 {
			c.updateNodeAnnotationsForManualUncordon(node, annotationsToRemove, annotationsToAdd)
		}

		c.removeLabels(ctx, node, labelsToRemove, nodename)

		return nil
	}

	return c.UpdateNode(ctx, nodename, updateFn)
}

// HandleManualUntaintCleanup atomically removes FQ annotations/taints/labels and adds manual untaint annotation
// This is used when a node is manually untainted while having FQ quarantine state
func (c *FaultQuarantineClient) HandleManualUntaintCleanup(
	ctx context.Context,
	nodename string,
	annotationsToRemove []string,
	annotationsToAdd map[string]string,
	labelsToRemove []string,
) error {
	updateFn := func(node *v1.Node) error {
		if len(annotationsToRemove) > 0 || len(annotationsToAdd) > 0 {
			c.updateNodeAnnotationsForManualUncordon(node, annotationsToRemove, annotationsToAdd)
		}

		c.removeLabels(ctx, node, labelsToRemove, nodename)

		return nil
	}

	return c.UpdateNode(ctx, nodename, updateFn)
}

func (c *FaultQuarantineClient) removeNodeTaints(ctx context.Context, node *v1.Node, taintsToRemove []config.Taint) {
	if c.DryRunMode {
		slog.InfoContext(ctx, "DryRun mode enabled, skipping node taint removal")
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
