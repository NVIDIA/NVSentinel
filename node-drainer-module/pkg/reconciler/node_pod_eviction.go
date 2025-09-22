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

package reconciler

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	multierror "github.com/hashicorp/go-multierror"
	storeconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/store"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	policyv1client "k8s.io/client-go/kubernetes/typed/policy/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

type NodeDrainerClient struct {
	clientset              kubernetes.Interface
	eviction               policyv1client.PolicyV1Interface
	dryRunMode             []string
	notReadyTimeoutMinutes *int
}

func NewNodeDrainerClient(kubeconfig string, dryRun bool, notReadyTimeoutMinutes *int) (*NodeDrainerClient, error) {
	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		if kubeconfig == "" {
			return nil, fmt.Errorf("kubeconfig is not set")
		}

		// build config from kubeconfig file
		k8sConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("error creating Kubernetes config from kubeconfig: %w", err)
		}
	}

	// TODO remove this after KACE-1708 is implemtned
	k8sConfig.QPS = 2
	k8sConfig.Burst = 5

	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return nil, fmt.Errorf("error creating clientset: %w", err)
	}
	client := &NodeDrainerClient{
		clientset:              clientset,
		eviction:               clientset.PolicyV1(),
		notReadyTimeoutMinutes: notReadyTimeoutMinutes,
	}
	if dryRun {
		client.dryRunMode = []string{metav1.DryRunAll}
	} else {
		client.dryRunMode = []string{}
	}
	return client, nil
}

func (c *NodeDrainerClient) findAllPodsInNamespaceAndNode(ctx context.Context, namespace string, nodeName string) ([]v1.Pod, error) {
	pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s on node %s: %w", namespace, nodeName, err)
	}

	if len(pods.Items) == 0 {
		klog.Infof("No pods present in namespace %s on node %s\n", namespace, nodeName)
	}

	// ignore daemonset pods and other pods that should be ignored
	filteredPods := c.findRemainingPods(pods.Items, namespace, nodeName)
	return filteredPods, nil
}

// isPodStuckInTerminating checks if a pod is stuck in terminating state beyond its grace period
func (c *NodeDrainerClient) isPodStuckInTerminating(pod *v1.Pod) bool {
	// Check if pod is marked for deletion
	if pod.DeletionTimestamp == nil {
		return false
	}

	timeoutThreshold := pod.DeletionTimestamp.Add(time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second)

	// If current time is beyond the timeout threshold, pod is considered stuck
	if time.Now().After(timeoutThreshold) {
		klog.Infof("Pod %s in namespace %s is stuck in terminating state - deletion timestamp: %v, grace period: %ds, timeout threshold: %v",
			pod.Name, pod.Namespace, pod.DeletionTimestamp, *pod.Spec.TerminationGracePeriodSeconds, timeoutThreshold)
		return true
	}

	return false
}

func (c *NodeDrainerClient) findRemainingPods(pods []v1.Pod, namespace string, nodeName string) []v1.Pod {
	filteredPods := []v1.Pod{}
	for _, pod := range pods {
		// Skip DaemonSet pods
		if c.isDaemonSetPod(&pod, namespace, nodeName) {
			continue
		}

		// Skip completed pods (Succeeded or Failed)
		if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			klog.Infof("Ignoring completed pod %s in namespace %s on node %s (status: %s) during eviction check",
				pod.Name, namespace, nodeName, pod.Status.Phase)
			continue
		}

		// skip long stuck pods
		if c.isPodStuckInTerminating(&pod) || c.isPodNotReady(&pod) {
			klog.Infof("Ignoring pod %s in namespace %s on node %s", pod.Name, namespace, nodeName)
			continue
		}
		filteredPods = append(filteredPods, pod)
	}
	return filteredPods
}

