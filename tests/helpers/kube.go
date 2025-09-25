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

package helpers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
)

const (
	WaitTimeout  = 10 * time.Minute
	WaitInterval = 30 * time.Second
)

// WaitForNodesCordonState waits for nodes with names specified in `nodeNames` to be either cordoned or uncrodoned based on `shouldCordon`. If `shouldCordon` is
// true then the function will wait for nodes to be cordoned, else it will wait for nodes to be uncordoned
func WaitForNodesCordonState(ctx context.Context, t *testing.T, c klient.Client, nodeNames []string, shouldCordon bool) {
	require.Eventually(t, func() bool {
		targetCount := len(nodeNames)
		actualCount := 0

		for _, nodeName := range nodeNames {
			var node v1.Node
			err := c.Resources().Get(ctx, nodeName, "", &node)
			if err != nil {
				t.Logf("failed to get node %s: %v", nodeName, err)
				continue
			}

			if node.Spec.Unschedulable == shouldCordon {
				actualCount++
			}
		}

		t.Logf("Nodes with cordon state %v: %d/%d", shouldCordon, actualCount, targetCount)
		return actualCount == targetCount
	}, WaitTimeout, WaitInterval, "nodes should have cordon state %v", shouldCordon)
}

// CreateNamespace creates a new Kubernetes namespace with the specified `name`.
func CreateNamespace(ctx context.Context, c klient.Client, name string) error {
	namespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	err := c.Resources().Create(ctx, namespace)
	if err != nil {
		return fmt.Errorf("failed to create namespace %s: %w", name, err)
	}

	return nil
}

// DeleteNamespace deletes the Kubernetes namespace with the specified `name` and waits for the deletion to complete.
func DeleteNamespace(ctx context.Context, t *testing.T, c klient.Client, name string) error {
	namespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	err := c.Resources().Delete(ctx, namespace)
	if err != nil {
		return fmt.Errorf("failed to delete namespace %s: %w", name, err)
	}

	require.Eventually(t, func() bool {
		var ns v1.Namespace
		err := c.Resources().Get(ctx, name, "", &ns)
		return err != nil && apierrors.IsNotFound(err)
	}, WaitTimeout, WaitInterval, "namespace %s should be deleted", name)

	return nil
}

// WaitForNodesWithLabel waits for nodes with names specified in `nodeNames` to have a label with key `labelKey` set to `expectedValue`.
func WaitForNodesWithLabel(ctx context.Context, t *testing.T, c klient.Client, nodeNames []string, labelKey, expectedValue string) {
	require.Eventually(t, func() bool {
		targetCount := len(nodeNames)
		actualCount := 0

		for _, nodeName := range nodeNames {
			node, err := GetNodeByName(ctx, c, nodeName)
			if err != nil {
				t.Logf("failed to get node %s: %v", nodeName, err)
				continue
			}

			if actualValue, exists := node.Labels[labelKey]; exists && actualValue == expectedValue {
				actualCount++
			}
		}

		t.Logf("Nodes with label %s=%s: %d/%d", labelKey, expectedValue, actualCount, targetCount)
		return actualCount == targetCount
	}, WaitTimeout, WaitInterval, "all nodes should have label %s=%s", labelKey, expectedValue)
}

// GetNodeByName retrieves a Kubernetes node by its `nodeName` and returns the node object.
func GetNodeByName(ctx context.Context, c klient.Client, nodeName string) (*v1.Node, error) {
	var node v1.Node
	err := c.Resources().Get(ctx, nodeName, "", &node)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	return &node, nil
}

// DeletePod deletes a Kubernetes pod with the specified `podName` in the given `namespace`.
func DeletePod(ctx context.Context, c klient.Client, namespace, podName string) error {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
	}

	err := c.Resources().Delete(ctx, pod)
	if err != nil {
		return fmt.Errorf("failed to delete pod %s: %w", podName, err)
	}

	return nil
}

