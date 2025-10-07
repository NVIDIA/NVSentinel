// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package healthEventsAnnotation

import (
	"encoding/json"
	"testing"

	platformconnectorprotos "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
)

// TestHealthEventsAnnotationMap_AddOrUpdateEvent tests adding and updating events
func TestHealthEventsAnnotationMap_AddOrUpdateEvent(t *testing.T) {
	tests := []struct {
		name          string
		events        []*platformconnectorprotos.HealthEvent
		expectedAdded []bool
		expectedCount int
	}{
		{
			name: "single entity event",
			events: []*platformconnectorprotos.HealthEvent{
				{
					Agent:          "gpu-health-monitor",
					ComponentClass: "GPU",
					CheckName:      "GpuXidError",
					NodeName:       "node1",
					IsFatal:        true,
					IsHealthy:      false,
					ErrorCode:      []string{"62"},
					EntitiesImpacted: []*platformconnectorprotos.Entity{
						{EntityType: "GPU", EntityValue: "1"},
					},
				},
			},
			expectedAdded: []bool{true},
			expectedCount: 1,
		},
		{
			name: "multiple entities in single event",
			events: []*platformconnectorprotos.HealthEvent{
				{
					Agent:          "gpu-health-monitor",
					ComponentClass: "GPU",
					CheckName:      "GpuXidError",
					NodeName:       "node1",
					IsFatal:        true,
					IsHealthy:      false,
					ErrorCode:      []string{"62"},
					EntitiesImpacted: []*platformconnectorprotos.Entity{
						{EntityType: "GPU", EntityValue: "1"},
						{EntityType: "GPU", EntityValue: "2"},
						{EntityType: "GPU", EntityValue: "3"},
					},
				},
			},
			expectedAdded: []bool{true},
			expectedCount: 3, // Each entity tracked separately
		},
		{
			name: "duplicate event should not be added",
			events: []*platformconnectorprotos.HealthEvent{
				{
					Agent:          "gpu-health-monitor",
					ComponentClass: "GPU",
					CheckName:      "GpuXidError",
					NodeName:       "node1",
					IsFatal:        true,
					IsHealthy:      false,
					ErrorCode:      []string{"62"},
					EntitiesImpacted: []*platformconnectorprotos.Entity{
						{EntityType: "GPU", EntityValue: "1"},
					},
				},
				{
					Agent:          "gpu-health-monitor",
					ComponentClass: "GPU",
					CheckName:      "GpuXidError",
					NodeName:       "node1",
					IsFatal:        true,
					IsHealthy:      false,
					ErrorCode:      []string{"63"}, // Different error code, but same key
					EntitiesImpacted: []*platformconnectorprotos.Entity{
						{EntityType: "GPU", EntityValue: "1"}, // Same entity
					},
				},
			},
			expectedAdded: []bool{true, false},
			expectedCount: 1,
		},
		{
			name: "different entities should be tracked separately",
			events: []*platformconnectorprotos.HealthEvent{
				{
					Agent:          "gpu-health-monitor",
					ComponentClass: "GPU",
					CheckName:      "GpuXidError",
					NodeName:       "node1",
					EntitiesImpacted: []*platformconnectorprotos.Entity{
						{EntityType: "GPU", EntityValue: "1"},
					},
				},
				{
					Agent:          "gpu-health-monitor",
					ComponentClass: "GPU",
					CheckName:      "GpuXidError",
					NodeName:       "node1",
					EntitiesImpacted: []*platformconnectorprotos.Entity{
						{EntityType: "GPU", EntityValue: "2"},
					},
				},
			},
			expectedAdded: []bool{true, true},
			expectedCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hem := NewHealthEventsAnnotationMap()

			for i, event := range tt.events {
				added := hem.AddOrUpdateEvent(event)
				if added != tt.expectedAdded[i] {
					t.Errorf("AddOrUpdateEvent() for event %d = %v, want %v", i, added, tt.expectedAdded[i])
				}
			}

			if count := hem.Count(); count != tt.expectedCount {
				t.Errorf("Count() = %v, want %v", count, tt.expectedCount)
			}
		})
	}
}