// Implementing a custom function
// instead of using https://pkg.go.dev/k8s.io/kubernetes/pkg/api/v1/pod#IsPodReady
// because it doesn't consider timeout for condition checking
// isPodNotReady checks if a pod is in NotReady state
func (c *NodeDrainerClient) isPodNotReady(pod *v1.Pod) bool {
	// Get timeout values from configuration, with defaults
	notReadyTimeout := 5 * time.Minute

	if c.notReadyTimeoutMinutes != nil {
		notReadyTimeout = time.Duration(*c.notReadyTimeoutMinutes) * time.Minute
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

// gracefully evicts all pods in a namespace
func (c *NodeDrainerClient) EvictAllPodsInImmediateMode(ctx context.Context, namespace string, nodeName string, timeout time.Duration) error {
	pods, err := c.findAllPodsInNamespaceAndNode(ctx, namespace, nodeName)
	if err != nil {
		nodeDrainError.WithLabelValues("listing_pods_error", nodeName).Inc()
		return fmt.Errorf("error while fetching pods in namespace %s on node %s : %w", namespace, nodeName, err)
	}

	if len(pods) == 0 {
		return nil
	}

	err = c.evictPodsInNamespaceAndNode(ctx, namespace, nodeName, timeout, pods)
	if err != nil {
		nodeDrainError.WithLabelValues("pods_eviction_error", nodeName).Inc()
		return fmt.Errorf("error in evicting pods immediately in namespace %s: %w", namespace, err)
	}

	return nil
}

func (c *NodeDrainerClient) evictPodsInNamespaceAndNode(ctx context.Context, namespace string, nodeName string, timeout time.Duration, pods []v1.Pod) error {
	var wg sync.WaitGroup
	var mErr *multierror.Error
	errChan := make(chan error, len(pods))

	for _, pod := range pods {
		wg.Add(1)
		go func(ctx context.Context, namespace, podName string, timeout time.Duration) {
			defer wg.Done()
			err := c.sendEvictionRequestForPod(ctx, namespace, timeout, podName, nodeName)
			if err != nil {
				if errors.IsNotFound(err) {
					// if the pod is already deleted, ignore the error
					klog.Infof("Pod %s already evicted from namespace %s on node %s\n", podName, namespace, nodeName)
				} else {
					errChan <- fmt.Errorf("failed to evict pod %s from namespace %s on node %s: %w", podName, namespace, nodeName, err)
				}
			} else {
				klog.Infof("Pod %s evicted successfully from namespace %s on node %s\n", podName, namespace, nodeName)
			}
		}(ctx, namespace, pod.Name, timeout)
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		mErr = multierror.Append(mErr, err)
	}

	if mErr.ErrorOrNil() != nil {
		return mErr
	}
	return nil
}

// evicts a pod
func (c *NodeDrainerClient) sendEvictionRequestForPod(ctx context.Context, namespace string, timeout time.Duration, podName string, nodeName string) error {
	var err error
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
		DeleteOptions: &metav1.DeleteOptions{
			GracePeriodSeconds: ptr.To(int64(timeout.Seconds())),
			DryRun:             c.dryRunMode,
		},
	}

	for i := 1; i <= maxRetries; i++ {
		klog.Infof("Attempt %d, evicting pod %s in namespace %s...", i, podName, namespace)
		err = c.eviction.Evictions(namespace).Evict(ctx, eviction)
		if err == nil {
			return nil
		}

		if errors.IsTooManyRequests(err) {
			klog.Errorf("PDB blocking eviction, retrying in %s... ", retryDelay)
			nodeDrainError.WithLabelValues("PDB_blocking_eviction_error", nodeName).Inc()
			time.Sleep(retryDelay)
			continue
		}
		return fmt.Errorf("error in evicting the pod %s from namespace %s: %w", podName, namespace, err)
	}
	return fmt.Errorf("max attempt reached, eviction of pod %s from namespace %s couldn't complete: %w", podName, namespace, err)
}

// poll to check if all pods are successfully evicted from namespaces on a node
func (c *NodeDrainerClient) CheckIfAllPodsAreEvictedInImmediateMode(ctx context.Context, namespaces []string, nodeName string, timeout time.Duration) bool {

	allEvicted, remainingPods := c.checkIfPodsPresentInNamespaceAndNode(ctx, namespaces, nodeName)

	if allEvicted {
		klog.Infof("Evicted all pods in namespace %v from node %s", namespaces, nodeName)
		return true
	}

	klog.Infof("Following pods are not evicted from node %s, waiting %v for them to finish: \n%+v", nodeName, timeout, remainingPods)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		klog.Infof("Context cancelled, stopping the monitoring")
		return false
	case <-timer.C:

		allEvicted, remainingPods := c.checkIfPodsPresentInNamespaceAndNode(ctx, namespaces, nodeName)

		if !allEvicted {
			err := c.forceDeletePods(ctx, remainingPods)
			if err != nil {
				nodeDrainError.WithLabelValues("pods_force_deletion_error", nodeName).Inc()
				klog.Errorf("Failed to force delete pods on node %s: %+v\n", nodeName, err)
				return false
			}
			return c.verifyPodsDeletion(ctx, namespaces, nodeName, time.Minute)
		} else {
			klog.Infof("Evicted all pods in namespace %v from node %s", namespaces, nodeName)
			return true
		}
	}
}

