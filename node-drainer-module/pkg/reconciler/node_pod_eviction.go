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
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	policyv1client "k8s.io/client-go/kubernetes/typed/policy/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

type NodeDrainerClient struct {
	clientset  kubernetes.Interface
	eviction   policyv1client.PolicyV1Interface
	dryRunMode []string
}

func NewNodeDrainerClient(kubeconfig string, dryRun bool) (*NodeDrainerClient, error) {
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
	client := &NodeDrainerClient{clientset: clientset, eviction: clientset.PolicyV1()}
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

	// ignore daemonset pods
	filteredPods := []v1.Pod{}
	for _, pod := range pods.Items {
		isDaemonSet := false
		for _, owner := range pod.OwnerReferences {
			if owner.Kind == "DaemonSet" {
				isDaemonSet = true
				break
			}
		}
		if !isDaemonSet {
			filteredPods = append(filteredPods, pod)
		}
	}
	return filteredPods, nil
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

	klog.Infof("Following pods are not evictecd from node %s, waiting %v for them to finish: \n%+v", nodeName, timeout, remainingPods)

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
			return c.verifyPodsDeletion(ctx, namespaces, nodeName, 60*time.Second)
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
			var remainingPods []v1.Pod
			for _, pod := range pods.Items {
				isDaemonSet := false
				for _, owner := range pod.OwnerReferences {
					if owner.Kind == "DaemonSet" {
						isDaemonSet = true
						break
					}
				}
				if !isDaemonSet {
					klog.InfoS("Pod not evicted", "node", nodeName, "name", pod.Name, "namespace", pod.Namespace)
					remainingPods = append(remainingPods, pod)
				}
			}
			resultChan <- result{namespace: namespace, pods: remainingPods, err: nil}
			return
		}
		resultChan <- result{namespace: namespace, pods: nil, err: nil}
	}

	allEvicted := true
	var remainingPods []v1.Pod
	resultChan := make(chan result, len(namespaces))

	for _, ns := range namespaces {
		go func(namespace string) {
			checkNamespace(namespace, resultChan)
		}(ns)
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
	ticker := time.NewTicker(10 * time.Second)
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

			allCompleted := true
			for _, pod := range podsList {
				allCompleted = false
				klog.InfoS("Still waiting for this pod to finish", "node", nodeName, "name", pod.Name, "namespace", pod.Namespace)
			}

			if allCompleted {
				return nil
			}
		}
	}
}
