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

package helpers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/statemanager"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
)

const (
	WaitTimeout  = 10 * time.Minute
	WaitInterval = 10 * time.Second
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
// If the namespace already exists, it returns nil (idempotent operation).
func CreateNamespace(ctx context.Context, c klient.Client, name string) error {
	namespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	err := c.Resources().Create(ctx, namespace)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
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

/*
This function ensures that the given node follows the given label values in order for the dgxc.nvidia.com/nvsentinel-state
node label. We leverage this helper function to ensure that a node progresses through the following NVSentinel states:

- dgxc.nvidia.com/nvsentinel-state: quarantined
- dgxc.nvidia.com/nvsentinel-state: draining
- dgxc.nvidia.com/nvsentinel-state: drain-succeeded
- dgxc.nvidia.com/nvsentinel-state: remediating
- dgxc.nvidia.com/nvsentinel-state: remediation-succeeded
- label removed

TODO: this test currently tolerates starting the watch when the node has a label value that does not start with our
intended sequence rather than starting without the label value. This is required because there's a race condition
between the end of the TestScaleHealthEvents where a node may have the remediation-succeeded when the test completes.
This workaround can be removed after KACE-1703 is completed.
*/
func StartNodeLabelWatcher(ctx context.Context, t *testing.T, c klient.Client, nodeName string,
	labelValueSequence []string, success chan bool) error {
	currentLabelIndex := 0
	prevLabelValue := ""

	// Check the node's current label value before starting the watch to handle fast transitions
	node, err := GetNodeByName(ctx, c, nodeName)
	if err == nil {
		if currentValue, exists := node.Labels[statemanager.NVSentinelStateLabelKey]; exists {
			// Find where this value is in the expected sequence
			for i, expected := range labelValueSequence {
				if currentValue == expected {
					t.Logf("[LabelWatcher] Node %s already has label=%s (index %d), adjusting start position",
						nodeName, currentValue, i)
					currentLabelIndex = i + 1 // Start watching for the NEXT label
					prevLabelValue = currentValue
					break
				}
			}
		}
	}

	// Lock to prevent concurrent access to currentLabelIndex/prevLabelValue/foundInvalidSequence.
	// Note that sends to the success channel will be blocked on the test runner reading the result of this label sequence
	// in TestFatalHealthEventEndToEnd. The UpdateFunc thread which has acquired the lock will be waiting for the main
	// thead to read from the success channel. This is the desired behavior because we only want to have 1 UpdateFunc
	// thread write true/false.
	var lock sync.Mutex
	t.Logf("[LabelWatcher] Starting watcher for node %s, expecting sequence: %v (starting at index %d)",
		nodeName, labelValueSequence, currentLabelIndex)
	return c.Resources().Watch(&v1.NodeList{}, resources.WithFieldSelector(
		labels.FormatLabels(map[string]string{"metadata.name": nodeName}))).
		WithUpdateFunc(func(updated interface{}) {
			lock.Lock()
			defer lock.Unlock()
			node := updated.(*v1.Node)
			actualValue, exists := node.Labels[statemanager.NVSentinelStateLabelKey]
			if exists && currentLabelIndex < len(labelValueSequence) {
				expectedValue := labelValueSequence[currentLabelIndex]
				t.Logf("[LabelWatcher] Node %s update: %s=%s (progress: %d/%d, expected: %s, prev: %s)",
					nodeName, statemanager.NVSentinelStateLabelKey, actualValue,
					currentLabelIndex, len(labelValueSequence), expectedValue, prevLabelValue)
				if currentLabelIndex < len(labelValueSequence) && actualValue == labelValueSequence[currentLabelIndex] {
					t.Logf("[LabelWatcher] ✓ MATCHED expected label [%d]: %s", currentLabelIndex, actualValue)
					prevLabelValue = labelValueSequence[currentLabelIndex]
					currentLabelIndex++
					if currentLabelIndex == len(labelValueSequence) {
						t.Logf("[LabelWatcher] ✓ All %d labels matched! Waiting for label removal...", len(labelValueSequence))
					}
				} else if actualValue != labelValueSequence[currentLabelIndex] && prevLabelValue != actualValue {
					// If this is the first label we're seeing (currentLabelIndex == 0) and it doesn't match,
					// we missed early labels due to race condition. Check if this label appears later in sequence.
					if currentLabelIndex == 0 {
						foundLaterInSequence := false
						for i := 1; i < len(labelValueSequence); i++ {
							if actualValue == labelValueSequence[i] {
								foundLaterInSequence = true
								t.Logf("[LabelWatcher] ✗ MISSED early labels: First label received is '%s' (expected index %d), but expected to start with '%s' (index 0)",
									actualValue, i, labelValueSequence[0])
								break
							}
						}
						if !foundLaterInSequence {
							t.Logf("[LabelWatcher] ✗ UNEXPECTED first label: got '%s', not in expected sequence at all", actualValue)
						}
						t.Logf("[LabelWatcher] Sending FAILURE to channel (missed early labels)")
						sendNodeLabelResult(ctx, success, false)
					} else {
						// Not the first label, unexpected transition
						t.Logf("[LabelWatcher] ✗ UNEXPECTED label transition: got '%s', expected '%s' (prev: '%s')",
							actualValue, labelValueSequence[currentLabelIndex], prevLabelValue)
						t.Logf("[LabelWatcher] Sending FAILURE to channel")
						sendNodeLabelResult(ctx, success, false)
					}
				} else if actualValue == prevLabelValue {
					t.Logf("[LabelWatcher] Ignoring duplicate update for label: %s", actualValue)
				}
			} else {
				t.Logf("[LabelWatcher] Node %s update: %s label doesn't exist (progress: %d/%d)",
					nodeName, statemanager.NVSentinelStateLabelKey, currentLabelIndex, len(labelValueSequence))
				if currentLabelIndex == len(labelValueSequence) {
					t.Logf("[LabelWatcher] ✓ All labels observed and now removed. Sending SUCCESS to channel")
					sendNodeLabelResult(ctx, success, true)
				} else if currentLabelIndex != 0 {
					t.Logf("[LabelWatcher] ✗ Label removed prematurely (only saw %d/%d labels). Sending FAILURE to channel",
						currentLabelIndex, len(labelValueSequence))
					sendNodeLabelResult(ctx, success, false)
				} else {
					t.Logf("[LabelWatcher] Waiting for first label to appear...")
				}
			}
			// Do nothing if the label exists and the actualValue equals the previous label value, if the first label
			// observed doesn't match yet, or if we already found all the expected labels and we're waiting for the
			// label to be removed. Additionally, do nothing if the label doesn't exist and we still haven't seen the
			// label added.
		}).Start(ctx)
}

func sendNodeLabelResult(ctx context.Context, success chan bool, result bool) {
	select {
	case success <- result:
	case <-ctx.Done():
		return
	}
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

			if actualValue, exists := node.Labels[labelKey]; exists {
				if actualValue == expectedValue {
					actualCount++
				} else {
					t.Logf("Node %s has label %s=%s but expected %s", nodeName, labelKey, actualValue, expectedValue)
				}
			}
		}

		t.Logf("Nodes with label %s=%s: %d/%d", labelKey, expectedValue, actualCount, targetCount)
		return actualCount == targetCount
	}, WaitTimeout, WaitInterval, "all nodes should have label %s=%s", labelKey, expectedValue)
}

