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
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v2"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	kwokv1alpha1 "sigs.k8s.io/kwok/pkg/apis/v1alpha1"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
)

const (
	EventuallyWaitTimeout = 10 * time.Minute
	NeverWaitTimeout      = 10 * time.Second
	WaitInterval          = 5 * time.Second
	NVSentinelNamespace   = "nvsentinel"
)

var (
	RebootNodeGVK = schema.GroupVersionKind{
		Group:   "janitor.dgxc.nvidia.com",
		Version: "v1alpha1",
		Kind:    "RebootNode",
	}
	GPUResetGVK = schema.GroupVersionKind{
		Group:   "janitor.dgxc.nvidia.com",
		Version: "v1alpha1",
		Kind:    "GPUReset",
	}
)

func WaitForNodesCordonState(
	ctx context.Context, t *testing.T, c klient.Client, nodeNames []string, shouldCordon bool,
) {
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
This function ensures that the given node follows the given label values in order for the
dgxc.nvidia.com/nvsentinel-state
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
//nolint:cyclop,gocognit // Test helper with complex state machine logic
func StartNodeLabelWatcher(ctx context.Context, t *testing.T, c klient.Client, nodeName string,
	labelValueSequence []string, waitForLabelRemoval bool, success chan bool) error {
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
			if exists && currentLabelIndex < len(labelValueSequence) { //nolint:nestif // Complex label state tracking logic
				expectedValue := labelValueSequence[currentLabelIndex]
				t.Logf("[LabelWatcher] Node %s update: %s=%s (progress: %d/%d, expected: %s, prev: %s)",
					nodeName, statemanager.NVSentinelStateLabelKey, actualValue,
					currentLabelIndex, len(labelValueSequence), expectedValue, prevLabelValue)
				//nolint:gocritic // Complex boolean logic appropriate for state transitions
				if currentLabelIndex < len(labelValueSequence) && actualValue == labelValueSequence[currentLabelIndex] {
					t.Logf("[LabelWatcher] ✓ MATCHED expected label [%d]: %s", currentLabelIndex, actualValue)
					prevLabelValue = labelValueSequence[currentLabelIndex]

					currentLabelIndex++
					if currentLabelIndex == len(labelValueSequence) {
						if !waitForLabelRemoval {
							t.Logf("[LabelWatcher] ✓ All labels observed, not waiting for removal. Sending SUCCESS to channel")
							sendNodeLabelResult(ctx, success, true)
						}

						t.Logf("[LabelWatcher] ✓ All %d labels matched! Waiting for label removal...", len(labelValueSequence))
					}
				} else if actualValue != labelValueSequence[currentLabelIndex] && prevLabelValue != actualValue {
					// Check if the actual label appears later in the sequence (skipped intermediate states)
					// This can happen with fast state transitions, especially with PostgreSQL LISTEN/NOTIFY
					foundLaterInSequence := false
					skippedIndex := -1

					for i := currentLabelIndex + 1; i < len(labelValueSequence); i++ {
						if actualValue == labelValueSequence[i] {
							foundLaterInSequence = true
							skippedIndex = i

							break
						}
					}

					if foundLaterInSequence {
						// We skipped intermediate states - log warning but continue
						skippedLabels := labelValueSequence[currentLabelIndex:skippedIndex]
						t.Logf(
							"[LabelWatcher] ⚠ SKIPPED intermediate labels: %v (jumped from '%s' to '%s')",
							skippedLabels, prevLabelValue, actualValue,
						)
						t.Logf("[LabelWatcher] ⚠ This is expected with fast state transitions (PostgreSQL LISTEN/NOTIFY)")

						// Update state to the current label
						prevLabelValue = actualValue
						currentLabelIndex = skippedIndex + 1

						if currentLabelIndex == len(labelValueSequence) {
							t.Logf("[LabelWatcher] ✓ Reached final state despite skipped labels! Waiting for label removal...")
						}
					} else if currentLabelIndex == 0 {
						// First label doesn't match and isn't in sequence - missed early labels
						t.Logf(
							"[LabelWatcher] ✗ MISSED early labels: First label received is '%s', "+
								"but expected to start with '%s' (index 0)",
							actualValue, labelValueSequence[0],
						)
						t.Logf("[LabelWatcher] Sending FAILURE to channel (missed early labels)")
						sendNodeLabelResult(ctx, success, false)
					} else {
						// Completely unexpected transition (not in sequence at all)
						t.Logf("[LabelWatcher] ✗ UNEXPECTED label transition: got '%s', expected '%s' (prev: '%s')",
							actualValue, labelValueSequence[currentLabelIndex], prevLabelValue)
						t.Logf("[LabelWatcher] ✗ Label '%s' is not in expected sequence", actualValue)
						t.Logf("[LabelWatcher] Sending FAILURE to channel")
						sendNodeLabelResult(ctx, success, false)
					}
				} else if actualValue == prevLabelValue {
					t.Logf("[LabelWatcher] Ignoring duplicate update for label: %s", actualValue)
				}
			} else {
				t.Logf("[LabelWatcher] Node %s update: %s label doesn't exist (progress: %d/%d)",
					nodeName, statemanager.NVSentinelStateLabelKey, currentLabelIndex, len(labelValueSequence))

				switch {
				case currentLabelIndex == len(labelValueSequence):
					t.Logf("[LabelWatcher] ✓ All labels observed and now removed. Sending SUCCESS to channel")
					sendNodeLabelResult(ctx, success, true)
				case currentLabelIndex != 0:
					t.Logf("[LabelWatcher] ✗ Label removed prematurely (only saw %d/%d labels). Sending FAILURE to channel",
						currentLabelIndex, len(labelValueSequence))
					sendNodeLabelResult(ctx, success, false)
				default:
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

// WaitForNodesWithLabel waits for nodes with names specified in `nodeNames` to have a label
// with key `labelKey` set to `expectedValue`.
func WaitForNodesWithLabel(
	ctx context.Context, t *testing.T, c klient.Client, nodeNames []string, labelKey, expectedValue string,
) {
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

// WaitForNodeLabelNotEqual waits for a single node's label to NOT equal a specific value.
// This is useful for waiting for state transitions when you don't know the final state.
func WaitForNodeLabelNotEqual(
	ctx context.Context, t *testing.T, c klient.Client, nodeName, labelKey, notExpectedValue string,
) {
	t.Helper()

	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, c, nodeName)
		if err != nil {
			t.Logf("failed to get node %s: %v", nodeName, err)
			return false
		}

		actualValue, exists := node.Labels[labelKey]
		if !exists {
			t.Logf("Node %s: label %s does not exist", nodeName, labelKey)
			return false
		}

		if actualValue != notExpectedValue {
			t.Logf("Node %s: label %s=%s (not %s) ✓", nodeName, labelKey, actualValue, notExpectedValue)
			return true
		}

		t.Logf("Node %s: label %s still equals %s (waiting for change)", nodeName, labelKey, actualValue)

		return false
	}, EventuallyWaitTimeout, WaitInterval,
		"expected node %s label %s to not equal %s", nodeName, labelKey, notExpectedValue)
}

func WaitForNodeEvent(ctx context.Context, t *testing.T, c klient.Client, nodeName string,
	expectedEvent v1.Event) {
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
				if expectedEvent.Message != "" {
					t.Logf("Matching message for event %v", expectedEvent.Type)
					t.Logf("Event message: %s", event.Message)
					t.Logf("Expected message: %s", expectedEvent.Message)

					if event.Message != expectedEvent.Message {
						t.Logf("Event message does not match expected message: %s != %s", event.Message, expectedEvent.Message)
						continue
					}
				}

				t.Logf("Matching event for node %s: %v", nodeName, event)

				return true
			}
		}

		t.Logf("Did not find any events for node %s matching event %v", nodeName, expectedEvent)

		return false
	}, EventuallyWaitTimeout, WaitInterval, "node %s should have event %v", nodeName, expectedEvent)
}

