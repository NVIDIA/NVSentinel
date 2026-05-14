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

package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const podGPUAllocationAPIVersion = "1"

// gpuResourceName is the Kubernetes extended resource name that the NVIDIA
// device plugin advertises for whole-GPU allocations. Pods request GPUs via
// container.resources.requests["nvidia.com/gpu"].
const gpuResourceName = "nvidia.com/gpu"

// PodGPUAllocationInput is the structured input for the pod_gpu_allocation
// tool. Both fields are optional: omitting Namespace lists across all
// namespaces; omitting Node lists across all nodes.
type PodGPUAllocationInput struct {
	Namespace string `json:"namespace,omitempty"`
	Node      string `json:"node,omitempty"`
}

// PodGPUAllocation is a single pod's GPU-allocation record.
type PodGPUAllocation struct {
	Pod       string   `json:"pod"`
	Namespace string   `json:"namespace"`
	Node      string   `json:"node"`
	Requested int      `json:"requested"`
	GPUs      []string `json:"gpus,omitempty"`
}

// PodGPUAllocationOutput is the structured output for pod_gpu_allocation.
type PodGPUAllocationOutput struct {
	APIVersion  string             `json:"api_version"`
	Status      string             `json:"status"`
	Allocations []PodGPUAllocation `json:"allocations"`
}

// PodGPUAllocationHandler handles the pod_gpu_allocation MCP tool. The zero
// value is not usable; construct with NewPodGPUAllocationHandler.
type PodGPUAllocationHandler struct {
	k8sClient kubernetes.Interface
}

// NewPodGPUAllocationHandler wires the handler with a kubernetes.Interface.
// Unlike describe_gpu_node, a nil client is rejected at Handle time because
// this tool has no fallback data source.
func NewPodGPUAllocationHandler(k kubernetes.Interface) *PodGPUAllocationHandler {
	return &PodGPUAllocationHandler{k8sClient: k}
}

// Handle lists pods with GPU requests across the requested scope and
// resolves their assigned UUIDs from NVIDIA_VISIBLE_DEVICES where present.
// Pods on the same node that do not request GPUs are filtered out at the
// resource-summing stage. Results are sorted by namespace and pod name for
// stable output.
func (h *PodGPUAllocationHandler) Handle(ctx context.Context, in PodGPUAllocationInput) (PodGPUAllocationOutput, error) {
	if h.k8sClient == nil {
		return PodGPUAllocationOutput{}, errors.New("pod_gpu_allocation: k8s API not configured")
	}

	podList, err := h.k8sClient.CoreV1().Pods(in.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return PodGPUAllocationOutput{}, fmt.Errorf("pod_gpu_allocation: list pods: %w", err)
	}

	allocations := make([]PodGPUAllocation, 0)

	for i := range podList.Items {
		pod := &podList.Items[i]

		if in.Node != "" && pod.Spec.NodeName != in.Node {
			continue
		}

		requested := totalGPURequest(pod)
		if requested == 0 {
			continue
		}

		allocations = append(allocations, PodGPUAllocation{
			Pod:       pod.Name,
			Namespace: pod.Namespace,
			Node:      pod.Spec.NodeName,
			Requested: requested,
			GPUs:      visibleGPUsFromPod(pod),
		})
	}

	sort.Slice(allocations, func(i, j int) bool {
		if allocations[i].Namespace != allocations[j].Namespace {
			return allocations[i].Namespace < allocations[j].Namespace
		}

		return allocations[i].Pod < allocations[j].Pod
	})

	return PodGPUAllocationOutput{
		APIVersion:  podGPUAllocationAPIVersion,
		Status:      "success",
		Allocations: allocations,
	}, nil
}

// totalGPURequest sums nvidia.com/gpu resource requests across all containers.
// Init containers are excluded because they don't hold GPU allocations during
// the pod's steady-state lifetime; the device plugin admits init-container
// GPU requests separately.
func totalGPURequest(pod *corev1.Pod) int {
	total := 0
	for _, c := range pod.Spec.Containers {
		if q, ok := c.Resources.Requests[gpuResourceName]; ok {
			total += int(q.Value())
		}
	}

	return total
}

// visibleGPUsFromPod parses NVIDIA_VISIBLE_DEVICES across containers and
// returns the union of valid UUIDs. The sentinel values "all" and "none" are
// excluded — they encode policy rather than specific devices.
func visibleGPUsFromPod(pod *corev1.Pod) []string {
	seen := map[string]struct{}{}

	for _, c := range pod.Spec.Containers {
		for _, env := range c.Env {
			if env.Name != "NVIDIA_VISIBLE_DEVICES" || env.Value == "" {
				continue
			}

			for _, v := range strings.Split(env.Value, ",") {
				v = strings.TrimSpace(v)
				if v == "" || v == "all" || v == "none" || v == "void" {
					continue
				}

				seen[v] = struct{}{}
			}
		}
	}

	if len(seen) == 0 {
		return nil
	}

	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}

	sort.Strings(out)

	return out
}
