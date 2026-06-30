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

	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/tools"
)

// TestGPUHealth_AggregatesEventCountsPerGPU verifies the handler counts every
// event touching a GPU and separately tracks unhealthy events, while picking
// the *latest* event's IsHealthy/Message/ErrorCode as the current state.
// Mirrors the construction pattern of NewGPUInventoryHandler (same store-only
// data path) but adds aggregate counters and an optional UUID filter.
// Deleting the count-accumulation logic or the latest-wins logic causes
// distinct assertions to fail.
func TestGPUHealth_AggregatesEventCountsPerGPU(t *testing.T) {
	t0 := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	r := store.NewFakeReader()
	r.SeedNodeEvents(invTestNode,
		at(t0.Add(1*time.Minute), gpuClassed("GPU-A", "c1", "healthy", true)),
		at(t0.Add(2*time.Minute), gpuClassed("GPU-A", "c2", "fault", false, "13")),
		at(t0.Add(3*time.Minute), gpuClassed("GPU-A", "c3", "fault", false, "79")),
		at(t0.Add(4*time.Minute), gpuClassed("GPU-A", "c4", "healthy again", true)),
		at(t0.Add(5*time.Minute), gpuClassed("GPU-B", "c5", "fault", false, "31")),
	)

	h := tools.NewGPUHealthHandler(r)
	out, err := h.Handle(context.Background(), tools.GPUHealthInput{Node: invTestNode})

	require.NoError(t, err)
	require.Equal(t, "1", out.APIVersion)
	require.Equal(t, "success", out.Status)
	require.Equal(t, invTestNode, out.Node)
	require.Equal(t, 2, out.GPUCount)

	byUUID := map[string]tools.GPUHealthEntry{}
	for _, g := range out.GPUs {
		byUUID[g.UUID] = g
	}

	a := byUUID["GPU-A"]
	require.Equal(t, 4, a.EventCount, "GPU-A had 4 events touching it")
	require.Equal(t, 2, a.UnhealthyEventCount, "GPU-A had 2 unhealthy events")
	require.True(t, a.Healthy, "GPU-A latest event at t+4 was healthy")
	require.Equal(t, t0.Add(4*time.Minute), a.LastEventTime)
	require.Equal(t, "healthy again", a.LastMessage)
	require.Equal(t, "c4", a.LastCheck)

	b := byUUID["GPU-B"]
	require.Equal(t, 1, b.EventCount)
	require.Equal(t, 1, b.UnhealthyEventCount)
	require.False(t, b.Healthy)
	require.Equal(t, []string{"31"}, b.ErrorCodes)
}

// TestGPUHealth_GPUUUIDFilterNarrowsResult verifies the optional GPUUUID
// input narrows the response to a single GPU even when other GPUs have
// events on the same node. Catches the bug "filter ignored, returns every
// GPU regardless of GPUUUID".
func TestGPUHealth_GPUUUIDFilterNarrowsResult(t *testing.T) {
	t0 := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	r := store.NewFakeReader()
	r.SeedNodeEvents(invTestNode,
		at(t0, gpuClassed("GPU-A", "c", "ok", true)),
		at(t0, gpuClassed("GPU-B", "c", "ok", true)),
		at(t0, gpuClassed("GPU-C", "c", "ok", true)),
	)

	h := tools.NewGPUHealthHandler(r)
	out, err := h.Handle(context.Background(), tools.GPUHealthInput{Node: invTestNode, GPUUUID: "GPU-B"})

	require.NoError(t, err)
	require.Equal(t, 1, out.GPUCount)
	require.Len(t, out.GPUs, 1)
	require.Equal(t, "GPU-B", out.GPUs[0].UUID)
}

// TestGPUHealth_NodeWithNoEvents_ReturnsEmpty asserts a node with no stored
// events returns a well-formed empty response, not an error.
func TestGPUHealth_NodeWithNoEvents_ReturnsEmpty(t *testing.T) {
	h := tools.NewGPUHealthHandler(store.NewFakeReader())

	out, err := h.Handle(context.Background(), tools.GPUHealthInput{Node: "empty-node"})

	require.NoError(t, err)
	require.Equal(t, "empty-node", out.Node)
	require.Equal(t, 0, out.GPUCount)
	require.Empty(t, out.GPUs)
}

// TestGPUHealth_EmptyNode_ReturnsValidationError asserts an empty node
// argument is rejected before any store call. Catches the bug "silently
// calls EventsByNode(”) and returns every event".
func TestGPUHealth_EmptyNode_ReturnsValidationError(t *testing.T) {
	h := tools.NewGPUHealthHandler(store.NewFakeReader())

	_, err := h.Handle(context.Background(), tools.GPUHealthInput{Node: ""})

	require.Error(t, err)
	require.Contains(t, err.Error(), "node")
}
