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
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v2"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
)

const (
	EventuallyWaitTimeout = 10 * time.Minute
	NeverWaitTimeout      = 30 * time.Second
	WaitInterval          = 5 * time.Second
	NVSentinelNamespace   = "nvsentinel"
)

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
	}, EventuallyWaitTimeout, WaitInterval, "nodes should have cordon state %v", shouldCordon)
}

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
	}, EventuallyWaitTimeout, WaitInterval, "namespace %s should be deleted", name)

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

	node, err := GetNodeByName(ctx, c, nodeName)
	if err == nil {
		if currentValue, exists := node.Labels[statemanager.NVSentinelStateLabelKey]; exists {
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

			if actualValue, exists := node.Labels[labelKey]; exists && actualValue == expectedValue {
				actualCount++
			}
		}

		t.Logf("Nodes with label %s=%s: %d/%d", labelKey, expectedValue, actualCount, targetCount)
		return actualCount == targetCount
	}, EventuallyWaitTimeout, WaitInterval, "all nodes should have label %s=%s", labelKey, expectedValue)
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
	}, EventuallyWaitTimeout, WaitInterval, "node %s should have event %v", nodeName, expectedEvent)
}

// SelectTestNodeFromUnusedPool selects an available test node from the cluster.
// Prefers uncordoned nodes but will fall back to the first node if none are available.
func SelectTestNodeFromUnusedPool(ctx context.Context, t *testing.T, client klient.Client) string {
	t.Log("Selecting an available uncordoned test node")
	nodes, err := GetAllNodesNames(ctx, client)
	require.NoError(t, err)
	require.NotEmpty(t, nodes, "no nodes found in cluster")

	// Try to find an uncordoned node
	for _, name := range nodes {
		node, err := GetNodeByName(ctx, client, name)
		if err != nil {
			continue
		}
		if !node.Spec.Unschedulable {
			t.Logf("Selected uncordoned node: %s", name)
			return name
		}
	}

	nodeName := nodes[0]
	t.Logf("No uncordoned node found, using first node: %s", nodeName)
	return nodeName
}

func GetNodeByName(ctx context.Context, c klient.Client, nodeName string) (*v1.Node, error) {
	var node v1.Node
	err := c.Resources().Get(ctx, nodeName, "", &node)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	return &node, nil
}

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

// WaitForNoRebootNodeCR asserts that no RebootNode CR is created for a node within a timeout period.
// Uses require.Never to continuously check that CR never appears.
func WaitForNoRebootNodeCR(ctx context.Context, t *testing.T, c klient.Client, nodeName string) {
	t.Helper()
	t.Logf("Asserting no RebootNode CR is created for node %s", nodeName)
	require.Never(t, func() bool {
		rebootNodeList, err := listAllRebootNodes(ctx, c)
		if err != nil {
			t.Logf("Error listing RebootNode CRs: %v", err)
			return false
		}

		for _, item := range rebootNodeList.Items {
			nodeNameInCR, found, err := unstructured.NestedString(item.Object, "spec", "nodeName")
			if err != nil || !found {
				continue
			}

			if nodeNameInCR == nodeName {
				t.Logf("RebootNode CR created for node %s (should not exist)!", nodeName)
				return true
			}
		}
		return false
	}, NeverWaitTimeout, WaitInterval, "RebootNode CR should not be created for node %s", nodeName)
}

func WaitForRebootNodeCR(ctx context.Context, t *testing.T, c klient.Client, nodeName string) *unstructured.Unstructured {
	var resultCR *unstructured.Unstructured

	t.Logf("Waiting for RebootNode CR to be created for node %s", nodeName)
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
		t.Logf("No RebootNode CR found for node %s", nodeName)
		return false
	}, EventuallyWaitTimeout, WaitInterval, "RebootNode CR should complete for node %s", nodeName)

	t.Logf("RebootNode CR created for node %s", nodeName)
	return resultCR
}

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

func DeleteRebootNodeCR(ctx context.Context, c klient.Client, rebootNode *unstructured.Unstructured) error {
	err := c.Resources().Delete(ctx, rebootNode)
	if err != nil {
		return fmt.Errorf("failed to delete RebootNode CR %s: %w", rebootNode.GetName(), err)
	}

	return nil
}

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

