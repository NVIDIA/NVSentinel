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
	"time"

	"github.com/stretchr/testify/require"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/tools"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

const invTestNode = "gpu-node-1"

func at(when time.Time, ev *protos.HealthEvent) datastore.HealthEventWithStatus {
	return datastore.HealthEventWithStatus{CreatedAt: when, HealthEvent: ev}
}

func gpuClassed(uuid, check, msg string, healthy bool, codes ...string) *protos.HealthEvent {
	return &protos.HealthEvent{
		NodeName: invTestNode, ComponentClass: "GPU", CheckName: check,
		IsHealthy: healthy, Message: msg, ErrorCode: codes,
		EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: uuid}},
	}
}

func podMentioning(podRef, gpuUUID, code string) *protos.HealthEvent {
	return &protos.HealthEvent{
		NodeName: invTestNode, ComponentClass: "POD", CheckName: "pod_failure",
		IsHealthy: false, Message: "Pod evicted due to GPU fault", ErrorCode: []string{code},
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "Pod", EntityValue: podRef},
			{EntityType: "GPU", EntityValue: gpuUUID},
		},
	}
}

func networkOnly() *protos.HealthEvent {
	return &protos.HealthEvent{
		NodeName: invTestNode, ComponentClass: "NETWORK", CheckName: "link_down", IsHealthy: false,
		EntitiesImpacted: []*protos.Entity{{EntityType: "Interface", EntityValue: "eth0"}},
	}
}

// TestGPUInventory_DedupesByUUIDAndPicksLatestEvent exercises the core
// behaviour: collapse multiple events per GPU to the latest, and include
// non-GPU-classed events that mention a GPU via entitiesImpacted (e.g., a Pod
// eviction). Deleting the latest-wins logic or the entitiesImpacted-based
// inclusion path makes assertions fail.
func TestGPUInventory_DedupesByUUIDAndPicksLatestEvent(t *testing.T) {
	t0 := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	t1, t2 := t0.Add(time.Hour), t0.Add(2*time.Hour)

	r := store.NewFakeReader()
	r.SeedNodeEvents(invTestNode,
		at(t0, gpuClassed("GPU-1", "gpu_healthy", "GPU healthy", true)),
		at(t2, podMentioning("ns/my-pod", "GPU-1", "XID-79")),
		at(t1, gpuClassed("GPU-2", "gpu_fault", "GPU bus fall-off", false, "79")),
		at(t1, networkOnly()),
	)

	h := tools.NewGPUInventoryHandler(r)
	out, err := h.Handle(context.Background(), tools.GPUInventoryInput{Node: invTestNode})

	require.NoError(t, err)
	require.Equal(t, "1", out.APIVersion)
	require.Equal(t, "success", out.Status)
	require.Equal(t, invTestNode, out.Node)
	require.Equal(t, 2, out.GPUCount)
	require.Len(t, out.GPUs, 2)

	byUUID := make(map[string]tools.GPUEntry, len(out.GPUs))
	for _, g := range out.GPUs {
		byUUID[g.UUID] = g
	}

	gpu1 := byUUID["GPU-1"]
	require.Equal(t, t2, gpu1.LastEventTime, "GPU-1 must reflect the latest event at t2, not the earlier healthy one at t0")
	require.False(t, gpu1.Healthy, "GPU-1 should be unhealthy from latest event")
	require.Equal(t, "Pod evicted due to GPU fault", gpu1.LastMessage)
	require.Equal(t, "pod_failure", gpu1.LastCheck)
	require.Equal(t, []string{"XID-79"}, gpu1.ErrorCodes)

	gpu2 := byUUID["GPU-2"]
	require.Equal(t, t1, gpu2.LastEventTime)
	require.False(t, gpu2.Healthy)
	require.Equal(t, "GPU bus fall-off", gpu2.LastMessage)
	require.Equal(t, "gpu_fault", gpu2.LastCheck)
	require.Equal(t, []string{"79"}, gpu2.ErrorCodes)
}

// TestGPUInventory_NodeWithNoEvents_ReturnsEmpty asserts a node with no
// stored events returns a well-formed empty inventory, not an error.
func TestGPUInventory_NodeWithNoEvents_ReturnsEmpty(t *testing.T) {
	h := tools.NewGPUInventoryHandler(store.NewFakeReader())

	out, err := h.Handle(context.Background(), tools.GPUInventoryInput{Node: "empty-node"})

	require.NoError(t, err)
	require.Equal(t, "empty-node", out.Node)
	require.Equal(t, 0, out.GPUCount)
	require.Empty(t, out.GPUs)
}

// TestGPUInventory_EmptyNode_ReturnsValidationError asserts the handler
// rejects an empty node argument before any store call. Catches the bug
// "silently calls EventsByNode(”) and returns every event in the database".
func TestGPUInventory_EmptyNode_ReturnsValidationError(t *testing.T) {
	h := tools.NewGPUInventoryHandler(store.NewFakeReader())

	_, err := h.Handle(context.Background(), tools.GPUInventoryInput{Node: ""})

	require.Error(t, err)
	require.Contains(t, err.Error(), "node")
}
