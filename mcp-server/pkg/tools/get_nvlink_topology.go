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
)

const (
	getNVLinkTopologyAPIVersion = "1"
	dataGapCode                 = "NVSENTINEL_DATA_GAP"
	dataGapStatus               = "unsupported"

	// nvlinkTopologyNeededExtension documents the monitor work required to
	// fill this gap. The full proposal lives in mcp-server/AUDIT.md § 6.1
	// (option A: emit periodic HealthEvents of componentClass=NVLINK_TOPOLOGY;
	// option B: add a dedicated TopologyMetadata collection).
	nvlinkTopologyNeededExtension = "NVSentinel does not yet persist NVLink topology in the store. " +
		"A monitor extension (likely in syslog-health-monitor, which already reads the per-node " +
		"gpu_metadata.json file) must emit topology data as a HealthEvent of componentClass=NVLINK_TOPOLOGY, " +
		"or a dedicated TopologyMetadata collection must be added alongside HealthEvents. " +
		"See mcp-server/AUDIT.md § 6.1 for the full proposal."

	nvlinkTopologyMessage = "NVLink topology is not yet exposed through NVSentinel's store. " +
		"This tool returns a structured data-gap envelope so MCP clients can branch on out.code == \"" +
		dataGapCode + "\" rather than parse error strings."
)

// GetNVLinkTopologyInput is the structured input for get_nvlink_topology.
type GetNVLinkTopologyInput struct {
	Node string `json:"node"`
}

// GetNVLinkTopologyOutput is the structured output for get_nvlink_topology.
// Per design spec § 6.3, all stub tools return this NVSENTINEL_DATA_GAP
// envelope shape so clients can branch on Code.
type GetNVLinkTopologyOutput struct {
	APIVersion             string `json:"api_version"`
	Status                 string `json:"status"`
	Code                   string `json:"code"`
	Node                   string `json:"node"`
	NeededMonitorExtension string `json:"needed_monitor_extension"`
	TrackingIssue          string `json:"tracking_issue,omitempty"`
	Message                string `json:"message"`
}

// GetNVLinkTopologyHandler handles the get_nvlink_topology MCP tool. The
// implementation is intentionally a stub: AUDIT.md § 3 confirmed that
// NVSentinel does not persist NVLink topology in its store. Once a monitor
// extension lands (see AUDIT.md § 6.1), this handler should be replaced
// with a real one that reads from the new data path.
type GetNVLinkTopologyHandler struct{}

// NewGetNVLinkTopologyHandler returns a new stub handler.
func NewGetNVLinkTopologyHandler() *GetNVLinkTopologyHandler {
	return &GetNVLinkTopologyHandler{}
}

// Handle validates the required node argument and returns the structured
// data-gap envelope. The tracking_issue field is intentionally empty:
// per donor direction (AUDIT.md § 6), no GitHub issue is filed in this
// PR. If maintainers request one during review, it can be filed using
// the body in AUDIT.md § 6.1 and substituted here.
func (h *GetNVLinkTopologyHandler) Handle(
	_ context.Context, in GetNVLinkTopologyInput,
) (GetNVLinkTopologyOutput, error) {
	if in.Node == "" {
		return GetNVLinkTopologyOutput{}, errors.New("get_nvlink_topology: node is required")
	}

	return GetNVLinkTopologyOutput{
		APIVersion:             getNVLinkTopologyAPIVersion,
		Status:                 dataGapStatus,
		Code:                   dataGapCode,
		Node:                   in.Node,
		NeededMonitorExtension: nvlinkTopologyNeededExtension,
		TrackingIssue:          "",
		Message:                nvlinkTopologyMessage,
	}, nil
}