// TestHealthEventsAnnotationMap_GetEvent tests retrieving events
func TestHealthEventsAnnotationMap_GetEvent(t *testing.T) {
	hem := NewHealthEventsAnnotationMap()

	// Add an event
	originalEvent := &platformconnectorprotos.HealthEvent{
		Agent:          "gpu-health-monitor",
		ComponentClass: "GPU",
		CheckName:      "GpuXidError",
		NodeName:       "node1",
		IsFatal:        true,
		IsHealthy:      false,
		ErrorCode:      []string{"62"},
		EntitiesImpacted: []*platformconnectorprotos.Entity{
			{EntityType: "GPU", EntityValue: "1"},
		},
	}
	hem.AddOrUpdateEvent(originalEvent)

	tests := []struct {
		name        string
		queryEvent  *platformconnectorprotos.HealthEvent
		shouldFind  bool
		description string
	}{
		{
			name: "exact match should be found",
			queryEvent: &platformconnectorprotos.HealthEvent{
				Agent:          "gpu-health-monitor",
				ComponentClass: "GPU",
				CheckName:      "GpuXidError",
				NodeName:       "node1",
				EntitiesImpacted: []*platformconnectorprotos.Entity{
					{EntityType: "GPU", EntityValue: "1"},
				},
			},
			shouldFind:  true,
			description: "Same key fields should match",
		},
		{
			name: "healthy event should match unhealthy",
			queryEvent: &platformconnectorprotos.HealthEvent{
				Agent:          "gpu-health-monitor",
				ComponentClass: "GPU",
				CheckName:      "GpuXidError",
				NodeName:       "node1",
				IsHealthy:      true, // Different from stored event
				EntitiesImpacted: []*platformconnectorprotos.Entity{
					{EntityType: "GPU", EntityValue: "1"},
				},
			},
			shouldFind:  true,
			description: "IsHealthy should not affect matching",
		},
		{
			name: "different entity should not match",
			queryEvent: &platformconnectorprotos.HealthEvent{
				Agent:          "gpu-health-monitor",
				ComponentClass: "GPU",
				CheckName:      "GpuXidError",
				NodeName:       "node1",
				EntitiesImpacted: []*platformconnectorprotos.Entity{
					{EntityType: "GPU", EntityValue: "2"}, // Different entity
				},
			},
			shouldFind:  false,
			description: "Different entity should not match",
		},
		{
			name: "different check name should not match",
			queryEvent: &platformconnectorprotos.HealthEvent{
				Agent:          "gpu-health-monitor",
				ComponentClass: "GPU",
				CheckName:      "NVLinkError", // Different check
				NodeName:       "node1",
				EntitiesImpacted: []*platformconnectorprotos.Entity{
					{EntityType: "GPU", EntityValue: "1"},
				},
			},
			shouldFind:  false,
			description: "Different check name should not match",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storedEvent, found := hem.GetEvent(tt.queryEvent)
			if found != tt.shouldFind {
				t.Errorf("GetEvent() found = %v, want %v (%s)", found, tt.shouldFind, tt.description)
			}
			if found && storedEvent == nil {
				t.Errorf("GetEvent() returned found=true but storedEvent=nil")
			}
			if !found && storedEvent != nil {
				t.Errorf("GetEvent() returned found=false but storedEvent!=nil")
			}
		})
	}
}

