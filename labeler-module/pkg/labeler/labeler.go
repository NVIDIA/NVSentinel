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
	"regexp"
	"time"

	"github.com/nvidia/nvsentinel/labeler-module/pkg/metrics"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
)

const (
	DCGMVersionLabel     = "nvsentinel.dgxc.nvidia.com/dcgm.version"
	DriverInstalledLabel = "nvsentinel.dgxc.nvidia.com/driver.installed"

	NodeDCGMIndex   = "nodeDCGM"
	NodeDriverIndex = "nodeDriver"
)

var (
	dcgm4Regex = regexp.MustCompile(`.*dcgm:4\..*`)
	dcgm3Regex = regexp.MustCompile(`.*dcgm:3\..*`)
)

// Labeler manages node labeling based on pod information
type Labeler struct {
	clientset      kubernetes.Interface
	informer       cache.SharedIndexInformer
	informerSynced cache.InformerSynced
	ctx            context.Context
	dcgmAppLabel   string
	driverAppLabel string
}

// NewLabeler creates a new Labeler instance
// nolint: cyclop // todo
func NewLabeler(clientset kubernetes.Interface, resyncPeriod time.Duration,
	dcgmApp, driverApp string) (*Labeler, error) {
	labelSelector, err := labels.Parse(fmt.Sprintf("app in (%s,%s)", dcgmApp, driverApp))
	if err != nil {
		return nil, fmt.Errorf("failed to parse label selector: %w", err)
	}

	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		resyncPeriod,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = labelSelector.String()
		}),
	)

	err = informerFactory.Core().V1().Pods().Informer().GetIndexer().AddIndexers(
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
		clientset:      clientset,
		informer:       informerFactory.Core().V1().Pods().Informer(),
		informerSynced: informerFactory.Core().V1().Pods().Informer().HasSynced,
		ctx:            context.Background(),
		dcgmAppLabel:   dcgmApp,
		driverAppLabel: driverApp,
	}

	_, err = l.informer.AddEventHandler(cache.FilteringResourceEventHandler{
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
					klog.Errorf("Failed to handle pod add event: %v", err)
				} else {
					metrics.EventsProcessed.WithLabelValues(metrics.StatusSuccess).Inc()
				}
			},
			UpdateFunc: func(oldObj, newObj any) {
				oldPod, oldOk := oldObj.(*v1.Pod)
				newPod, newOk := newObj.(*v1.Pod)
				if !oldOk || !newOk {
					klog.Errorf("Failed to cast objects to pods in UpdateFunc")
					return
				}

				oldReady := podutil.IsPodReady(oldPod)
				newReady := podutil.IsPodReady(newPod)
				if oldReady == newReady {
					klog.V(4).Infof("Pod %s readiness unchanged (ready=%t)", newPod.Name, newReady)
					return
				}

				if err := l.handlePodEvent(newPod); err != nil {
					metrics.EventsProcessed.WithLabelValues(metrics.StatusFailed).Inc()
					klog.Errorf("Failed to handle pod update event: %v", err)
				} else {
					metrics.EventsProcessed.WithLabelValues(metrics.StatusSuccess).Inc()
				}
			},
			DeleteFunc: func(obj any) {
				if err := l.handlePodDeleteEvent(obj); err != nil {
					metrics.EventsProcessed.WithLabelValues(metrics.StatusFailed).Inc()
					klog.Errorf("Failed to handle pod delete event: %v", err)
				} else {
					metrics.EventsProcessed.WithLabelValues(metrics.StatusSuccess).Inc()
				}
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add event handler: %w", err)
	}

	klog.Info("Labeler created, watching DCGM and driver pods")

	return l, nil
}

// Run starts the labeler and waits for cache sync
func (l *Labeler) Run(ctx context.Context) error {
	l.ctx = ctx

	klog.Info("Starting Labeler")

	go l.informer.Run(ctx.Done())

	klog.Info("Waiting for Labeler cache to sync...")

	if ok := cache.WaitForCacheSync(ctx.Done(), l.informerSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("Labeler cache synced")

	<-ctx.Done()
	klog.Info("Labeler stopped")

	return nil
}

// getDCGMVersionForNode returns the expected DCGM version for a specific node
func (l *Labeler) getDCGMVersionForNode(nodeName string) (string, error) {
	objs, err := l.informer.GetIndexer().ByIndex(NodeDCGMIndex, nodeName)
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
	objs, err := l.informer.GetIndexer().ByIndex(NodeDriverIndex, nodeName)
	if err != nil {
		return "", fmt.Errorf("failed to get driver pods by node index for node %s: %w", nodeName, err)
	}

	for _, obj := range objs {
		pod, ok := obj.(*v1.Pod)
		if !ok {
			continue
		}

		if podutil.IsPodReady(pod) {
			return "true", nil
		}
	}

	return "", nil
}

// getDCGMVersionForNodeExcluding returns the expected DCGM version for a specific node,
// excluding a specific pod from consideration (used for delete events)
func (l *Labeler) getDCGMVersionForNodeExcluding(nodeName string, excludePod *v1.Pod) (string, error) {
	objs, err := l.informer.GetIndexer().ByIndex(NodeDCGMIndex, nodeName)
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
	objs, err := l.informer.GetIndexer().ByIndex(NodeDriverIndex, nodeName)
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
			return "true", nil
		}
	}

	return "", nil
}

// updateNodeLabels updates node labels based on expected DCGM and driver label values
// nolint: cyclop // todo
func (l *Labeler) updateNodeLabels(nodeName, expectedDCGMVersion, expectedDriverLabel string) error {
	updateStartTime := time.Now()
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		node, err := l.clientset.CoreV1().Nodes().Get(l.ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get node %s: %w", nodeName, err)
		}

		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}

		needsUpdate := false

		if node.Labels[DCGMVersionLabel] != expectedDCGMVersion {
			needsUpdate = true

			if expectedDCGMVersion == "" {
				delete(node.Labels, DCGMVersionLabel)
				klog.Infof("Removing DCGM version label from node %s", nodeName)
			} else {
				node.Labels[DCGMVersionLabel] = expectedDCGMVersion
				klog.Infof("Setting DCGM version label on node %s to %s", nodeName, expectedDCGMVersion)
			}
		}

		if node.Labels[DriverInstalledLabel] != expectedDriverLabel {
			needsUpdate = true

			if expectedDriverLabel == "" {
				delete(node.Labels, DriverInstalledLabel)
				klog.Infof("Removing driver installed label from node %s", nodeName)
			} else {
				node.Labels[DriverInstalledLabel] = expectedDriverLabel
				klog.Infof("Setting driver installed label on node %s to %s", nodeName, expectedDriverLabel)
			}
		}

		if !needsUpdate {
			klog.V(4).Infof("Node %s already has correct labels", nodeName)
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

	return l.updateNodeLabels(pod.Spec.NodeName, expectedDCGMVersion, expectedDriverLabel)
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

	return l.updateNodeLabels(pod.Spec.NodeName, expectedDCGMVersion, expectedDriverLabel)
}