func WaitForNodeEvent(ctx context.Context, t *testing.T, c klient.Client, nodeName string, expectedEvent v1.Event) {
	require.Eventually(t, func() bool {
		fieldSelector := resources.WithFieldSelector(fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Node", nodeName))
		var eventsForNode v1.EventList
		err := c.Resources().List(ctx, &eventsForNode, fieldSelector)
		if err != nil {
			t.Logf("Got an error listing events for node %s: %s", nodeName, err)
			return false
		}
		for _, event := range eventsForNode.Items {
			if event.Type == expectedEvent.Type && event.Reason == expectedEvent.Reason {
				t.Logf("Matching event for node %s: %v", nodeName, event)
				return true
			}
		}
		t.Logf("Did not find any events for node %s matching event %v", nodeName, expectedEvent)
		return false
	}, WaitTimeout, WaitInterval, "node %s should have event %v", nodeName, expectedEvent)
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

func listAllRebootNodes(ctx context.Context, c klient.Client) (*unstructured.UnstructuredList, error) {
	rebootNodeList := &unstructured.UnstructuredList{}
	rebootNodeList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "janitor.dgxc.nvidia.com",
		Version: "v1alpha1",
		Kind:    "RebootNodeList",
	})
	err := c.Resources().List(ctx, rebootNodeList)
	if err != nil {
		return nil, fmt.Errorf("failed to list rebootnodes: %v", err)
	}
	return rebootNodeList, nil
}

// WaitForRebootNodeCR waits for a RebootNode custom resource to be created and completed for the node with the specified `nodeName` and returns the CR object.
func WaitForRebootNodeCR(ctx context.Context, t *testing.T, c klient.Client, nodeName string) *unstructured.Unstructured {
	var resultCR *unstructured.Unstructured

	require.Eventually(t, func() bool {
		rebootNodeList, err := listAllRebootNodes(ctx, c)
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
				completionTime, found, err := unstructured.NestedString(item.Object, "status", "completionTime")
				if err != nil || !found || completionTime == "" {
					t.Logf("RebootNode for node %s: waiting for completion", nodeName)
					return false
				}

				t.Logf("RebootNode for node %s completed", nodeName)
				resultCR = &item
				return true
			}
		}
		return false
	}, WaitTimeout, WaitInterval, "RebootNode CR should complete for node %s", nodeName)

	return resultCR
}

// DeleteAllRebootNodesCRs deletes all RebootNode custom resources in the cluster
func DeleteAllRebootNodeCRs(ctx context.Context, t *testing.T, c klient.Client) error {
	rebootNodeList, err := listAllRebootNodes(ctx, c)
	if err != nil {
		return fmt.Errorf("failed to list rebootnodes: %v", err)
	}
	for _, item := range rebootNodeList.Items {
		err = DeleteRebootNodeCR(ctx, c, &item)
		if err != nil {
			return fmt.Errorf("failed to delete reboot node: %v", err)
		}
	}
	return nil
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

				if drainStatus, exists := node.Labels[statemanager.NVSentinelStateLabelKey]; exists {
					// Accept nodes in draining state or already drain-succeeded state
					// (nodes that were already drained won't transition through draining again)
					if drainStatus == string(statemanager.DrainingLabelValue) ||
						drainStatus == string(statemanager.DrainSucceededLabelValue) {
						drainedCount++
					}
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

// GetPodsOnNode returns all pods running on the specified node using field selector for efficient filtering
func GetPodsOnNode(ctx context.Context, client *resources.Resources, nodeName string) ([]v1.Pod, error) {
	var podList v1.PodList
	err := client.List(ctx, &podList, resources.WithFieldSelector(fmt.Sprintf("spec.nodeName=%s", nodeName)))
	if err != nil {
		return nil, fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
	}

	return podList.Items, nil
}
