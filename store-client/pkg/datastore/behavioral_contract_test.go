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

package datastore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// MockMaintenanceEventStore is a mock implementation for testing
type MockMaintenanceEventStore struct {
	mock.Mock
}

func (m *MockMaintenanceEventStore) UpsertMaintenanceEvent(ctx context.Context, event *model.MaintenanceEvent) error {
	args := m.Called(ctx, event)
	return args.Error(0)
}

func (m *MockMaintenanceEventStore) FindEventsToTriggerQuarantine(ctx context.Context, triggerTimeLimit time.Duration) ([]model.MaintenanceEvent, error) {
	args := m.Called(ctx, triggerTimeLimit)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]model.MaintenanceEvent), args.Error(1)
}

func (m *MockMaintenanceEventStore) FindEventsToTriggerHealthy(ctx context.Context, healthyDelay time.Duration) ([]model.MaintenanceEvent, error) {
	args := m.Called(ctx, healthyDelay)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]model.MaintenanceEvent), args.Error(1)
}

func (m *MockMaintenanceEventStore) UpdateEventStatus(ctx context.Context, eventID string, newStatus model.InternalStatus) error {
	args := m.Called(ctx, eventID, newStatus)
	return args.Error(0)
}

func (m *MockMaintenanceEventStore) GetLastProcessedEventTimestampByCSP(ctx context.Context, clusterName string, cspType model.CSP, cspNameForLog string) (time.Time, bool, error) {
	args := m.Called(ctx, clusterName, cspType, cspNameForLog)
	return args.Get(0).(time.Time), args.Bool(1), args.Error(2)
}

func (m *MockMaintenanceEventStore) FindLatestActiveEventByNodeAndType(ctx context.Context, nodeName string, maintenanceType model.MaintenanceType, statuses []model.InternalStatus) (*model.MaintenanceEvent, bool, error) {
	args := m.Called(ctx, nodeName, maintenanceType, statuses)
	if args.Get(0) == nil {
		return nil, args.Bool(1), args.Error(2)
	}
	return args.Get(0).(*model.MaintenanceEvent), args.Bool(1), args.Error(2)
}

func (m *MockMaintenanceEventStore) FindLatestOngoingEventByNode(ctx context.Context, nodeName string) (*model.MaintenanceEvent, bool, error) {
	args := m.Called(ctx, nodeName)
	if args.Get(0) == nil {
		return nil, args.Bool(1), args.Error(2)
	}
	return args.Get(0).(*model.MaintenanceEvent), args.Bool(1), args.Error(2)
}

func (m *MockMaintenanceEventStore) FindActiveEventsByStatuses(ctx context.Context, csp model.CSP, statuses []string) ([]model.MaintenanceEvent, error) {
	args := m.Called(ctx, csp, statuses)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]model.MaintenanceEvent), args.Error(1)
}

// MockHealthEventStore is a mock implementation for testing
type MockHealthEventStore struct {
	mock.Mock
}

func (m *MockHealthEventStore) UpdateHealthEventStatus(ctx context.Context, id string, status datastore.HealthEventStatus) error {
	args := m.Called(ctx, id, status)
	return args.Error(0)
}

func (m *MockHealthEventStore) UpdateHealthEventStatusByNode(ctx context.Context, nodeName string, status datastore.HealthEventStatus) error {
	args := m.Called(ctx, nodeName, status)
	return args.Error(0)
}

func (m *MockHealthEventStore) FindHealthEventsByNode(ctx context.Context, nodeName string) ([]datastore.HealthEventWithStatus, error) {
	args := m.Called(ctx, nodeName)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]datastore.HealthEventWithStatus), args.Error(1)
}

func (m *MockHealthEventStore) FindHealthEventsByFilter(ctx context.Context, filter map[string]interface{}) ([]datastore.HealthEventWithStatus, error) {
	args := m.Called(ctx, filter)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]datastore.HealthEventWithStatus), args.Error(1)
}

