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

package reconciler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestEquivalenceGroupStateWithActionName(t *testing.T) {
	// Test that our enhanced EquivalenceGroupState can store action name
	state := EquivalenceGroupState{
		MaintenanceCR: "maintenance-node1-event123",
		ActionName:    protos.RecommendedAction_RESTART_BM.String(),
		CreatedAt:     time.Now().UTC(),
	}

	// Test JSON marshaling/unmarshaling
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("Failed to marshal state: %v", err)
	}

	var unmarshaled EquivalenceGroupState
	err = json.Unmarshal(data, &unmarshaled)
	if err != nil {
		t.Fatalf("Failed to unmarshal state: %v", err)
	}

	// Verify all fields are preserved
	if unmarshaled.MaintenanceCR != state.MaintenanceCR {
		t.Errorf("MaintenanceCR not preserved: expected %s, got %s",
			state.MaintenanceCR, unmarshaled.MaintenanceCR)
	}

	if unmarshaled.ActionName != state.ActionName {
		t.Errorf("ActionName not preserved: expected %s, got %s",
			state.ActionName, unmarshaled.ActionName)
	}

	// CreatedAt should be close (within 1 second due to precision)
	timeDiff := unmarshaled.CreatedAt.Sub(state.CreatedAt)
	if timeDiff > time.Second || timeDiff < -time.Second {
		t.Errorf("CreatedAt not preserved accurately: expected %v, got %v",
			state.CreatedAt, unmarshaled.CreatedAt)
	}
}

func TestRemediationStateAnnotationWithMultipleActions(t *testing.T) {
	// Test annotation with different actions in same equivalence group
	annotation := RemediationStateAnnotation{
		EquivalenceGroups: map[string]EquivalenceGroupState{
			"restart": {
				MaintenanceCR: "maintenance-node1-event456",
				ActionName:    protos.RecommendedAction_RESTART_BM.String(),
				CreatedAt:     time.Now().UTC(),
			},
			"gpu-reset": {
				MaintenanceCR: "maintenance-node1-event789",
				ActionName:    protos.RecommendedAction_COMPONENT_RESET.String(),
				CreatedAt:     time.Now().UTC(),
			},
		},
	}

	// Test JSON serialization
	data, err := json.Marshal(annotation)
	if err != nil {
		t.Fatalf("Failed to marshal annotation: %v", err)
	}

	var unmarshaled RemediationStateAnnotation
	err = json.Unmarshal(data, &unmarshaled)
	if err != nil {
		t.Fatalf("Failed to unmarshal annotation: %v", err)
	}

	// Verify structure is preserved
	if len(unmarshaled.EquivalenceGroups) != 2 {
		t.Errorf("Expected 2 equivalence groups, got %d", len(unmarshaled.EquivalenceGroups))
	}

	// Verify restart group
	restartGroup, exists := unmarshaled.EquivalenceGroups["restart"]
	if !exists {
		t.Fatal("restart group not found")
	}

	if restartGroup.ActionName != protos.RecommendedAction_RESTART_BM.String() {
		t.Errorf("Expected RESTART_BM action name, got %s", restartGroup.ActionName)
	}

	// Verify gpu-reset group
	gpuResetGroup, exists := unmarshaled.EquivalenceGroups["gpu-reset"]
	if !exists {
		t.Fatal("gpu-reset group not found")
	}

	if gpuResetGroup.ActionName != protos.RecommendedAction_COMPONENT_RESET.String() {
		t.Errorf("Expected COMPONENT_RESET action name, got %s", gpuResetGroup.ActionName)
	}
}

func TestBackwardCompatibility(t *testing.T) {
	// Test that old JSON format (without ActionName) can still be unmarshaled
	oldFormatJSON := `{
		"equivalenceGroups": {
			"restart": {
				"maintenanceCR": "maintenance-node1-old",
				"createdAt": "2023-01-01T00:00:00Z"
			}
		}
	}`

	var state RemediationStateAnnotation
	err := json.Unmarshal([]byte(oldFormatJSON), &state)
	if err != nil {
		t.Fatalf("Failed to unmarshal old format: %v", err)
	}

	restartGroup, exists := state.EquivalenceGroups["restart"]
	if !exists {
		t.Fatal("restart group not found")
	}

	if restartGroup.MaintenanceCR != "maintenance-node1-old" {
		t.Errorf("MaintenanceCR not preserved: got %s", restartGroup.MaintenanceCR)
	}

	// ActionName should be empty for old format
	if restartGroup.ActionName != "" {
		t.Errorf("Expected empty ActionName for old format, got %s", restartGroup.ActionName)
	}
}
