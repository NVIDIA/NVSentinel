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
	"time"

	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

const (
	// UrgencyEmergency maps to the Lambda API urgency value.
	UrgencyEmergency = "emergency"
	// UrgencyCriticalWithDeadline maps to the Lambda API urgency value.
	UrgencyCriticalWithDeadline = "critical_with_deadline"

	lambdaResourceType = "lambda_instance"
)

// LambdaEventMetadata carries all fields needed to normalize a Lambda event.
// The lambda.Client populates this from the raw Event before calling Normalize,
// avoiding an import cycle between the event and lambda packages.
type LambdaEventMetadata struct {
	ID                string
	Detail            string
	Urgency           string
	Status            string
	NotBefore         *time.Time
	NotBeforeDeadline *time.Time // nil if event cannot be rescheduled
	NotAfter          *time.Time // nil if not provided
	NodeName          string
	ClusterName       string
	// TriggerTimeLimit is the configured quarantine workflow time limit.
	// Emergency events set scheduledStartTime = now + TriggerTimeLimit so the
	// trigger-engine query (scheduledStartTime <= now+limit) keeps finding them
	// across every poll cycle until they leave DETECTED status.
	TriggerTimeLimit time.Duration
}

// LambdaNormalizer implements the Normalizer interface for Lambda mock events.
type LambdaNormalizer struct{}

var _ Normalizer = (*LambdaNormalizer)(nil)

// Normalize converts a Lambda mock event into a MaintenanceEvent.
// rawEvent is unused; all fields are conveyed via additionalInfo[0] (LambdaEventMetadata).
func (n *LambdaNormalizer) Normalize(rawEvent interface{}, additionalInfo ...interface{}) (*model.MaintenanceEvent, error) {
	if len(additionalInfo) < 1 {
		return nil, fmt.Errorf("LambdaNormalizer: missing LambdaEventMetadata")
	}

	meta, ok := additionalInfo[0].(LambdaEventMetadata)
	if !ok {
		return nil, fmt.Errorf("LambdaNormalizer: expected LambdaEventMetadata, got %T", additionalInfo[0])
	}

	if meta.ID == "" {
		return nil, fmt.Errorf("LambdaNormalizer: event has empty id")
	}

	internalStatus, cspStatus, actualStartTime, actualEndTime := mapLambdaStatus(meta.Status)

	// Map urgency → scheduling fields.
	// EMERGENCY:              ScheduledStartTime = now so the emergency query fires on next poll.
	// CRITICAL_WITH_DEADLINE: ScheduledStartTime = not_before, ScheduledEndTime = not_before_deadline.
	scheduledStartTime := meta.NotBefore
	scheduledEndTime := meta.NotBeforeDeadline

	if meta.Urgency == UrgencyEmergency {
		// Set scheduledStartTime far enough in the future that the trigger-engine
		// query (scheduledStartTime <= now+triggerTimeLimit) keeps finding this
		// event across every poll cycle. Without this, an event stamped "now" at
		// normalization time falls below the query's lower bound 60s later.
		limit := meta.TriggerTimeLimit
		if limit <= 0 {
			limit = 30 * time.Minute // safe fallback matching config default
		}
		t := time.Now().UTC().Add(limit)
		scheduledStartTime = &t
		scheduledEndTime = nil
	}

	metadata := map[string]string{
		"urgency": meta.Urgency,
		"detail":  meta.Detail,
	}

	if meta.NotAfter != nil {
		metadata["notAfter"] = meta.NotAfter.Format(time.RFC3339)
	}

	slog.Debug("Normalizing Lambda mock event",
		"eventID", meta.ID,
		"urgency", meta.Urgency,
		"status", meta.Status,
		"node", meta.NodeName)

	return &model.MaintenanceEvent{
		EventID:                meta.ID,
		CSP:                    "lambda",
		ClusterName:            meta.ClusterName,
		ResourceType:           lambdaResourceType,
		ResourceID:             meta.ID,
		NodeName:               meta.NodeName,
		MaintenanceType:        model.TypeScheduled,
		Status:                 internalStatus,
		CSPStatus:              cspStatus,
		ScheduledStartTime:     scheduledStartTime,
		ScheduledEndTime:       scheduledEndTime,
		ActualStartTime:        actualStartTime,
		ActualEndTime:          actualEndTime,
		EventReceivedTimestamp: time.Now().UTC(),
		LastUpdatedTimestamp:   time.Now().UTC(),
		RecommendedAction:      "NONE",
		Metadata:               metadata,
	}, nil
}

// mapLambdaStatus converts a Lambda API status string to NVSentinel internal statuses.
func mapLambdaStatus(status string) (model.InternalStatus, model.ProviderStatus, *time.Time, *time.Time) {
	now := time.Now().UTC()

	switch status {
	case "scheduled":
		return model.StatusDetected, model.ProviderStatus("scheduled"), nil, nil
	case "in_progress":
		return model.StatusMaintenanceOngoing, model.ProviderStatus("in_progress"), &now, nil
	case "completed":
		return model.StatusMaintenanceComplete, model.ProviderStatus("completed"), nil, &now
	case "canceled":
		return model.StatusCancelled, model.ProviderStatus("canceled"), nil, nil
	default:
		slog.Warn("LambdaNormalizer: unknown status, defaulting to DETECTED", "status", status)
		return model.StatusDetected, model.ProviderStatus(status), nil, nil
	}
}