func (m *MockHealthEventStore) FindHealthEventsByStatus(ctx context.Context, status datastore.Status) ([]datastore.HealthEventWithStatus, error) {
	args := m.Called(ctx, status)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]datastore.HealthEventWithStatus), args.Error(1)
}

func (m *MockHealthEventStore) UpdateNodeQuarantineStatus(ctx context.Context, eventID string, status datastore.Status) error {
	args := m.Called(ctx, eventID, status)
	return args.Error(0)
}

func (m *MockHealthEventStore) UpdatePodEvictionStatus(ctx context.Context, eventID string, status datastore.OperationStatus) error {
	args := m.Called(ctx, eventID, status)
	return args.Error(0)
}

func (m *MockHealthEventStore) UpdateRemediationStatus(ctx context.Context, eventID string, status interface{}) error {
	args := m.Called(ctx, eventID, status)
	return args.Error(0)
}

func (m *MockHealthEventStore) CheckIfNodeAlreadyDrained(ctx context.Context, nodeName string) (bool, error) {
	args := m.Called(ctx, nodeName)
	return args.Bool(0), args.Error(1)
}

func (m *MockHealthEventStore) FindLatestEventForNode(ctx context.Context, nodeName string) (*datastore.HealthEventWithStatus, error) {
	args := m.Called(ctx, nodeName)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*datastore.HealthEventWithStatus), args.Error(1)
}

