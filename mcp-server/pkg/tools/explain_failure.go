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
	"strings"
	"time"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

const (
	explainFailureAPIVersion       = "1"
	explainFailureDefaultSinceMins = 60
)

// ExplainFailureInput is the structured input for the explain_failure tool.
type ExplainFailureInput struct {
	Node         string `json:"node"`
	GPUUUID      string `json:"gpu_uuid,omitempty"`
	SinceMinutes int    `json:"since_minutes,omitempty"`
}

// ExplainFailureOutput is the structured output for explain_failure.
// Narrative is a single human-readable paragraph; Incidents is the full
// scored list of pattern matches sorted by confidence.
type ExplainFailureOutput struct {
	APIVersion       string     `json:"api_version"`
	Status           string     `json:"status"`
	Node             string     `json:"node"`
	GPUUUID          string     `json:"gpu_uuid,omitempty"`
	SinceMinutes     int        `json:"since_minutes"`
	EventsConsidered int        `json:"events_considered"`
	Narrative        string     `json:"narrative"`
	Incidents        []Incident `json:"incidents,omitempty"`
}

// ExplainFailureHandler handles the explain_failure MCP tool. The zero
// value is not usable; construct with NewExplainFailureHandler.
type ExplainFailureHandler struct {
	reader store.Reader
	now    func() time.Time
}

// NewExplainFailureHandler wires the handler with a Reader. The time source
// is fixed to time.Now in production; tests that need deterministic windows
// would build the events with relative timestamps as in this file.
func NewExplainFailureHandler(r store.Reader) *ExplainFailureHandler {
	return &ExplainFailureHandler{reader: r, now: time.Now}
}

// Handle queries the node's events within the time window, optionally
// narrows to events touching GPUUUID, runs MatchIncidents, and composes a
// short narrative naming the top pattern (if any).
func (h *ExplainFailureHandler) Handle(ctx context.Context, in ExplainFailureInput) (ExplainFailureOutput, error) {
	if in.Node == "" {
		return ExplainFailureOutput{}, errors.New("explain_failure: node is required")
	}

	since := in.SinceMinutes
	if since <= 0 {
		since = explainFailureDefaultSinceMins
	}

	all, err := h.reader.EventsByNode(ctx, in.Node)
	if err != nil {
		return ExplainFailureOutput{}, fmt.Errorf("explain_failure: %w", err)
	}

	cutoff := h.now().Add(-time.Duration(since) * time.Minute)
	considered := filterEventsForExplain(all, cutoff, in.GPUUUID)

	incidents := MatchIncidents(considered)

	return ExplainFailureOutput{
		APIVersion:       explainFailureAPIVersion,
		Status:           successStatus,
		Node:             in.Node,
		GPUUUID:          in.GPUUUID,
		SinceMinutes:     since,
		EventsConsidered: len(considered),
		Narrative:        buildExplainNarrative(incidents, len(considered), since),
		Incidents:        incidents,
	}, nil
}

// filterEventsForExplain narrows the event slice to events within the time
// window and (when gpuUUID is set) events that mention the GPU via
// entitiesImpacted.
func filterEventsForExplain(
	events []datastore.HealthEventWithStatus, cutoff time.Time, gpuUUID string,
) []datastore.HealthEventWithStatus {
	out := make([]datastore.HealthEventWithStatus, 0, len(events))

	for i := range events {
		ews := events[i]

		if ews.CreatedAt.Before(cutoff) {
			continue
		}

		if gpuUUID != "" && !eventMentionsGPU(ews.HealthEvent, gpuUUID) {
			continue
		}

		out = append(out, ews)
	}

	return out
}

func eventMentionsGPU(heAny any, uuid string) bool {
	he, ok := heAny.(*protos.HealthEvent)
	if !ok || he == nil {
		return false
	}

	for _, e := range he.GetEntitiesImpacted() {
		if strings.EqualFold(e.GetEntityType(), "GPU") && e.GetEntityValue() == uuid {
			return true
		}
	}

	return false
}

// buildExplainNarrative composes a one-paragraph human narrative. When no
// patterns matched, the narrative is a deterministic "no known pattern"
// string. When at least one pattern matched, the top match's name,
// not-your-code label, and evidence are summarised.
func buildExplainNarrative(incidents []Incident, eventsConsidered, sinceMinutes int) string {
	if eventsConsidered == 0 {
		return fmt.Sprintf(
			"No health events were observed for this scope in the last %d minutes.",
			sinceMinutes,
		)
	}

	if len(incidents) == 0 {
		return fmt.Sprintf(
			"No known failure pattern matched the %d event(s) observed in the last %d minutes.",
			eventsConsidered, sinceMinutes,
		)
	}

	top := incidents[0]

	owner := "user code"
	if top.NotYourCode {
		owner = "infrastructure (not your code)"
	}

	evidence := "none"
	if len(top.Evidence) > 0 {
		evidence = strings.Join(top.Evidence, "; ")
	}

	return fmt.Sprintf(
		"The most likely cause across %d event(s) in the last %d minutes is %s (confidence %.2f, owned by %s). Evidence: %s.",
		eventsConsidered, sinceMinutes, top.PatternName, top.Confidence, owner, evidence,
	)
}
