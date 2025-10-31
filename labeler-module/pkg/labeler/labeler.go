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

package labeler

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/stringutil"
	"github.com/nvidia/nvsentinel/labeler-module/pkg/metrics"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
)

const (
	DCGMVersionLabel        = "nvsentinel.dgxc.nvidia.com/dcgm.version"
	DriverInstalledLabel    = "nvsentinel.dgxc.nvidia.com/driver.installed"
	KataEnabledLabel        = "nvsentinel.dgxc.nvidia.com/kata.enabled"
	KataRuntimeDefaultLabel = "katacontainers.io/kata-runtime"

	NodeDCGMIndex   = "nodeDCGM"
	NodeDriverIndex = "nodeDriver"

	// Label values
	LabelValueTrue  = "true"
	LabelValueFalse = "false"
)

var (
	dcgm4Regex = regexp.MustCompile(`.*dcgm:4\..*`)
	dcgm3Regex = regexp.MustCompile(`.*dcgm:3\..*`)
)

// Labeler manages node labeling based on pod information
type Labeler struct {
	clientset       kubernetes.Interface
	podInformer     cache.SharedIndexInformer
	nodeInformer    cache.SharedIndexInformer
	informersSynced []cache.InformerSynced
	ctx             context.Context
	dcgmAppLabel    string
	driverAppLabel  string
	kataLabels      []string // Instance-specific kata labels
}

// NewLabeler creates a new Labeler instance
// nolint: cyclop // todo
func NewLabeler(clientset kubernetes.Interface, resyncPeriod time.Duration,
	dcgmApp, driverApp, kataLabelOverride string) (*Labeler, error) {
	labelSelector, err := labels.Parse(fmt.Sprintf("app in (%s,%s)", dcgmApp, driverApp))
	if err != nil {
		return nil, fmt.Errorf("failed to parse label selector: %w", err)
	}

	// Build kata labels list with default plus optional override
	kataLabels := []string{KataRuntimeDefaultLabel}
	if kataLabelOverride != "" {
		kataLabels = append(kataLabels, kataLabelOverride)
	}

	// Create pod informer factory with label selector
	podInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		resyncPeriod,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = labelSelector.String()
		}),
	)

	// Create node informer factory (no filtering needed)
	nodeInformerFactory := informers.NewSharedInformerFactory(clientset, resyncPeriod)

	podInformer := podInformerFactory.Core().V1().Pods().Informer()
	nodeInformer := nodeInformerFactory.Core().V1().Nodes().Informer()

	err = podInformer.GetIndexer().AddIndexers(
		cache.Indexers{
			NodeDCGMIndex: func(obj any) ([]string, error) {
				pod, ok := obj.(*v1.Pod)
				if !ok {
					return nil, fmt.Errorf("object is not a pod")
				}

				if app, exists := pod.Labels["app"]; exists && app == dcgmApp {
					return []string{pod.Spec.NodeName}, nil
				}
				return []string{}, nil
			},
			NodeDriverIndex: func(obj any) ([]string, error) {
				pod, ok := obj.(*v1.Pod)
				if !ok {
					return nil, fmt.Errorf("object is not a pod")
				}

				if app, exists := pod.Labels["app"]; exists && app == driverApp {
					return []string{pod.Spec.NodeName}, nil
				}
				return []string{}, nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to add indexer: %w", err)
	}

	l := &Labeler{
		clientset:       clientset,
		podInformer:     podInformer,
		nodeInformer:    nodeInformer,
		informersSynced: []cache.InformerSynced{podInformer.HasSynced, nodeInformer.HasSynced},
		ctx:             context.Background(),
		dcgmAppLabel:    dcgmApp,
		driverAppLabel:  driverApp,
		kataLabels:      kataLabels,
	}

	// Register event handlers
	if err := l.registerPodEventHandlers(); err != nil {
		return nil, err
	}

	if err := l.registerNodeEventHandlers(); err != nil {
		return nil, err
	}

	slog.Info("Labeler created, watching DCGM and driver pods, and nodes for kata detection")

	return l, nil
}

// registerPodEventHandlers sets up event handlers for pod informer
func (l *Labeler) registerPodEventHandlers() error {
	_, err := l.podInformer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj any) bool {
			pod, ok := obj.(*v1.Pod)
			if !ok {
				return false
			}

			return pod.Spec.NodeName != ""
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) {
				if err := l.handlePodEvent(obj); err != nil {
					metrics.EventsProcessed.WithLabelValues(metrics.StatusFailed).Inc()
					slog.Error("Failed to handle pod add event", "error", err)
				} else {
					metrics.EventsProcessed.WithLabelValues(metrics.StatusSuccess).Inc()
				}
			},
			UpdateFunc: func(oldObj, newObj any) {
				oldPod, oldOk := oldObj.(*v1.Pod)
				newPod, newOk := newObj.(*v1.Pod)
				if !oldOk || !newOk {
					slog.Error("Failed to cast objects to pods in UpdateFunc")
					return
				}

				oldReady := podutil.IsPodReady(oldPod)
				newReady := podutil.IsPodReady(newPod)
				if oldReady == newReady {
					slog.Debug("Pod readiness unchanged", "pod", newPod.Name, "ready", newReady)
					return
				}

				if err := l.handlePodEvent(newPod); err != nil {
					metrics.EventsProcessed.WithLabelValues(metrics.StatusFailed).Inc()
					slog.Error("Failed to handle pod update event", "error", err)
				} else {
					metrics.EventsProcessed.WithLabelValues(metrics.StatusSuccess).Inc()
				}
			},
			DeleteFunc: func(obj any) {
				if err := l.handlePodDeleteEvent(obj); err != nil {
					metrics.EventsProcessed.WithLabelValues(metrics.StatusFailed).Inc()
					slog.Error("Failed to handle pod delete event", "error", err)
				} else {
					metrics.EventsProcessed.WithLabelValues(metrics.StatusSuccess).Inc()
				}
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add pod event handler: %w", err)
	}

	return nil
}

