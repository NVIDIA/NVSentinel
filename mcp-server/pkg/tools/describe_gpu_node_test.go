// Copyright 2026 k8s-gpu-mcp-server contributors
// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package tools_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/tools"
)

func readyGPUNode(name string, gpuCount int64) *corev1.Node {
	q := resource.NewQuantity(gpuCount, resource.DecimalSI)

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{"nvidia.com/gpu": "true"},
			Annotations: map[string]string{"nvidia.com/gpu-product": "H100-80GB"},
		},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{
				{Key: "nvidia.com/gpu", Value: "present", Effect: corev1.TaintEffectNoSchedule},
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"},
			},
			Capacity:    corev1.ResourceList{"nvidia.com/gpu": *q},
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": *q},
		},
	}
}

// TestDescribeGPUNode_PopulatesBothEventAndK8sNode asserts the happy path:
// LatestEvent from the store and K8sNode from the Kubernetes API are both
// surfaced. The K8sNode summary preserves labels, taints, conditions, and
// GPU capacity. Mirrors the construction pattern of NewGPUInventoryHandler
// but adds the K8s client dependency. Deleting either data path causes
// distinct assertions to fail.
func TestDescribeGPUNode_PopulatesBothEventAndK8sNode(t *testing.T) {
	t0 := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	r := store.NewFakeReader()
	r.SeedNodeEvents(invTestNode,
		at(t0, gpuClassed("GPU-1", "gpu_healthy", "GPU healthy", true)),
	)

	k := fake.NewSimpleClientset(readyGPUNode(invTestNode, 8))

	h := tools.NewDescribeGPUNodeHandler(r, k)
	out, err := h.Handle(context.Background(), tools.DescribeGPUNodeInput{Node: invTestNode})

	require.NoError(t, err)
	require.Equal(t, "1", out.APIVersion)
	require.Equal(t, "success", out.Status)
	require.Equal(t, invTestNode, out.Node)
	require.Empty(t, out.Warnings)

	require.NotNil(t, out.LatestEvent)
	require.Equal(t, "GPU", out.LatestEvent.ComponentClass)
	require.Equal(t, "gpu_healthy", out.LatestEvent.CheckName)
	require.Equal(t, "GPU healthy", out.LatestEvent.Message)
	require.True(t, out.LatestEvent.IsHealthy)

	require.Equal(t, invTestNode, out.K8sNode.Name)
	require.True(t, out.K8sNode.Ready)
	require.Equal(t, "true", out.K8sNode.Labels["nvidia.com/gpu"])
	require.Equal(t, "H100-80GB", out.K8sNode.Annotations["nvidia.com/gpu-product"])
	require.Len(t, out.K8sNode.Taints, 1)
	require.Equal(t, "nvidia.com/gpu", out.K8sNode.Taints[0].Key)
	require.Equal(t, "NoSchedule", out.K8sNode.Taints[0].Effect)
	require.Equal(t, "8", out.K8sNode.Capacity["nvidia.com/gpu"])
}

// TestDescribeGPUNode_K8sNodeMissing_ReturnsWarningKeepsEvent asserts that
// when the K8s API has no entry for the node, the handler returns the store
// event with a warning rather than failing. Catches the bug "missing K8s
// node short-circuits the entire response".
func TestDescribeGPUNode_K8sNodeMissing_ReturnsWarningKeepsEvent(t *testing.T) {
	t0 := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	r := store.NewFakeReader()
	r.SeedNodeEvents(invTestNode, at(t0, gpuClassed("GPU-1", "gpu_healthy", "ok", true)))

	k := fake.NewSimpleClientset() // no nodes

	h := tools.NewDescribeGPUNodeHandler(r, k)
	out, err := h.Handle(context.Background(), tools.DescribeGPUNodeInput{Node: invTestNode})

	require.NoError(t, err)
	require.NotNil(t, out.LatestEvent)
	require.Empty(t, out.K8sNode.Name)
	require.NotEmpty(t, out.Warnings)
	joined := joinWarnings(out.Warnings)
	require.Contains(t, joined, "not found")
}

// TestDescribeGPUNode_NoStoreEvents_ReturnsWarningKeepsK8sNode asserts that a
// node with no events still returns the K8s portion plus a warning. Catches
// the bug "no events → entire response empty".
func TestDescribeGPUNode_NoStoreEvents_ReturnsWarningKeepsK8sNode(t *testing.T) {
	k := fake.NewSimpleClientset(readyGPUNode(invTestNode, 4))

	h := tools.NewDescribeGPUNodeHandler(store.NewFakeReader(), k)
	out, err := h.Handle(context.Background(), tools.DescribeGPUNodeInput{Node: invTestNode})

	require.NoError(t, err)
	require.Nil(t, out.LatestEvent)
	require.Equal(t, invTestNode, out.K8sNode.Name)
	require.Equal(t, "4", out.K8sNode.Capacity["nvidia.com/gpu"])
	joined := joinWarnings(out.Warnings)
	require.Contains(t, joined, "no events")
}

// TestDescribeGPUNode_NilK8sClient_ReturnsWarningKeepsEvent asserts that
// when the handler is constructed without a K8s client, it still returns the
// store event with a warning. This is the documented behaviour from Config:
// "nil disables those tools — they may still register but will return a
// structured 'k8s API not configured' error".
func TestDescribeGPUNode_NilK8sClient_ReturnsWarningKeepsEvent(t *testing.T) {
	t0 := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	r := store.NewFakeReader()
	r.SeedNodeEvents(invTestNode, at(t0, gpuClassed("GPU-1", "gpu_healthy", "ok", true)))

	h := tools.NewDescribeGPUNodeHandler(r, nil)
	out, err := h.Handle(context.Background(), tools.DescribeGPUNodeInput{Node: invTestNode})

	require.NoError(t, err)
	require.NotNil(t, out.LatestEvent)
	require.Empty(t, out.K8sNode.Name)
	joined := joinWarnings(out.Warnings)
	require.Contains(t, joined, "k8s API not configured")
}

// TestDescribeGPUNode_EmptyNode_ReturnsValidationError asserts an empty node
// argument is rejected before any store or K8s call.
func TestDescribeGPUNode_EmptyNode_ReturnsValidationError(t *testing.T) {
	h := tools.NewDescribeGPUNodeHandler(store.NewFakeReader(), nil)

	_, err := h.Handle(context.Background(), tools.DescribeGPUNodeInput{Node: ""})

	require.Error(t, err)
	require.Contains(t, err.Error(), "node")
}

func joinWarnings(ws []string) string {
	return strings.Join(ws, "\n")
}
