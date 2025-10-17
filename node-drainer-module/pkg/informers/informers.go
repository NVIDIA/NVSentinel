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

package informers

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/metrics"
	storeconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
)

const (
	NodeIndex            = "node"
	NamespaceNodeIndex   = "namespace-node"
	NodeEventReasonIndex = "node-event-reason"
)

type Informers struct {
	podInformer            cache.SharedIndexInformer
	eventInformer          cache.SharedIndexInformer
	clientset              kubernetes.Interface
	notReadyTimeoutMinutes *int
	dryRunMode             []string
	namespace              string
}

func NewInformers(clientset kubernetes.Interface, resyncPeriod time.Duration,
	notReadyTimeoutMinutes *int, dryRun bool) (*Informers, error) {
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		resyncPeriod,
	)

	podInformer := informerFactory.Core().V1().Pods().Informer()
	err := podInformer.GetIndexer().AddIndexers(
		cache.Indexers{
			NodeIndex:          NodeIndexFunc,
			NamespaceNodeIndex: NamespaceNodeIndexFunc,
		})

	if err != nil {
		return nil, fmt.Errorf("failed to add indexer: %w", err)
	}

	eventInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		resyncPeriod,
		informers.WithNamespace(metav1.NamespaceDefault),
	)

	eventInformer := eventInformerFactory.Core().V1().Events().Informer()
	err = eventInformer.GetIndexer().AddIndexers(
		cache.Indexers{
			NodeEventReasonIndex: NodeEventReasonIndexFunc,
		})

	if err != nil {
		return nil, fmt.Errorf("failed to add event indexer: %w", err)
	}

	dryRunMode := []string{}
	if dryRun {
		dryRunMode = []string{metav1.DryRunAll}
	}

	return &Informers{
		clientset:              clientset,
		podInformer:            podInformer,
		eventInformer:          eventInformer,
		notReadyTimeoutMinutes: notReadyTimeoutMinutes,
		dryRunMode:             dryRunMode,
		namespace:              metav1.NamespaceDefault,
	}, nil
}

func (i *Informers) HasSynced() bool {
	return i.podInformer.HasSynced() && i.eventInformer.HasSynced()
}

func NodeIndexFunc(obj any) ([]string, error) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return []string{}, nil
	}

	if pod.Spec.NodeName == "" {
		return []string{}, nil
	}

	return []string{pod.Spec.NodeName}, nil
}

func NamespaceNodeIndexFunc(obj any) ([]string, error) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return []string{}, nil
	}

	if pod.Spec.NodeName == "" {
		return []string{}, nil
	}

	compositeKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Spec.NodeName)

	return []string{compositeKey}, nil
}

func NodeEventReasonIndexFunc(obj any) ([]string, error) {
	event, ok := obj.(*v1.Event)
	if !ok {
		return []string{}, nil
	}

	if event.InvolvedObject.Kind != "Node" {
		return []string{}, nil
	}

	compositeKey := fmt.Sprintf("%s/%s", event.InvolvedObject.Name, event.Reason)

	return []string{compositeKey}, nil
}

func (i *Informers) Run(ctx context.Context) error {
	go i.podInformer.Run(ctx.Done())
	go i.eventInformer.Run(ctx.Done())

	if ok := cache.WaitForCacheSync(ctx.Done(),
		i.HasSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	return nil
}

func (i *Informers) FindEvictablePodsInNamespaceAndNode(namespace, nodeName string) ([]*v1.Pod, error) {
	compositeKey := fmt.Sprintf("%s/%s", namespace, nodeName)

	objs, err := i.podInformer.GetIndexer().ByIndex(NamespaceNodeIndex, compositeKey)
	if err != nil {
		klog.Errorf("Failed to get pods by index: %v", err)
		return nil, fmt.Errorf("failed to get pods by index: %w", err)
	}

	pods := make([]*v1.Pod, 0, len(objs))

	for _, obj := range objs {
		if pod, ok := obj.(*v1.Pod); ok {
			pods = append(pods, pod)
		}
	}

	return i.filterEvictablePods(pods), nil
}

func (i *Informers) filterEvictablePods(pods []*v1.Pod) []*v1.Pod {
	filteredPods := []*v1.Pod{}

	for _, pod := range pods {
		if i.isDaemonSetPod(pod) {
			continue
		}

		if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			klog.Infof("Ignoring completed pod %s in namespace %s on node %s (status: %s) during eviction check",
				pod.Name, pod.Namespace, pod.Spec.NodeName, pod.Status.Phase)
			continue
		}

		if i.isPodStuckInTerminating(pod) || i.isPodNotReady(pod) {
			klog.Infof("Ignoring pod %s in namespace %s on node %s", pod.Name, pod.Namespace, pod.Spec.NodeName)
			continue
		}

		filteredPods = append(filteredPods, pod)
	}

	return filteredPods
}