func waitForPodRunning(ctx context.Context, t *testing.T, c klient.Client, podName, namespace string) {
	require.Eventually(t, func() bool {
		isRunning, err := isPodRunning(ctx, c, namespace, podName)
		if err != nil {
			t.Logf("failed to check pod %s status: %v", podName, err)
			return false
		}
		return isRunning
	}, EventuallyWaitTimeout, WaitInterval, "pod %s should be running", podName)

}

func isPodRunning(ctx context.Context, c klient.Client, namespace, podName string) (bool, error) {
	var pod v1.Pod
	err := c.Resources().Get(ctx, podName, namespace, &pod)
	if err != nil {
		return false, fmt.Errorf("failed to get pod %s in namespace %s: %w", podName, namespace, err)
	}

	return pod.Status.Phase == v1.PodRunning, nil
}

func GetPodsOnNode(ctx context.Context, client *resources.Resources, nodeName string) ([]v1.Pod, error) {
	var podList v1.PodList
	err := client.List(ctx, &podList, resources.WithFieldSelector(fmt.Sprintf("spec.nodeName=%s", nodeName)))
	if err != nil {
		return nil, fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
	}

	return podList.Items, nil
}

// CreateRebootNodeCR creates a RebootNode custom resource for the specified node.
// Returns the created CR object and any error that occurred.
// If creation fails (e.g., webhook rejection), the error is returned for the caller to inspect.
func CreateRebootNodeCR(
	ctx context.Context,
	c klient.Client,
	nodeName string,
	crName string,
) (*unstructured.Unstructured, error) {
	rebootNode := &unstructured.Unstructured{}
	rebootNode.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "janitor.dgxc.nvidia.com",
		Version: "v1alpha1",
		Kind:    "RebootNode",
	})
	rebootNode.SetName(crName)

	err := unstructured.SetNestedField(rebootNode.Object, nodeName, "spec", "nodeName")
	if err != nil {
		return nil, fmt.Errorf("failed to set nodeName in spec: %w", err)
	}

	err = c.Resources().Create(ctx, rebootNode)
	if err != nil {
		return nil, err
	}

	return rebootNode, nil
}

func createConfigMapFromBytes(ctx context.Context, c klient.Client, yamlData []byte, name, namespace string) error {
	cm := &v1.ConfigMap{}
	err := yaml.Unmarshal(yamlData, cm)
	if err != nil {
		return fmt.Errorf("failed to unmarshal config map: %w", err)
	}

	if name != "" {
		cm.Name = name
	}

	if namespace != "" {
		cm.ObjectMeta.Namespace = namespace
	}

	cm.ObjectMeta.ResourceVersion = ""
	cm.ObjectMeta.UID = ""
	cm.ObjectMeta.Generation = 0
	cm.ObjectMeta.CreationTimestamp = metav1.Time{}
	cm.ObjectMeta.ManagedFields = nil

	existingCM := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cm.ObjectMeta.Name,
			Namespace: cm.ObjectMeta.Namespace,
		},
	}
	_ = c.Resources().Delete(ctx, existingCM)

	backoff := retry.DefaultBackoff
	backoff.Steps = 3
	err = retry.OnError(backoff, apierrors.IsAlreadyExists, func() error {
		createErr := c.Resources().Create(ctx, cm)
		if apierrors.IsAlreadyExists(createErr) {
			_ = c.Resources().Delete(ctx, existingCM)
		}
		return createErr
	})

	if err != nil {
		return fmt.Errorf("failed to create config map: %w", err)
	}

	return nil
}

func createConfigMapFromFilePath(ctx context.Context, c klient.Client, filePath, name, namespace string) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	return createConfigMapFromBytes(ctx, c, content, name, namespace)
}

func BackupConfigMap(ctx context.Context, c klient.Client, name, namespace string) ([]byte, error) {
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	err := c.Resources().Get(ctx, name, namespace, cm)
	if err != nil {
		return nil, fmt.Errorf("failed to get config map: %w", err)
	}

	yamlData, err := yaml.Marshal(cm)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config map to yaml: %w", err)
	}

	return yamlData, nil
}