// TestHealthEventsAnnotationMap_RemoveEvent tests removing events
func TestHealthEventsAnnotationMap_RemoveEvent(t *testing.T) {
	tests := []struct {
		name            string
		initialEvents   []*platformconnectorprotos.HealthEvent
		removeEvent     *platformconnectorprotos.HealthEvent
		expectedRemoved int
		expectedCount   int
	}{
		{
			name: "remove single entity",
			initialEvents: []*platformconnectorprotos.HealthEvent{
				{
					Agent:          "gpu-health-monitor",
					ComponentClass: "GPU",
					CheckName:      "GpuXidError",
					NodeName:       "node1",
					EntitiesImpacted: []*platformconnectorprotos.Entity{
						{EntityType: "GPU", EntityValue: "1"},
						{EntityType: "GPU", EntityValue: "2"},
					},
				},
			},
			removeEvent: &platformconnectorprotos.HealthEvent{
				Agent:          "gpu-health-monitor",
				ComponentClass: "GPU",
				CheckName:      "GpuXidError",
				NodeName:       "node1",
				IsHealthy:      true, // Healthy event
				EntitiesImpacted: []*platformconnectorprotos.Entity{
					{EntityType: "GPU", EntityValue: "1"}, // Remove only GPU 1
				},
			},
			expectedRemoved: 1,
			expectedCount:   1, // GPU 2 remains
		},
		{
			name: "remove multiple entities",
			initialEvents: []*platformconnectorprotos.HealthEvent{
				{
					Agent:          "gpu-health-monitor",
					ComponentClass: "GPU",
					CheckName:      "GpuXidError",
					NodeName:       "node1",
					EntitiesImpacted: []*platformconnectorprotos.Entity{
						{EntityType: "GPU", EntityValue: "1"},
						{EntityType: "GPU", EntityValue: "2"},
						{EntityType: "GPU", EntityValue: "3"},
					},
				},
			},
			removeEvent: &platformconnectorprotos.HealthEvent{
				Agent:          "gpu-health-monitor",
				ComponentClass: "GPU",
				CheckName:      "GpuXidError",
				NodeName:       "node1",
				IsHealthy:      true,
				EntitiesImpacted: []*platformconnectorprotos.Entity{
					{EntityType: "GPU", EntityValue: "1"},
					{EntityType: "GPU", EntityValue: "3"}, // Remove GPU 1 and 3
				},
			},
			expectedRemoved: 2,
			expectedCount:   1, // GPU 2 remains
		},
		{
			name: "remove non-existent entity",
			initialEvents: []*platformconnectorprotos.HealthEvent{
				{
					Agent:          "gpu-health-monitor",
					ComponentClass: "GPU",
					CheckName:      "GpuXidError",
					NodeName:       "node1",
					EntitiesImpacted: []*platformconnectorprotos.Entity{
						{EntityType: "GPU", EntityValue: "1"},
					},
				},
			},
			removeEvent: &platformconnectorprotos.HealthEvent{
				Agent:          "gpu-health-monitor",
				ComponentClass: "GPU",
				CheckName:      "GpuXidError",
				NodeName:       "node1",
				IsHealthy:      true,
				EntitiesImpacted: []*platformconnectorprotos.Entity{
					{EntityType: "GPU", EntityValue: "99"}, // Non-existent
				},
			},
			expectedRemoved: 0,
			expectedCount:   1, // Original remains
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hem := NewHealthEventsAnnotationMap()

			// Add initial events
			for _, event := range tt.initialEvents {
				hem.AddOrUpdateEvent(event)
			}

			// Remove event
			removed := hem.RemoveEvent(tt.removeEvent)
			if removed != tt.expectedRemoved {
				t.Errorf("RemoveEvent() removed = %v, want %v", removed, tt.expectedRemoved)
			}

			if count := hem.Count(); count != tt.expectedCount {
				t.Errorf("Count() after removal = %v, want %v", count, tt.expectedCount)
			}
		})
	}
}

// TestHealthEventsAnnotationMap_GetAllCheckNames tests getting all check names
func TestHealthEventsAnnotationMap_GetAllCheckNames(t *testing.T) {
	hem := NewHealthEventsAnnotationMap()

	// Add events with different check names
	events := []*platformconnectorprotos.HealthEvent{
		{
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			CheckName:      "GpuXidError",
			NodeName:       "node1",
			EntitiesImpacted: []*platformconnectorprotos.Entity{
				{EntityType: "GPU", EntityValue: "1"},
				{EntityType: "GPU", EntityValue: "2"},
			},
		},
		{
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			CheckName:      "GpuNvLinkWatch",
			NodeName:       "node1",
			EntitiesImpacted: []*platformconnectorprotos.Entity{
				{EntityType: "GPU", EntityValue: "1"},
			},
		},
		{
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			CheckName:      "GpuThermalWatch",
			NodeName:       "node1",
			EntitiesImpacted: []*platformconnectorprotos.Entity{
				{EntityType: "GPU", EntityValue: "3"},
			},
		},
	}

	for _, event := range events {
		hem.AddOrUpdateEvent(event)
	}

	checkNames := hem.GetAllCheckNames()
	expectedCheckNames := []string{"GpuXidError", "GpuNvLinkWatch", "GpuThermalWatch"}

	if len(checkNames) != len(expectedCheckNames) {
		t.Errorf("GetAllCheckNames() returned %d names, want %d", len(checkNames), len(expectedCheckNames))
	}

	checkNameMap := make(map[string]bool)
	for _, name := range checkNames {
		checkNameMap[name] = true
	}

	for _, expectedName := range expectedCheckNames {
		if !checkNameMap[expectedName] {
			t.Errorf("GetAllCheckNames() missing expected check name: %s", expectedName)
		}
	}
}