func EnsureNodeEventNotPresent(ctx context.Context, t *testing.T,
	c klient.Client, nodeName string, eventType, eventReason string) {
	t.Helper()

	require.Never(t, func() bool {
		events, err := GetNodeEvents(ctx, c, nodeName, eventType)
		if err != nil {
			t.Logf("failed to get events for node %s: %v", nodeName, err)
			return false
		}

		for _, event := range events.Items {
			if event.Type == eventType && event.Reason == eventReason {
				t.Logf("node %s has event %v", nodeName, event)
				return true
			}
		}

		t.Logf("node %s does not have event %v", nodeName, eventType)

		return false
	}, NeverWaitTimeout, WaitInterval, "node %s should not have event %v", nodeName, eventType)
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

// SelectTestNodeWithEmptyProviderID selects a test node with empty providerID.
// This is required for CSP monitor tests because Kubernetes doesn't allow changing providerID once set.
func SelectTestNodeWithEmptyProviderID(ctx context.Context, t *testing.T, client klient.Client) string {
	t.Log("Selecting an available uncordoned test node with empty providerID")

	nodes, err := GetAllNodesNames(ctx, client)
	require.NoError(t, err)
	require.NotEmpty(t, nodes, "no nodes found in cluster")

	for _, name := range nodes {
		node, err := GetNodeByName(ctx, client, name)
		if err != nil {
			continue
		}

		if !node.Spec.Unschedulable && node.Spec.ProviderID == "" {
			t.Logf("Selected uncordoned node with empty providerID: %s", name)
			return name
		}
	}

	require.Fail(t,
		"no uncordoned node with empty providerID found - CSP monitor tests require nodes without providerID set")

	return ""
}

func GetNodeByName(ctx context.Context, c klient.Client, nodeName string) (*v1.Node, error) {
	var node v1.Node

	err := c.Resources().Get(ctx, nodeName, "", &node)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	return &node, nil
}

func DeletePod(ctx context.Context, t *testing.T, c klient.Client, namespace, podName string,
	waitForRemoval bool) error {
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

	if waitForRemoval {
		require.Eventually(t, func() bool {
			err = c.Resources().Get(ctx, podName, namespace, pod)
			if err != nil {
				return apierrors.IsNotFound(err)
			}

			return false
		}, EventuallyWaitTimeout, WaitInterval, "pod %s should be removed from API", podName)
	}

	return nil
}

func ListAllCRs(ctx context.Context, c klient.Client,
	groupVersionKind schema.GroupVersionKind) (*unstructured.UnstructuredList, error) {
	crList := &unstructured.UnstructuredList{}
	crList.SetGroupVersionKind(groupVersionKind)

	err := c.Resources().List(ctx, crList)
	if err != nil {
		return nil, fmt.Errorf("failed to list custom resources: %w", err)
	}

	return crList, nil
}

// WaitForNoCR asserts that no CR is created for a node within a timeout period.
// Uses require.Never to continuously check that CR never appears.
func WaitForNoCR(ctx context.Context, t *testing.T, c klient.Client, nodeName string,
	groupVersionKind schema.GroupVersionKind) {
	t.Helper()
	t.Logf("Asserting no CR is created for node %s", nodeName)
	require.Never(t, func() bool {
		crList, err := ListAllCRs(ctx, c, groupVersionKind)
		if err != nil {
			t.Logf("Error listing CRs: %v", err)
			return false
		}

		for _, item := range crList.Items {
			nodeNameInCR, found, err := unstructured.NestedString(item.Object, "spec", "nodeName")
			if err != nil || !found {
				continue
			}

			if nodeNameInCR == nodeName {
				t.Logf("CR created for node %s (should not exist)!", nodeName)
				return true
			}
		}

		return false
	}, NeverWaitTimeout, WaitInterval,
		"CR should not be created for node %s", nodeName)
}

func WaitForCR(ctx context.Context, t *testing.T, c klient.Client, nodeName string,
	groupVersionKind schema.GroupVersionKind) *unstructured.Unstructured {
	t.Helper()

	var resultCR *unstructured.Unstructured

	require.Eventually(t, func() bool {
		crList, err := ListAllCRs(ctx, c, groupVersionKind)
		if err != nil {
			t.Logf("failed to list CRs: %v", err)
			return false
		}

		for i := range crList.Items {
			item := &crList.Items[i]

			nodeNameInCR, found, err := unstructured.NestedString(item.Object, "spec", "nodeName")
			if err != nil || !found || nodeNameInCR != nodeName {
				continue
			}

			// Found the CR for this node
			completionTime, found, err := unstructured.NestedString(item.Object, "status", "completionTime")
			if err != nil || !found || completionTime == "" {
				t.Logf("CR for node %s: waiting for completion %s", nodeName, item.GetName())

				return false
			}

			t.Logf("CR for node %s completed at %s", nodeName, completionTime)

			resultCR = item

			return true
		}

		t.Logf("No CR found for node %s yet", nodeName)

		return false
	}, EventuallyWaitTimeout, WaitInterval,
		"CR should be created and complete for node %s", nodeName)

	return resultCR
}

func DeleteAllCRs(ctx context.Context, t *testing.T, c klient.Client, groupVersionKind schema.GroupVersionKind) error {
	crList, err := ListAllCRs(ctx, c, groupVersionKind)
	if err != nil {
		return fmt.Errorf("failed to list CRs: %w", err)
	}

	for _, item := range crList.Items {
		err = DeleteCR(ctx, c, &item)
		if err != nil {
			return fmt.Errorf("failed to delete CR: %w", err)
		}
	}

	return nil
}

func DeleteCR(ctx context.Context, c klient.Client, cr *unstructured.Unstructured) error {
	err := c.Resources().Delete(ctx, cr)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("failed to delete CR %s: %w", cr.GetName(), err)
	}

	return nil
}

func GetStartAndCompletionTimes(maintenanceCR *unstructured.Unstructured) (*time.Time, *time.Time, error) {
	startTimeStr, found, err := unstructured.NestedString(maintenanceCR.Object, "status", "startTime")
	if err != nil || !found {
		return nil, nil, fmt.Errorf("failed to get startTime for maintenance CR: %s", maintenanceCR.GetName())
	}

	completionTimeStr, found, err := unstructured.NestedString(maintenanceCR.Object, "status", "completionTime")
	if err != nil || !found {
		return nil, nil, fmt.Errorf("failed to get completionTime for maintenance CR: %s", maintenanceCR.GetName())
	}

	startTime, err := time.Parse(time.RFC3339, startTimeStr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse start time for maintenance CR %s: %w",
			maintenanceCR.GetName(), err)
	}

	completionTime, err := time.Parse(time.RFC3339, completionTimeStr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse completionTime for maintenance CR %s: %w",
			maintenanceCR.GetName(), err)
	}

	return &startTime, &completionTime, nil
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

// CountSchedulableNodes returns the count of nodes that are schedulable (not cordoned).
// It accepts a v1.NodeList and counts only nodes where Spec.Unschedulable is false.
func CountSchedulableNodes(nodeList v1.NodeList) int {
	count := 0

	for _, node := range nodeList.Items {
		if !node.Spec.Unschedulable {
			count++
		}
	}

	return count
}

