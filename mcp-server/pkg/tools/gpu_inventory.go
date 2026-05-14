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

// Package tools implements MCP tool handlers for NVSentinel's GPU health
// surface. Each handler reads from store.Reader (and, for K8s-touching tools,
// kubernetes.Interface) and returns a structured response that the
// pkg/mcp/server.go AddTool registration adapts to the mcp-go transport.
package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
)

// gpuInventoryAPIVersion is the schema version reported in every gpu_inventory
// response. Bumped when the output shape changes in a backwards-incompatible
// way so MCP clients can branch without inspecting individual fields.
const gpuInventoryAPIVersion = "1"

// GPUInventoryInput is the structured input for the gpu_inventory tool.
type GPUInventoryInput struct {
	Node string `json:"node"`
}

// GPUEntry is the per-GPU view returned by gpu_inventory. It carries only
// event-derived fields: NVSentinel's store does not persist static GPU
// metadata (model name, memory total, driver/CUDA version), so callers
// needing those must consult NVML directly.
type GPUEntry struct {
	UUID          string    `json:"uuid"`
	LastEventTime time.Time `json:"last_event_time"`
	Healthy       bool      `json:"healthy"`
	LastMessage   string    `json:"last_message,omitempty"`
	LastCheck     string    `json:"last_check,omitempty"`
	ErrorCodes    []string  `json:"error_codes,omitempty"`
}

// GPUInventoryOutput is the structured output for the gpu_inventory tool.
type GPUInventoryOutput struct {
	APIVersion string     `json:"api_version"`
	Status     string     `json:"status"`
	Node       string     `json:"node"`
	GPUCount   int        `json:"gpu_count"`
	GPUs       []GPUEntry `json:"gpus"`
}

// GPUInventoryHandler handles the gpu_inventory MCP tool. The zero value is
// not usable; construct with NewGPUInventoryHandler.
type GPUInventoryHandler struct {
	reader store.Reader
}

// NewGPUInventoryHandler wires the handler with a read-only Reader.
func NewGPUInventoryHandler(r store.Reader) *GPUInventoryHandler {
	return &GPUInventoryHandler{reader: r}
}

// Handle queries every health event for the given node, attributes each
// event to the GPU UUIDs it mentions via entitiesImpacted (entityType=GPU),
// and reduces to the latest event per UUID. Results are sorted alphabetically
// by UUID so snapshotting and diffing across calls are stable.
func (h *GPUInventoryHandler) Handle(ctx context.Context, in GPUInventoryInput) (GPUInventoryOutput, error) {
	if in.Node == "" {
		return GPUInventoryOutput{}, errors.New("gpu_inventory: node is required")
	}

	events, err := h.reader.EventsByNode(ctx, in.Node)
	if err != nil {
		return GPUInventoryOutput{}, fmt.Errorf("gpu_inventory: %w", err)
	}

	type seen struct {
		entry  GPUEntry
		seenAt time.Time
	}

	byUUID := map[string]seen{}

	for _, ews := range events {
		he, ok := ews.HealthEvent.(*protos.HealthEvent)
		if !ok || he == nil {
			continue
		}

		for _, uuid := range gpuUUIDsFromEvent(he) {
			if prev, found := byUUID[uuid]; found && !ews.CreatedAt.After(prev.seenAt) {
				continue
			}

			byUUID[uuid] = seen{
				entry: GPUEntry{
					UUID:          uuid,
					LastEventTime: ews.CreatedAt,
					Healthy:       he.GetIsHealthy(),
					LastMessage:   he.GetMessage(),
					LastCheck:     he.GetCheckName(),
					ErrorCodes:    append([]string{}, he.GetErrorCode()...),
				},
				seenAt: ews.CreatedAt,
			}
		}
	}

	gpus := make([]GPUEntry, 0, len(byUUID))
	for _, s := range byUUID {
		gpus = append(gpus, s.entry)
	}

	sort.Slice(gpus, func(i, j int) bool { return gpus[i].UUID < gpus[j].UUID })

	return GPUInventoryOutput{
		APIVersion: gpuInventoryAPIVersion,
		Status:     "success",
		Node:       in.Node,
		GPUCount:   len(gpus),
		GPUs:       gpus,
	}, nil
}

// gpuUUIDsFromEvent extracts the unique GPU UUIDs an event is "about" via its
// entitiesImpacted entries. An event with no GPU-typed entity is excluded
// from inventory even when its componentClass is "GPU" — without a UUID the
// reading is not attributable to any specific device. Match on entityType is
// case-insensitive to tolerate monitor-side casing drift.
func gpuUUIDsFromEvent(he *protos.HealthEvent) []string {
	set := map[string]struct{}{}

	for _, ent := range he.GetEntitiesImpacted() {
		if strings.EqualFold(ent.GetEntityType(), "GPU") && ent.GetEntityValue() != "" {
			set[ent.GetEntityValue()] = struct{}{}
		}
	}

	if len(set) == 0 {
		return nil
	}

	out := make([]string, 0, len(set))
	for u := range set {
		out = append(out, u)
	}

	return out
}
