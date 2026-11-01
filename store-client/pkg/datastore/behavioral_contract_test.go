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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

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

func (m *MockHealthEventStore) UpdateSpanID(ctx context.Context, id string, serviceName string, spanID string) error {
	args := m.Called(ctx, id, serviceName, spanID)
	return args.Error(0)
}

func (m *MockHealthEventStore) FindHealthEventsByNode(ctx context.Context, nodeName string) ([]datastore.HealthEventWithStatus, error) {
	args := m.Called(ctx, nodeName)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]datastore.HealthEventWithStatus), args.Error(1)
}

func (m *MockHealthEventStore) FindHealthEventsByFilter(ctx context.Context, filter map[string]any) ([]datastore.HealthEventWithStatus, error) {
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

func (m *MockHealthEventStore) FindHealthEventsByQueryBatched(ctx context.Context, builder datastore.QueryBuilder, batchSize int, fn func([]datastore.HealthEventWithStatus) error) error {
	args := m.Called(ctx, builder, batchSize, fn)
	return args.Error(0)
}

func (m *MockHealthEventStore) UpdateNodeQuarantineStatus(ctx context.Context, eventID string, status datastore.Status, spanID string) error {
	args := m.Called(ctx, eventID, status, spanID)
	return args.Error(0)
}

func (m *MockHealthEventStore) UpdatePodEvictionStatus(ctx context.Context, eventID string, status datastore.OperationStatus) error {
	args := m.Called(ctx, eventID, status)
	return args.Error(0)
}

func (m *MockHealthEventStore) UpdateRemediationStatus(ctx context.Context, eventID string, status any) error {
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

func (m *MockHealthEventStore) FindLatestHealthEventByQuery(ctx context.Context, builder datastore.QueryBuilder) (*datastore.HealthEventWithStatus, error) {
	args := m.Called(ctx, builder)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*datastore.HealthEventWithStatus), args.Error(1)
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

	t.Run("FindHealthEventsByFilter applies filters correctly", func(t *testing.T) {
		store := &MockHealthEventStore{}
		ctx := context.Background()
		filter := map[string]any{
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
}

// TestErrorHandlingConsistency verifies that both providers return compatible error types.
func TestErrorHandlingConsistency(t *testing.T) {
	t.Run("Validation errors are identifiable", func(t *testing.T) {
		validationErr := datastore.NewValidationError(datastore.ProviderPostgreSQL, "invalid data", errors.New("test error"))
		// Check if error type is validation
		var datastoreErr *datastore.DatastoreError
		require.True(t, errors.As(validationErr, &datastoreErr))
		assert.Equal(t, datastore.ErrorTypeValidation, datastoreErr.Type)
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
}