func (i *Informers) isDaemonSetPod(pod *v1.Pod) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			klog.Infof("Ignoring DaemonSet pod %s in namespace %s on node %s during eviction check",
				pod.Name, pod.Namespace, pod.Spec.NodeName)
			return true
		}
	}

	return false
}

func (i *Informers) isPodStuckInTerminating(pod *v1.Pod) bool {
	if pod.DeletionTimestamp == nil {
		return false
	}

	gracePeriod := 30
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		gracePeriod = int(*pod.Spec.TerminationGracePeriodSeconds)
	}

	timeoutThreshold := pod.DeletionTimestamp.Add(time.Duration(gracePeriod) * time.Second)

	// If current time is beyond the timeout threshold, pod is considered stuck
	if time.Now().After(timeoutThreshold) {
		klog.Infof("Pod %s in namespace %s is stuck in terminating state - deletion timestamp: %v:,"+
			"grace period: %ds, timeout threshold: %v", pod.Name, pod.Namespace, pod.DeletionTimestamp,
			gracePeriod, timeoutThreshold)

		return true
	}

	return false
}

func (i *Informers) isPodNotReady(pod *v1.Pod) bool {
	notReadyTimeout := 5 * time.Minute

	if i.notReadyTimeoutMinutes != nil {
		notReadyTimeout = time.Duration(*i.notReadyTimeoutMinutes) * time.Minute
	}

	// Check if pod has any NotReady conditions
	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodReady && condition.Status == v1.ConditionFalse {
			// Additional check: if the pod has been in NotReady state for the configured time
			if condition.LastTransitionTime.Add(notReadyTimeout).Before(time.Now()) {
				klog.Infof("Pod %s in namespace %s is in NotReady state since %v (timeout: %v)",
					pod.Name, pod.Namespace, condition.LastTransitionTime, notReadyTimeout)
				return true
			}
		}
	}

	return false
}

func (i *Informers) EvictAllPodsInImmediateMode(ctx context.Context,
	namespace, nodeName string, timeout time.Duration) error {
	pods, err := i.FindEvictablePodsInNamespaceAndNode(namespace, nodeName)
	if err != nil {
		klog.Errorf("Failed to find evictable pods in namespace %s on node %s: %v", namespace, nodeName, err)
		return fmt.Errorf("failed to find evictable pods in namespace %s on node %s: %w", namespace, nodeName, err)
	}

	if len(pods) == 0 {
		return nil
	}

	err = i.evictPodsInNamespaceAndNode(ctx, namespace, timeout, pods)
	if err != nil {
		klog.Errorf("Failed to evict pods in namespace %s on node %s: %v", namespace, nodeName, err)
		return fmt.Errorf("failed to evict pods in namespace %s on node %s: %w", namespace, nodeName, err)
	}

	return nil
}

func (i *Informers) evictPodsInNamespaceAndNode(ctx context.Context,
	namespace string, timeout time.Duration, pods []*v1.Pod) error {
	var wg sync.WaitGroup

	var mu sync.Mutex

	var result *multierror.Error

	for _, pod := range pods {
		wg.Add(1)

		go func(ctx context.Context, pod *v1.Pod, timeout time.Duration) {
			defer wg.Done()

			err := i.sendEvictionRequestForPod(ctx, namespace, timeout, pod)
			if err != nil {
				if errors.IsNotFound(err) {
					klog.Infof("Pod %s already evicted from namespace %s on node %s", pod.Name, pod.Namespace, pod.Spec.NodeName)
				} else {
					klog.Errorf("Failed to evict pod %s from namespace %s on node %s: %v",
						pod.Name, pod.Namespace, pod.Spec.NodeName, err)
					mu.Lock()
					result = multierror.Append(result, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err))
					mu.Unlock()
				}
			} else {
				klog.Infof("Pod %s eviction initiated for namespace %s on node %s", pod.Name, pod.Namespace, pod.Spec.NodeName)
			}
		}(ctx, pod, timeout)
	}

	wg.Wait()

	return result.ErrorOrNil()
}