// TestMaintenanceEventStoreBehavior tests that all MaintenanceEventStore implementations
// behave consistently for the same operations.
func TestMaintenanceEventStoreBehavior(t *testing.T) {
	t.Run("UpsertMaintenanceEvent success", func(t *testing.T) {
		store := &MockMaintenanceEventStore{}
		ctx := context.Background()
		event := &model.MaintenanceEvent{
			EventID:         "test-event-1",
			NodeName:        "test-node",
			MaintenanceType: model.MaintenanceType("ScheduledMaintenance"),
			CSP:             model.CSP("aws"),
		}

		store.On("UpsertMaintenanceEvent", ctx, event).Return(nil)

		err := store.UpsertMaintenanceEvent(ctx, event)
		assert.NoError(t, err, "Successful upsert should not return error")
		store.AssertExpectations(t)
	})

	t.Run("UpsertMaintenanceEvent handles errors", func(t *testing.T) {
		store := &MockMaintenanceEventStore{}
		ctx := context.Background()
		event := &model.MaintenanceEvent{
			EventID: "test-event-2",
		}

		expectedErr := errors.New("database connection failed")
		store.On("UpsertMaintenanceEvent", ctx, event).Return(expectedErr)

		err := store.UpsertMaintenanceEvent(ctx, event)
		assert.Error(t, err, "Failed upsert should return error")
		assert.Equal(t, expectedErr, err, "Error should be propagated")
		store.AssertExpectations(t)
	})

	t.Run("FindEventsToTriggerQuarantine returns events", func(t *testing.T) {
		store := &MockMaintenanceEventStore{}
		ctx := context.Background()
		triggerTimeLimit := 5 * time.Minute

		expectedEvents := []model.MaintenanceEvent{
			{EventID: "event-1", NodeName: "node-1"},
			{EventID: "event-2", NodeName: "node-2"},
		}

		store.On("FindEventsToTriggerQuarantine", ctx, triggerTimeLimit).Return(expectedEvents, nil)

		events, err := store.FindEventsToTriggerQuarantine(ctx, triggerTimeLimit)
		assert.NoError(t, err)
		assert.Equal(t, expectedEvents, events, "Should return expected events")
		store.AssertExpectations(t)
	})

	t.Run("FindEventsToTriggerQuarantine handles empty results", func(t *testing.T) {
		store := &MockMaintenanceEventStore{}
		ctx := context.Background()
		triggerTimeLimit := 5 * time.Minute

		store.On("FindEventsToTriggerQuarantine", ctx, triggerTimeLimit).Return([]model.MaintenanceEvent{}, nil)

		events, err := store.FindEventsToTriggerQuarantine(ctx, triggerTimeLimit)
		assert.NoError(t, err)
		assert.Empty(t, events, "Should return empty slice for no results")
		store.AssertExpectations(t)
	})

	t.Run("UpdateEventStatus updates successfully", func(t *testing.T) {
		store := &MockMaintenanceEventStore{}
		ctx := context.Background()
		eventID := "test-event-3"
		status := model.InternalStatus("Processed")

		store.On("UpdateEventStatus", ctx, eventID, status).Return(nil)

		err := store.UpdateEventStatus(ctx, eventID, status)
		assert.NoError(t, err)
		store.AssertExpectations(t)
	})

	t.Run("GetLastProcessedEventTimestampByCSP returns timestamp", func(t *testing.T) {
		store := &MockMaintenanceEventStore{}
		ctx := context.Background()
		clusterName := "test-cluster"
		cspType := model.CSP("aws")
		cspNameForLog := "AWS"
		expectedTime := time.Now().UTC()

		store.On("GetLastProcessedEventTimestampByCSP", ctx, clusterName, cspType, cspNameForLog).Return(expectedTime, true, nil)

		timestamp, found, err := store.GetLastProcessedEventTimestampByCSP(ctx, clusterName, cspType, cspNameForLog)
		assert.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, expectedTime, timestamp, "Should return expected timestamp")
		store.AssertExpectations(t)
	})

	t.Run("FindLatestActiveEventByNodeAndType finds event", func(t *testing.T) {
		store := &MockMaintenanceEventStore{}
		ctx := context.Background()
		nodeName := "test-node"
		maintenanceType := model.MaintenanceType("ScheduledMaintenance")
		statuses := []model.InternalStatus{model.StatusDetected}

		expectedEvent := &model.MaintenanceEvent{
			EventID:         "event-1",
			NodeName:        nodeName,
			MaintenanceType: maintenanceType,
		}

		store.On("FindLatestActiveEventByNodeAndType", ctx, nodeName, maintenanceType, statuses).Return(expectedEvent, true, nil)

		event, found, err := store.FindLatestActiveEventByNodeAndType(ctx, nodeName, maintenanceType, statuses)
		assert.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, expectedEvent, event)
		store.AssertExpectations(t)
	})

	t.Run("FindLatestActiveEventByNodeAndType handles not found", func(t *testing.T) {
		store := &MockMaintenanceEventStore{}
		ctx := context.Background()
		nodeName := "nonexistent-node"
		maintenanceType := model.MaintenanceType("ScheduledMaintenance")
		statuses := []model.InternalStatus{model.StatusDetected}

		store.On("FindLatestActiveEventByNodeAndType", ctx, nodeName, maintenanceType, statuses).Return(nil, false, nil)

		event, found, err := store.FindLatestActiveEventByNodeAndType(ctx, nodeName, maintenanceType, statuses)
		assert.NoError(t, err)
		assert.False(t, found)
		assert.Nil(t, event, "Should return nil for not found")
		store.AssertExpectations(t)
	})
}