// WaitForRebootNodeCR waits for a RebootNode custom resource to be created for the node with the specified `nodeName` and returns the CR object.
func WaitForRebootNodeCR(ctx context.Context, t *testing.T, c klient.Client, nodeName string) *unstructured.Unstructured {
	var resultCR *unstructured.Unstructured

	require.Eventually(t, func() bool {
		rebootNodeList := &unstructured.UnstructuredList{}
		rebootNodeList.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "janitor.dgxc.nvidia.com",
			Version: "v1alpha1",
			Kind:    "RebootNodeList",
		})

		err := c.Resources().List(ctx, rebootNodeList)
		if err != nil {
			t.Logf("failed to list rebootnodes: %v", err)
			return false
		}

		for _, item := range rebootNodeList.Items {
			nodeNameInCR, found, err := unstructured.NestedString(item.Object, "spec", "nodeName")
			if err != nil {
				continue
			}
			if !found {
				continue
			}

			if nodeNameInCR == nodeName {
				resultCR = &item
				return true
			}
		}
		return false
	}, WaitTimeout, WaitInterval, "RebootNode CR should exist for node %s", nodeName)

	return resultCR
}

// DeleteRebootNodeCR deletes the specified RebootNode custom resource `rebootNode`.
func DeleteRebootNodeCR(ctx context.Context, c klient.Client, rebootNode *unstructured.Unstructured) error {
	err := c.Resources().Delete(ctx, rebootNode)
	if err != nil {
		return fmt.Errorf("failed to delete RebootNode CR %s: %w", rebootNode.GetName(), err)
	}

	return nil
}

// GetAllNodesNames retrieves the names of all Kubernetes nodes in the cluster and returns them as a slice of strings.
func GetAllNodesNames(ctx context.Context, c klient.Client) ([]string, error) {
	var nodeList v1.NodeList
	err := c.Resources().List(ctx, &nodeList, resources.WithLabelSelector("type=kwok"))
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	var nodeNames []string
	for _, node := range nodeList.Items {
		nodeNames = append(nodeNames, node.Name)
	}

	return nodeNames, nil
}

// CreatePodsAndWaitTillRunning creates 8 GPU pods per node for each node specified in `nodeNames` using the provided `podTemplate` and waits for all pods to reach running state.
func CreatePodsAndWaitTillRunning(ctx context.Context, t *testing.T, c klient.Client, nodeNames []string, podTemplate *v1.Pod) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	gpuCount := 8
	totalPods := len(nodeNames) * gpuCount

	for _, nodeName := range nodeNames {
		for gpuIndex := 1; gpuIndex <= gpuCount; gpuIndex++ {
			wg.Add(1)
			go func(nodeName string) {
				defer wg.Done()

				pod := podTemplate.DeepCopy()
				pod.Spec.NodeName = nodeName

				err := c.Resources().Create(ctx, pod)
				if err != nil {
					mu.Lock()
					defer mu.Unlock()
					errs = append(errs, fmt.Errorf("failed to create pod on node %s: %w", nodeName, err))
					return
				}

				waitForPodRunning(ctx, t, c, pod.Name, pod.Namespace)
			}(nodeName)
		}
	}

	wg.Wait()

	if joinedErr := errors.Join(errs...); joinedErr != nil {
		t.Fatalf("failed to create and start %d out of %d pods:\n%v", len(errs), totalPods, joinedErr)
	}

	t.Logf("Created and verified %d pods total", totalPods)
}