// GetRealNodeName returns a real (non-KWOK) worker node name from the cluster.
// Prefers schedulable workers, falls back to unschedulable workers if needed.
func GetRealNodeName(ctx context.Context, c klient.Client) (string, error) {
	var nodeList v1.NodeList

	err := c.Resources().List(ctx, &nodeList,
		resources.WithLabelSelector("type!=kwok,!node-role.kubernetes.io/control-plane,!node-role.kubernetes.io/agent"))
	if err != nil {
		return "", fmt.Errorf("failed to list nodes: %w", err)
	}

	if len(nodeList.Items) == 0 {
		return "", fmt.Errorf("no real worker nodes found in cluster")
	}

	for _, node := range nodeList.Items {
		if !node.Spec.Unschedulable {
			return node.Name, nil
		}
	}

	return nodeList.Items[0].Name, nil
}
func CreatePodsAndWaitTillRunning(
	ctx context.Context, t *testing.T, c klient.Client, nodeNames []string, podTemplate *v1.Pod,
) {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

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

// DrainRunningPodsInNamespace finds all running pods in the specified `namespace`
// and deletes them to simulate node draining.
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

			err = DeletePod(ctx, t, c, namespace, pod.Name, false)
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
					Args:    []string{"sleep 3600"},
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

func waitForPodRunning(
	ctx context.Context, t *testing.T, c klient.Client, podName, namespace string,
) {
	require.Eventually(t, func() bool {
		isRunning, err := isPodRunning(ctx, c, namespace, podName)
		if err != nil {
			t.Logf("failed to check pod %s status: %v", podName, err)
			return false
		}

		return isRunning
	}, EventuallyWaitTimeout, WaitInterval, "pod %s should be running", podName)
}

func isPodRunning(
	ctx context.Context, c klient.Client, namespace, podName string,
) (bool, error) {
	var pod v1.Pod

	err := c.Resources().Get(ctx, podName, namespace, &pod)
	if err != nil {
		return false, fmt.Errorf("failed to get pod %s in namespace %s: %w", podName, namespace, err)
	}

	return pod.Status.Phase == v1.PodRunning, nil
}

func GetPodsOnNode(
	ctx context.Context, client *resources.Resources, nodeName string,
) ([]v1.Pod, error) {
	var podList v1.PodList

	err := client.List(ctx, &podList,
		resources.WithFieldSelector(fmt.Sprintf("spec.nodeName=%s", nodeName)))
	if err != nil {
		return nil, fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
	}

	return podList.Items, nil
}

// CreateRebootNodeCR creates a RebootNode custom resource for the specified node.
// Returns the created CR object and any error that occurred.
// If creation fails (e.g., webhook rejection), the error is returned for the caller to inspect.
func CreateRebootNodeCR(ctx context.Context, c klient.Client, nodeName string,
	crName string) (*unstructured.Unstructured, error) {
	rebootNode := &unstructured.Unstructured{}
	rebootNode.SetGroupVersionKind(RebootNodeGVK)
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

func CreateGPUResetCR(ctx context.Context, c klient.Client, nodeName string, crName string,
	uuid string) (*unstructured.Unstructured, error) {
	gpuReset := &unstructured.Unstructured{}
	gpuReset.SetGroupVersionKind(GPUResetGVK)
	gpuReset.SetName(crName)

	err := unstructured.SetNestedField(gpuReset.Object, nodeName, "spec", "nodeName")
	if err != nil {
		return nil, fmt.Errorf("failed to set nodeName in spec: %w", err)
	}

	err = unstructured.SetNestedSlice(gpuReset.Object, []interface{}{uuid}, "spec", "selector", "uuids")
	if err != nil {
		return nil, fmt.Errorf("failed to set selector.uuids in spec: %w", err)
	}

	err = c.Resources().Create(ctx, gpuReset)
	if err != nil {
		return nil, err
	}

	return gpuReset, nil
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
		cm.Namespace = namespace
	}

	cm.ResourceVersion = ""
	cm.UID = ""
	cm.Generation = 0
	cm.CreationTimestamp = metav1.Time{}
	cm.ManagedFields = nil

	existingCM := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cm.Name,
			Namespace: cm.Namespace,
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

func BackupConfigMap(
	ctx context.Context, c klient.Client, name, namespace string,
) ([]byte, error) {
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

func UpdateConfigMapTOMLField[T any](
	ctx context.Context, c klient.Client, name, namespace, tomlKey string, modifier func(*T) error,
) error {
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

func ScaleDeployment(ctx context.Context, t *testing.T, c klient.Client, name, namespace string, replicas int32) error {
	t.Logf("Scaling deployment %s/%s to %d", namespace, name, replicas)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &appsv1.Deployment{}
		if err := c.Resources().Get(ctx, name, namespace, current); err != nil {
			return err
		}

		current.Spec.Replicas = ptr.To(replicas)

		return c.Resources().Update(ctx, current)
	})
}

//nolint:cyclop,gocognit // Test helper with complex deployment rollout logic
func WaitForDeploymentRollout(
	ctx context.Context, t *testing.T, c klient.Client, name, namespace string,
) {
	t.Logf("Waiting for rollout to complete for deployment %s/%s",
		namespace, name)

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
			if _, err := fmt.Sscanf(revisionStr, "%d", &revision); err != nil {
				// Skip this replica set if revision parsing fails
				continue
			}

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
func RestartDeployment(
	ctx context.Context, t *testing.T, c klient.Client, name, namespace string,
) error {
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

// SetDeploymentEnvVars sets or updates environment variables for containers in a deployment.
// If containerName is empty, applies to all containers. Otherwise, applies only to the named container.
// Uses retry.RetryOnConflict for automatic retry handling.
func SetDeploymentEnvVars(
	ctx context.Context, c klient.Client, deploymentName, namespace, containerName string, envVars map[string]string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if err := c.Resources().Get(ctx, deploymentName, namespace, deployment); err != nil {
			return err
		}

		if len(deployment.Spec.Template.Spec.Containers) == 0 {
			return fmt.Errorf("deployment %s/%s has no containers", namespace, deploymentName)
		}

		found := false

		for i := range deployment.Spec.Template.Spec.Containers {
			container := &deployment.Spec.Template.Spec.Containers[i]

			if containerName != "" && container.Name != containerName {
				continue
			}

			found = true

			setEnvVarsOnContainer(container, envVars)
		}

		if containerName != "" && !found {
			return fmt.Errorf("container %q not found in deployment %s/%s", containerName, namespace, deploymentName)
		}

		return c.Resources().Update(ctx, deployment)
	})
}

func setEnvVarsOnContainer(container *v1.Container, envVars map[string]string) {
	for key, value := range envVars {
		exists := false

		for i := range container.Env {
			if container.Env[i].Name == key {
				container.Env[i].Value = value
				exists = true

				break
			}
		}

		if !exists {
			container.Env = append(container.Env, v1.EnvVar{
				Name:  key,
				Value: value,
			})
		}
	}
}

// RemoveDeploymentEnvVars removes environment variables from containers in a deployment.
// If containerName is empty, removes from all containers. Otherwise, removes only from the named container.
// Uses retry.RetryOnConflict for automatic retry handling.
func RemoveDeploymentEnvVars(
	ctx context.Context, c klient.Client, deploymentName, namespace, containerName string, envVarNames []string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if err := c.Resources().Get(ctx, deploymentName, namespace, deployment); err != nil {
			return err
		}

		if len(deployment.Spec.Template.Spec.Containers) == 0 {
			return fmt.Errorf("deployment %s/%s has no containers", namespace, deploymentName)
		}

		toRemove := make(map[string]bool, len(envVarNames))
		for _, name := range envVarNames {
			toRemove[name] = true
		}

		found := false

		for i := range deployment.Spec.Template.Spec.Containers {
			container := &deployment.Spec.Template.Spec.Containers[i]

			if containerName != "" && container.Name != containerName {
				continue
			}

			found = true

			removeEnvVarsFromContainer(container, toRemove)
		}

		if containerName != "" && !found {
			return fmt.Errorf("container %q not found in deployment %s/%s", containerName, namespace, deploymentName)
		}

		return c.Resources().Update(ctx, deployment)
	})
}

func removeEnvVarsFromContainer(container *v1.Container, toRemove map[string]bool) {
	newEnv := make([]v1.EnvVar, 0, len(container.Env))
	for _, env := range container.Env {
		if !toRemove[env.Name] {
			newEnv = append(newEnv, env)
		}
	}

	container.Env = newEnv
}

// CheckNodeConditionExists checks if a node has a specific condition type and reason.
// Returns:
//   - (*v1.NodeCondition, nil) if the node exists and has the specified condition
//   - (nil, nil) if the node exists but doesn't have the specified condition
//   - (nil, error) if the node cannot be retrieved
func CheckNodeConditionExists(
	ctx context.Context, c klient.Client, nodeName, conditionType, reason string,
) (*v1.NodeCondition, error) {
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
func CheckNodeEventExists(
	ctx context.Context, c klient.Client, nodeName string, eventType, eventReason string, afterTime ...time.Time,
) (bool, *v1.Event) {
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

		svc.Spec.Ports[0].Port = int32(targetPort) // #nosec G115 - test port values are within int32 range
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

// SetNodeManagedByNVSentinel sets the ManagedByNVSentinel label on a node.
func SetNodeManagedByNVSentinel(ctx context.Context, c klient.Client, nodeName string, managed bool) error {
	labelValue := "false"
	if managed {
		labelValue = "true"
	}

	return SetNodeLabel(ctx, c, nodeName, "k8saas.nvidia.com/ManagedByNVSentinel", labelValue)
}

// RemoveNodeManagedByNVSentinelLabel removes the ManagedByNVSentinel label from a node.
func RemoveNodeManagedByNVSentinelLabel(ctx context.Context, c klient.Client, nodeName string) error {
	return RemoveNodeLabel(ctx, c, nodeName, "k8saas.nvidia.com/ManagedByNVSentinel")
}

func RemoveNodeLabel(ctx context.Context, c klient.Client, nodeName, labelKey string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := GetNodeByName(ctx, c, nodeName)
		if err != nil {
			return err
		}

		if node.Labels != nil {
			delete(node.Labels, labelKey)
			return c.Resources().Update(ctx, node)
		}

		return nil
	})
}