// check if pods are present in given namespace
func (c *NodeDrainerClient) checkIfPodsPresentInNamespaceAndNode(ctx context.Context, namespaces []string, nodeName string) (bool, []v1.Pod) {

	type result struct {
		namespace string
		pods      []v1.Pod
		err       error
	}

	checkNamespace := func(namespace string, resultChan chan<- result) {
		pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
		})
		if err != nil {
			nodeDrainError.WithLabelValues("listing_pods_error", nodeName).Inc()
			resultChan <- result{namespace: namespace, pods: nil, err: fmt.Errorf("failed to list pods in namespace %s on node %s: %w", namespace, nodeName, err)}
			return
		}
		if len(pods.Items) > 0 {
			remainingPods := c.findRemainingPods(pods.Items, namespace, nodeName)

			resultChan <- result{namespace: namespace, pods: remainingPods, err: nil}
			return
		}
		resultChan <- result{namespace: namespace, pods: nil, err: nil}
	}

	allEvicted := true
	var remainingPods []v1.Pod
	resultChan := make(chan result, len(namespaces))

	for _, ns := range namespaces {
		checkNamespace(ns, resultChan)
	}

	for i := 0; i < len(namespaces); i++ {
		res := <-resultChan
		if res.err != nil {
			klog.Errorf("Failed to check namespace %s on node %s: %+v", res.namespace, nodeName, res.err)
			allEvicted = false
			continue
		}
		if len(res.pods) > 0 {
			remainingPods = append(remainingPods, res.pods...)
			allEvicted = false
		}
	}

	close(resultChan)
	return allEvicted, remainingPods
}

func (c *NodeDrainerClient) isDaemonSetPod(pod *v1.Pod, namespace string, nodeName string) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			klog.Infof("Ignoring DaemonSet pod %s in namespace %s on node %s during eviction check",
				pod.Name, namespace, nodeName)
			return true
		}
	}

	return false
}

// verify if all pods are deleted after force deletion
func (c *NodeDrainerClient) verifyPodsDeletion(ctx context.Context, namespaces []string, nodeName string, timeout time.Duration) bool {
	startTime := time.Now()

	for {
		if ctx.Err() != nil {
			return false
		}
		if time.Since(startTime) > timeout {
			nodeDrainError.WithLabelValues("force_deletion_not_completed_error", nodeName).Inc()
			klog.Errorf("Timeout exceeded while waiting for pods to be deleted in namespace %v on node %s", namespaces, nodeName)
			return false
		}

		allDeleted, remainingPods := c.checkIfPodsPresentInNamespaceAndNode(ctx, namespaces, nodeName)
		if allDeleted {
			klog.Infof("Deleted all pods in namespace %v from node %s", namespaces, nodeName)
			return true
		}

		klog.Infof("Following pods are not deleted from node %s, waiting for them to terminate: \n%+v", nodeName, remainingPods)

		time.Sleep(retryDelay)
	}
}

