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
	"time"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
)

const (
	getIncidentReportAPIVersion   = "1"
	healthEventsAnalyzerAgent     = "health-events-analyzer"
	getIncidentReportWindowSecs   = 30 * 60 // +/- 30 minutes around incident time
	getIncidentReportTitleMaxLen  = 120
	getIncidentReportRelatedLimit = 50
)

// GetIncidentReportInput is the structured input for get_incident_report.
type GetIncidentReportInput struct {
	IncidentID string `json:"incident_id"`
}

// GetIncidentReportOutput is the structured output for get_incident_report.
type GetIncidentReportOutput struct {
	APIVersion         string           `json:"api_version"`
	Status             string           `json:"status"`
	IncidentID         string           `json:"incident_id"`
	Title              string           `json:"title"`
	Severity           string           `json:"severity"`
	FirstSeen          time.Time        `json:"first_seen"`
	LastSeen           time.Time        `json:"last_seen"`
	AffectedNodes      []string         `json:"affected_nodes,omitempty"`
	AffectedGPUs       []string         `json:"affected_gpus,omitempty"`
	RootCauseSummary   string           `json:"root_cause_summary,omitempty"`
	RecommendedActions []Recommendation `json:"recommended_actions,omitempty"`
	RelatedEvents      []EventSummary   `json:"related_events,omitempty"`
}

// GetIncidentReportHandler handles the get_incident_report MCP tool. The
// zero value is not usable; construct with NewGetIncidentReportHandler.
type GetIncidentReportHandler struct {
	reader store.Reader
}

// NewGetIncidentReportHandler wires the handler with a Reader. The handler
// fetches the analyzer-synthesized incident event via EventsByQuery and
// enriches with same-node related events plus pattern-matched
// recommendations.
func NewGetIncidentReportHandler(r store.Reader) *GetIncidentReportHandler {
	return &GetIncidentReportHandler{reader: r}
}

// Handle looks up the analyzer-synthesized event with the requested
// incident_id, derives the report's structured fields, and pairs the result
// with recommendations from MatchIncidents over same-node events around the
// incident time.
func (h *GetIncidentReportHandler) Handle(
	ctx context.Context, in GetIncidentReportInput,
) (GetIncidentReportOutput, error) {
	if in.IncidentID == "" {
		return GetIncidentReportOutput{}, errors.New("get_incident_report: incident_id is required")
	}

	incidentEvent, incidentEvt, err := h.findIncidentEvent(ctx, in.IncidentID)
	if err != nil {
		return GetIncidentReportOutput{}, err
	}

	out := GetIncidentReportOutput{
		APIVersion: getIncidentReportAPIVersion,
		Status:     successStatus,
		IncidentID: in.IncidentID,
		Title:      truncate(incidentEvt.GetMessage(), getIncidentReportTitleMaxLen),
		Severity:   severityFor(incidentEvt),
		FirstSeen:  incidentEvent.CreatedAt,
		LastSeen:   incidentEvent.CreatedAt,
	}

	if node := incidentEvt.GetNodeName(); node != "" {
		out.AffectedNodes = []string{node}
	}

	out.AffectedGPUs = entitiesOfType(incidentEvt, "GPU")

	related, err := h.relatedEvents(ctx, incidentEvent, incidentEvt)
	if err != nil {
		return GetIncidentReportOutput{}, err
	}

	out.RelatedEvents = make([]EventSummary, 0, len(related))

	for i := range related {
		if summary := eventSummaryFromStored(&related[i]); summary != nil {
			out.RelatedEvents = append(out.RelatedEvents, *summary)
		}
	}

	combined := append([]datastore.HealthEventWithStatus{incidentEvent}, related...)

	matches := MatchIncidents(combined)
	if len(matches) > 0 {
		out.RootCauseSummary = matches[0].PatternName
		out.RecommendedActions = matches[0].Recommendations
	}

	return out, nil
}

// findIncidentEvent runs an EventsByQuery filtered to analyzer-synthesized
// events with the given id. The store backend implements the actual lookup
// (Mongo or Postgres); this helper picks the first matching event from the
// result.
func (h *GetIncidentReportHandler) findIncidentEvent(
	ctx context.Context, id string,
) (datastore.HealthEventWithStatus, *protos.HealthEvent, error) {
	builder := query.New().Build(query.And(
		query.Eq("healthevent.agent", healthEventsAnalyzerAgent),
		query.Eq("healthevent.id", id),
	))

	events, err := h.reader.EventsByQuery(ctx, builder)
	if err != nil {
		return datastore.HealthEventWithStatus{}, nil, fmt.Errorf("get_incident_report: %w", err)
	}

	for i := range events {
		if he, ok := events[i].HealthEvent.(*protos.HealthEvent); ok && he != nil {
			return events[i], he, nil
		}
	}

	return datastore.HealthEventWithStatus{}, nil, fmt.Errorf("get_incident_report: incident %q not found", id)
}

// relatedEvents fetches events on the incident's node within a +/- 30 minute
// window of the incident's CreatedAt, excluding the incident event itself.
// The window cap is a sensible default; a future enhancement could make it
// configurable.
func (h *GetIncidentReportHandler) relatedEvents(
	ctx context.Context,
	incidentEvent datastore.HealthEventWithStatus,
	incidentEvt *protos.HealthEvent,
) ([]datastore.HealthEventWithStatus, error) {
	node := incidentEvt.GetNodeName()
	if node == "" {
		return nil, nil
	}

	all, err := h.reader.EventsByNode(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("get_incident_report: events by node: %w", err)
	}

	window := time.Duration(getIncidentReportWindowSecs) * time.Second
	earliest := incidentEvent.CreatedAt.Add(-window)
	latest := incidentEvent.CreatedAt.Add(window)

	out := make([]datastore.HealthEventWithStatus, 0, len(all))

	for i := range all {
		ews := all[i]
		if !inWindowAndDistinct(ews, incidentEvt, earliest, latest) {
			continue
		}

		out = append(out, ews)
	}

	if len(out) > getIncidentReportRelatedLimit {
		out = out[:getIncidentReportRelatedLimit]
	}

	return out, nil
}

// inWindowAndDistinct reports whether the candidate event falls inside the
// [earliest, latest] window and is not the analyzer-synthesized incident
// event itself. Extracted from relatedEvents to keep that function under the
// cyclop limit.
func inWindowAndDistinct(
	ews datastore.HealthEventWithStatus,
	incidentEvt *protos.HealthEvent,
	earliest, latest time.Time,
) bool {
	if ews.CreatedAt.Before(earliest) || ews.CreatedAt.After(latest) {
		return false
	}

	he, ok := ews.HealthEvent.(*protos.HealthEvent)
	if !ok || he == nil {
		return true
	}

	if he.GetId() == incidentEvt.GetId() && he.GetAgent() == healthEventsAnalyzerAgent {
		return false
	}

	return true
}

func severityFor(he *protos.HealthEvent) string {
	switch {
	case he.GetIsFatal():
		return "critical"
	case !he.GetIsHealthy():
		return "warning"
	default:
		return "info"
	}
}

func entitiesOfType(he *protos.HealthEvent, entityType string) []string {
	seen := map[string]struct{}{}

	for _, e := range he.GetEntitiesImpacted() {
		if strings.EqualFold(e.GetEntityType(), entityType) && e.GetEntityValue() != "" {
			seen[e.GetEntityValue()] = struct{}{}
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "..."
}