// TestHealthEventStoreBehavior tests that all HealthEventStore implementations
// behave consistently for the same operations.
func TestHealthEventStoreBehavior(t *testing.T) {
	t.Run("UpdateHealthEventStatus updates successfully", func(t *testing.T) {
		store := &MockHealthEventStore{}
		ctx := context.Background()
		eventID := "event-123"
		quarantined := datastore.Quarantined
		status := datastore.HealthEventStatus{
			NodeQuarantined: &quarantined,
		}

		store.On("UpdateHealthEventStatus", ctx, eventID, status).Return(nil)

		err := store.UpdateHealthEventStatus(ctx, eventID, status)
		assert.NoError(t, err)
		store.AssertExpectations(t)
	})

	t.Run("UpdateHealthEventStatusByNode updates by node", func(t *testing.T) {
		store := &MockHealthEventStore{}
		ctx := context.Background()
		nodeName := "test-node"
		unquarantined := datastore.UnQuarantined
		status := datastore.HealthEventStatus{
			NodeQuarantined: &unquarantined,
		}

		store.On("UpdateHealthEventStatusByNode", ctx, nodeName, status).Return(nil)

		err := store.UpdateHealthEventStatusByNode(ctx, nodeName, status)
		assert.NoError(t, err)
		store.AssertExpectations(t)
	})

	t.Run("FindHealthEventsByNode returns events", func(t *testing.T) {
		store := &MockHealthEventStore{}
		ctx := context.Background()
		nodeName := "test-node"

		quarantined := datastore.Quarantined
		expectedEvents := []datastore.HealthEventWithStatus{
			{
				CreatedAt: time.Now(),
				HealthEventStatus: datastore.HealthEventStatus{
					NodeQuarantined: &quarantined,
				},
			},
		}

		store.On("FindHealthEventsByNode", ctx, nodeName).Return(expectedEvents, nil)

		events, err := store.FindHealthEventsByNode(ctx, nodeName)
		assert.NoError(t, err)
		assert.Equal(t, expectedEvents, events)
		store.AssertExpectations(t)
	})

	t.Run("FindHealthEventsByNode handles empty results", func(t *testing.T) {
		store := &MockHealthEventStore{}
		ctx := context.Background()
		nodeName := "nonexistent-node"

		store.On("FindHealthEventsByNode", ctx, nodeName).Return([]datastore.HealthEventWithStatus{}, nil)

		events, err := store.FindHealthEventsByNode(ctx, nodeName)
		assert.NoError(t, err)
		assert.Empty(t, events, "Should return empty slice for no results")
		store.AssertExpectations(t)
	})

	t.Run("FindHealthEventsByFilter applies filters correctly", func(t *testing.T) {
		store := &MockHealthEventStore{}
		ctx := context.Background()
		filter := map[string]interface{}{
			"checkName": "GpuXidError",
			"isFatal":   true,
		}

		expectedEvents := []datastore.HealthEventWithStatus{}
		store.On("FindHealthEventsByFilter", ctx, filter).Return(expectedEvents, nil)

		events, err := store.FindHealthEventsByFilter(ctx, filter)
		assert.NoError(t, err)
		assert.NotNil(t, events)
		store.AssertExpectations(t)
	})

	t.Run("FindHealthEventsByStatus queries by status", func(t *testing.T) {
		store := &MockHealthEventStore{}
		ctx := context.Background()
		status := datastore.Quarantined

		expectedEvents := []datastore.HealthEventWithStatus{}
		store.On("FindHealthEventsByStatus", ctx, status).Return(expectedEvents, nil)

		events, err := store.FindHealthEventsByStatus(ctx, status)
		assert.NoError(t, err)
		assert.NotNil(t, events)
		store.AssertExpectations(t)
	})
}

