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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nvidia/nvsentinel/mcp-server/pkg/tools"
)

func gpuPod(ns, name, node string, gpuRequest int64, visibleDevices string) *corev1.Pod {
	q := resource.NewQuantity(gpuRequest, resource.DecimalSI)
	c := corev1.Container{
		Name:  "worker",
		Image: "nvidia/cuda:12-runtime",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"nvidia.com/gpu": *q},
			Limits:   corev1.ResourceList{"nvidia.com/gpu": *q},
		},
	}

	if visibleDevices != "" {
		c.Env = []corev1.EnvVar{{Name: "NVIDIA_VISIBLE_DEVICES", Value: visibleDevices}}
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{c}},
	}
}

func cpuOnlyPod(ns, name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c"}}},
	}
}

// TestPodGPUAllocation_GPUPodWithEnvVar_RecordsUUIDs verifies that a pod
// requesting GPUs and carrying NVIDIA_VISIBLE_DEVICES is reported with both
// the request count and the parsed UUID list. Mirrors the construction
// pattern of NewGPUInventoryHandler. Catches the bug "env var ignored".
func TestPodGPUAllocation_GPUPodWithEnvVar_RecordsUUIDs(t *testing.T) {
	k := fake.NewSimpleClientset(
		gpuPod("ns-a", "worker-1", invTestNode, 2, "GPU-a,GPU-b"),
	)

	h := tools.NewPodGPUAllocationHandler(k)
	out, err := h.Handle(context.Background(), tools.PodGPUAllocationInput{})

	require.NoError(t, err)
	require.Equal(t, "1", out.APIVersion)
	require.Len(t, out.Allocations, 1)

	a := out.Allocations[0]
	require.Equal(t, "worker-1", a.Pod)
	require.Equal(t, "ns-a", a.Namespace)
	require.Equal(t, invTestNode, a.Node)
	require.Equal(t, 2, a.Requested)
	require.Equal(t, []string{"GPU-a", "GPU-b"}, a.GPUs)
}

// TestPodGPUAllocation_GPUPodWithoutEnvVar_RecordsRequestOnly verifies a pod
// requesting GPUs but not declaring NVIDIA_VISIBLE_DEVICES is still included,
// with Requested set and GPUs empty (device-plugin-only environments expose
// allocation differently — the tool reports what it can see, not what it
// can't).
func TestPodGPUAllocation_GPUPodWithoutEnvVar_RecordsRequestOnly(t *testing.T) {
	k := fake.NewSimpleClientset(
		gpuPod("ns-b", "worker-2", invTestNode, 1, ""),
	)

	h := tools.NewPodGPUAllocationHandler(k)
	out, err := h.Handle(context.Background(), tools.PodGPUAllocationInput{})

	require.NoError(t, err)
	require.Len(t, out.Allocations, 1)
	require.Equal(t, 1, out.Allocations[0].Requested)
	require.Empty(t, out.Allocations[0].GPUs)
}

// TestPodGPUAllocation_NodeFilterNarrowsResult verifies the optional Node
// filter excludes pods on other nodes. Catches the bug "node filter ignored".
func TestPodGPUAllocation_NodeFilterNarrowsResult(t *testing.T) {
	k := fake.NewSimpleClientset(
		gpuPod("ns", "p1", "node-a", 1, "GPU-x"),
		gpuPod("ns", "p2", "node-b", 1, "GPU-y"),
		gpuPod("ns", "p3", "node-a", 1, "GPU-z"),
		cpuOnlyPod("ns", "cpu-only", "node-a"),
	)

	h := tools.NewPodGPUAllocationHandler(k)
	out, err := h.Handle(context.Background(), tools.PodGPUAllocationInput{Node: "node-a"})

	require.NoError(t, err)
	require.Len(t, out.Allocations, 2, "only GPU pods on node-a should be included; cpu-only pod and node-b pod must be excluded")

	names := []string{out.Allocations[0].Pod, out.Allocations[1].Pod}
	require.ElementsMatch(t, []string{"p1", "p3"}, names)
}

// TestPodGPUAllocation_NilK8sClient_ReturnsError asserts the tool errors out
// when no K8s client is configured. Unlike describe_gpu_node, this tool has
// no fallback data source — without K8s it cannot answer at all.
func TestPodGPUAllocation_NilK8sClient_ReturnsError(t *testing.T) {
	h := tools.NewPodGPUAllocationHandler(nil)

	_, err := h.Handle(context.Background(), tools.PodGPUAllocationInput{})

	require.Error(t, err)
	require.Contains(t, err.Error(), "k8s API")
}