func (i *Informers) sendEvictionRequestForPod(ctx context.Context, namespace string,
	timeout time.Duration, pod *v1.Pod) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: namespace,
		},
		DeleteOptions: &metav1.DeleteOptions{
			GracePeriodSeconds: ptr.To(int64(timeout.Seconds())),
			DryRun:             i.dryRunMode,
		},
	}

	err := i.clientset.PolicyV1().Evictions(pod.Namespace).Evict(ctx, eviction)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}

		if errors.IsTooManyRequests(err) {
			metrics.NodeDrainError.WithLabelValues("PDB_blocking_eviction_error", pod.Spec.NodeName).Inc()
		}

		return fmt.Errorf("error evicting pod %s from namespace %s: %w", pod.Name, pod.Namespace, err)
	}

	return nil
}

func (i *Informers) UpdateNodeEvent(ctx context.Context, nodeName string, reason string, message string) error {
	compositeKey := fmt.Sprintf("%s/%s", nodeName, reason)

	cachedEvents, err := i.eventInformer.GetIndexer().ByIndex(NodeEventReasonIndex, compositeKey)
	if err != nil {
		klog.Errorf("Failed to query event cache: %v", err)
		return fmt.Errorf("error querying event cache: %w", err)
	}

	now := metav1.NewTime(time.Now())
	eventsClient := i.clientset.CoreV1().Events(i.namespace)

	for _, obj := range cachedEvents {
		existingEvent, ok := obj.(*v1.Event)
		if !ok {
			continue
		}

		if existingEvent.Message == message {
			eventCopy := existingEvent.DeepCopy()
			eventCopy.Count++
			eventCopy.LastTimestamp = now
			_, err = eventsClient.Update(ctx, eventCopy, metav1.UpdateOptions{})

			if err != nil {
				klog.Errorf("Failed to update event occurrence count: %v", err)
				return fmt.Errorf("error in updating event occurrence count: %w", err)
			}

			return nil
		}
	}

	newEvent := &v1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: nodeName + "-",
			Namespace:    i.namespace,
		},
		InvolvedObject: v1.ObjectReference{
			Kind:       "Node",
			Name:       nodeName,
			APIVersion: "v1",
		},
		Reason:         reason,
		Message:        message,
		Type:           "NodeDraining",
		Source:         v1.EventSource{Component: "nvsentinel-node-drainer"},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}

	_, err = eventsClient.Create(ctx, newEvent, metav1.CreateOptions{})
	if err != nil {
		klog.Errorf("Failed to create event: %v", err)
		return fmt.Errorf("error in creating event: %w", err)
	}

	return nil
}

