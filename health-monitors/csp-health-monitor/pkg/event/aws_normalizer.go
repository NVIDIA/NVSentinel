// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/health/types"
)

type EventMetadata struct {
	Event            types.Event
	NodeName         string
	InstanceId       string
	EntityArn        string
	ClusterName      string
	Action           string
	EventDescription string
}

// AWSNormalizer implements the Normalizer interface for AWS Cloud health events.
type AWSNormalizer struct{}

// Ensure AWSNormalizer implements the Normalizer interface.
var _ Normalizer = (*AWSNormalizer)(nil)

// validateAWSNormalizerInput checks the input to the Normalize function.
func validateAWSNormalizerInput(
	rawEvent interface{},
	additionalInfo ...interface{},
) (types.Event, EventMetadata, error) {
	event, ok := rawEvent.(types.Event)
	if !ok {
		return types.Event{}, EventMetadata{}, fmt.Errorf(
			"error normalizing AWS event: expected types.Event, got %T",
			rawEvent,
		)
	}

	if len(additionalInfo) < 1 {
		return types.Event{}, EventMetadata{}, fmt.Errorf("missing additional metadata for AWS event")
	}

	meta, ok := additionalInfo[0].(EventMetadata)
	if !ok {
		return types.Event{}, EventMetadata{}, fmt.Errorf(
			"invalid metadata type: expected EventMetadata, got %T",
			additionalInfo[0],
		)
	}

	if event.Arn == nil || event.Service == nil || event.EventTypeCode == nil ||
		event.StartTime == nil || event.EndTime == nil {
		return types.Event{}, EventMetadata{}, fmt.Errorf(
			"AWS event %s has missing required fields",
			aws.ToString(event.Arn),
		)
	}

	return event, meta, nil
}

// Normalize converts a AWS Cloud health events into a standard MaintenanceEvent.
// It expects rawEvent to be of type types.Event.
func (n *AWSNormalizer) Normalize(
	rawEvent interface{},
	additionalInfo ...interface{},
) (*model.MaintenanceEvent, error) {
	event, meta, err := validateAWSNormalizerInput(rawEvent, additionalInfo...)
	if err != nil {
		return nil, fmt.Errorf("AWS normalizer input validation failed: %w", err)
	}

	slog.Debug("Normalizing AWS event",
		"eventArn", aws.ToString(event.Arn),
		"node", meta.NodeName,
		"instanceID", meta.InstanceId)

	// always scheduled type for AWS
	maintenanceType := model.TypeScheduled

	var status model.InternalStatus

	var actualStartTime *time.Time

	var actualEndTime *time.Time

	switch event.StatusCode {
	case types.EventStatusCodeUpcoming:
		status = model.StatusDetected
	case types.EventStatusCodeOpen:
		status = model.StatusMaintenanceOngoing
		now := time.Now().UTC()
		actualStartTime = &now
	case types.EventStatusCodeClosed:
		status = model.StatusMaintenanceComplete
		now := time.Now().UTC()
		actualEndTime = &now
	default:
		slog.Error("Unknown event status code found", "event", event)
		return nil, fmt.Errorf("unknown event status code found %+v", event)
	}

	cspStatus := model.ProviderStatus(string(event.StatusCode))

	// Convert time values to pointers for the model
	startTime := *event.StartTime
	endTime := *event.EndTime
	// Create normalized event
	normalizedEvent := &model.MaintenanceEvent{
		EventID:                meta.EntityArn,
		CSP:                    model.CSPAWS,
		ClusterName:            meta.ClusterName,
		ResourceType:           *event.Service,
		ResourceID:             meta.InstanceId,
		NodeName:               meta.NodeName,
		MaintenanceType:        maintenanceType,
		Status:                 status,
		CSPStatus:              cspStatus,
		ScheduledStartTime:     &startTime,
		ScheduledEndTime:       &endTime,
		ActualStartTime:        actualStartTime,
		ActualEndTime:          actualEndTime,
		EventReceivedTimestamp: time.Now().UTC(),
		LastUpdatedTimestamp:   time.Now().UTC(),
		RecommendedAction:      meta.Action,
		Metadata: map[string]string{
			"eventArn":       aws.ToString(event.Arn),
			"eventTypeCode":  *event.EventTypeCode,
			"eventScopeCode": string(event.EventScopeCode),
			"description":    meta.EventDescription,
		},
	}

	slog.Debug("Normalized AWS event",
		"node", meta.NodeName,
		"eventID", normalizedEvent.EventID,
		"status", normalizedEvent.Status)

	return normalizedEvent, nil
}