func UpdateConfigMapTOMLField[T any](ctx context.Context, c klient.Client, name, namespace, tomlKey string,
	modifier func(*T) error) error {

	cm := &v1.ConfigMap{}
	err := c.Resources().Get(ctx, name, namespace, cm)
	if err != nil {
		return fmt.Errorf("failed to get configmap %s/%s: %w", namespace, name, err)
	}

	if cm.Data == nil {
		return fmt.Errorf("configmap %s/%s has no data", namespace, name)
	}

	tomlData, exists := cm.Data[tomlKey]
	if !exists {
		return fmt.Errorf("configmap %s/%s missing key %s", namespace, name, tomlKey)
	}

	var cfg T
	if err := toml.Unmarshal([]byte(tomlData), &cfg); err != nil {
		return fmt.Errorf("failed to unmarshal TOML: %w", err)
	}

	if err := modifier(&cfg); err != nil {
		return fmt.Errorf("modifier failed: %w", err)
	}

	var buf strings.Builder
	encoder := toml.NewEncoder(&buf)
	if err := encoder.Encode(&cfg); err != nil {
		return fmt.Errorf("failed to marshal TOML: %w", err)
	}

	cm.Data[tomlKey] = buf.String()
	if err := c.Resources().Update(ctx, cm); err != nil {
		return fmt.Errorf("failed to update configmap %s/%s: %w", namespace, name, err)
	}

	return nil
}

func WaitForDeploymentRollout(ctx context.Context, t *testing.T, c klient.Client, name, namespace string) {
	t.Logf("Waiting for rollout to complete for deployment %s/%s", namespace, name)

	require.Eventually(t, func() bool {
		deployment := &appsv1.Deployment{}
		if err := c.Resources().Get(ctx, name, namespace, deployment); err != nil {
			t.Logf("Error getting deployment: %v", err)
			return false
		}

		// Check all conditions for a successful rollout (same as kubectl rollout status)
		if deployment.Spec.Replicas == nil {
			t.Logf("Deployment spec replicas is nil")
			return false
		}

		expectedReplicas := *deployment.Spec.Replicas

		if deployment.Status.UpdatedReplicas != expectedReplicas {
			t.Logf("Waiting: UpdatedReplicas=%d, Expected=%d", deployment.Status.UpdatedReplicas, expectedReplicas)
			return false
		}

		if deployment.Status.ReadyReplicas != expectedReplicas {
			t.Logf("Waiting: ReadyReplicas=%d, Expected=%d", deployment.Status.ReadyReplicas, expectedReplicas)
			return false
		}

		if deployment.Status.AvailableReplicas != expectedReplicas {
			t.Logf("Waiting: AvailableReplicas=%d, Expected=%d", deployment.Status.AvailableReplicas, expectedReplicas)
			return false
		}

		if deployment.Status.ObservedGeneration < deployment.Generation {
			t.Logf("Waiting: ObservedGeneration=%d, Generation=%d", deployment.Status.ObservedGeneration, deployment.Generation)
			return false
		}

		// Find the current ReplicaSet (highest revision) to verify we're checking the right pods
		rsList := &appsv1.ReplicaSetList{}
		rsLabelSelector := ""
		rsLabels := []string{}
		for k, v := range deployment.Spec.Selector.MatchLabels {
			rsLabels = append(rsLabels, fmt.Sprintf("%s=%s", k, v))
		}
		rsLabelSelector = strings.Join(rsLabels, ",")

		if err := c.Resources(namespace).List(ctx, rsList, resources.WithLabelSelector(rsLabelSelector)); err != nil {
			t.Logf("Error listing ReplicaSets: %v", err)
			return false
		}

		// Find the ReplicaSet with highest revision (current one)
		var currentRS *appsv1.ReplicaSet
		highestRevision := int64(-1)
		for i := range rsList.Items {
			rs := &rsList.Items[i]
			if rs.Annotations == nil {
				continue
			}
			revisionStr := rs.Annotations["deployment.kubernetes.io/revision"]
			if revisionStr == "" {
				continue
			}
			revision := int64(0)
			fmt.Sscanf(revisionStr, "%d", &revision)
			if revision > highestRevision {
				highestRevision = revision
				currentRS = rs
			}
		}

		if currentRS == nil {
			t.Logf("Could not find current ReplicaSet")
			return false
		}

		currentPodTemplateHash := currentRS.Labels["pod-template-hash"]
		t.Logf("Current ReplicaSet: %s (revision: %d, hash: %s)", currentRS.Name, highestRevision, currentPodTemplateHash)

		// Now verify at least one pod from the current ReplicaSet is ready
		labelSelector := ""
		labels := []string{}
		for k, v := range deployment.Spec.Selector.MatchLabels {
			labels = append(labels, fmt.Sprintf("%s=%s", k, v))
		}
		labelSelector = strings.Join(labels, ",")

		pods := &v1.PodList{}
		if err := c.Resources(namespace).List(ctx, pods, resources.WithLabelSelector(labelSelector)); err != nil {
			t.Logf("Error listing pods: %v", err)
			return false
		}

		readyPodFound := false
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil {
				continue
			}

			podHash := pod.Labels["pod-template-hash"]
			if podHash != currentPodTemplateHash {
				t.Logf("Skipping pod %s from old ReplicaSet (hash: %s)", pod.Name, podHash)
				continue
			}

			if pod.Status.Phase == v1.PodRunning && IsPodReady(pod) {
				t.Logf("Found ready pod from current ReplicaSet: %s", pod.Name)
				readyPodFound = true
				break
			}
		}

		if !readyPodFound {
			return false
		}

		t.Logf("Rollout complete: all %d replicas are updated, ready, and available", expectedReplicas)
		return true
	}, EventuallyWaitTimeout, WaitInterval, "deployment %s/%s rollout should complete", namespace, name)

	t.Logf("Deployment %s/%s rollout completed successfully", namespace, name)
}