// WaitForNodesCordonedAndDrained waits for nodes specified in `nodeNames` to be both cordoned and have the drain label applied.
func WaitForNodesCordonedAndDrained(ctx context.Context, t *testing.T, c klient.Client, nodeNames []string) {
	expectedCount := len(nodeNames)

	require.Eventually(t, func() bool {
		var cordonedCount, drainedCount int

		for _, nodeName := range nodeNames {
			node, err := GetNodeByName(ctx, c, nodeName)
			if err != nil {
				t.Logf("failed to get node %s: %v", nodeName, err)
				continue
			}

			if node.Spec.Unschedulable {
				cordonedCount++

				if drainStatus, exists := node.Labels["nvsentinel.dgxc.nvidia.com/node-drain-status"]; exists && drainStatus == "IN_PROGRESS" {
					drainedCount++
				}
			}
		}

		t.Logf("Cordoned nodes: %d/%d, Drained nodes: %d/%d", cordonedCount, expectedCount, drainedCount, expectedCount)
		if drainedCount > cordonedCount {
			t.Errorf("drained count is larger then cordoned count: %d/%d", drainedCount, cordonedCount)
		}
		return cordonedCount >= expectedCount && drainedCount >= expectedCount
	}, WaitTimeout, WaitInterval, "nodes should be cordoned and drained")
}

// DrainRunningPodsInNamespace finds all running pods in the specified `namespace` and deletes them to simulate node draining.
func DrainRunningPodsInNamespace(ctx context.Context, t *testing.T, c klient.Client, namespace string) {
	var podList v1.PodList
	err := c.Resources(namespace).List(ctx, &podList)
	if err != nil {
		t.Fatalf("Failed to list pods in namespace %s: %v", namespace, err)
	}

	if len(podList.Items) == 0 {
		t.Logf("No pods found in namespace %s", namespace)
		return
	}

	runningPodsFound := 0
	runningPodsDeleted := 0

	for _, pod := range podList.Items {
		isRunning, err := isPodRunning(ctx, c, namespace, pod.Name)
		if err != nil {
			t.Errorf("Failed to check pod %s status: %v", pod.Name, err)
			continue
		}

		if isRunning {
			runningPodsFound++
			t.Logf("Found running pod: %s, deleting it", pod.Name)
			err = DeletePod(ctx, c, namespace, pod.Name)
			if err != nil {
				t.Errorf("Failed to delete pod %s: %v", pod.Name, err)
			} else {
				runningPodsDeleted++
			}
		} else {
			t.Logf("Pod %s is not running (status: %s), skipping deletion", pod.Name, pod.Status.Phase)
		}
	}

	if runningPodsFound == 0 {
		t.Errorf("Expected at least one running pod in namespace %s, but found none", namespace)
	} else {
		t.Logf("Successfully deleted %d/%d running pods in namespace %s", runningPodsDeleted, runningPodsFound, namespace)
	}
}

// NewGPUPodSpec creates a new GPU pod template in the specified `namespace` with the requested `gpuCount` resources.
func NewGPUPodSpec(namespace string, gpuCount int) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-gpu-pod-",
			Namespace:    namespace,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    "gpu-container",
					Image:   "busybox:latest",
					Command: []string{"/bin/sh", "-c"},
					Args:    []string{"sleep infinity"},
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse(fmt.Sprintf("%d", gpuCount)),
						},
						Limits: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse(fmt.Sprintf("%d", gpuCount)),
						},
					},
				},
			},
			Tolerations: []v1.Toleration{
				{Operator: v1.TolerationOpExists},
			},
		},
	}
}

// waitForPodRunning waits for the pod with the specified `podName` in the given `namespace` to reach running state.
func waitForPodRunning(ctx context.Context, t *testing.T, c klient.Client, podName, namespace string) {
	require.Eventually(t, func() bool {
		isRunning, err := isPodRunning(ctx, c, namespace, podName)
		if err != nil {
			t.Logf("failed to check pod %s status: %v", podName, err)
			return false
		}
		return isRunning
	}, WaitTimeout, WaitInterval, "pod %s should be running", podName)

}

// isPodRunning checks if the pod with the specified `podName` in the given `namespace` is in running state and returns the result.
func isPodRunning(ctx context.Context, c klient.Client, namespace, podName string) (bool, error) {
	var pod v1.Pod
	err := c.Resources().Get(ctx, podName, namespace, &pod)
	if err != nil {
		return false, fmt.Errorf("failed to get pod %s in namespace %s: %w", podName, namespace, err)
	}

	return pod.Status.Phase == v1.PodRunning, nil
}