func SetNodeLabel(ctx context.Context, c klient.Client, nodeName, labelKey, labelValue string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := GetNodeByName(ctx, c, nodeName)
		if err != nil {
			return err
		}

		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}

		node.Labels[labelKey] = labelValue

		return c.Resources().Update(ctx, node)
	})
}

// GetPodOnWorkerNode returns a running pod matching the given name pattern on a real worker node
func GetPodOnWorkerNode(
	ctx context.Context, t *testing.T, client klient.Client, namespace, podNamePattern string,
) (*v1.Pod, error) {
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
func WaitForNodeLabel(
	ctx context.Context, t *testing.T, client klient.Client, nodeName, labelKey, expectedValue string,
) {
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

func AssertPodsNeverDeleted(
	ctx context.Context, t *testing.T, client klient.Client, namespace string, podNames []string,
) {
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
func ExecInPod(
	ctx context.Context, restConfig *rest.Config, namespace, podName, containerName string, command []string,
) (string, string, error) {
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
func PortForwardPod(
	ctx context.Context, restConfig *rest.Config, namespace, podName string, localPort, podPort int,
) (chan struct{}, chan struct{}) {
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
			close(readyChan)

			return
		}
	}()

	return stopChan, readyChan
}

// WaitForNodeConditionWithCheckName waits for a node condition matching the given criteria.
// Optional parameters (use empty string to skip): expectedMessage, expectedReason, expectedStatus.
func WaitForNodeConditionWithCheckName(
	ctx context.Context, t *testing.T, c klient.Client, nodeName, checkName, expectedMessage, expectedReason string,
	expectedStatus v1.ConditionStatus,
) {
	t.Helper()

	require.Eventually(t, func() bool {
		node, err := GetNodeByName(ctx, c, nodeName)
		if err != nil {
			t.Logf("failed to get node %s: %v", nodeName, err)
			return false
		}

		for _, condition := range node.Status.Conditions {
			if string(condition.Type) != checkName {
				continue
			}

			// Check message if specified
			if expectedMessage != "" && !containsAllExpectedParts(condition.Message, expectedMessage) {
				t.Logf("Checking if message matches: expected=%s, actual=%s", expectedMessage, condition.Message)
				continue
			}

			// Check reason if specified
			if expectedReason != "" && condition.Reason != expectedReason {
				continue
			}

			// Check status if specified
			if expectedStatus != "" && condition.Status != expectedStatus {
				continue
			}

			t.Logf("Found matching node condition: Type=%s, Reason=%s, Status=%s, Message=%s",
				condition.Type, condition.Reason, condition.Status, condition.Message)

			return true
		}

		t.Logf("Node %s does not have matching condition for check name '%s'", nodeName, checkName)

		return false
	}, EventuallyWaitTimeout, WaitInterval, "node %s should have a condition with check name %s", nodeName, checkName)
}

// containsAllExpectedParts checks if all semicolon-separated parts of expected are in actual.
// This allows for flexible matching where the expected error codes don't need to be contiguous.
func containsAllExpectedParts(actual, expected string) bool {
	// Split expected message by semicolon
	parts := strings.Split(expected, ";")
	matchCount := 0

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if !strings.Contains(actual, part) {
			return false
		}

		matchCount++
	}

	// Ensure at least one part matched
	return matchCount > 0
}

// SetNodeConditionStatus sets a node condition to a specific status for testing purposes.
func SetNodeConditionStatus(
	ctx context.Context,
	t *testing.T,
	client klient.Client,
	nodeName string,
	conditionType v1.NodeConditionType,
	status v1.ConditionStatus,
) {
	t.Helper()

	require.Eventually(t, func() bool {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			node, err := GetNodeByName(ctx, client, nodeName)
			if err != nil {
				return err
			}

			found := false
			modified := false

			for i := range node.Status.Conditions {
				if node.Status.Conditions[i].Type == conditionType {
					found = true

					if node.Status.Conditions[i].Status != status {
						node.Status.Conditions[i].Status = status
						node.Status.Conditions[i].LastTransitionTime = metav1.Now()
						node.Status.Conditions[i].LastHeartbeatTime = metav1.Now()
						modified = true
					}

					break
				}
			}

			if !found {
				now := metav1.Now()
				node.Status.Conditions = append(node.Status.Conditions, v1.NodeCondition{
					Type:               conditionType,
					Status:             status,
					LastTransitionTime: now,
					LastHeartbeatTime:  now,
					Reason:             "TestCondition",
					Message:            "Set by test",
				})
				modified = true
			}

			if !modified {
				return nil
			}

			return client.Resources().UpdateStatus(ctx, node)
		})
		if err != nil {
			t.Logf("Failed to update node status: %v", err)
			return false
		}

		return true
	}, EventuallyWaitTimeout, WaitInterval)
}

// EnsureNodeConditionNotPresent ensures that the node does NOT have a condition with the reason as checkName.
func EnsureNodeConditionNotPresent(ctx context.Context, t *testing.T, c klient.Client, nodeName, checkName string) {
	require.Never(t, func() bool {
		node, err := GetNodeByName(ctx, c, nodeName)
		if err != nil {
			t.Logf("failed to get node %s: %v", nodeName, err)
			return false
		}

		for _, condition := range node.Status.Conditions {
			if condition.Status == v1.ConditionTrue && condition.Reason == checkName+"IsNotHealthy" {
				t.Logf("ERROR: Found unexpected node condition: Type=%s, Reason=%s, Status=%s, Message=%s",
					condition.Type, condition.Reason, condition.Status, condition.Message)

				return true
			}
		}

		t.Logf("Node %s correctly does not have a condition with check name '%s'", nodeName, checkName)

		return false
	}, NeverWaitTimeout, WaitInterval, "node %s should NOT have a condition with check name %s", nodeName, checkName)
}

func InjectSyslogMessages(t *testing.T, httpPort int, messages []string) {
	t.Helper()

	httpClient := &http.Client{Timeout: 10 * time.Second}
	ctx := context.Background()

	t.Logf("Injecting %d syslog messages", len(messages))

	for i, msg := range messages {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			fmt.Sprintf("http://localhost:%d/add", httpPort),
			strings.NewReader(msg))
		require.NoError(t, err, "failed to create request for message %d", i+1)
		req.Header.Set("Content-Type", "text/plain")

		resp, err := httpClient.Do(req)
		require.NoError(t, err, "failed to inject message %d", i+1)
		require.Equal(t, http.StatusOK, resp.StatusCode, "stub journal should return 200 OK for message %d", i+1)

		resp.Body.Close()
	}

	t.Logf("All %d messages injected successfully", len(messages))
}

// VerifyNodeConditionMatchesSequence verifies that a node condition contains expected patterns in sequence using regex
func VerifyNodeConditionMatchesSequence(t *testing.T, ctx context.Context,
	c klient.Client, nodeName, checkName, conditionReason string, expectedPatterns []string) bool {
	t.Helper()

	condition, err := CheckNodeConditionExists(ctx, c, nodeName, checkName, conditionReason)
	if err != nil || condition == nil {
		t.Logf("Condition not found: %v", err)
		return false
	}

	message := condition.Message
	lastIndex := 0

	for i, pattern := range expectedPatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			t.Logf("Invalid regex pattern at position %d: %s (error: %v)", i+1, pattern, err)
			return false
		}

		loc := re.FindStringIndex(message[lastIndex:])
		if loc == nil {
			t.Logf("Missing expected pattern at position %d: %s", i+1, pattern)
			t.Logf("Searched in: %s", message[lastIndex:])

			return false
		}

		matchStart := lastIndex + loc[0]
		matchEnd := lastIndex + loc[1]
		t.Logf("Pattern %d matched: %s", i+1, message[matchStart:matchEnd])
		lastIndex = matchEnd
	}

	t.Logf("Node condition verified: all %d patterns matched in sequence", len(expectedPatterns))

	return true
}

