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
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/e2e-framework/klient"
)

// WaitForNodeCordonState checks if a node is cordoned (unschedulable) with retry logic
func WaitForNodeCordonState(ctx context.Context, t *testing.T, c klient.Client, nodeName string, shouldCordon bool) (*v1.Node, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("reached timeout")
		case <-ticker.C:
			t.Log("polling for node state")
			var node v1.Node
			err := c.Resources().Get(ctx, nodeName, "", &node)
			if err != nil {
				return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
			}

			if node.Spec.Unschedulable == shouldCordon {
				return &node, nil
			}
		}
	}
}

// CreateNamespace creates a new namespace
func CreateNamespace(ctx context.Context, c klient.Client, name string) error {
	namespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	return c.Resources().Create(ctx, namespace)
}

// DeleteNamespace deletes a namespace
func DeleteNamespace(ctx context.Context, c klient.Client, name string) error {
	namespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	return c.Resources().Delete(ctx, namespace)
}

// CreateGPUPod creates a simple pod that requests GPUs
func CreateGPUPod(ctx context.Context, c klient.Client, namespace, podName, nodeName string) error {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "gpu-container",
					Image: "busybox:latest",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse(fmt.Sprintf("%d", 8)),
						},
						Limits: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse(fmt.Sprintf("%d", 8)),
						},
					},
				},
			},
			NodeName: nodeName,
			Tolerations: []v1.Toleration{
				{
					Operator: "Exists",
				},
			},
		},
	}

	return c.Resources().Create(ctx, pod)
}

// WaitForNodeLabel waits for a node to have a specific label with the expected value
func WaitForNodeLabel(ctx context.Context, t *testing.T, c klient.Client, nodeName, labelKey, expectedValue string) (*v1.Node, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("reached timeout")
		case <-ticker.C:
			t.Logf("checking if node %s has label %s=%s", nodeName, labelKey, expectedValue)
			var node v1.Node
			err := c.Resources().Get(ctx, nodeName, "", &node)
			if err != nil {
				return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
			}

			if actualValue, exists := node.Labels[labelKey]; exists && actualValue == expectedValue {
				return &node, nil
			}
		}
	}
}

// GetNodeByName retrieves a node by name
func GetNodeByName(ctx context.Context, c klient.Client, nodeName string) (*v1.Node, error) {
	var node v1.Node
	err := c.Resources().Get(ctx, nodeName, "", &node)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	return &node, nil
}

// IsPodRunning checks if a pod is in Running phase
func IsPodRunning(ctx context.Context, c klient.Client, namespace, podName string) (bool, error) {
	var pod v1.Pod
	err := c.Resources().Get(ctx, podName, namespace, &pod)
	if err != nil {
		return false, fmt.Errorf("failed to get pod %s in namespace %s: %w", podName, namespace, err)
	}

	return pod.Status.Phase == v1.PodRunning, nil
}

// DeletePod deletes a pod
func DeletePod(ctx context.Context, c klient.Client, namespace, podName string) error {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
	}

	return c.Resources().Delete(ctx, pod)
}

// WaitForRebootNodeCR waits for a RebootNode custom resource to be created for a specific node
func WaitForRebootNodeCR(ctx context.Context, t *testing.T, c klient.Client, nodeName string) (*unstructured.Unstructured, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("reached timeout")
		case <-ticker.C:
			t.Logf("checking if RebootNode CR exists for node %s", nodeName)

			rebootNodeList := &unstructured.UnstructuredList{}
			rebootNodeList.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "janitor.dgxc.nvidia.com",
				Version: "v1alpha1",
				Kind:    "RebootNodeList",
			})

			err := c.Resources().List(ctx, rebootNodeList)
			if err != nil {
				return nil, fmt.Errorf("failed to list rebootnodes: %w", err)
			}

			for _, item := range rebootNodeList.Items {
				nodeNameInCR, found, err := unstructured.NestedString(item.Object, "spec", "nodeName")
				if err != nil {
					continue
				}
				if !found {
					continue
				}

				if found && nodeNameInCR == nodeName {
					t.Logf("found rebootnode CR: %+v", item)
					return &item, nil
				}
			}
		}
	}
}

// DeleteRebootNodeCR deletes a RebootNode custom resource for a specific node
func DeleteRebootNodeCR(ctx context.Context, c klient.Client, rebootNode *unstructured.Unstructured) error {
	err := c.Resources().Delete(ctx, rebootNode)
	if err != nil {
		return fmt.Errorf("failed to delete RebootNode CR: %w", err)
	}

	return nil
}
