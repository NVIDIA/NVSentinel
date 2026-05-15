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
	"strconv"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
)

const (
	analyzeXIDAPIVersion       = "1"
	analyzeXIDRecentEventLimit = 50
)

// AnalyzeXIDInput is the structured input for the analyze_xid tool.
type AnalyzeXIDInput struct {
	XIDCode int    `json:"xid_code"`
	Node    string `json:"node,omitempty"`
}

// AnalyzeXIDOutput is the structured output for analyze_xid.
type AnalyzeXIDOutput struct {
	APIVersion    string         `json:"api_version"`
	Status        string         `json:"status"`
	XIDCode       int            `json:"xid_code"`
	EventCount    int            `json:"event_count"`
	AffectedNodes []string       `json:"affected_nodes,omitempty"`
	AffectedGPUs  []string       `json:"affected_gpus,omitempty"`
	Pattern       *Incident      `json:"pattern,omitempty"`
	RecentEvents  []EventSummary `json:"recent_events,omitempty"`
}

// AnalyzeXIDHandler handles the analyze_xid MCP tool. The zero value is
// not usable; construct with NewAnalyzeXIDHandler.
type AnalyzeXIDHandler struct {
	reader store.Reader
}

// NewAnalyzeXIDHandler wires the handler with a Reader. XID events live in
// the store and are persisted by syslog-health-monitor (per AUDIT.md § 2).
func NewAnalyzeXIDHandler(r store.Reader) *AnalyzeXIDHandler {
	return &AnalyzeXIDHandler{reader: r}
}

// Handle queries the store for events whose ErrorCode contains the numeric
// XID string, optionally narrows to a single node, attributes them to nodes
// and GPUs, and surfaces the matching donor pattern.
func (h *AnalyzeXIDHandler) Handle(ctx context.Context, in AnalyzeXIDInput) (AnalyzeXIDOutput, error) {
	if in.XIDCode <= 0 {
		return AnalyzeXIDOutput{}, errors.New("analyze_xid: xid_code must be a positive integer")
	}

	codeStr := strconv.Itoa(in.XIDCode)

	builder := query.New().Build(query.Eq("healthevent.errorcode", codeStr))

	events, err := h.reader.EventsByQuery(ctx, builder)
	if err != nil {
		return AnalyzeXIDOutput{}, fmt.Errorf("analyze_xid: %w", err)
	}

	matching := filterEventsForXID(events, in.Node)

	out := AnalyzeXIDOutput{
		APIVersion: analyzeXIDAPIVersion,
		Status:     successStatus,
		XIDCode:    in.XIDCode,
		EventCount: len(matching),
	}

	out.AffectedNodes, out.AffectedGPUs = nodesAndGPUsFromEvents(matching)
	out.RecentEvents = recentEventSummaries(matching, analyzeXIDRecentEventLimit)

	if pattern := pickPatternForXID(matching); pattern != nil {
		out.Pattern = pattern
	}

	return out, nil
}

func filterEventsForXID(events []datastore.HealthEventWithStatus, node string) []datastore.HealthEventWithStatus {
	out := make([]datastore.HealthEventWithStatus, 0, len(events))

	for i := range events {
		ews := events[i]

		he, ok := ews.HealthEvent.(*protos.HealthEvent)
		if !ok || he == nil {
			continue
		}

		if node != "" && he.GetNodeName() != node {
			continue
		}

		out = append(out, ews)
	}

	return out
}

func nodesAndGPUsFromEvents(events []datastore.HealthEventWithStatus) (nodes, gpus []string) {
	nodeSet := map[string]struct{}{}
	gpuSet := map[string]struct{}{}

	for i := range events {
		he, ok := events[i].HealthEvent.(*protos.HealthEvent)
		if !ok || he == nil {
			continue
		}

		if n := he.GetNodeName(); n != "" {
			nodeSet[n] = struct{}{}
		}

		for _, e := range he.GetEntitiesImpacted() {
			if e.GetEntityType() == "GPU" && e.GetEntityValue() != "" {
				gpuSet[e.GetEntityValue()] = struct{}{}
			}
		}
	}

	for n := range nodeSet {
		nodes = append(nodes, n)
	}

	for g := range gpuSet {
		gpus = append(gpus, g)
	}

	sort.Strings(nodes)
	sort.Strings(gpus)

	return nodes, gpus
}

func recentEventSummaries(events []datastore.HealthEventWithStatus, limit int) []EventSummary {
	out := make([]EventSummary, 0, len(events))

	for i := range events {
		if summary := eventSummaryFromStored(&events[i]); summary != nil {
			out = append(out, *summary)
		}
	}

	if len(out) > limit {
		out = out[:limit]
	}

	return out
}

// pickPatternForXID runs MatchIncidents over the events and returns the
// first matched pattern (highest confidence). Returns nil when no patterns
// matched.
func pickPatternForXID(events []datastore.HealthEventWithStatus) *Incident {
	matches := MatchIncidents(events)
	if len(matches) == 0 {
		return nil
	}

	top := matches[0]

	return &top
}
