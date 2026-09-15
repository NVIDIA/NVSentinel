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

package eventutil

import (
	"encoding/json"
	"fmt"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// ParseHealthEventFromEvent extracts and parses a health event from a database event.
// It handles both change stream events (with fullDocument) and direct document events.
// This is a shared utility used by multiple reconcilers (fault-remediation, node-drainer, etc.)
func ParseHealthEventFromEvent(event datastore.Event) (model.HealthEventWithStatus, error) {
	var healthEventWithStatus model.HealthEventWithStatus

	tempMap, err := healthEventDocumentMap(event)
	if err != nil {
		return healthEventWithStatus, err
	}

	normalizeProtoWrapperBoolFields(tempMap)

	jsonBytes, err := json.Marshal(tempMap)
	if err != nil {
		return healthEventWithStatus, fmt.Errorf("failed to marshal normalized health event: %w", err)
	}

	// Now unmarshal to the actual structure
	if err := json.Unmarshal(jsonBytes, &healthEventWithStatus); err != nil {
		return healthEventWithStatus, fmt.Errorf("failed to unmarshal health event: %w", err)
	}

	// Safety check - ensure HealthEvent is not nil
	if healthEventWithStatus.HealthEvent == nil {
		return healthEventWithStatus, fmt.Errorf("health event is nil after unmarshaling")
	}

	if healthEventWithStatus.HealthEventStatus == nil {
		return healthEventWithStatus, fmt.Errorf("health event status is nil after unmarshaling")
	}

	// Set default value for NodeQuarantined if nil (e.g., for new events)
	if healthEventWithStatus.HealthEventStatus.NodeQuarantined == "" {
		healthEventWithStatus.HealthEventStatus.NodeQuarantined = string(model.StatusNotStarted)
	}

	return healthEventWithStatus, nil
}

func healthEventDocumentMap(event datastore.Event) (map[string]any, error) {
	documentToUnmarshal := healthEventDocument(event)

	tempMap, err := toMap(documentToUnmarshal, "event")
	if err != nil {
		return nil, err
	}

	// PostgreSQL watcher events store the actual health event under "document".
	if doc, ok := tempMap["document"]; ok {
		return toMap(doc, "document field")
	}

	return tempMap, nil
}

func healthEventDocument(event datastore.Event) any {
	if fullDoc, ok := event["fullDocument"]; ok {
		return fullDoc
	}

	return event
}

func toMap(value any, name string) (map[string]any, error) {
	jsonBytes, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal %s to JSON: %w", name, err)
	}

	var result map[string]any
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal %s to map: %w", name, err)
	}

	return result, nil
}

func normalizeProtoWrapperBoolFields(document map[string]any) {
	for _, statusKey := range []string{"healtheventstatus", "healthEventStatus", "HealthEventStatus"} {
		status, ok := document[statusKey].(map[string]any)
		if !ok {
			continue
		}

		normalizeWrappedBoolField(status, "faultremediated")
		normalizeWrappedBoolField(status, "faultRemediated")
		normalizeWrappedBoolField(status, "FaultRemediated")
	}
}

func normalizeWrappedBoolField(document map[string]any, field string) {
	value, ok := document[field]
	if !ok {
		return
	}

	if boolValue, ok := value.(bool); ok {
		document[field] = map[string]any{"value": boolValue}
	}
}
