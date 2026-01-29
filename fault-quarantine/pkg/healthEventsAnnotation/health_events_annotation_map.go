// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package healthEventsAnnotation

import (
	"encoding/json"
	"fmt"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// EventKey represents the identifying fields of a HealthEvent for entity-level matching
// IMPORTANT: This struct is used ONLY for matching/comparison.
// The full event details (including IsFatal, IsHealthy, ErrorCodes, Message) are stored
// in the annotation for visibility, but matching only uses these identifying fields.
type HealthEventKey struct {
	Agent          string // e.g., "gpu-health-monitor"
	ComponentClass string // e.g., "GPU"
	CheckName      string // e.g., "GpuXidError"
	NodeName       string // e.g., "node-123"
	// Entity-specific fields for granular tracking
	EntityType  string // e.g., "GPU", "NIC"
	EntityValue string // e.g., "1", "eth0"
	// Version is included in the key to distinguish between different versions of the same event
	Version uint32 // e.g., 1
}

// HealthEventsAnnotationMap represents a collection of unique health events
type HealthEventsAnnotationMap struct {
	Events map[HealthEventKey]*protos.HealthEvent
}

// NewHealthEventsAnnotationMap creates a new HealthEventsAnnotationMap instance
func NewHealthEventsAnnotationMap() *HealthEventsAnnotationMap {
	return &HealthEventsAnnotationMap{
		Events: make(map[HealthEventKey]*protos.HealthEvent),
	}
}

// CreateEventKeyForEntity creates a comparable key for a specific entity in a HealthEvent
func CreateEventKeyForEntity(
	event *protos.HealthEvent,
	entity *protos.Entity,
) HealthEventKey {
	key := HealthEventKey{
		Agent:          event.Agent,
		ComponentClass: event.ComponentClass,
		CheckName:      event.CheckName,
		NodeName:       event.NodeName,
		Version:        event.Version,
	}

	// Add entity-specific information if provided
	if entity != nil {
		key.EntityType = entity.EntityType
		key.EntityValue = entity.EntityValue
	}

	return key
}

// createEventKeys creates keys for all entities in a HealthEvent
func createEventKeys(event *protos.HealthEvent) []HealthEventKey {
	if len(event.EntitiesImpacted) == 0 {
		return []HealthEventKey{CreateEventKeyForEntity(event, nil)}
	}

	keys := make([]HealthEventKey, 0, len(event.EntitiesImpacted))

	for _, entity := range event.EntitiesImpacted {
		keys = append(keys, CreateEventKeyForEntity(event, entity))
	}

	return keys
}

// AddOrUpdateEvent adds a health event for each impacted entity
// Returns true if at least one entity was added/updated
func (he *HealthEventsAnnotationMap) AddOrUpdateEvent(event *protos.HealthEvent) bool {
	keys := createEventKeys(event)
	added := false

	for _, key := range keys {
		if _, exists := he.Events[key]; !exists {
			he.Events[key] = event
			added = true
		}
	}

	return added
}

// GetEvent checks if any entity from the event exists in the map
// Returns the stored event for the first matching entity
// If the event has no entities (empty EntitiesImpacted), it performs check-level matching
// to find any stored event with the same Agent/ComponentClass/CheckName/Version
func (he *HealthEventsAnnotationMap) GetEvent(
	event *protos.HealthEvent,
) (*protos.HealthEvent, bool) {
	keys := createEventKeys(event)

	// Try entity-level matching first
	for _, key := range keys {
		if storedEvent, exists := he.Events[key]; exists {
			return storedEvent, true
		}
	}

	// If no entities in incoming event (check-level event), do check-level matching
	// This handles cases where healthy events don't specify entities but mean "all entities for this check are healthy"
	if len(event.EntitiesImpacted) == 0 {
		return he.getEventByCheck(event)
	}

	return nil, false
}

// getEventByCheck finds any stored event matching the check (ignoring entities)
// Used for check-level healthy events that clear all entities for a check
func (he *HealthEventsAnnotationMap) getEventByCheck(
	event *protos.HealthEvent,
) (*protos.HealthEvent, bool) {
	for key, storedEvent := range he.Events {
		if key.Agent == event.Agent &&
			key.ComponentClass == event.ComponentClass &&
			key.CheckName == event.CheckName &&
			key.Version == event.Version &&
			key.NodeName == event.NodeName {
			return storedEvent, true
		}
	}

	return nil, false
}

// HasMatchingEntities checks if the event has any entities that match stored events
func (he *HealthEventsAnnotationMap) HasMatchingEntities(event *protos.HealthEvent) bool {
	keys := createEventKeys(event)
	for _, key := range keys {
		if _, exists := he.Events[key]; exists {
			return true
		}
	}

	return false
}

// IsEmpty checks if there are no events in the collection
func (he *HealthEventsAnnotationMap) IsEmpty() bool {
	return len(he.Events) == 0
}

// Count returns the number of events in the collection
func (he *HealthEventsAnnotationMap) Count() int {
	return len(he.Events)
}

// RemoveEvent removes all matching entities from the collection
// This is used when a healthy event clears specific entity failures
// If the event has no entities (empty EntitiesImpacted), it removes ALL entities for that check
// This handles check-level healthy events that mean "all entities for this check are healthy"
func (he *HealthEventsAnnotationMap) RemoveEvent(event *protos.HealthEvent) int {
	keys := createEventKeys(event)

	// If no entities specified (check-level healthy event), remove all entities for this check
	if len(event.EntitiesImpacted) == 0 {
		return he.removeAllEntitiesForCheck(event)
	}

	// Entity-level removal: count matches first
	removed := 0

	for _, key := range keys {
		if _, exists := he.Events[key]; exists {
			removed++
		}
	}

	for _, key := range keys {
		delete(he.Events, key)
	}

	return removed
}

// removeAllEntitiesForCheck removes all entities for a specific check
// Used when a healthy event has no entities, meaning "all entities for this check are healthy"
func (he *HealthEventsAnnotationMap) removeAllEntitiesForCheck(event *protos.HealthEvent) int {
	removed := 0
	keysToRemove := []HealthEventKey{}

	// Find all keys matching this check (regardless of entity)
	for key := range he.Events {
		if key.Agent == event.Agent &&
			key.ComponentClass == event.ComponentClass &&
			key.CheckName == event.CheckName &&
			key.Version == event.Version &&
			key.NodeName == event.NodeName {
			keysToRemove = append(keysToRemove, key)
			removed++
		}
	}

	for _, key := range keysToRemove {
		delete(he.Events, key)
	}

	return removed
}

// RemoveEntitiesForCheck removes specific entities for a check
func (he *HealthEventsAnnotationMap) RemoveEntitiesForCheck(event *protos.HealthEvent) {
	keys := createEventKeys(event)
	for _, key := range keys {
		delete(he.Events, key)
	}
}

// GetAllCheckNames returns all unique check names from stored events
func (he *HealthEventsAnnotationMap) GetAllCheckNames() []string {
	checkNamesMap := make(map[string]bool)

	for _, event := range he.Events {
		if event != nil && event.CheckName != "" {
			checkNamesMap[event.CheckName] = true
		}
	}

	checkNames := make([]string, 0, len(checkNamesMap))

	for name := range checkNamesMap {
		checkNames = append(checkNames, name)
	}

	return checkNames
}

// MarshalJSON converts the map to a JSON-serializable format (slice of events)
// Deduplicates events since multiple entity keys may point to the same event object
func (he *HealthEventsAnnotationMap) MarshalJSON() ([]byte, error) {
	// Use a map to deduplicate event pointers
	// Multiple entities can reference the same event object, we only want unique events
	seen := make(map[*protos.HealthEvent]bool)
	events := make([]*protos.HealthEvent, 0, len(he.Events))

	for _, event := range he.Events {
		if !seen[event] {
			seen[event] = true

			events = append(events, event)
		}
	}

	return json.Marshal(events)
}

// UnmarshalJSON reconstructs the map from JSON data (slice of events)
func (he *HealthEventsAnnotationMap) UnmarshalJSON(data []byte) error {
	var events []*protos.HealthEvent
	if err := json.Unmarshal(data, &events); err != nil {
		return fmt.Errorf("failed to unmarshal health events: %w", err)
	}

	if he.Events == nil {
		he.Events = make(map[HealthEventKey]*protos.HealthEvent)
	}

	for k := range he.Events {
		delete(he.Events, k)
	}

	for _, event := range events {
		// Re-create entity-level tracking from the stored events
		keys := createEventKeys(event)
		for _, key := range keys {
			he.Events[key] = event
		}
	}

	return nil
}
