// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
)

const (
	driverLabelKey   = "nvidia.com/driver.installed"
	driverLabelValue = "true"
	appLabelSelector = "app=nvidia-driver-daemonset"
	namespace        = "gpu-operator"
)

// EventType represents the type of Kubernetes resource event
type EventType string

const (
	EventAdded    EventType = "ADDED"
	EventModified EventType = "MODIFIED"
	EventDeleted  EventType = "DELETED"
)

type DriverWatcher struct {
	clientset  kubernetes.Interface
	nodePodMap map[string]bool // tracks which nodes have ready driver pods
}

func (w *DriverWatcher) Run(ctx context.Context) {
	klog.Info("Starting NVIDIA driver watcher")

	// Create informer for watching pods - this automatically handles initial sync
	factory := informers.NewSharedInformerFactoryWithOptions(w.clientset, time.Minute,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = appLabelSelector
		}))

	informer := factory.Core().V1().Pods().Informer()

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			w.handlePodEvent(ctx, pod, EventAdded)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			pod := newObj.(*corev1.Pod)
			w.handlePodEvent(ctx, pod, EventModified)
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			w.handlePodEvent(ctx, pod, EventDeleted)
		},
	})
	if err != nil {
		klog.Fatalf("Failed to add event handler: %v", err)
	}

	factory.Start(ctx.Done())

	klog.Info("Starting informer")
	<-ctx.Done()
	klog.Info("Shutting down driver watcher")
}

func (w *DriverWatcher) handlePodEvent(ctx context.Context, pod *corev1.Pod, eventType EventType) {
	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		klog.V(4).Infof("Pod %s has no node assigned, skipping (event: %s)", pod.Name, eventType)
		return
	}

	klog.V(4).Infof("Processing pod event: %s for pod %s on node %s", eventType, pod.Name, nodeName)

	switch eventType {
	case EventAdded, EventModified:
		isReady := podutil.IsPodReady(pod)
		currentState, exists := w.nodePodMap[nodeName]

		// Update node label only if state changed
		if !exists || currentState != isReady {
			w.nodePodMap[nodeName] = isReady
			if err := w.updateNodeLabel(ctx, nodeName, isReady); err != nil {
				klog.Errorf("Failed to update node label for node %s: %v", nodeName, err)
			} else {
				klog.Infof("Updated node %s driver status to ready=%t", nodeName, isReady)
			}
		} else {
			klog.V(4).Infof("Node %s driver status unchanged (ready=%t)", nodeName, isReady)
		}

	case EventDeleted:
		delete(w.nodePodMap, nodeName)
		if err := w.updateNodeLabel(ctx, nodeName, false); err != nil {
			klog.Errorf("Failed to remove node label from node %s: %v", nodeName, err)
		} else {
			klog.Infof("Removed driver label from node %s", nodeName)
		}
	}
}

func (w *DriverWatcher) updateNodeLabel(ctx context.Context, nodeName string, shouldLabel bool) error {
	// Early optimization: check current state to avoid unnecessary patches
	if w.shouldSkipUpdate(ctx, nodeName, shouldLabel) {
		return nil
	}

	patchData := w.buildPatchData(shouldLabel)

	return wait.ExponentialBackoffWithContext(ctx, retry.DefaultBackoff, func(ctx context.Context) (bool, error) {
		_, err := w.clientset.CoreV1().Nodes().Patch(
			ctx, nodeName, types.StrategicMergePatchType, patchData, metav1.PatchOptions{},
		)
		if err == nil {
			// Success
			action := "added"
			if !shouldLabel {
				action = "removed"
			}
			klog.V(4).Infof("Successfully %s driver label %s for node %s", action, driverLabelKey, nodeName)
			return true, nil // Done, don't retry
		}

		// Handle non-retryable errors
		if w.isNonRetryableError(err, shouldLabel) {
			return true, nil // Done, don't retry (but not an error)
		}

		// Retryable error
		klog.V(4).Infof("Retryable error updating node %s: %v", nodeName, err)
		return false, nil // Retry
	})
}

func (w *DriverWatcher) shouldSkipUpdate(ctx context.Context, nodeName string, shouldLabel bool) bool {
	node, err := w.clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			klog.V(4).Infof("Node %s not found, skipping label update", nodeName)
			return true
		}
		// If we can't get the node, proceed with patch
		return false
	}

	currentValue, hasLabel := node.Labels[driverLabelKey]

	if shouldLabel {
		// Want to add label - skip if already correct
		return hasLabel && currentValue == driverLabelValue
	} else {
		// Want to remove label - skip if already absent
		return !hasLabel
	}
}

func (w *DriverWatcher) buildPatchData(shouldLabel bool) []byte {
	if shouldLabel {
		return []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, driverLabelKey, driverLabelValue))
	}
	return []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:null}}}`, driverLabelKey))
}

func (w *DriverWatcher) isNonRetryableError(err error, shouldLabel bool) bool {
	if kerrors.IsNotFound(err) {
		klog.V(4).Infof("Node was deleted during update")
		return true
	}

	// Check for 422 Unprocessable Entity when trying to remove non-existent label
	if !shouldLabel {
		var statusErr *kerrors.StatusError
		if errors.As(err, &statusErr) && statusErr.Status().Code == 422 {
			klog.V(4).Infof("Label %s doesn't exist, nothing to remove", driverLabelKey)
			return true
		}
	}

	return false
}