// VerifyEventsMatchPatterns verifies that events contain expected regex patterns
func VerifyEventsMatchPatterns(t *testing.T, ctx context.Context,
	c klient.Client, nodeName, checkName, eventReason string, expectedPatterns []string) bool {
	t.Helper()

	eventList, err := GetNodeEvents(ctx, c, nodeName, checkName)
	if err != nil {
		t.Logf("Error listing events: %v", err)
		return false
	}

	var allMessages []string

	for _, event := range eventList.Items {
		if event.Reason == eventReason {
			allMessages = append(allMessages, event.Message)
		}
	}

	if len(allMessages) == 0 {
		t.Log("No events found yet")
		return false
	}

	allMessagesStr := strings.Join(allMessages, " ")
	foundPatterns := make(map[string]bool)

	for i, pattern := range expectedPatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			t.Logf("Invalid regex pattern at position %d: %s (error: %v)", i+1, pattern, err)
			return false
		}

		if re.MatchString(allMessagesStr) {
			foundPatterns[pattern] = true
			match := re.FindString(allMessagesStr)

			matchPreview := match
			if len(match) > 150 {
				matchPreview = match[:70] + " ... " + match[len(match)-75:]
			}

			t.Logf("Pattern %d matched: %s", i+1, matchPreview)
		} else {
			t.Logf("Pattern %d not found: %s", i+1, pattern)
		}
	}

	if len(foundPatterns) != len(expectedPatterns) {
		t.Logf("Found %d/%d expected patterns in events", len(foundPatterns), len(expectedPatterns))
		return false
	}

	t.Logf("Events verified: all %d patterns matched", len(expectedPatterns))

	return true
}

// ApplyKwokStageFromFile applies a KWOK Stage YAML for testing (e.g., failure injection).
// If nodeName is provided, replaces NODE_NAME_PLACEHOLDER in the YAML with the actual node name.
func ApplyKwokStageFromFile(
	ctx context.Context, t *testing.T, c klient.Client, stageFilePath string, nodeName ...string,
) {
	t.Helper()

	content, err := os.ReadFile(stageFilePath)
	require.NoError(t, err, "failed to read KWOK stage file: %s", stageFilePath)

	stageYAML := string(content)
	if len(nodeName) > 0 && nodeName[0] != "" {
		stageYAML = strings.ReplaceAll(stageYAML, "NODE_NAME_PLACEHOLDER", nodeName[0])
		t.Logf("Applying KWOK stage from %s (node: %s)", stageFilePath, nodeName[0])
	} else {
		t.Logf("Applying KWOK stage from %s", stageFilePath)
	}

	// Decode and create KWOK Stage using the test client's scheme which has KWOK types registered
	decoder := serializer.NewCodecFactory(c.Resources().GetScheme()).UniversalDeserializer()
	obj, _, err := decoder.Decode([]byte(stageYAML), nil, nil)
	require.NoError(t, err, "failed to decode KWOK stage YAML")

	stage, ok := obj.(*kwokv1alpha1.Stage)
	require.True(t, ok, "decoded object is not a KWOK Stage")

	err = c.Resources().Create(ctx, stage)
	require.NoError(t, err, "failed to create KWOK stage %s", stage.Name)
}

// DeleteKwokStage removes a KWOK Stage by name (idempotent - won't fail if not found).
func DeleteKwokStage(ctx context.Context, t *testing.T, c klient.Client, stageName string) {
	t.Helper()

	stage := &kwokv1alpha1.Stage{}
	stage.SetName(stageName)

	err := c.Resources().Delete(ctx, stage)
	if err != nil {
		if apierrors.IsNotFound(err) {
			t.Logf("KWOK stage %s already deleted", stageName)
		} else {
			t.Logf("Warning: failed to delete KWOK stage %s: %v", stageName, err)
		}

		return
	}
}

// DeleteAllLogCollectorJobs deletes all log-collector jobs in the nvsentinel namespace.
// This is useful for cleaning up leftover jobs from previous tests.
func DeleteAllLogCollectorJobs(ctx context.Context, t *testing.T, c klient.Client) {
	t.Helper()

	var jobList batchv1.JobList

	err := c.Resources(NVSentinelNamespace).List(ctx, &jobList, resources.WithLabelSelector("app=log-collector"))
	if err != nil {
		t.Logf("Warning: failed to list log-collector jobs: %v", err)
		return
	}

	if len(jobList.Items) == 0 {
		t.Logf("No log-collector jobs to clean up")
		return
	}

	t.Logf("Deleting %d old log-collector job(s)", len(jobList.Items))

	for _, job := range jobList.Items {
		// Delete with PropagationPolicy=Background to also delete pods
		deletePolicy := metav1.DeletePropagationBackground

		err := c.Resources(NVSentinelNamespace).Delete(ctx, &job, func(do *metav1.DeleteOptions) {
			do.PropagationPolicy = &deletePolicy
		})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Warning: failed to delete job %s: %v", job.Name, err)
		}
	}

	// Wait for all jobs to be deleted
	require.Eventually(t, func() bool {
		var remainingJobs batchv1.JobList

		err := c.Resources(NVSentinelNamespace).List(ctx, &remainingJobs, resources.WithLabelSelector("app=log-collector"))
		if err != nil {
			t.Logf("failed to list jobs: %v", err)
			return false
		}

		return len(remainingJobs.Items) == 0
	}, EventuallyWaitTimeout, WaitInterval, "log-collector jobs should be deleted")

	t.Logf("Log-collector jobs cleanup completed")
}

// checkLogCollectorJobStatus checks if a job matches the expected status.
// Returns (matched, shouldReturn) where matched indicates if status matches,
// shouldReturn indicates if we should exit the wait loop.
func checkLogCollectorJobStatus(
	t *testing.T, job *batchv1.Job, nodeName, expectedStatus string,
) (matched, shouldReturn bool) {
	// Check for job completion conditions
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == v1.ConditionTrue {
			t.Logf("Log-collector job %s completed successfully on node %s", job.Name, nodeName)

			return expectedStatus == "Complete", true
		}

		if condition.Type == batchv1.JobFailed && condition.Status == v1.ConditionTrue {
			t.Logf("Log-collector job %s failed (exhausted retries) on node %s", job.Name, nodeName)

			return expectedStatus == "Failed", true
		}
	}

	// For "Failed" expectation, check if pods are failing (faster than waiting for retries)
	if expectedStatus == "Failed" && job.Status.Failed > 0 {
		t.Logf("Log-collector job %s has %d failed pod(s) on node %s - pods are failing as expected",
			job.Name, job.Status.Failed, nodeName)

		return true, true
	}

	t.Logf("Log-collector job %s on node %s: active=%d, succeeded=%d, failed=%d (waiting for %s)",
		job.Name, nodeName, job.Status.Active, job.Status.Succeeded, job.Status.Failed, expectedStatus)

	return false, false
}

// WaitForLogCollectorJobStatus waits for a log-collector job to reach the specified status.
// For "Failed" status, checks if pods are in Error state (no wait for job retry exhaustion).
func WaitForLogCollectorJobStatus(
	ctx context.Context, t *testing.T, c klient.Client, nodeName, expectedStatus string,
) *batchv1.Job {
	t.Helper()

	var foundJob *batchv1.Job

	require.Eventually(t, func() bool {
		var jobList batchv1.JobList

		err := c.Resources(NVSentinelNamespace).List(ctx, &jobList, resources.WithLabelSelector("app=log-collector"))
		if err != nil {
			t.Logf("failed to list log-collector jobs: %v", err)
			return false
		}

		for i := range jobList.Items {
			job := &jobList.Items[i]
			if job.Spec.Template.Spec.NodeName != nodeName {
				continue
			}

			matched, shouldReturn := checkLogCollectorJobStatus(t, job, nodeName, expectedStatus)
			if shouldReturn {
				if matched {
					foundJob = job
				}

				return matched
			}
		}

		t.Logf("No log-collector job found for node %s yet", nodeName)

		return false
	}, EventuallyWaitTimeout, WaitInterval,
		"log-collector job for node %s should reach status %s", nodeName, expectedStatus)

	return foundJob
}