// registerNodeEventHandlers sets up event handlers for node informer
func (l *Labeler) registerNodeEventHandlers() error {
	_, err := l.nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if err := l.handleNodeEvent(obj); err != nil {
				slog.Error("Failed to handle node add event", "error", err)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			if err := l.handleNodeEvent(newObj); err != nil {
				slog.Error("Failed to handle node update event", "error", err)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add node event handler: %w", err)
	}

	return nil
}

// Run starts the labeler and waits for cache sync
func (l *Labeler) Run(ctx context.Context) error {
	l.ctx = ctx

	go l.podInformer.Run(ctx.Done())
	go l.nodeInformer.Run(ctx.Done())

	slog.Info("Waiting for Labeler caches to sync...")

	if ok := cache.WaitForCacheSync(ctx.Done(), l.informersSynced...); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	slog.Info("Labeler caches synced")

	<-ctx.Done()
	slog.Info("Labeler stopped")

	return nil
}

// getDCGMVersionForNode returns the expected DCGM version for a specific node
func (l *Labeler) getDCGMVersionForNode(nodeName string) (string, error) {
	objs, err := l.podInformer.GetIndexer().ByIndex(NodeDCGMIndex, nodeName)
	if err != nil {
		return "", fmt.Errorf("failed to get DCGM pods by node index for node %s: %w", nodeName, err)
	}

	for _, obj := range objs {
		pod, ok := obj.(*v1.Pod)
		if !ok {
			continue
		}

		for _, container := range pod.Spec.Containers {
			if dcgm4Regex.MatchString(container.Image) {
				return "4.x", nil
			} else if dcgm3Regex.MatchString(container.Image) {
				return "3.x", nil
			}
		}
	}

	return "", nil
}

// getDriverLabelForNode returns the expected driver label value for a specific node
func (l *Labeler) getDriverLabelForNode(nodeName string) (string, error) {
	objs, err := l.podInformer.GetIndexer().ByIndex(NodeDriverIndex, nodeName)
	if err != nil {
		return "", fmt.Errorf("failed to get driver pods by node index for node %s: %w", nodeName, err)
	}

	for _, obj := range objs {
		pod, ok := obj.(*v1.Pod)
		if !ok {
			continue
		}

		if podutil.IsPodReady(pod) {
			return LabelValueTrue, nil
		}
	}

	return "", nil
}

// getKataLabelForNode detects if Kata is enabled on the specified node by checking node metadata.
// Returns "true" if Kata is enabled, "false" if not.
func (l *Labeler) getKataLabelForNode(node *v1.Node) string {
	// Check if Kata is enabled using multiple detection methods
	if isKataEnabled(node, l.kataLabels) {
		return LabelValueTrue
	}

	return LabelValueFalse
}

// isKataEnabled checks if a node has Kata Containers enabled by examining node labels.
// Checks the configured kata labels (either custom override or default) for truthy values.
// Returns true if ANY of the configured labels has a truthy value (OR logic).
// Truthy values are: "true", "enabled", "1", "yes" (case-insensitive).
func isKataEnabled(node *v1.Node, kataLabels []string) bool {
	for _, label := range kataLabels {
		if value, exists := node.Labels[label]; exists && stringutil.IsTruthyValue(value) {
			slog.Debug("Kata detected",
				"source", "label",
				"node", node.Name,
				"label", label,
				"value", value,
			)

			return true
		}
	}

	return false
}

// getDCGMVersionForNodeExcluding returns the expected DCGM version for a specific node,
// excluding a specific pod from consideration (used for delete events)
func (l *Labeler) getDCGMVersionForNodeExcluding(nodeName string, excludePod *v1.Pod) (string, error) {
	objs, err := l.podInformer.GetIndexer().ByIndex(NodeDCGMIndex, nodeName)
	if err != nil {
		return "", fmt.Errorf("failed to get DCGM pods by node index for node %s: %w", nodeName, err)
	}

	for _, obj := range objs {
		pod, ok := obj.(*v1.Pod)
		if !ok {
			continue
		}

		// Skip the pod we're excluding (the one being deleted)
		if pod.UID == excludePod.UID {
			continue
		}

		for _, container := range pod.Spec.Containers {
			if dcgm4Regex.MatchString(container.Image) {
				return "4.x", nil
			} else if dcgm3Regex.MatchString(container.Image) {
				return "3.x", nil
			}
		}
	}

	return "", nil
}

// getDriverLabelForNodeExcluding returns the expected driver label value for a specific node,
// excluding a specific pod from consideration (used for delete events)
func (l *Labeler) getDriverLabelForNodeExcluding(nodeName string, excludePod *v1.Pod) (string, error) {
	objs, err := l.podInformer.GetIndexer().ByIndex(NodeDriverIndex, nodeName)
	if err != nil {
		return "", fmt.Errorf("failed to get driver pods by node index for node %s: %w", nodeName, err)
	}

	for _, obj := range objs {
		pod, ok := obj.(*v1.Pod)
		if !ok {
			continue
		}

		// Skip the pod we're excluding (the one being deleted)
		if pod.UID == excludePod.UID {
			continue
		}

		if podutil.IsPodReady(pod) {
			return LabelValueTrue, nil
		}
	}

	return "", nil
}

// updateNodeLabelsForPod updates only DCGM and driver labels (kata is handled separately by node events)
func (l *Labeler) updateNodeLabelsForPod(nodeName, expectedDCGMVersion, expectedDriverLabel string) error {
	updateStartTime := time.Now()
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		node, err := l.clientset.CoreV1().Nodes().Get(l.ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}

		needsUpdate := false

		if node.Labels[DCGMVersionLabel] != expectedDCGMVersion {
			needsUpdate = true

			if expectedDCGMVersion == "" {
				delete(node.Labels, DCGMVersionLabel)
				slog.Info("Removing DCGM version label from node", "node", nodeName)
			} else {
				node.Labels[DCGMVersionLabel] = expectedDCGMVersion
				slog.Info("Setting DCGM version label on node", "node", nodeName, "version", expectedDCGMVersion)
			}
		}

		if node.Labels[DriverInstalledLabel] != expectedDriverLabel {
			needsUpdate = true

			if expectedDriverLabel == "" {
				delete(node.Labels, DriverInstalledLabel)
				slog.Info("Removing driver installed label from node", "node", nodeName)
			} else {
				node.Labels[DriverInstalledLabel] = expectedDriverLabel
				slog.Info("Setting driver installed label on node", "node", nodeName, "label", expectedDriverLabel)
			}
		}

		if !needsUpdate {
			slog.Debug("Node already has correct pod-related labels", "node", nodeName)
			return nil
		}

		_, err = l.clientset.CoreV1().Nodes().Update(l.ctx, node, metav1.UpdateOptions{})

		return err
	})

	if err != nil {
		metrics.NodeUpdateFailures.Inc()
		return fmt.Errorf("failed to reconcile node labeling for %s: %w", nodeName, err)
	}

	metrics.NodeUpdateDuration.Observe(time.Since(updateStartTime).Seconds())

	return nil
}