// RestartDeployment triggers a rolling restart of the specified deployment by updating
// the restartedAt annotation on the pod template, then waits for the rollout to complete.
// Uses retry.RetryOnConflict for automatic retry handling with exponential backoff.
func RestartDeployment(ctx context.Context, t *testing.T, c klient.Client, name, namespace string) error {
	t.Logf("Triggering rollout restart for deployment %s/%s", namespace, name)

	restartTime := time.Now().Format(time.RFC3339)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if err := c.Resources().Get(ctx, name, namespace, deployment); err != nil {
			return fmt.Errorf("failed to get deployment %s/%s: %w", namespace, name, err)
		}

		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = make(map[string]string)
		}
		deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = restartTime

		if err := c.Resources().Update(ctx, deployment); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to trigger rollout restart: %w", err)
	}

	WaitForDeploymentRollout(ctx, t, c, name, namespace)
	return nil
}

// CheckNodeConditionExists checks if a node has a specific condition type and reason.
// Returns:
//   - (*v1.NodeCondition, nil) if the node exists and has the specified condition
//   - (nil, nil) if the node exists but doesn't have the specified condition
//   - (nil, error) if the node cannot be retrieved
func CheckNodeConditionExists(ctx context.Context, c klient.Client, nodeName, conditionType, reason string) (*v1.NodeCondition, error) {
	node, err := GetNodeByName(ctx, c, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	targetConditionType := v1.NodeConditionType(conditionType)
	for _, condition := range node.Status.Conditions {
		if condition.Type == targetConditionType && condition.Reason == reason {
			slog.Info("Found condition", "node", nodeName, "condition", condition)
			return &condition, nil
		}
	}
	return nil, nil
}

// GetNodeEvents retrieves all events for a node in the default namespace
// - eventType: if empty, all event types are returned
func GetNodeEvents(ctx context.Context, c klient.Client, nodeName string, eventType string) (*v1.EventList, error) {
	eventList := &v1.EventList{}

	fieldSelector := fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Node", nodeName)
	if eventType != "" {
		fieldSelector = fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Node,type=%s", nodeName, eventType)
	}

	err := c.Resources().WithNamespace("default").List(ctx, eventList,
		resources.WithFieldSelector(fieldSelector))
	if err != nil {
		return nil, err
	}

	return eventList, nil
}

// CheckNodeEventExists checks if an event exists for a node with specific type and optional reason/time filters.
// Node events are queried from the default namespace with field selectors for efficient filtering.
// - eventReason: if empty, reason is not checked
// - afterTime: if zero, time is not checked
func CheckNodeEventExists(ctx context.Context, c klient.Client, nodeName string, eventType, eventReason string, afterTime ...time.Time) (bool, *v1.Event) {
	eventList, err := GetNodeEvents(ctx, c, nodeName, eventType)
	if err != nil {
		return false, nil
	}

	var timeFilter time.Time
	if len(afterTime) > 0 {
		timeFilter = afterTime[0]
	}

	for _, event := range eventList.Items {
		if eventReason != "" && event.Reason != eventReason {
			continue
		}

		if !timeFilter.IsZero() {
			if !event.FirstTimestamp.After(timeFilter) && !event.LastTimestamp.After(timeFilter) {
				continue
			}
		}

		return true, &event
	}
	return false, nil
}

func PatchServicePort(ctx context.Context, c klient.Client, namespace, serviceName string, targetPort int) error {
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
		},
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Resources().Get(ctx, serviceName, namespace, svc); err != nil {
			return fmt.Errorf("failed to get service %s/%s: %w", namespace, serviceName, err)
		}

		if len(svc.Spec.Ports) == 0 {
			return fmt.Errorf("service %s/%s has no ports", namespace, serviceName)
		}

		svc.Spec.Ports[0].Port = int32(targetPort)
		svc.Spec.Ports[0].TargetPort = intstr.FromInt(targetPort)

		if err := c.Resources().Update(ctx, svc); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to patch service port: %w", err)
	}

	return nil
}

