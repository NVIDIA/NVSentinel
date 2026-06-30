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
	"time"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

const gpuHealthAPIVersion = "1"

// GPUHealthInput is the structured input for the gpu_health tool.
type GPUHealthInput struct {
	Node string `json:"node"`
	// GPUUUID, when set, narrows the response to a single GPU's entry.
	// Empty returns every GPU seen in events for the node.
	GPUUUID string `json:"gpu_uuid,omitempty"`
}

// GPUHealthEntry is the per-GPU view returned by gpu_health. Compared with
// GPUEntry from gpu_inventory it adds aggregate counters useful for triage.
type GPUHealthEntry struct {
	UUID                string    `json:"uuid"`
	Healthy             bool      `json:"healthy"`
	LastEventTime       time.Time `json:"last_event_time"`
	EventCount          int       `json:"event_count"`
	UnhealthyEventCount int       `json:"unhealthy_event_count"`
	LastMessage         string    `json:"last_message,omitempty"`
	LastCheck           string    `json:"last_check,omitempty"`
	ErrorCodes          []string  `json:"error_codes,omitempty"`
}

// GPUHealthOutput is the structured output for the gpu_health tool.
type GPUHealthOutput struct {
	APIVersion string           `json:"api_version"`
	Status     string           `json:"status"`
	Node       string           `json:"node"`
	GPUCount   int              `json:"gpu_count"`
	GPUs       []GPUHealthEntry `json:"gpus"`
}

// GPUHealthHandler handles the gpu_health MCP tool. The zero value is not
// usable; construct with NewGPUHealthHandler.
type GPUHealthHandler struct {
	reader store.Reader
}

// NewGPUHealthHandler wires the handler with a read-only Reader.
func NewGPUHealthHandler(r store.Reader) *GPUHealthHandler {
	return &GPUHealthHandler{reader: r}
}

// Handle queries every health event for the given node, attributes each event
// to the GPU UUIDs it mentions, and accumulates per-UUID counters while
// tracking the latest event's state. When GPUUUID is set, all other UUIDs are
// dropped from the response. Results are sorted by UUID for stable output.
func (h *GPUHealthHandler) Handle(ctx context.Context, in GPUHealthInput) (GPUHealthOutput, error) {
	if in.Node == "" {
		return GPUHealthOutput{}, errors.New("gpu_health: node is required")
	}

	events, err := h.reader.EventsByNode(ctx, in.Node)
	if err != nil {
		return GPUHealthOutput{}, fmt.Errorf("gpu_health: %w", err)
	}

	byUUID := map[string]*GPUHealthEntry{}

	for _, ews := range events {
		accumulateGPUHealthEvent(byUUID, ews, in.GPUUUID)
	}

	gpus := make([]GPUHealthEntry, 0, len(byUUID))
	for _, e := range byUUID {
		gpus = append(gpus, *e)
	}

	sort.Slice(gpus, func(i, j int) bool { return gpus[i].UUID < gpus[j].UUID })

	return GPUHealthOutput{
		APIVersion: gpuHealthAPIVersion,
		Status:     successStatus,
		Node:       in.Node,
		GPUCount:   len(gpus),
		GPUs:       gpus,
	}, nil
}

// accumulateGPUHealthEvent folds a single stored event into the per-UUID
// aggregate map. Extracted from Handle to keep that function under the cyclop
// limit; the same logic was previously inlined as a nested for/if cascade.
// When filterUUID is non-empty, only that UUID's bucket is updated.
func accumulateGPUHealthEvent(
	byUUID map[string]*GPUHealthEntry,
	ews datastore.HealthEventWithStatus,
	filterUUID string,
) {
	he, ok := ews.HealthEvent.(*protos.HealthEvent)
	if !ok || he == nil {
		return
	}

	for _, uuid := range gpuUUIDsFromEvent(he) {
		if filterUUID != "" && uuid != filterUUID {
			continue
		}

		entry, found := byUUID[uuid]
		if !found {
			entry = &GPUHealthEntry{UUID: uuid}
			byUUID[uuid] = entry
		}

		entry.EventCount++
		if !he.GetIsHealthy() {
			entry.UnhealthyEventCount++
		}

		if ews.CreatedAt.After(entry.LastEventTime) {
			entry.LastEventTime = ews.CreatedAt
			entry.Healthy = he.GetIsHealthy()
			entry.LastMessage = he.GetMessage()
			entry.LastCheck = he.GetCheckName()
			entry.ErrorCodes = append([]string{}, he.GetErrorCode()...)
		}
	}
}