// VerifyNoLogCollectorJobExists verifies that no log-collector job exists for the node.
func VerifyNoLogCollectorJobExists(ctx context.Context, t *testing.T, c klient.Client, nodeName string) {
	t.Helper()

	t.Logf("Waiting %v to verify no log-collector job exists for node %s", NeverWaitTimeout, nodeName)

	time.Sleep(NeverWaitTimeout)

	var jobList batchv1.JobList

	err := c.Resources(NVSentinelNamespace).List(ctx, &jobList, resources.WithLabelSelector("app=log-collector"))
	require.NoError(t, err, "failed to list log-collector jobs")

	for _, job := range jobList.Items {
		if job.Spec.Template.Spec.NodeName == nodeName {
			t.Fatalf("Found log-collector job %s for node %s - log-collector should NOT run for unsupported events",
				job.Name, nodeName)
		}
	}
}

// VerifyNodeLabelNotEqual verifies that a node's label is NOT equal to the specified value.
func VerifyNodeLabelNotEqual(
	ctx context.Context, t *testing.T, c klient.Client, nodeName, labelKey, notExpectedValue string,
) {
	t.Helper()

	node, err := GetNodeByName(ctx, c, nodeName)
	require.NoError(t, err, "failed to get node %s", nodeName)

	actualValue, exists := node.Labels[labelKey]
	if !exists {
		t.Logf("Node %s does not have label %s (expected it to NOT be %s)", nodeName, labelKey, notExpectedValue)
		return
	}

	require.NotEqual(t, notExpectedValue, actualValue,
		"Node %s label %s should NOT be %s", nodeName, labelKey, notExpectedValue)

	t.Logf("Node %s label %s=%s (not %s as expected)", nodeName, labelKey, actualValue, notExpectedValue)
}

// VerifyLogFilesUploaded verifies that the log-collector job uploaded files to the file server.
// It checks if files exist by querying the NGINX autoindex endpoint via HTTP port-forward.
func VerifyLogFilesUploaded(ctx context.Context, t *testing.T, c klient.Client, job *batchv1.Job) {
	t.Helper()

	require.NotNil(t, job, "job cannot be nil")
	nodeName := job.Spec.Template.Spec.NodeName

	// Find running file server pod
	var podList v1.PodList

	err := c.Resources("nvsentinel").List(ctx, &podList,
		resources.WithLabelSelector("app.kubernetes.io/name=incluster-file-server"))
	require.NoError(t, err, "failed to list file server pods")

	var fileServerPod *v1.Pod

	for i := range podList.Items {
		if podList.Items[i].Status.Phase == v1.PodRunning {
			fileServerPod = &podList.Items[i]
			break
		}
	}

	require.NotNil(t, fileServerPod, "no running file server pod found")

	// Setup port-forward
	localPort := 18080
	stopChan, readyChan := PortForwardPod(
		ctx, c.RESTConfig(), fileServerPod.Namespace, fileServerPod.Name, localPort, 8080)
	<-readyChan

	defer close(stopChan)

	time.Sleep(500 * time.Millisecond)

	// Helper to fetch directory listing
	httpClient := &http.Client{Timeout: 10 * time.Second}
	fetchListing := func(path string) string {
		req, err := http.NewRequestWithContext(
			ctx, http.MethodGet, fmt.Sprintf("http://localhost:%d%s", localPort, path), nil)
		require.NoError(t, err, "failed to create request")

		resp, err := httpClient.Do(req)
		require.NoError(t, err, "failed to fetch %s", path)

		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		return string(body)
	}

	// Get timestamp directory
	listing := fetchListing(fmt.Sprintf("/upload/%s/", nodeName))
	timestampRegex := regexp.MustCompile(`<a href="(\d{8}-\d{6})/`)
	matches := timestampRegex.FindStringSubmatch(listing)
	require.NotEmpty(t, matches, "no upload directories found for node %s", nodeName)

	// Check files in timestamp directory
	filesListing := fetchListing(fmt.Sprintf("/upload/%s/%s/", nodeName, matches[1]))
	require.Contains(t, filesListing, fmt.Sprintf("nvidia-bug-report-%s-", nodeName),
		"no nvidia-bug-report files found")
	require.Contains(t, filesListing, ".log.gz", "no .log.gz files found")

	t.Logf("✓ Log files verified for node %s", nodeName)
}

// WaitForDaemonSetRollout waits for a DaemonSet to complete its rollout.
// It checks that all pods are updated and ready.
func waitForDaemonSetRollout(ctx context.Context, t *testing.T, client klient.Client, name string) {
	t.Helper()

	t.Logf("Waiting for daemonset %s/%s rollout to complete", NVSentinelNamespace, name)

	require.Eventually(t, func() bool {
		daemonSet := &appsv1.DaemonSet{}
		if err := client.Resources().Get(ctx, name, NVSentinelNamespace, daemonSet); err != nil {
			t.Logf("Failed to get daemonset: %v", err)
			return false
		}

		// Check if the controller has observed the latest generation
		// This prevents returning early when status reflects the old generation
		if daemonSet.Status.ObservedGeneration < daemonSet.Generation {
			t.Logf("DaemonSet controller hasn't observed latest generation yet: observed=%d, current=%d",
				daemonSet.Status.ObservedGeneration, daemonSet.Generation)

			return false
		}

		// Check if all desired pods are scheduled, updated, and ready
		if daemonSet.Status.DesiredNumberScheduled == 0 {
			t.Logf("DaemonSet has no desired pods scheduled yet")
			return false
		}

		if daemonSet.Status.UpdatedNumberScheduled != daemonSet.Status.DesiredNumberScheduled {
			t.Logf("DaemonSet rollout in progress: %d/%d pods updated",
				daemonSet.Status.UpdatedNumberScheduled, daemonSet.Status.DesiredNumberScheduled)

			return false
		}

		if daemonSet.Status.NumberReady != daemonSet.Status.DesiredNumberScheduled {
			t.Logf("DaemonSet rollout in progress: %d/%d pods ready",
				daemonSet.Status.NumberReady, daemonSet.Status.DesiredNumberScheduled)

			return false
		}

		t.Logf("DaemonSet %s/%s rollout complete: %d/%d pods ready and updated",
			NVSentinelNamespace, name, daemonSet.Status.NumberReady, daemonSet.Status.DesiredNumberScheduled)

		return true
	}, EventuallyWaitTimeout, WaitInterval, "daemonset %s/%s rollout should complete", NVSentinelNamespace, name)

	t.Logf("DaemonSet %s/%s rollout completed successfully", NVSentinelNamespace, name)
}

// UpdateDaemonSetArgs updates the daemonset with the specified arguments and completes the rollout.
// Returns the original args slice that can be used with RestoreDaemonSetArgs to rollback the changes.
func UpdateDaemonSetArgs(ctx context.Context, t *testing.T,
	client klient.Client, daemonsetName string, containerName string,
	args map[string]string) ([]string, error) {
	t.Helper()

	t.Logf("Updating daemonset %s/%s with args %v", NVSentinelNamespace, daemonsetName, args)

	var originalArgs []string

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		daemonSet := &appsv1.DaemonSet{}
		if err := client.Resources().Get(ctx, daemonsetName, NVSentinelNamespace, daemonSet); err != nil {
			return err
		}

		containers := daemonSet.Spec.Template.Spec.Containers
		containerFound := false

		for i := range containers {
			if containers[i].Name == containerName {
				// Capture original args before modification
				originalArgs = make([]string, len(containers[i].Args))
				copy(originalArgs, containers[i].Args)

				setArgsOnContainer(t, &containers[i], args)

				containerFound = true

				break
			}
		}

		if !containerFound {
			return fmt.Errorf("container %q not found in daemonset %s/%s", containerName, NVSentinelNamespace, daemonsetName)
		}

		return client.Resources().Update(ctx, daemonSet)
	})
	if err != nil {
		return nil, err
	}

	t.Logf("Waiting for daemonset %s/%s rollout to complete", NVSentinelNamespace, daemonsetName)
	waitForDaemonSetRollout(ctx, t, client, daemonsetName)

	return originalArgs, nil
}

