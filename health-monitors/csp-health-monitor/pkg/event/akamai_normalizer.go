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

package event

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/akamai"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

const (
	akamaiResourceType = "akamai_instance"

	akamaiStatusCompleted = "completed"
)

// AkamaiEventIDPrefix starts every event ID this normalizer produces. It is
// exported so the Akamai monitor can match listed entries to stored events.
const AkamaiEventIDPrefix = "akamai-"

// Akamai maintenance statuses, as the API reports them. They are exported
// because the Akamai monitor reconciles stored events against them. The monitor
// does not rely on the API returning canceled, so it writes that value itself.
const (
	AkamaiStatusPending    = "pending"
	AkamaiStatusScheduled  = "scheduled"
	AkamaiStatusStarted    = "started"
	AkamaiStatusInProgress = "in-progress"
	AkamaiStatusCanceled   = "canceled"
)

// AkamaiEventContext carries what the Akamai client resolved about the event
// that the raw maintenance record does not hold.
type AkamaiEventContext struct {
	NodeName    string
	ClusterName string
}

// AkamaiNormalizer implements the Normalizer interface for Akamai Cloud
// maintenance events.
type AkamaiNormalizer struct{}

var _ Normalizer = (*AkamaiNormalizer)(nil)

// Normalize converts an Akamai maintenance event into a MaintenanceEvent.
// rawEvent is an akamai.Maintenance and additionalInfo[0] an AkamaiEventContext.
//
// The maintenance type and policy are carried through in Metadata but do not
// change the outcome: a GPU node cannot live-migrate with its workload, so no
// type is treated as harmless.
func (n *AkamaiNormalizer) Normalize(
	rawEvent any, additionalInfo ...any,
) (*model.MaintenanceEvent, error) {
	raw, ok := rawEvent.(akamai.Maintenance)
	if !ok {
		return nil, fmt.Errorf("AkamaiNormalizer: expected akamai.Maintenance, got %T", rawEvent)
	}

	if len(additionalInfo) < 1 {
		return nil, fmt.Errorf("AkamaiNormalizer: missing AkamaiEventContext")
	}

	eventCtx, ok := additionalInfo[0].(AkamaiEventContext)
	if !ok {
		return nil, fmt.Errorf("AkamaiNormalizer: expected AkamaiEventContext, got %T", additionalInfo[0])
	}

	if raw.ID <= 0 {
		return nil, fmt.Errorf("AkamaiNormalizer: event has invalid id %d", raw.ID)
	}

	internalStatus, actualStart, actualEnd := mapAkamaiStatus(raw)
	emergency := raw.Description == akamai.MaintenanceDescriptionEmergency

	// Emergency events have no natural scheduledStartTime, so both fields are
	// left nil and FindEmergencyEventsToTriggerQuarantine, which filters on
	// metadata.urgency and applies no time window, fires the quarantine.
	// Anything else is quarantined ahead of its start time. The API reports no
	// end time, so ScheduledEndTime stays nil.
	var scheduledStart *time.Time
	if !emergency {
		scheduledStart = firstTime(raw.When, raw.NotBefore)
	}

	maintenanceType := model.TypeScheduled
	if emergency {
		maintenanceType = model.TypeUnscheduled
	}

	slog.Debug("Normalizing Akamai event",
		"eventID", raw.ID, "type", raw.Type, "status", raw.Status, "node", eventCtx.NodeName)

	now := time.Now().UTC()

	return &model.MaintenanceEvent{
		EventID:                AkamaiEventIDPrefix + strconv.Itoa(raw.ID),
		CSP:                    model.CSPAkamai,
		ClusterName:            eventCtx.ClusterName,
		ResourceType:           akamaiResourceType,
		ResourceID:             strconv.Itoa(raw.Entity.ID),
		NodeName:               eventCtx.NodeName,
		MaintenanceType:        maintenanceType,
		Status:                 internalStatus,
		CSPStatus:              model.ProviderStatus(raw.Status),
		ScheduledStartTime:     scheduledStart,
		ActualStartTime:        actualStart,
		ActualEndTime:          actualEnd,
		EventReceivedTimestamp: now,
		LastUpdatedTimestamp:   now,
		RecommendedAction:      "NONE",
		Metadata:               akamaiMetadata(raw, emergency),
	}, nil
}

// akamaiMetadata builds the event metadata, including the change fingerprint.
func akamaiMetadata(raw akamai.Maintenance, emergency bool) map[string]string {
	metadata := map[string]string{
		"maintenanceType":            raw.Type,
		"policy":                     raw.MaintenancePolicySet,
		"reason":                     raw.Reason,
		"source":                     raw.Source,
		"urgency":                    raw.Description,
		model.ProviderLastUpdatedKey: akamaiFingerprint(raw),
	}

	// The emergency trigger query matches this exact value, so write the shared
	// constant rather than relying on the API's spelling.
	if emergency {
		metadata["urgency"] = model.MetadataUrgencyEmergency
	}

	return metadata
}

// akamaiFingerprint stands in for the provider's last-updated timestamp, which
// the Akamai API does not report. The datastore skips an upsert when this value
// is unchanged from the stored one, which is what stops each poll from
// resetting an event we already moved to QUARANTINE_TRIGGERED back to
// DETECTED and firing the trigger again. It only has to change whenever
// anything the normalizer reads changes, so it joins those fields.
func akamaiFingerprint(raw akamai.Maintenance) string {
	return strings.Join([]string{
		raw.Status,
		raw.Type,
		raw.Description,
		formatTime(raw.When),
		formatTime(raw.NotBefore),
		formatTime(raw.StartTime),
		formatTime(raw.CompleteTime),
	}, "|")
}

// mapAkamaiStatus converts an Akamai maintenance status to the internal status
// and the actual start and end times it implies. A completed event with no
// complete_time is stamped with now so the healthy trigger, which needs an end
// time, can still fire; the datastore preserves the first one it stores.
func mapAkamaiStatus(raw akamai.Maintenance) (model.InternalStatus, *time.Time, *time.Time) {
	now := time.Now().UTC()

	switch raw.Status {
	case AkamaiStatusPending, AkamaiStatusScheduled:
		return model.StatusDetected, nil, nil
	case AkamaiStatusStarted, AkamaiStatusInProgress:
		start := firstTime(raw.StartTime)
		if start == nil {
			start = &now
		}

		return model.StatusMaintenanceOngoing, start, nil
	case akamaiStatusCompleted:
		end := firstTime(raw.CompleteTime)
		if end == nil {
			end = &now
		}

		return model.StatusMaintenanceComplete, firstTime(raw.StartTime), end
	case AkamaiStatusCanceled:
		return model.StatusCancelled, nil, nil
	default:
		slog.Warn("AkamaiNormalizer: unknown status, defaulting to DETECTED", "status", raw.Status)

		return model.StatusDetected, nil, nil
	}
}

// firstTime returns the first non-nil, non-zero timestamp as a UTC time, or nil.
func firstTime(candidates ...*akamai.Timestamp) *time.Time {
	for _, c := range candidates {
		if c != nil && !c.IsZero() {
			t := c.UTC()

			return &t
		}
	}

	return nil
}

// formatTime renders a timestamp for the fingerprint, empty when unset.
func formatTime(t *akamai.Timestamp) string {
	if t == nil || t.IsZero() {
		return ""
	}

	return t.UTC().Format(time.RFC3339)
}
