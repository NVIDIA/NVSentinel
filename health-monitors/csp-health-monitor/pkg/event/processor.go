// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. You may obtain a copy of
// the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations under
// the License.

package event

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/datastore"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

// Processor persists normalized maintenance events to the backing datastore.
// Any CSP-specific node-mapping must already have been resolved by the caller.
type Processor struct {
	store  datastore.Store
	config *config.Config
	mu     sync.Mutex
}

// NewProcessor returns an initialized Processor. k8sMapper parameter is
// removed.
func NewProcessor(cfg *config.Config, store datastore.Store) (*Processor, error) {
	if cfg == nil || store == nil {
		return nil, fmt.Errorf("unable to create processor with nil dependencies (config or store)")
	}

	return &Processor{
		config: cfg,
		store:  store,
	}, nil
}

// ensureClusterName sets the ClusterName on the event from config if missing.
func (p *Processor) ensureClusterName(event *model.MaintenanceEvent) {
	if event.ClusterName == "" && p.config.ClusterName != "" {
		event.ClusterName = p.config.ClusterName
		slog.Debug("Event missing cluster name; set from config",
			"eventID", event.EventID,
			"clusterName", event.ClusterName)
	} else if event.ClusterName == "" {
		slog.Warn("Event missing cluster name and no global config available",
			"eventID", event.EventID)
	}
}

// defaultStatus ensures event.Status has a default value.
func defaultStatus(event *model.MaintenanceEvent) {
	if event.Status == "" {
		slog.Warn("Event has empty status; defaulting",
			"eventID", event.EventID,
			"defaultStatus", model.StatusDetected)

		event.Status = model.StatusDetected
	}
}

// inheritState applies stateful inheritance based on event.Status.
func (p *Processor) inheritState(ctx context.Context, event *model.MaintenanceEvent) {
	// We only need NodeName to attempt a lookup.
	// MaintenanceType might be empty on the incoming event (e.g. sparse COMPLETED log)
	// and the specific inheritance functions will handle trying to find a match.
	if event.NodeName == "" {
		slog.Debug("Event missing NodeName; skipping inheritance",
			"eventID", event.EventID)

		return
	}

	switch event.Status {
	case model.StatusMaintenanceOngoing:
		p.inheritPendingToOngoing(
			ctx,
			event,
		) // This function expects event.MaintenanceType to be non-empty from normalizer if possible
	case model.StatusMaintenanceComplete:
		p.inheritOngoingToCompleted(ctx, event) // This function handles if event.MaintenanceType is initially empty
	case model.StatusDetected,
		model.StatusQuarantineTriggered,
		model.StatusHealthyTriggered,
		model.StatusNodeReadinessTimeout,
		model.StatusCancelled,
		model.StatusError:
		slog.Info("Event has status; no specific state inheritance rules apply",
			"eventID", event.EventID,
			"status", event.Status)
	default:
		// This case handles any unexpected or future InternalStatus values.
		slog.Warn(
			"Event has unhandled status in inheritState; no action taken.",
			"eventID", event.EventID,
			"status", event.Status,
		)
	}
}

// inheritPendingToOngoing inherits fields from a prior PENDING event.
//
//nolint:cyclop // splitting would reduce readability
func (p *Processor) inheritPendingToOngoing(ctx context.Context, event *model.MaintenanceEvent) {
	prior, found, err := p.store.FindLatestActiveEventByNodeAndType(
		ctx, event.NodeName, event.MaintenanceType, []model.InternalStatus{model.StatusDetected})
	if err != nil {
		slog.Warn("Error finding prior PENDING for event",
			"eventID", event.EventID,
			"error", err)

		return
	}

	if !found || prior == nil {
		return
	}

	if prior.ScheduledStartTime != nil {
		event.ScheduledStartTime = prior.ScheduledStartTime
	}

	if prior.ScheduledEndTime != nil {
		event.ScheduledEndTime = prior.ScheduledEndTime
	}

	event.MaintenanceType = prior.MaintenanceType
	if emptyOrDefault(event.ResourceID) && !emptyOrDefault(prior.ResourceID) {
		event.ResourceID = prior.ResourceID
	}

	if emptyOrDefault(event.ResourceType) && !emptyOrDefault(prior.ResourceType) {
		event.ResourceType = prior.ResourceType
	}
}

