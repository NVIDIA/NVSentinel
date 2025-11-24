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

package testutils

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

// TestEventBuilder provides database-agnostic event creation for tests
type TestEventBuilder struct {
	eventID          string
	nodeName         string
	checkName        string
	isHealthy        bool
	isFatal          bool
	entities         []*protos.Entity
	quarantineStatus model.Status
}

// NewTestEventBuilder creates a new database-agnostic event builder
func NewTestEventBuilder() *TestEventBuilder {
	return &TestEventBuilder{
		eventID:          generateTestID(),
		quarantineStatus: model.Status("NotQuarantined"),
		isHealthy:        true,
		isFatal:          false,
	}
}

// WithEventID sets a specific event ID
func (b *TestEventBuilder) WithEventID(id string) *TestEventBuilder {
	b.eventID = id
	return b
}

// WithNode sets the node name
func (b *TestEventBuilder) WithNode(nodeName string) *TestEventBuilder {
	b.nodeName = nodeName
	return b
}

// WithCheck sets the check name
func (b *TestEventBuilder) WithCheck(checkName string) *TestEventBuilder {
	b.checkName = checkName
	return b
}

// WithHealthStatus sets the health status
func (b *TestEventBuilder) WithHealthStatus(isHealthy, isFatal bool) *TestEventBuilder {
	b.isHealthy = isHealthy
	b.isFatal = isFatal

	return b
}

// WithEntities sets the impacted entities
func (b *TestEventBuilder) WithEntities(entities []*protos.Entity) *TestEventBuilder {
	b.entities = entities
	return b
}

// WithQuarantineStatus sets the quarantine status
func (b *TestEventBuilder) WithQuarantineStatus(status model.Status) *TestEventBuilder {
	b.quarantineStatus = status
	return b
}

// BuildEvent creates a database-agnostic event that can be used with any database backend
func (b *TestEventBuilder) BuildEvent() client.Event {
	return &testEvent{
		id:               b.eventID,
		nodeName:         b.nodeName,
		checkName:        b.checkName,
		isHealthy:        b.isHealthy,
		isFatal:          b.isFatal,
		entities:         b.entities,
		quarantineStatus: b.quarantineStatus,
	}
}

// testEvent implements the client.Event interface for testing
type testEvent struct {
	id               string
	nodeName         string
	checkName        string
	isHealthy        bool
	isFatal          bool
	entities         []*protos.Entity
	quarantineStatus model.Status
}

func (e *testEvent) GetDocumentID() (string, error) {
	return e.id, nil
}

func (e *testEvent) GetNodeName() (string, error) {
	return e.nodeName, nil
}

func (e *testEvent) GetResumeToken() []byte {
	// For testing, return an empty token
	return []byte{}
}

func (e *testEvent) UnmarshalDocument(v interface{}) error {
	// Create the health event structure that modules expect
	healthEvent := &model.HealthEventWithStatus{
		CreatedAt: time.Now(),
		HealthEventStatus: model.HealthEventStatus{
			NodeQuarantined: &e.quarantineStatus,
		},
		HealthEvent: &protos.HealthEvent{
			NodeName:         e.nodeName,
			Agent:            "gpu-health-monitor",
			ComponentClass:   "GPU",
			CheckName:        e.checkName,
			Version:          1,
			IsHealthy:        e.isHealthy,
			IsFatal:          e.isFatal,
			EntitiesImpacted: e.entities,
		},
	}

	// Type assert and copy the data
	if target, ok := v.(*model.HealthEventWithStatus); ok {
		*target = *healthEvent
		return nil
	}

	return fmt.Errorf("unsupported target type: %T", v)
}

// generateTestID creates a unique test ID without using database-specific types
func generateTestID() string {
	bytes := make([]byte, 12)
	_, _ = rand.Read(bytes) // ignore error for test utility

	return hex.EncodeToString(bytes)
}

// GenerateTestNodeName creates a unique test node name without database-specific types
func GenerateTestNodeName(prefix string) string {
	return prefix + "-" + generateTestID()[:8]
}