// RestoreDaemonSetArgs restores the daemonset container args to the original state.
// Use the originalArgs returned by UpdateDaemonSetArgs to rollback the changes.
func RestoreDaemonSetArgs(ctx context.Context, t *testing.T, client klient.Client,
	daemonsetName string,
	containerName string, originalArgs []string,
) {
	t.Helper()

	t.Logf("Restoring args for daemonset %s/%s container %s to original state",
		NVSentinelNamespace, daemonsetName, containerName)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		daemonSet := &appsv1.DaemonSet{}
		if err := client.Resources().Get(ctx, daemonsetName, NVSentinelNamespace, daemonSet); err != nil {
			return err
		}

		containers := daemonSet.Spec.Template.Spec.Containers
		containerFound := false

		for i := range containers {
			if containers[i].Name == containerName {
				// Restore the original args
				containers[i].Args = make([]string, len(originalArgs))
				copy(containers[i].Args, originalArgs)

				containerFound = true

				break
			}
		}

		if !containerFound {
			return fmt.Errorf("container %q not found in daemonset %s/%s", containerName, NVSentinelNamespace, daemonsetName)
		}

		return client.Resources().Update(ctx, daemonSet)
	})
	require.NoError(t, err, "failed to restore args for daemonset %s/%s", NVSentinelNamespace, daemonsetName)

	t.Logf("Waiting for daemonset %s/%s rollout to complete after restoration", NVSentinelNamespace, daemonsetName)
	waitForDaemonSetRollout(ctx, t, client, daemonsetName)

	t.Log("DaemonSet restored successfully")
}

// tryUpdateExistingArg attempts to update an existing argument at position j.
// Returns true if the argument was found and updated.
func tryUpdateExistingArg(container *v1.Container, j int, flag, value string) bool {
	existingArg := container.Args[j]

	// Match --flag=value style
	if strings.HasPrefix(existingArg, flag+"=") {
		if value != "" {
			container.Args[j] = flag + "=" + value
		} else {
			container.Args[j] = flag
		}

		return true
	}

	// Match --flag or --flag value style
	if existingArg == flag {
		if value != "" {
			if j+1 < len(container.Args) && !strings.HasPrefix(container.Args[j+1], "-") {
				container.Args[j+1] = value
			} else {
				container.Args = append(container.Args[:j+1], append([]string{value}, container.Args[j+1:]...)...)
			}
		}

		return true
	}

	return false
}

func setArgsOnContainer(t *testing.T, container *v1.Container, args map[string]string) {
	t.Helper()
	t.Logf("Setting args %v on container %s", args, container.Name)

	for flag, value := range args {
		found := false

		for j := 0; j < len(container.Args); j++ {
			if tryUpdateExistingArg(container, j, flag, value) {
				found = true
				break
			}
		}

		if !found {
			if value != "" {
				container.Args = append(container.Args, flag+"="+value)
			} else {
				container.Args = append(container.Args, flag)
			}
		}
	}
}

// isPodRunningAndReady checks if a pod is running, ready, and not being deleted.
func isPodRunningAndReady(pod *v1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}

	if pod.Status.Phase != v1.PodRunning {
		return false
	}

	for _, cond := range pod.Status.Conditions {
		if cond.Type == v1.PodReady {
			return cond.Status == v1.ConditionTrue
		}
	}

	return false
}

// buildNodePattern returns a regex pattern for node matching.
// If nodeName is empty, matches any node containing "worker".
// If nodeName is specified, matches exactly that node.
func buildNodePattern(nodeName string) string {
	if nodeName == "" {
		return "^.*worker.*$"
	}

	return "^" + regexp.QuoteMeta(nodeName) + "$"
}

// GetDaemonSetPodOnWorkerNode returns the running and ready pod for a daemonset.
// If nodeName is empty, it finds a pod on any worker node (node name containing "worker").
// If nodeName is specified, it finds the pod on that exact node.
func GetDaemonSetPodOnWorkerNode(ctx context.Context, t *testing.T, client klient.Client,
	daemonsetName string, podNamePattern string, nodeName ...string) (*v1.Pod, error) {
	t.Helper()

	specificNode := ""
	if len(nodeName) > 0 {
		specificNode = nodeName[0]
	}

	nodePattern := regexp.MustCompile(buildNodePattern(specificNode))
	podPattern := regexp.MustCompile(podNamePattern)

	var resultPod *v1.Pod

	require.Eventually(t, func() bool {
		pods := &v1.PodList{}

		err := client.Resources().List(ctx, pods, func(opts *metav1.ListOptions) {
			opts.FieldSelector = fmt.Sprintf("metadata.namespace=%s", NVSentinelNamespace)
		})
		if err != nil {
			t.Logf("Failed to list pods: %v", err)
			return false
		}

		for i := range pods.Items {
			pod := &pods.Items[i]
			if !podPattern.MatchString(pod.Name) || !nodePattern.MatchString(pod.Spec.NodeName) {
				continue
			}

			if !isPodRunningAndReady(pod) {
				t.Logf("Pod %s not ready yet (phase=%s)", pod.Name, pod.Status.Phase)

				return false
			}

			t.Logf("Found pod %s on node %s", pod.Name, pod.Spec.NodeName)
			resultPod = pod

			return true
		}

		t.Logf("No matching pod found for daemonset %s", daemonsetName)

		return false
	}, EventuallyWaitTimeout, WaitInterval, "daemonset pod should be running and ready")

	if resultPod == nil {
		return nil, fmt.Errorf("failed to get ready pod for daemonset %s", daemonsetName)
	}

	return resultPod, nil
}

// SetDeploymentArgs sets or updates arguments for containers in a deployment.
// Args is a map of flag to value, e.g., {"--processing-strategy": "STORE_ONLY", "--verbose": ""}.
func SetDeploymentArgs(
	ctx context.Context, t *testing.T,
	c klient.Client, deploymentName, namespace, containerName string, args map[string]string,
) ([]string, error) {
	var originalArgs []string

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if err := c.Resources().Get(ctx, deploymentName, namespace, deployment); err != nil {
			return err
		}

		if len(deployment.Spec.Template.Spec.Containers) == 0 {
			return fmt.Errorf("deployment %s/%s has no containers", namespace, deploymentName)
		}

		updatedContainer := false

		for i := range deployment.Spec.Template.Spec.Containers {
			container := &deployment.Spec.Template.Spec.Containers[i]

			if container.Name == containerName {
				originalArgs = make([]string, len(container.Args))
				copy(originalArgs, container.Args)

				setArgsOnContainer(t, container, args)

				updatedContainer = true
			}
		}

		if !updatedContainer {
			return fmt.Errorf("container %q not found in deployment %s/%s", containerName, namespace, deploymentName)
		}

		return c.Resources().Update(ctx, deployment)
	})
	if err != nil {
		return nil, err
	}

	return originalArgs, nil
}

// RestoreDeploymentArgs restores the deployment container args to the original state.
// Use the originalArgs returned by SetDeploymentArgs to rollback the changes.
func RestoreDeploymentArgs(
	t *testing.T, ctx context.Context, c klient.Client,
	deploymentName, namespace, containerName string, originalArgs []string,
) error {
	if originalArgs == nil {
		return nil
	}

	t.Helper()
	t.Logf("Restoring args %v for deployment %s/%s container %s", originalArgs, namespace, deploymentName, containerName)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if err := c.Resources().Get(ctx, deploymentName, namespace, deployment); err != nil {
			return err
		}

		containers := deployment.Spec.Template.Spec.Containers
		containerFound := false

		for i := range containers {
			if containers[i].Name == containerName {
				// Restore the original args
				containers[i].Args = make([]string, len(originalArgs))
				copy(containers[i].Args, originalArgs)

				containerFound = true

				break
			}
		}

		if !containerFound {
			return fmt.Errorf("container %q not found in deployment %s/%s", containerName, namespace, deploymentName)
		}

		return c.Resources().Update(ctx, deployment)
	})
}