func (i *Informers) DeletePodsAfterTimeout(ctx context.Context, nodeName string, namespaces []string,
	timeout int, event *storeconnector.HealthEventWithStatus) error {
	drainTimeout, err := i.getNodeDrainTimeout(timeout, event)
	if err != nil {
		klog.Errorf("Failed to get node drain timeout: %v", err)
		return fmt.Errorf("failed to get node drain timeout: %w", err)
	}

	timeoutDeadline := event.CreatedAt.Add(time.Duration(timeout) * time.Minute)
	deleteDateTimeUTC := timeoutDeadline.UTC().Format(time.RFC3339)
	timeoutReached := drainTimeout <= 0

	evicted, remainingPods := i.checkIfPodsPresentInNamespaceAndNode(namespaces, nodeName)
	if evicted {
		klog.Infof("All pods on node %s have been deleted", nodeName)
		return nil
	}

	if timeoutReached {
		klog.Infof("Timeout reached for node %s, force deleting %d remaining pods", nodeName, len(remainingPods))

		// Track timeout reached for each namespace
		for _, ns := range namespaces {
			metrics.NodeDrainTimeoutReached.WithLabelValues(nodeName, ns).Inc()
		}

		if err := i.forceDeletePods(ctx, remainingPods); err != nil {
			klog.Errorf("Failed to force delete pods on node %s: %v", nodeName, err)
			return fmt.Errorf("failed to force delete pods on node %s: %w", nodeName, err)
		}

		// After force deleting, requeue to verify pods are gone
		return fmt.Errorf("force deleted %d pods, requeueing to verify deletion on node %s", len(remainingPods), nodeName)
	}

	podNames := make([]string, 0, len(remainingPods))
	for _, pod := range remainingPods {
		podNames = append(podNames, pod.Name)
	}

	message := fmt.Sprintf(
		"Waiting for following pods to finish: %v in namespace: %v or they will be force deleted on: %s",
		podNames, namespaces, deleteDateTimeUTC,
	)

	reason := "WaitingBeforeForceDelete"

	if err := i.UpdateNodeEvent(ctx, nodeName, reason, message); err != nil {
		klog.Errorf("Failed to update node event for %s: %v", nodeName, err)
	}

	klog.Infof("Still waiting for these pods to finish: %v in namespace %v on node %s (timeout in %v)",
		podNames, namespaces, nodeName, drainTimeout)

	return fmt.Errorf("waiting for %d pods to complete or timeout (%v remaining) on node %s",
		len(remainingPods), drainTimeout, nodeName)
}

func (i *Informers) getNodeDrainTimeout(timeout int,
	event *storeconnector.HealthEventWithStatus) (time.Duration, error) {
	elapsed := time.Since(event.CreatedAt)
	drainTimeout := time.Duration(timeout) * time.Minute

	return drainTimeout - elapsed, nil
}

func (i *Informers) forceDeletePods(ctx context.Context, pods []*v1.Pod) error {
	gracePeriod := int64(0)

	var wg sync.WaitGroup

	var mu sync.Mutex

	var result *multierror.Error

	for _, pod := range pods {
		wg.Add(1)

		go func(p *v1.Pod) {
			defer wg.Done()

			err := i.clientset.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
				DryRun:             i.dryRunMode,
			})
			if err != nil {
				if !errors.IsNotFound(err) {
					klog.Errorf("Failed to force delete pod %s in namespace %s: %v", p.Name, p.Namespace, err)
					mu.Lock()
					result = multierror.Append(result, fmt.Errorf("pod %s/%s: %w", p.Namespace, p.Name, err))
					mu.Unlock()
				}
			} else {
				klog.Infof("Force deleted pod %s in namespace %s", p.Name, p.Namespace)
			}
		}(pod)
	}

	wg.Wait()

	return result.ErrorOrNil()
}

func (i *Informers) GetNamespacesMatchingPattern(ctx context.Context,
	includePattern string, excludePattern string, nodeName string) ([]string, error) {
	objs, err := i.podInformer.GetIndexer().ByIndex(NodeIndex, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get pods on node %s: %w", nodeName, err)
	}

	excludeRegex, err := i.compileExcludePattern(excludePattern)
	if err != nil {
		klog.Errorf("Failed to compile exclude pattern: %v", err)
		return nil, fmt.Errorf("failed to compile exclude pattern: %w", err)
	}

	namespaceSet, err := i.extractNamespacesFromPods(objs, includePattern, excludeRegex)
	if err != nil {
		klog.Errorf("Failed to extract namespaces from pods: %v", err)
		return nil, fmt.Errorf("failed to extract namespaces from pods: %w", err)
	}

	return i.convertSetToSlice(namespaceSet), nil
}

func (i *Informers) compileExcludePattern(excludePattern string) (*regexp.Regexp, error) {
	if excludePattern == "" {
		return nil, nil
	}

	excludeRegex, err := regexp.Compile(excludePattern)
	if err != nil {
		klog.Errorf("Failed to compile exclude pattern: %v", err)
		return nil, fmt.Errorf("invalid exclude regex %s: %w", excludePattern, err)
	}

	return excludeRegex, nil
}

