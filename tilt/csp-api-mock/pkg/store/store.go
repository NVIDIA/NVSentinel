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

package store

import (
	"sync"
	"time"
)

type CSPType string

const (
	CSPGCP CSPType = "gcp"
	CSPAWS CSPType = "aws"
)

type MaintenanceEvent struct {
	ID               string            `json:"id"`
	CSP              CSPType           `json:"csp"`
	InstanceID       string            `json:"instanceId"`
	NodeName         string            `json:"nodeName"`
	Zone             string            `json:"zone"`
	Region           string            `json:"region"`
	ProjectID        string            `json:"projectId"`
	AccountID        string            `json:"accountId"`
	Status           string            `json:"status"`
	EventTypeCode    string            `json:"eventTypeCode"`
	MaintenanceType  string            `json:"maintenanceType"`
	ScheduledStart   *time.Time        `json:"scheduledStart,omitempty"`
	ScheduledEnd     *time.Time        `json:"scheduledEnd,omitempty"`
	Description      string            `json:"description"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	CreatedAt        time.Time         `json:"createdAt"`
	UpdatedAt        time.Time         `json:"updatedAt"`
	EntityARN        string            `json:"entityArn,omitempty"`
	EventARN         string            `json:"eventArn,omitempty"`
	AffectedEntities []string          `json:"affectedEntities,omitempty"`
}

type EventStore struct {
	mu         sync.RWMutex
	events     map[string]*MaintenanceEvent
	pollCounts map[CSPType]int64
}

func NewEventStore() *EventStore {
	return &EventStore{
		events:     make(map[string]*MaintenanceEvent),
		pollCounts: make(map[CSPType]int64),
	}
}

func (s *EventStore) IncrementPollCount(csp CSPType) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pollCounts[csp]++
}

func (s *EventStore) GetPollCount(csp CSPType) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pollCounts[csp]
}

func (s *EventStore) ResetPollCount(csp CSPType) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pollCounts[csp] = 0
}

func (s *EventStore) Add(event *MaintenanceEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	event.CreatedAt = time.Now().UTC()
	event.UpdatedAt = event.CreatedAt
	s.events[event.ID] = event
}

func (s *EventStore) Update(event *MaintenanceEvent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.events[event.ID]; !exists {
		return false
	}

	event.UpdatedAt = time.Now().UTC()
	s.events[event.ID] = event
	return true
}

func (s *EventStore) Get(id string) (*MaintenanceEvent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	event, exists := s.events[id]
	return event, exists
}

func (s *EventStore) ListByCSP(csp CSPType) []*MaintenanceEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*MaintenanceEvent
	for _, event := range s.events {
		if event.CSP == csp {
			result = append(result, event)
		}
	}
	return result
}

func (s *EventStore) ClearByCSP(csp CSPType) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, event := range s.events {
		if event.CSP == csp {
			delete(s.events, id)
		}
	}
}