// get namespaces that matches the given pattern
func (c *NodeDrainerClient) GetNamespacesMatchingPattern(ctx context.Context, includePattern string, excludePattern string) ([]string, error) {
	namespaces, err := c.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list namespaces: %w", err)
	}

	// Compile exclude regex once
	var excludeRegex *regexp.Regexp
	if excludePattern != "" {
		excludeRegex, err = regexp.Compile(excludePattern)
		if err != nil {
			return nil, fmt.Errorf("invalid exclude regex %s: %w", excludePattern, err)
		}
	}

	var matchedNamespaces []string
	for _, ns := range namespaces.Items {
		// If excludeRegex is supplied and it matches, skip
		if excludeRegex != nil && excludeRegex.MatchString(ns.Name) {
			continue
		}

		// Match include glob pattern first
		includeMatches, err := filepath.Match(includePattern, ns.Name)
		if err != nil {
			return nil, fmt.Errorf("error matching include pattern %s: %w", includePattern, err)
		}
		if !includeMatches {
			continue
		}

		matchedNamespaces = append(matchedNamespaces, ns.Name)
	}

	return matchedNamespaces, nil
}

// force delete pods by removing their entries from etcd
func (c *NodeDrainerClient) forceDeletePods(ctx context.Context, pods []v1.Pod) error {

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs *multierror.Error

	// 0 grace period means force delete
	gracePeriod := int64(0)
	for _, pod := range pods {
		wg.Add(1)
		go func(p v1.Pod) {
			defer wg.Done()
			err := c.clientset.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
				DryRun:             c.dryRunMode,
			})
			if err != nil {
				if !errors.IsNotFound(err) {
					mu.Lock()
					errs = multierror.Append(errs, fmt.Errorf("failed to force delete pod %s in namespace %s: %w", p.Name, p.Namespace, err))
					mu.Unlock()
				}
			} else {
				klog.Infof("Force deleted pod %s in namespace %s\n", p.Name, p.Namespace)
			}
		}(pod)
	}

	wg.Wait()
	return errs.ErrorOrNil()
}

// monitor the pods to complete their execution in allow completion mode
func (c *NodeDrainerClient) MonitorPodCompletion(ctx context.Context, namespace string, nodeName string) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.Infof("Context cancelled, stopping the monitoring")
			return nil
		case <-ticker.C:
			podsList, err := c.findAllPodsInNamespaceAndNode(ctx, namespace, nodeName)
			if err != nil {
				nodeDrainError.WithLabelValues("listing_pods_error", nodeName).Inc()
				return fmt.Errorf("error in listing remaining pods in namespace %s on node %s", namespace, nodeName)
			}

			podNames := []string{}
			for _, pod := range podsList {
				podNames = append(podNames, pod.Name)
			}

			if len(podNames) == 0 {
				return nil
			}

			klog.InfoS("Still waiting for these pods to finish", "node", nodeName, "name", podNames, "namespace", namespace)
		}
	}
}

func (c *NodeDrainerClient) UpdateNodeLabel(ctx context.Context, nodeName string, isDraining bool) error {
	return retry.OnError(retry.DefaultRetry, errors.IsConflict, func() error {
		node, err := c.clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		currentValue, exists := node.Labels[NodeDrainStatusLabelKey]
		if isDraining {
			if exists && currentValue == string(InProgress) {
				klog.Infof("Node draining label is already set for node %s", nodeName)
				return nil
			}
			node.Labels[NodeDrainStatusLabelKey] = string(InProgress)
		} else {
			if !exists {
				klog.Infof("Node draining label is already absent for node %s", nodeName)
				return nil
			}
			delete(node.Labels, NodeDrainStatusLabelKey)
		}

		_, err = c.clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})

		if err != nil {
			return err
		}

		klog.Infof("Node draining label updated for node %s", nodeName)
		return nil
	})
}