// handleNodeEvent processes node events to update kata detection label
func (l *Labeler) handleNodeEvent(obj any) error {
	node, ok := obj.(*v1.Node)
	if !ok {
		return fmt.Errorf("node event: expected Node object, got %T", obj)
	}

	expectedKataLabel := l.getKataLabelForNode(node)

	currentKataLabel := node.Labels[KataEnabledLabel]
	if currentKataLabel == expectedKataLabel {
		slog.Debug("Node already has correct kata label", "node", node.Name, "kata", expectedKataLabel)
		return nil
	}

	// Only update kata label, leave DCGM/driver labels alone
	return l.updateKataLabel(node.Name, expectedKataLabel)
}

// updateKataLabel updates only the kata label on a node
func (l *Labeler) updateKataLabel(nodeName, expectedKataLabel string) error {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		node, err := l.clientset.CoreV1().Nodes().Get(l.ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}

		currentKataLabel := node.Labels[KataEnabledLabel]
		if currentKataLabel == expectedKataLabel {
			slog.Debug("Node already has correct kata label", "node", nodeName, "kata", expectedKataLabel)
			return nil
		}

		node.Labels[KataEnabledLabel] = expectedKataLabel
		slog.Info("Setting Kata enabled label on node", "node", nodeName, "kata", expectedKataLabel)

		_, err = l.clientset.CoreV1().Nodes().Update(l.ctx, node, metav1.UpdateOptions{})

		return err
	})

	if err != nil {
		return fmt.Errorf("failed to update kata label for %s: %w", nodeName, err)
	}

	return nil
}

