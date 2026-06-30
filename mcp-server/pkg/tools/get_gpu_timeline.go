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
)

const (
	getGPUTimelineAPIVersion       = "1"
	getGPUTimelineDefaultSinceMins = 60
)

// GetGPUTimelineInput is the structured input for the get_gpu_timeline tool.
type GetGPUTimelineInput struct {
	Node         string `json:"node"`
	GPUUUID      string `json:"gpu_uuid,omitempty"`
	SinceMinutes int    `json:"since_minutes,omitempty"`
}

// TimelineEvent is a chronologically-ordered entry in the timeline.
type TimelineEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type,omitempty"`
	Severity  string    `json:"severity"`
	Summary   string    `json:"summary,omitempty"`
}

// GetGPUTimelineOutput is the structured output for get_gpu_timeline.
type GetGPUTimelineOutput struct {
	APIVersion   string          `json:"api_version"`
	Status       string          `json:"status"`
	Node         string          `json:"node"`
	GPUUUID      string          `json:"gpu_uuid,omitempty"`
	SinceMinutes int             `json:"since_minutes"`
	Events       []TimelineEvent `json:"events"`
}

// GetGPUTimelineHandler handles the get_gpu_timeline MCP tool. The zero
// value is not usable; construct with NewGetGPUTimelineHandler.
type GetGPUTimelineHandler struct {
	reader store.Reader
	now    func() time.Time
}

// NewGetGPUTimelineHandler wires the handler with a Reader. The time
// source is fixed to time.Now in production.
func NewGetGPUTimelineHandler(r store.Reader) *GetGPUTimelineHandler {
	return &GetGPUTimelineHandler{reader: r, now: time.Now}
}

// Handle reads events for the node, optionally narrows to a single GPU
// UUID, filters to the requested time window, and returns the timeline in
// ascending order.
func (h *GetGPUTimelineHandler) Handle(ctx context.Context, in GetGPUTimelineInput) (GetGPUTimelineOutput, error) {
	if in.Node == "" {
		return GetGPUTimelineOutput{}, errors.New("get_gpu_timeline: node is required")
	}

	since := in.SinceMinutes
	if since <= 0 {
		since = getGPUTimelineDefaultSinceMins
	}

	events, err := h.reader.EventsByNode(ctx, in.Node)
	if err != nil {
		return GetGPUTimelineOutput{}, fmt.Errorf("get_gpu_timeline: %w", err)
	}

	cutoff := h.now().Add(-time.Duration(since) * time.Minute)

	timeline := make([]TimelineEvent, 0, len(events))

	for i := range events {
		ews := events[i]

		if ews.CreatedAt.Before(cutoff) {
			continue
		}

		he, ok := ews.HealthEvent.(*protos.HealthEvent)
		if !ok || he == nil {
			continue
		}

		if in.GPUUUID != "" && !eventMentionsGPU(he, in.GPUUUID) {
			continue
		}

		timeline = append(timeline, TimelineEvent{
			Timestamp: ews.CreatedAt,
			Type:      he.GetComponentClass(),
			Severity:  severityFor(he),
			Summary:   he.GetMessage(),
		})
	}

	sort.Slice(timeline, func(i, j int) bool {
		return timeline[i].Timestamp.Before(timeline[j].Timestamp)
	})

	return GetGPUTimelineOutput{
		APIVersion:   getGPUTimelineAPIVersion,
		Status:       successStatus,
		Node:         in.Node,
		GPUUUID:      in.GPUUUID,
		SinceMinutes: since,
		Events:       timeline,
	}, nil
}
