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

	"github.com/nvidia/nvsentinel/mcp-server/pkg/tools"
)

// TestGetNVLinkTopology_ReturnsDataGapEnvelope asserts the stub returns the
// NVSENTINEL_DATA_GAP envelope mandated by the design spec. The donor
// direction is to leave tracking_issue empty until maintainers ask for an
// issue (see mcp-server/AUDIT.md § 6.1). The needed_monitor_extension field
// explains the gap inline. Compare and contrast with NewGPUInventoryHandler
// which is Working — this is intentionally Stub per AUDIT § 3.
func TestGetNVLinkTopology_ReturnsDataGapEnvelope(t *testing.T) {
	h := tools.NewGetNVLinkTopologyHandler()

	out, err := h.Handle(context.Background(), tools.GetNVLinkTopologyInput{Node: "gpu-node-1"})

	require.NoError(t, err)
	require.Equal(t, "unsupported", out.Status)
	require.Equal(t, "NVSENTINEL_DATA_GAP", out.Code)
	require.Equal(t, "gpu-node-1", out.Node)
	require.NotEmpty(t, out.NeededMonitorExtension, "stub MUST explain the gap inline")
	require.Empty(t, out.TrackingIssue, "tracking_issue stays empty until maintainers request an issue (donor direction)")
	require.Contains(t, out.NeededMonitorExtension, "NVLink")
}

// TestGetNVLinkTopology_EmptyNode_ReturnsValidationError asserts the stub
// still validates required input — it doesn't blanket-respond regardless of
// arguments.
func TestGetNVLinkTopology_EmptyNode_ReturnsValidationError(t *testing.T) {
	h := tools.NewGetNVLinkTopologyHandler()

	_, err := h.Handle(context.Background(), tools.GetNVLinkTopologyInput{Node: ""})

	require.Error(t, err)
	require.Contains(t, err.Error(), "node")
}
