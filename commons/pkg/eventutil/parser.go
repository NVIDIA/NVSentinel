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

	// Determine what to unmarshal: check if this is a change stream event with fullDocument
	var documentToUnmarshal interface{}
	if fullDoc, ok := event["fullDocument"]; ok {
		// This is a change stream event, extract the fullDocument
		documentToUnmarshal = fullDoc
	} else {
		// This is already the document itself
		documentToUnmarshal = event
	}

	// Convert to JSON and then unmarshal to get proper structure
	jsonBytes, err := json.Marshal(documentToUnmarshal)
	if err != nil {
		return healthEventWithStatus, fmt.Errorf("failed to marshal event to JSON: %w", err)
	}

	if err := json.Unmarshal(jsonBytes, &healthEventWithStatus); err != nil {
		return healthEventWithStatus, fmt.Errorf("failed to unmarshal health event: %w", err)
	}

	// Safety check - ensure HealthEvent is not nil
	if healthEventWithStatus.HealthEvent == nil {
		return healthEventWithStatus, fmt.Errorf("health event is nil after unmarshaling")
	}

	// Safety check - ensure HealthEventStatus is not nil
	if healthEventWithStatus.HealthEventStatus.NodeQuarantined == nil {
		return healthEventWithStatus, fmt.Errorf("node quarantined status is nil after unmarshaling")
	}

	return healthEventWithStatus, nil
}