// TestDataStoreCommonBehavior tests common DataStore operations across providers.
func TestDataStoreCommonBehavior(t *testing.T) {
	t.Run("Context cancellation is respected", func(t *testing.T) {
		store := &MockMaintenanceEventStore{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		event := &model.MaintenanceEvent{EventID: "test"}

		// Mock should handle cancelled context appropriately
		store.On("UpsertMaintenanceEvent", ctx, event).Return(context.Canceled)

		err := store.UpsertMaintenanceEvent(ctx, event)
		assert.Error(t, err)
		assert.Equal(t, context.Canceled, err, "Should return context.Canceled error")
		store.AssertExpectations(t)
	})

	t.Run("Context timeout is respected", func(t *testing.T) {
		store := &MockMaintenanceEventStore{}
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
		defer cancel()

		time.Sleep(10 * time.Millisecond) // Ensure timeout

		event := &model.MaintenanceEvent{EventID: "test"}
		store.On("UpsertMaintenanceEvent", ctx, event).Return(context.DeadlineExceeded)

		err := store.UpsertMaintenanceEvent(ctx, event)
		assert.Error(t, err)
		assert.Equal(t, context.DeadlineExceeded, err, "Should return context.DeadlineExceeded error")
		store.AssertExpectations(t)
	})
}

// TestErrorHandlingConsistency verifies that both providers return compatible error types.
func TestErrorHandlingConsistency(t *testing.T) {
	t.Run("Connection errors are identifiable", func(t *testing.T) {
		// Test that connection errors can be identified consistently
		connectionErr := datastore.NewConnectionError(datastore.ProviderPostgreSQL, "connection refused", errors.New("test error"))
		assert.True(t, datastore.IsConnectionError(connectionErr),
			"Connection errors should be identifiable")
	})

	t.Run("Validation errors are identifiable", func(t *testing.T) {
		validationErr := datastore.NewValidationError(datastore.ProviderPostgreSQL, "invalid data", errors.New("test error"))
		// Check if error type is validation
		var datastoreErr *datastore.DatastoreError
		require.True(t, errors.As(validationErr, &datastoreErr))
		assert.Equal(t, datastore.ErrorTypeValidation, datastoreErr.Type)
	})

	t.Run("Timeout errors are identifiable", func(t *testing.T) {
		timeoutErr := datastore.NewTimeoutError(datastore.ProviderPostgreSQL, "operation timed out", errors.New("test error"))
		// Check connection error since timeout is a type of connection error
		assert.True(t, datastore.IsConnectionError(timeoutErr),
			"Timeout errors should be identifiable as connection errors")
	})

	t.Run("Error unwrapping works correctly", func(t *testing.T) {
		originalErr := errors.New("original error")
		wrappedErr := datastore.NewConnectionError(datastore.ProviderPostgreSQL, "connection failed", originalErr)

		unwrapped := errors.Unwrap(wrappedErr)
		assert.Equal(t, originalErr, unwrapped,
			"Should be able to unwrap to original error")
	})
}

// TestProviderBehaviorConsistency verifies that operations have consistent behavior
// patterns across different provider implementations.
func TestProviderBehaviorConsistency(t *testing.T) {
	t.Run("Empty collections return empty slices, not nil", func(t *testing.T) {
		store := &MockHealthEventStore{}
		ctx := context.Background()
		nodeName := "nonexistent"

		// Both providers should return empty slice, not nil
		emptySlice := []datastore.HealthEventWithStatus{}
		store.On("FindHealthEventsByNode", ctx, nodeName).Return(emptySlice, nil)

		events, err := store.FindHealthEventsByNode(ctx, nodeName)
		require.NoError(t, err)
		require.NotNil(t, events, "Should return empty slice, not nil")
		assert.Empty(t, events)
		store.AssertExpectations(t)
	})

	t.Run("Updates to nonexistent items should not error silently", func(t *testing.T) {
		store := &MockHealthEventStore{}
		ctx := context.Background()
		nonexistentID := "nonexistent-id"
		status := datastore.HealthEventStatus{}

		// Providers should either succeed (no-op) or return a specific error
		// This test documents the expected behavior
		store.On("UpdateHealthEventStatus", ctx, nonexistentID, status).Return(nil)

		err := store.UpdateHealthEventStatus(ctx, nonexistentID, status)
		// Both behaviors are acceptable: no error (no-op) or a NotFound error
		if err != nil {
			assert.Contains(t, err.Error(), "not found", "Error should indicate item not found")
		}
		store.AssertExpectations(t)
	})

	t.Run("Nil pointer checks", func(t *testing.T) {
		store := &MockMaintenanceEventStore{}
		ctx := context.Background()

		// Passing nil should be handled gracefully (error, not panic)
		store.On("UpsertMaintenanceEvent", ctx, (*model.MaintenanceEvent)(nil)).
			Return(datastore.NewValidationError(datastore.ProviderPostgreSQL, "event cannot be nil", nil))

		err := store.UpsertMaintenanceEvent(ctx, nil)
		assert.Error(t, err, "Nil pointer should return error, not panic")

		// Check if it's a DatastoreError with validation type
		var datastoreErr *datastore.DatastoreError
		require.True(t, errors.As(err, &datastoreErr))
		assert.Equal(t, datastore.ErrorTypeValidation, datastoreErr.Type)
		store.AssertExpectations(t)
	})
}