func (c *NodeDrainerClient) DeletePodsAfterTimeout(ctx context.Context, nodeName string, namespaces []string, timeout int, event *storeconnector.HealthEventWithStatus) error {
	// ticker to periodically check if pods are still present on the node
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	drainTimeout, err := c.GetNodeDrainTimeout(ctx, nodeName, timeout, event)
	deleteDateTimeUTC := time.Now().UTC().Add(drainTimeout).Format(time.RFC3339)
	if err != nil {
		return fmt.Errorf("error getting node drain timeout: %w", err)
	}

	// timer that fires once the overall timeout has elapsed
	timer := time.NewTimer(drainTimeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			// Context cancelled – nothing more to do
			return nil

		case <-ticker.C:
			// Periodically check if all pods are already gone. If yes – we're done early.
			podsRunning := []string{}
			for _, ns := range namespaces {
				pods, err := c.findAllPodsInNamespaceAndNode(ctx, ns, nodeName)
				if err != nil {
					return fmt.Errorf("error listing pods in namespace %s on node %s: %w", ns, nodeName, err)
				}
				if len(pods) > 0 {
					for _, pod := range pods {
						podsRunning = append(podsRunning, pod.Name)
					}
				}
			}
			if len(podsRunning) == 0 {
				// Nothing left to delete – return early.
				return nil
			}

			err := c.updateNodeEvent(ctx, nodeName, podsRunning, namespaces, deleteDateTimeUTC)
			if err != nil {
				return fmt.Errorf("error updating node event: %w", err)
			}

			klog.Infof("Still waiting for these pods to finish: %v in namespace %v on node %s", podsRunning, namespaces, nodeName)

		case <-timer.C:
			// Timeout hit – force delete all remaining pods.
			for _, ns := range namespaces {
				pods, err := c.findAllPodsInNamespaceAndNode(ctx, ns, nodeName)
				if err != nil {
					return fmt.Errorf("error listing pods in namespace %s on node %s: %w", ns, nodeName, err)
				}
				if err := c.forceDeletePods(ctx, pods); err != nil {
					return fmt.Errorf("error force deleting pods in namespace %s on node %s: %w", ns, nodeName, err)
				}
			}
			return nil
		}
	}
}

// get the timeout for the node drain using health event creation time and configured deleteAfterTimeout
func (c *NodeDrainerClient) GetNodeDrainTimeout(ctx context.Context, nodeName string, timeout int, event *storeconnector.HealthEventWithStatus) (time.Duration, error) {
	elapsed := time.Since(event.CreatedAt)
	drainTimeout := time.Duration(timeout) * time.Minute

	klog.Infof("Node %s has been in draining state for %v and waiting timeout is %v", nodeName, elapsed, drainTimeout)

	return drainTimeout - elapsed, nil

}

func (c *NodeDrainerClient) updateNodeEvent(ctx context.Context, nodeName string, podsRunning []string, namespaces []string, deleteTimeUTC string) error {
	node, err := c.clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	eventsClient := c.clientset.CoreV1().Events(metav1.NamespaceDefault)

	reason := "NodeDraining"
	message := fmt.Sprintf("The node has run into a fatal event and needs to be drained. Please terminate pods %s in namespace %s or they will be force deleted on %s.",
		podsRunning, namespaces, deleteTimeUTC)

	// Try to find an existing Event for this node with the same reason so we can just bump the count
	evtList, err := eventsClient.List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=%s,reason=%s", nodeName, "Node", reason),
	})
	if err != nil {
		return err
	}

	now := metav1.NewTime(time.Now())
	if len(evtList.Items) > 0 {
		// Update the first matching event – this follows kube-api server aggregation rules
		existing := evtList.Items[0]
		existing.Count++
		existing.LastTimestamp = now
		existing.Message = message
		_, err = eventsClient.Update(ctx, &existing, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("error in updating event occurrance count: %w", err)
		}
		return nil
	}

	// Otherwise create a new event
	newEvent := &v1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: nodeName + "-",
			Namespace:    metav1.NamespaceDefault,
		},
		InvolvedObject: v1.ObjectReference{
			Kind:       "Node",
			Name:       nodeName,
			UID:        node.UID,
			APIVersion: "v1",
		},
		Reason:         reason,
		Message:        message,
		Type:           v1.EventTypeNormal,
		Source:         v1.EventSource{Component: "nvsentinel-node-drainer-module"},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}

	_, err = eventsClient.Create(ctx, newEvent, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error in creating event: %w", err)
	}
	return nil
}