// inheritOngoingToCompleted inherits fields from a prior ONGOING event.
//
//nolint:cyclop // splitting would reduce readability
func (p *Processor) inheritOngoingToCompleted(ctx context.Context, event *model.MaintenanceEvent) {
	slog.Info(
		"[Processor.inheritOngoingToCompleted] Processing COMPLETED event, attempting to find latest ONGOING for node",
		"completedEventID",
		event.EventID,
		"nodeName",
		event.NodeName,
	)

	priorEvent, found, err := p.store.FindLatestOngoingEventByNode(ctx, event.NodeName)
	if err != nil {
		slog.Warn("[inheritOngoingToCompleted] Error finding prior ONGOING event for COMPLETED event; no inheritance applied",
			"eventID", event.EventID,
			"node", event.NodeName,
			"error", err)

		return
	}

	if !found || priorEvent == nil {
		slog.Warn(
			"[inheritOngoingToCompleted] No prior ONGOING event found for COMPLETED event"+
				"No inheritance applied.",
			"eventID", event.EventID,
			"nodeName", event.NodeName,
		)

		return
	}

	slog.Info("COMPLETED event before inheritance",
		"eventID", event.EventID,
		"maintenanceType", event.MaintenanceType,
		"scheduledStart", safeFormatTime(event.ScheduledStartTime),
		"actualStart", safeFormatTime(event.ActualStartTime),
	)
	slog.Info("Prior ONGOING event details",
		"priorEventID", priorEvent.EventID,
		"maintenanceType", priorEvent.MaintenanceType,
		"scheduledStart", safeFormatTime(priorEvent.ScheduledStartTime),
		"actualStart", safeFormatTime(priorEvent.ActualStartTime),
	)

	// Inherit from the found prior ONGOING event
	event.MaintenanceType = priorEvent.MaintenanceType // Take the type from the ONGOING event

	if priorEvent.ScheduledStartTime != nil {
		event.ScheduledStartTime = priorEvent.ScheduledStartTime
	}

	if priorEvent.ScheduledEndTime != nil {
		event.ScheduledEndTime = priorEvent.ScheduledEndTime
	}

	if priorEvent.ActualStartTime != nil {
		event.ActualStartTime = priorEvent.ActualStartTime
	}

	if emptyOrDefault(event.ResourceID) && !emptyOrDefault(priorEvent.ResourceID) {
		event.ResourceID = priorEvent.ResourceID
	}

	if emptyOrDefault(event.ResourceType) && !emptyOrDefault(priorEvent.ResourceType) {
		event.ResourceType = priorEvent.ResourceType
	}

	slog.Info("COMPLETED event after inheritance",
		"eventID", event.EventID,
		"maintenanceType", event.MaintenanceType,
		"scheduledStart", safeFormatTime(event.ScheduledStartTime),
		"actualStart", safeFormatTime(event.ActualStartTime),
	)
}

// emptyOrDefault returns true if s is empty or the default unknown placeholder.
func emptyOrDefault(s string) bool {
	return s == "" || s == defaultUnknown
}

// logEventDetails logs key event fields before upsert.
func (p *Processor) logEventDetails(event *model.MaintenanceEvent) {
	slog.Info("Event details before upsert",
		"EventID", event.EventID,
		"Cluster", event.ClusterName,
		"Node", event.NodeName,
		"MaintenanceType", event.MaintenanceType,
		"InternalStatus", event.Status,
		"CSPStatus", event.CSPStatus,
		"ScheduledStart", safeFormatTime(event.ScheduledStartTime),
		"ScheduledEnd", safeFormatTime(event.ScheduledEndTime),
		"ActualStart", safeFormatTime(event.ActualStartTime),
		"ActualEnd", safeFormatTime(event.ActualEndTime),
		"ResourceType", event.ResourceType,
		"ResourceID", event.ResourceID)
}

// logMissingNode warns if NodeName is missing but ResourceID is present.
func (p *Processor) logMissingNode(event *model.MaintenanceEvent) {
	if event.NodeName == "" && !emptyOrDefault(event.ResourceID) {
		slog.Warn(
			"Event has ResourceID but no NodeName; mapping may have failed",
			"eventID", event.EventID,
			"resourceID", event.ResourceID,
		)
	}
}

// ProcessEvent takes a normalized MaintenanceEvent and upserts it to the
// datastore. NodeName is expected to be pre-populated by the CSP client if
// mapping was successful.
func (p *Processor) ProcessEvent(ctx context.Context, event *model.MaintenanceEvent) error {
	if event == nil {
		metrics.MainProcessingErrors.WithLabelValues("unknown", "nil_event").Inc()
		return fmt.Errorf("cannot process nil event")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.ensureClusterName(event)
	defaultStatus(event)
	p.inheritState(ctx, event)
	p.logEventDetails(event)
	p.logMissingNode(event)

	metrics.MainDatastoreUpsertAttempts.WithLabelValues(string(event.CSP)).Inc()

	if err := p.store.UpsertMaintenanceEvent(ctx, event); err != nil {
		metrics.MainDatastoreUpsert.WithLabelValues(string(event.CSP), metrics.StatusFailed).Inc()
		metrics.MainProcessingErrors.WithLabelValues(string(event.CSP), "datastore_upsert").Inc()
		slog.Error("Failed to upsert event",
			"eventID", event.EventID,
			"error", err)

		return fmt.Errorf("failed to upsert event %s: %w", event.EventID, err)
	}

	metrics.MainDatastoreUpsert.WithLabelValues(string(event.CSP), metrics.StatusSuccess).Inc()

	slog.Debug("Processed event",
		"eventID", event.EventID,
		"node", event.NodeName)

	return nil
}