// DeleteExistingNodeEvents deletes Kubernetes node events for a given node name and event type and reason.
// This is useful for cleaning up test events that might interfere with subsequent tests.
func DeleteExistingNodeEvents(ctx context.Context, t *testing.T, c klient.Client,
	nodeName, eventType, eventReason string) error {
	t.Helper()
	t.Logf("Deleting events for node %s with type=%s, reason=%s", nodeName, eventType, eventReason)

	eventList, err := GetNodeEvents(ctx, c, nodeName, eventType)
	if err != nil {
		return fmt.Errorf("failed to get events for node %s: %w", nodeName, err)
	}

	deletedCount := 0

	for _, event := range eventList.Items {
		if eventReason != "" && event.Reason != eventReason {
			continue
		}

		// Delete the event (events are in default namespace)
		err := c.Resources().WithNamespace("default").Delete(ctx, &event)
		if err != nil {
			t.Logf("Warning: failed to delete event %s: %v", event.Name, err)
			continue
		}

		deletedCount++

		t.Logf("Deleted event: %s (type=%s, reason=%s)", event.Name, event.Type, event.Reason)
	}

	t.Logf("Deleted %d event(s) for node %s", deletedCount, nodeName)

	return nil
}

// ListDaemonSetPods returns all pods owned by the specified DaemonSet.
func ListDaemonSetPods(ctx context.Context, client klient.Client, namespace, dsName string) ([]v1.Pod, error) {
	var podList v1.PodList

	err := client.Resources(namespace).List(ctx, &podList)
	if err != nil {
		return nil, err
	}

	var dsPods []v1.Pod

	for _, pod := range podList.Items {
		for _, ownerRef := range pod.OwnerReferences {
			if ownerRef.Kind == "DaemonSet" && ownerRef.Name == dsName {
				dsPods = append(dsPods, pod)

				break
			}
		}
	}

	return dsPods, nil
}

// CleanupDaemonSet deletes a DaemonSet and waits for it and its pods to be fully deleted.
func CleanupDaemonSet(ctx context.Context, t *testing.T, client klient.Client, namespace, name string) {
	t.Helper()

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}

	// Check if DaemonSet exists first - skip cleanup if not found
	err := client.Resources(namespace).Get(ctx, name, namespace, ds)
	if apierrors.IsNotFound(err) {
		t.Logf("DaemonSet %s/%s does not exist, skipping cleanup", namespace, name)
		return
	}

	if err != nil {
		t.Logf("Warning: error checking DaemonSet existence: %v", err)
	}

	// Delete the DaemonSet
	if err := client.Resources().Delete(ctx, ds); err != nil {
		if apierrors.IsNotFound(err) {
			t.Logf("DaemonSet %s/%s already deleted", namespace, name)
			return
		}

		t.Logf("Note: DaemonSet deletion returned error: %v", err)
	}

	// Wait for pods to be deleted (short timeout since terminationGracePeriodSeconds=1)
	require.Eventually(t, func() bool {
		pods, err := ListDaemonSetPods(ctx, client, namespace, name)
		return err == nil && len(pods) == 0
	}, 30*time.Second, 1*time.Second, "timed out waiting for pods of DaemonSet %s/%s to be deleted", namespace, name)

	// Wait for DaemonSet to be fully deleted
	require.Eventually(t, func() bool {
		err := client.Resources(namespace).Get(ctx, name, namespace, &appsv1.DaemonSet{})
		if err == nil {
			return false // Object still exists
		}

		if apierrors.IsNotFound(err) {
			return true // Deleted successfully
		}

		// Transient error - log and retry
		t.Logf("Note: transient error checking DaemonSet deletion: %v", err)

		return false
	}, 15*time.Second, 1*time.Second, "timed out waiting for DaemonSet %s/%s to be fully deleted", namespace, name)
}

// UpdateDaemonSet gets a DaemonSet, applies a modifier function, and updates it.
func UpdateDaemonSet(
	ctx context.Context, client klient.Client, namespace, name string, modifier func(*appsv1.DaemonSet),
) error {
	ds := &appsv1.DaemonSet{}

	err := client.Resources(namespace).Get(ctx, name, namespace, ds)
	if err != nil {
		return err
	}

	modifier(ds)

	return client.Resources().Update(ctx, ds)
}

// CheckPodCrashLoopBackOff checks if a pod has CrashLoopBackOff in its container statuses.
// Returns true if CrashLoopBackOff is found.
func CheckPodCrashLoopBackOff(statuses []v1.ContainerStatus) bool {
	for _, cs := range statuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
	}

	return false
}

// LogContainerStatuses logs the current state of container statuses for debugging.
func LogContainerStatuses(t *testing.T, statuses []v1.ContainerStatus, containerType string) {
	t.Helper()

	for _, cs := range statuses {
		switch {
		case cs.State.Waiting != nil:
			t.Logf("  %s %s: waiting (reason=%s)", containerType, cs.Name, cs.State.Waiting.Reason)
		case cs.State.Running != nil:
			t.Logf("  %s %s: running", containerType, cs.Name)
		case cs.State.Terminated != nil:
			t.Logf("  %s %s: terminated (exitCode=%d, reason=%s)",
				containerType, cs.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
		}
	}
}

// WaitForCrashLoopBackOff waits for CrashLoopBackOff in either containerStatuses or
// initContainerStatuses based on the checkInit flag.
// When checkInit=false, checks main container statuses; when checkInit=true, checks init containers.
func WaitForCrashLoopBackOff(
	ctx context.Context, t *testing.T, client klient.Client,
	namespace, podNamePrefix string, checkInit bool,
) *v1.Pod {
	t.Helper()

	var foundPod *v1.Pod

	containerType := "Container"
	if checkInit {
		containerType = "InitContainer"
	}

	require.Eventually(t, func() bool {
		var podList v1.PodList
		if err := client.Resources(namespace).List(ctx, &podList); err != nil {
			return false
		}

		for i := range podList.Items {
			pod := &podList.Items[i]

			// Find pod by prefix (DaemonSet pods have generated suffixes)
			if !strings.HasPrefix(pod.Name, podNamePrefix) {
				continue
			}

			// Select which container statuses to check
			statuses := pod.Status.ContainerStatuses
			if checkInit {
				statuses = pod.Status.InitContainerStatuses
			}

			if CheckPodCrashLoopBackOff(statuses) {
				t.Logf("Pod %s %s is in CrashLoopBackOff (phase: %s)",
					pod.Name, containerType, pod.Status.Phase)
				foundPod = pod

				return true
			}

			// Log current state for debugging
			t.Logf("Pod %s: phase=%s, %sStatuses=%d",
				pod.Name, pod.Status.Phase, containerType, len(statuses))
			LogContainerStatuses(t, statuses, containerType)
		}

		return false
	}, EventuallyWaitTimeout, WaitInterval,
		"pod with prefix %s %s did not enter CrashLoopBackOff", podNamePrefix, containerType)

	return foundPod
}

// WaitForDaemonSetPodRunning waits for a DaemonSet pod to reach Running state with all
// containers ready, and ensures no terminating pods exist. The function waits until:
// 1. All terminating pods are fully deleted
// 2. Exactly one pod exists on the target node
// 3. That pod is Running and Ready (using isPodRunningAndReady)
func WaitForDaemonSetPodRunning(
	ctx context.Context, t *testing.T, client klient.Client, namespace, dsName, nodeName string,
) {
	t.Helper()

	require.Eventually(t, func() bool {
		pods, err := ListDaemonSetPods(ctx, client, namespace, dsName)
		if err != nil {
			t.Logf("Error listing pods: %v", err)
			return false
		}

		var podsOnNode []v1.Pod

		for _, pod := range pods {
			if pod.Spec.NodeName == nodeName {
				podsOnNode = append(podsOnNode, pod)
			}
		}

		// Ensure exactly one non-terminating pod exists on the node
		if len(podsOnNode) != 1 {
			t.Logf("Expected 1 pod on node %s, found %d", nodeName, len(podsOnNode))
			return false
		}

		pod := &podsOnNode[0]

		// isPodRunningAndReady checks DeletionTimestamp, Phase, and Ready condition
		if !isPodRunningAndReady(pod) {
			t.Logf("Pod %s: phase=%s, deletionTimestamp=%v", pod.Name, pod.Status.Phase, pod.DeletionTimestamp)
			return false
		}

		t.Logf("Pod %s is Running and Ready", pod.Name)

		return true
	}, EventuallyWaitTimeout, WaitInterval,
		"DaemonSet %s pod on node %s did not reach Running state", dsName, nodeName)
}