func (i *Informers) extractNamespacesFromPods(objs []any,
	includePattern string, excludeRegex *regexp.Regexp) (map[string]struct{}, error) {
	namespaceSet := make(map[string]struct{})

	for _, obj := range objs {
		pod, ok := obj.(*v1.Pod)
		if !ok {
			continue
		}

		shouldIncludeNamespace, err := i.shouldIncludeNamespace(pod.Namespace, includePattern, excludeRegex)
		if err != nil {
			klog.Errorf("Failed to check if namespace %s should be included: %v", pod.Namespace, err)
			return nil, fmt.Errorf("failed to check if namespace %s should be included: %w", pod.Namespace, err)
		}

		if shouldIncludeNamespace {
			namespaceSet[pod.Namespace] = struct{}{}
		}
	}

	return namespaceSet, nil
}

func (i *Informers) shouldIncludeNamespace(namespace string,
	includePattern string, excludeRegex *regexp.Regexp) (bool, error) {
	if excludeRegex != nil && excludeRegex.MatchString(namespace) {
		return false, nil
	}

	includeMatches, err := filepath.Match(includePattern, namespace)
	if err != nil {
		klog.Errorf("Failed to match include pattern: %v", err)
		return false, fmt.Errorf("failed to match include pattern: %w", err)
	}

	return includeMatches, nil
}

func (i *Informers) convertSetToSlice(namespaceSet map[string]struct{}) []string {
	namespaceNames := make([]string, 0, len(namespaceSet))
	for namespace := range namespaceSet {
		namespaceNames = append(namespaceNames, namespace)
	}

	return namespaceNames
}

func (i *Informers) checkIfPodsPresentInNamespaceAndNode(namespaces []string,
	nodeName string) (bool, []*v1.Pod) {
	allEvicted := true

	var remainingPods []*v1.Pod

	for _, namespace := range namespaces {
		pods, err := i.FindEvictablePodsInNamespaceAndNode(namespace, nodeName)

		if err != nil {
			klog.Errorf("Failed to check namespace %s on node %s: %v", namespace, nodeName, err)

			allEvicted = false

			continue
		}

		if len(pods) > 0 {
			remainingPods = append(remainingPods, pods...)
			allEvicted = false
		}
	}

	return allEvicted, remainingPods
}

func (i *Informers) CheckIfAllPodsAreEvictedInImmediateMode(ctx context.Context,
	namespaces []string, nodeName string, timeout time.Duration) bool {
	allEvicted, remainingPods := i.checkIfPodsPresentInNamespaceAndNode(namespaces, nodeName)

	if allEvicted {
		klog.Infof("All pods evicted in namespace %v from node %s", namespaces, nodeName)
		return true
	}

	now := time.Now()
	shouldForceDelete := false

	for _, pod := range remainingPods {
		if pod.DeletionTimestamp != nil {
			gracePeriod := 30 * time.Second
			if pod.Spec.TerminationGracePeriodSeconds != nil {
				gracePeriod = time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second
			}

			deletionDeadline := pod.DeletionTimestamp.Add(gracePeriod).Add(timeout)
			if now.After(deletionDeadline) {
				shouldForceDelete = true
				break
			}
		}
	}

	if shouldForceDelete {
		klog.Infof("Pods on node %s exceeded timeout, attempting force deletion", nodeName)

		err := i.forceDeletePods(ctx, remainingPods)
		if err != nil {
			metrics.NodeDrainError.WithLabelValues("pods_force_deletion_error", nodeName).Inc()
			klog.Errorf("Failed to force delete pods on node %s: %v", nodeName, err)

			return false
		}

		allEvicted, _ = i.checkIfPodsPresentInNamespaceAndNode(namespaces, nodeName)
		if allEvicted {
			klog.Infof("All pods evicted after force deletion on node %s", nodeName)
		}

		return allEvicted
	}

	remainingPodNames := make([]string, 0, len(remainingPods))
	for _, pod := range remainingPods {
		remainingPodNames = append(remainingPodNames, fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
	}

	klog.Infof("Pods still present on node %s, will retry: %v", nodeName, remainingPodNames)

	return false
}
