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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

type mockStore struct {
	mock.Mock
}

func (m *mockStore) UpsertMaintenanceEvent(ctx context.Context, event *model.MaintenanceEvent) error {
	return m.Called(ctx, event).Error(0)
}

func (m *mockStore) FindLatestOngoingEventByNode(ctx context.Context, nodeName string) (*model.MaintenanceEvent, bool, error) {
	args := m.Called(ctx, nodeName)
	ev, _ := args.Get(0).(*model.MaintenanceEvent)
	return ev, args.Bool(1), args.Error(2)
}

func (m *mockStore) FindLatestActiveEventByNodeAndType(ctx context.Context, nodeName string, mType model.MaintenanceType, statuses []model.InternalStatus) (*model.MaintenanceEvent, bool, error) {
	args := m.Called(ctx, nodeName, mType, statuses)
	ev, _ := args.Get(0).(*model.MaintenanceEvent)
	return ev, args.Bool(1), args.Error(2)
}

func (m *mockStore) FindEventsToTriggerQuarantine(ctx context.Context, limit time.Duration) ([]model.MaintenanceEvent, error) {
	args := m.Called(ctx, limit)
	return args.Get(0).([]model.MaintenanceEvent), args.Error(1)
}

func (m *mockStore) FindEmergencyEventsToTriggerQuarantine(ctx context.Context) ([]model.MaintenanceEvent, error) {
	args := m.Called(ctx)
	return args.Get(0).([]model.MaintenanceEvent), args.Error(1)
}

func (m *mockStore) FindEventsToTriggerHealthy(ctx context.Context, delay time.Duration) ([]model.MaintenanceEvent, error) {
	args := m.Called(ctx, delay)
	return args.Get(0).([]model.MaintenanceEvent), args.Error(1)
}

func (m *mockStore) FindCancelledEventsToTriggerHealthy(ctx context.Context) ([]model.MaintenanceEvent, error) {
	args := m.Called(ctx)
	return args.Get(0).([]model.MaintenanceEvent), args.Error(1)
}

func (m *mockStore) UpdateEventStatus(ctx context.Context, eventID string, status model.InternalStatus) error {
	return m.Called(ctx, eventID, status).Error(0)
}

func (m *mockStore) GetLastProcessedEventTimestampByCSP(ctx context.Context, clusterName string, csp model.CSP, cspLog string) (time.Time, bool, error) {
	args := m.Called(ctx, clusterName, csp, cspLog)
	return args.Get(0).(time.Time), args.Bool(1), args.Error(2)
}

func (m *mockStore) FindActiveEventsByStatuses(ctx context.Context, csp model.CSP, statuses []string) ([]model.MaintenanceEvent, error) {
	args := m.Called(ctx, csp, statuses)
	return args.Get(0).([]model.MaintenanceEvent), args.Error(1)
}

// TestProcessEvent_CompletedNoOngoing verifies that a MAINTENANCE_COMPLETE event is
// upserted even when no prior ONGOING event exists for the node (scheduled → completed
// direct transition, e.g. via the Lambda simulate transition API).
func TestProcessEvent_CompletedNoOngoing(t *testing.T) {
	store := &mockStore{}
	cfg := &config.Config{ClusterName: "test-cluster"}
	p, err := NewProcessor(cfg, store)
	require.NoError(t, err)

	now := time.Now().UTC()
	event := &model.MaintenanceEvent{
		EventID:       "evt-1",
		NodeName:      "node-1",
		ClusterName:   "test-cluster",
		Status:        model.StatusMaintenanceComplete,
		ActualEndTime: &now,
	}

	store.On("FindLatestOngoingEventByNode", mock.Anything, "node-1").
		Return(nil, false, nil)
	store.On("UpsertMaintenanceEvent", mock.Anything, mock.MatchedBy(func(e *model.MaintenanceEvent) bool {
		return e.EventID == "evt-1" && e.Status == model.StatusMaintenanceComplete
	})).Return(nil)

	err = p.ProcessEvent(context.Background(), event)
	require.NoError(t, err)
	store.AssertExpectations(t)
}

// TestProcessEvent_CompletedInheritsFromOngoing verifies that a MAINTENANCE_COMPLETE
// event inherits timing fields from the prior ONGOING event.
func TestProcessEvent_CompletedInheritsFromOngoing(t *testing.T) {
	store := &mockStore{}
	cfg := &config.Config{ClusterName: "test-cluster"}
	p, err := NewProcessor(cfg, store)
	require.NoError(t, err)

	scheduledStart := time.Now().UTC().Add(-2 * time.Hour)
	actualStart := time.Now().UTC().Add(-1 * time.Hour)
	actualEnd := time.Now().UTC()

	ongoing := &model.MaintenanceEvent{
		EventID:           "evt-ongoing",
		NodeName:          "node-1",
		Status:            model.StatusMaintenanceOngoing,
		ScheduledStartTime: &scheduledStart,
		ActualStartTime:   &actualStart,
		MaintenanceType:   model.MaintenanceType("POWER_CYCLE"),
	}

	event := &model.MaintenanceEvent{
		EventID:       "evt-completed",
		NodeName:      "node-1",
		Status:        model.StatusMaintenanceComplete,
		ActualEndTime: &actualEnd,
	}

	store.On("FindLatestOngoingEventByNode", mock.Anything, "node-1").
		Return(ongoing, true, nil)
	store.On("UpsertMaintenanceEvent", mock.Anything, mock.MatchedBy(func(e *model.MaintenanceEvent) bool {
		return e.EventID == "evt-completed" &&
			e.Status == model.StatusMaintenanceComplete &&
			e.ActualStartTime != nil &&
			e.ActualStartTime.Equal(actualStart)
	})).Return(nil)

	err = p.ProcessEvent(context.Background(), event)
	require.NoError(t, err)
	store.AssertExpectations(t)
}

func TestProcessEvent_NilEventReturnsError(t *testing.T) {
	store := &mockStore{}
	cfg := &config.Config{ClusterName: "test-cluster"}
	p, err := NewProcessor(cfg, store)
	require.NoError(t, err)

	assert.Error(t, p.ProcessEvent(context.Background(), nil))
}