// SetNodeManagedByNVSentinel sets the ManagedByNVSentinel label on a node
func SetNodeManagedByNVSentinel(ctx context.Context, c klient.Client, nodeName string, managed bool) error {
	node, err := GetNodeByName(ctx, c, nodeName)
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}

	if managed {
		node.Labels["k8saas.nvidia.com/ManagedByNVSentinel"] = "true"
	} else {
		node.Labels["k8saas.nvidia.com/ManagedByNVSentinel"] = "false"
	}

	err = c.Resources().Update(ctx, node)
	if err != nil {
		return fmt.Errorf("failed to update node labels: %w", err)
	}

	return nil
}

// RemoveNodeManagedByNVSentinelLabel removes the ManagedByNVSentinel label from a node
func RemoveNodeManagedByNVSentinelLabel(ctx context.Context, c klient.Client, nodeName string) error {
	node, err := GetNodeByName(ctx, c, nodeName)
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	if node.Labels != nil {
		delete(node.Labels, "k8saas.nvidia.com/ManagedByNVSentinel")
		err = c.Resources().Update(ctx, node)
		if err != nil {
			return fmt.Errorf("failed to update node labels: %w", err)
		}
	}

	return nil
}

// GetPodOnWorkerNode returns a running pod matching the given name pattern on a real worker node
func GetPodOnWorkerNode(ctx context.Context, t *testing.T, client klient.Client, namespace, podNamePattern string) (*v1.Pod, error) {
	t.Helper()

	pods := &v1.PodList{}
	err := client.Resources().List(ctx, pods, func(opts *metav1.ListOptions) {
		opts.FieldSelector = fmt.Sprintf("metadata.namespace=%s", namespace)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
	}

	for _, pod := range pods.Items {
		if !regexp.MustCompile(podNamePattern).MatchString(pod.Name) {
			continue
		}

		if pod.Status.Phase != v1.PodRunning {
			continue
		}

		if regexp.MustCompile("worker").MatchString(pod.Spec.NodeName) {
			t.Logf("Found pod %s on worker node %s", pod.Name, pod.Spec.NodeName)
			return &pod, nil
		}
	}

	return nil, fmt.Errorf("no running pod matching pattern '%s' found on real worker nodes", podNamePattern)
}

// WaitForNodeLabel waits for a node to have a specific label value
func WaitForNodeLabel(ctx context.Context, t *testing.T, client klient.Client, nodeName, labelKey, expectedValue string) {
	t.Helper()
	t.Logf("Waiting for node %s to have label %s=%s", nodeName, labelKey, expectedValue)
	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return false
		}
		if node.Labels == nil {
			return false
		}
		value, exists := node.Labels[labelKey]
		if !exists {
			return false
		}
		return value == expectedValue
	}, EventuallyWaitTimeout, WaitInterval)
	t.Logf("Node %s has label %s=%s", nodeName, labelKey, expectedValue)
}