// handlePodDeleteEvent processes pod delete events by recalculating node labels
// after excluding the deleted pod from consideration
func (l *Labeler) handlePodDeleteEvent(obj any) error {
	startTime := time.Now()
	defer func() {
		metrics.EventHandlingDuration.Observe(time.Since(startTime).Seconds())
	}()

	pod, ok := obj.(*v1.Pod)
	if !ok {
		return fmt.Errorf("pod delete event: expected Pod object, got %T", obj)
	}

	// For delete events, we need to calculate what the labels should be
	// after this pod is removed, so we exclude it from our calculations
	expectedDCGMVersion, err := l.getDCGMVersionForNodeExcluding(pod.Spec.NodeName, pod)
	if err != nil {
		return fmt.Errorf("failed to get DCGM version for node %s excluding deleted pod: %w", pod.Spec.NodeName, err)
	}

	expectedDriverLabel, err := l.getDriverLabelForNodeExcluding(pod.Spec.NodeName, pod)
	if err != nil {
		return fmt.Errorf("failed to get driver label for node %s excluding deleted pod: %w", pod.Spec.NodeName, err)
	}

	return l.updateNodeLabelsForPod(pod.Spec.NodeName, expectedDCGMVersion, expectedDriverLabel)
}

// handlePodEvent processes all pod events (add, update) idempotently
func (l *Labeler) handlePodEvent(obj any) error {
	startTime := time.Now()
	defer func() {
		metrics.EventHandlingDuration.Observe(time.Since(startTime).Seconds())
	}()

	pod, ok := obj.(*v1.Pod)
	if !ok {
		return fmt.Errorf("pod event: expected Pod object, got %T", obj)
	}

	expectedDCGMVersion, err := l.getDCGMVersionForNode(pod.Spec.NodeName)
	if err != nil {
		return fmt.Errorf("failed to get DCGM version for node %s: %w", pod.Spec.NodeName, err)
	}

	expectedDriverLabel, err := l.getDriverLabelForNode(pod.Spec.NodeName)
	if err != nil {
		return fmt.Errorf("failed to get driver label for node %s: %w", pod.Spec.NodeName, err)
	}

	return l.updateNodeLabelsForPod(pod.Spec.NodeName, expectedDCGMVersion, expectedDriverLabel)
}