// TestHealthEventsAnnotationMap_JSONSerialization tests JSON marshaling and unmarshaling
func TestHealthEventsAnnotationMap_JSONSerialization(t *testing.T) {
	// Create original map with events
	originalMap := NewHealthEventsAnnotationMap()

	event1 := &platformconnectorprotos.HealthEvent{
		Version:        1,
		Agent:          "gpu-health-monitor",
		ComponentClass: "GPU",
		CheckName:      "GpuXidError",
		NodeName:       "node1",
		IsFatal:        true,
		IsHealthy:      false,
		Message:        "XID error occurred",
		EntitiesImpacted: []*platformconnectorprotos.Entity{
			{EntityType: "GPU", EntityValue: "1"},
		},
	}

	event2 := &platformconnectorprotos.HealthEvent{
		Version:        1,
		Agent:          "gpu-health-monitor",
		ComponentClass: "GPU",
		CheckName:      "GpuNvLinkWatch",
		NodeName:       "node1",
		IsFatal:        true,
		IsHealthy:      false,
		Message:        "NVLink 0 is down",
		EntitiesImpacted: []*platformconnectorprotos.Entity{
			{EntityType: "GPU", EntityValue: "1"},
		},
	}

	originalMap.AddOrUpdateEvent(event1)
	originalMap.AddOrUpdateEvent(event2)

	// Test marshaling
	jsonData, err := json.Marshal(originalMap)
	if err != nil {
		t.Fatalf("Failed to marshal HealthEventsAnnotationMap: %v", err)
	}

	// Verify JSON structure - should be an array of events
	var jsonArray []map[string]interface{}
	if err := json.Unmarshal(jsonData, &jsonArray); err != nil {
		t.Fatalf("Marshaled JSON is not an array: %v", err)
	}

	if len(jsonArray) != 2 {
		t.Errorf("Expected 2 events in JSON array, got %d", len(jsonArray))
	}

	// Test unmarshaling
	newMap := NewHealthEventsAnnotationMap()
	if err := json.Unmarshal(jsonData, newMap); err != nil {
		t.Fatalf("Failed to unmarshal HealthEventsAnnotationMap: %v", err)
	}

	// Verify count matches
	if newMap.Count() != originalMap.Count() {
		t.Errorf("Unmarshaled map count = %v, want %v", newMap.Count(), originalMap.Count())
	}

	// Verify events can be retrieved
	if _, found := newMap.GetEvent(event1); !found {
		t.Errorf("Event1 not found in unmarshaled map")
	}
	if _, found := newMap.GetEvent(event2); !found {
		t.Errorf("Event2 not found in unmarshaled map")
	}

	// Test that entity-level keys are recreated correctly
	healthyEvent := &platformconnectorprotos.HealthEvent{
		Version:        1,
		Agent:          "gpu-health-monitor",
		ComponentClass: "GPU",
		CheckName:      "GpuXidError",
		NodeName:       "node1",
		IsHealthy:      true,
		EntitiesImpacted: []*platformconnectorprotos.Entity{
			{EntityType: "GPU", EntityValue: "1"},
		},
	}

	if _, found := newMap.GetEvent(healthyEvent); !found {
		t.Errorf("Healthy event should match stored unhealthy event after unmarshaling")
	}
}