func AssertPodsNeverDeleted(ctx context.Context, t *testing.T, client klient.Client, namespace string, podNames []string) {
	t.Helper()
	t.Logf("Asserting %d pods in namespace %s are never deleted", len(podNames), namespace)
	require.Never(t, func() bool {
		for _, podName := range podNames {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, namespace, pod)
			if err != nil {
				t.Logf("Pod %s was deleted unexpectedly", podName)
				return true
			}
		}
		return false
	}, NeverWaitTimeout, WaitInterval, "pods should not be deleted")
	t.Logf("All %d pods remain running in namespace %s", len(podNames), namespace)
}

func WaitForPodsDeleted(ctx context.Context, t *testing.T, client klient.Client, namespace string, podNames []string) {
	t.Helper()
	t.Logf("Waiting for %d pods to be deleted from namespace %s", len(podNames), namespace)
	require.Eventually(t, func() bool {
		for _, podName := range podNames {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, namespace, pod)
			if err == nil {
				t.Logf("Pod %s still exists", podName)
				return false
			}
		}
		return true
	}, EventuallyWaitTimeout, WaitInterval)
	t.Logf("All pods deleted from namespace %s", namespace)
}

func WaitForPodsRunning(ctx context.Context, t *testing.T, client klient.Client, namespace string, podNames []string) {
	t.Helper()
	t.Logf("Waiting for %d pods to be running in namespace %s", len(podNames), namespace)
	for _, podName := range podNames {
		require.Eventually(t, func() bool {
			pod := &v1.Pod{}
			err := client.Resources().Get(ctx, podName, namespace, pod)
			if err != nil {
				return false
			}
			return pod.Status.Phase == v1.PodRunning
		}, EventuallyWaitTimeout, WaitInterval)
	}
	t.Logf("All %d pods running", len(podNames))
}

func DeletePodsByNames(ctx context.Context, t *testing.T, client klient.Client, namespace string, podNames []string) {
	t.Helper()
	t.Logf("Deleting %d pods from namespace %s", len(podNames), namespace)
	for _, podName := range podNames {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: namespace,
			},
		}
		err := client.Resources().Delete(ctx, pod)
		require.NoError(t, err, "failed to delete pod %s", podName)
	}
}

// ExecInPod executes a command in a pod and returns stdout, stderr
func ExecInPod(ctx context.Context, restConfig *rest.Config, namespace, podName, containerName string, command []string) (string, string, error) {
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", "", fmt.Errorf("failed to create clientset: %w", err)
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&v1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("failed to create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})

	return stdout.String(), stderr.String(), err
}

func IsPodReady(pod v1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodReady {
			return condition.Status == v1.ConditionTrue
		}
	}
	return false
}

// PortForwardPod sets up port forwarding to a pod and returns channels to control it.
// Returns (stopChan, readyChan) where:
// - stopChan: close this to stop port forwarding
// - readyChan: will be closed when port-forward is ready
func PortForwardPod(ctx context.Context, restConfig *rest.Config, namespace, podName string, localPort, podPort int) (chan struct{}, chan struct{}) {
	stopChan := make(chan struct{}, 1)
	readyChan := make(chan struct{})

	go func() {
		clientset, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			close(readyChan)
			return
		}

		req := clientset.CoreV1().RESTClient().Post().
			Resource("pods").
			Namespace(namespace).
			Name(podName).
			SubResource("portforward")

		transport, upgrader, err := spdy.RoundTripperFor(restConfig)
		if err != nil {
			close(readyChan)
			return
		}

		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", req.URL())

		ports := []string{fmt.Sprintf("%d:%d", localPort, podPort)}

		devNull := &bytes.Buffer{}

		fw, err := portforward.New(dialer, ports, stopChan, readyChan, devNull, devNull)
		if err != nil {
			close(readyChan)
			return
		}

		if err := fw.ForwardPorts(); err != nil {
			slog.Error("Error forwarding ports", "error", err)
			return
		}
	}()

	return stopChan, readyChan
}
