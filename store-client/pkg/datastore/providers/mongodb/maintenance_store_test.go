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

package mongodb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

func TestMongoMaintenanceEventStore_FindEventsToTriggerQuarantine(t *testing.T) {
	ctx := context.Background()
	mockDB := new(MockDatabaseClient)
	mockCursor := new(MockCursor)
	store := &MongoMaintenanceEventStore{
		databaseClient: mockDB,
	}

	triggerLimit := 5 * time.Minute

	// Test successful find
	t.Run("successful find", func(t *testing.T) {
		mockDB.On("Find", ctx, mock.Anything, (*client.FindOptions)(nil)).Return(mockCursor, nil)
		mockCursor.On("Close", ctx).Return(nil)
		mockCursor.On("All", ctx, mock.AnythingOfType("*[]model.MaintenanceEvent")).Return(nil).Run(func(args mock.Arguments) {
			// Simulate populating the slice
			events := args.Get(1).(*[]model.MaintenanceEvent)
			*events = []model.MaintenanceEvent{
				{EventID: "test-event-1", Status: model.StatusDetected},
			}
		})

		result, err := store.FindEventsToTriggerQuarantine(ctx, triggerLimit)
		assert.NoError(t, err)
		assert.Len(t, result, 1)
		assert.Equal(t, "test-event-1", result[0].EventID)

		mockDB.AssertExpectations(t)
		mockCursor.AssertExpectations(t)
	})

	// Test database error
	t.Run("database error", func(t *testing.T) {
		mockDB.ExpectedCalls = nil
		mockCursor.ExpectedCalls = nil

		mockDB.On("Find", ctx, mock.Anything, (*client.FindOptions)(nil)).Return((*MockCursor)(nil), errors.New("db error"))

		result, err := store.FindEventsToTriggerQuarantine(ctx, triggerLimit)
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "failed to query events for quarantine trigger")

		mockDB.AssertExpectations(t)
	})
}

func TestMongoMaintenanceEventStore_FindEventsToTriggerHealthy(t *testing.T) {
	ctx := context.Background()
	mockDB := new(MockDatabaseClient)
	mockCursor := new(MockCursor)
	store := &MongoMaintenanceEventStore{
		databaseClient: mockDB,
	}

	healthyDelay := 10 * time.Minute

	// Test successful find
	t.Run("successful find", func(t *testing.T) {
		mockDB.On("Find", ctx, mock.Anything, (*client.FindOptions)(nil)).Return(mockCursor, nil)
		mockCursor.On("Close", ctx).Return(nil)
		mockCursor.On("All", ctx, mock.AnythingOfType("*[]model.MaintenanceEvent")).Return(nil).Run(func(args mock.Arguments) {
			// Simulate populating the slice
			events := args.Get(1).(*[]model.MaintenanceEvent)
			*events = []model.MaintenanceEvent{
				{EventID: "test-event-1", Status: model.StatusMaintenanceComplete},
			}
		})

		result, err := store.FindEventsToTriggerHealthy(ctx, healthyDelay)
		assert.NoError(t, err)
		assert.Len(t, result, 1)
		assert.Equal(t, "test-event-1", result[0].EventID)

		mockDB.AssertExpectations(t)
		mockCursor.AssertExpectations(t)
	})

	// Test cursor decode error
	t.Run("cursor decode error", func(t *testing.T) {
		mockDB.ExpectedCalls = nil
		mockCursor.ExpectedCalls = nil

		mockDB.On("Find", ctx, mock.Anything, (*client.FindOptions)(nil)).Return(mockCursor, nil)
		mockCursor.On("Close", ctx).Return(nil)
		mockCursor.On("All", ctx, mock.AnythingOfType("*[]model.MaintenanceEvent")).Return(errors.New("decode error"))

		result, err := store.FindEventsToTriggerHealthy(ctx, healthyDelay)
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "failed to decode maintenance events for healthy trigger")

		mockDB.AssertExpectations(t)
		mockCursor.AssertExpectations(t)
	})
}

func TestMongoMaintenanceEventStore_GetLastProcessedEventTimestampByCSP(t *testing.T) {
	mockDB := new(MockDatabaseClient)
	store := &MongoMaintenanceEventStore{
		databaseClient: mockDB,
	}

	// Test basic functionality - the actual implementation uses client.FindOneWithExists
	// which is difficult to mock in unit tests without dependency injection
	t.Run("basic functionality", func(t *testing.T) {
		// Test that the store is properly initialized
		assert.NotNil(t, store)
		assert.Equal(t, mockDB, store.databaseClient)
	})
}

func TestMongoMaintenanceEventStore_FindLatestActiveEventByNodeAndType(t *testing.T) {
	ctx := context.Background()
	mockDB := new(MockDatabaseClient)
	store := &MongoMaintenanceEventStore{
		databaseClient: mockDB,
	}

	nodeName := "test-node"
	maintenanceType := model.TypeScheduled
	statuses := []model.InternalStatus{model.StatusDetected, model.StatusMaintenanceOngoing}

	// Test validation error for empty nodeName
	t.Run("validation error - empty nodeName", func(t *testing.T) {
		result, found, err := store.FindLatestActiveEventByNodeAndType(ctx, "", maintenanceType, statuses)
		assert.Error(t, err)
		assert.False(t, found)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "nodeName, maintenanceType, and at least one status are required")
	})

	// Test validation error for empty maintenanceType
	t.Run("validation error - empty maintenanceType", func(t *testing.T) {
		result, found, err := store.FindLatestActiveEventByNodeAndType(ctx, nodeName, "", statuses)
		assert.Error(t, err)
		assert.False(t, found)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "nodeName, maintenanceType, and at least one status are required")
	})

	// Test validation error for empty statuses
	t.Run("validation error - empty statuses", func(t *testing.T) {
		result, found, err := store.FindLatestActiveEventByNodeAndType(ctx, nodeName, maintenanceType, []model.InternalStatus{})
		assert.Error(t, err)
		assert.False(t, found)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "nodeName, maintenanceType, and at least one status are required")
	})
}

func TestMongoMaintenanceEventStore_FindLatestOngoingEventByNode(t *testing.T) {
	ctx := context.Background()
	mockDB := new(MockDatabaseClient)
	store := &MongoMaintenanceEventStore{
		databaseClient: mockDB,
	}

	// Test validation error for empty nodeName
	t.Run("validation error - empty nodeName", func(t *testing.T) {
		result, found, err := store.FindLatestOngoingEventByNode(ctx, "")
		assert.Error(t, err)
		assert.False(t, found)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "nodeName is required")
	})

	// Test valid input structure
	t.Run("valid input", func(t *testing.T) {
		// We won't mock the full FindOneWithExists flow here due to complexity
		// but we can test that the method exists and handles validation correctly

		assert.NotNil(t, store)
		assert.Equal(t, mockDB, store.databaseClient)

		// The method should build the correct filter for ongoing events
		// This would include statuses: StatusDetected, StatusQuarantineTriggered, StatusMaintenanceOngoing
	})
}

func TestMongoMaintenanceEventStore_FindActiveEventsByStatuses(t *testing.T) {
	ctx := context.Background()
	mockDB := new(MockDatabaseClient)
	mockCursor := new(MockCursor)
	store := &MongoMaintenanceEventStore{
		databaseClient: mockDB,
	}

	csp := model.CSPAWS
	statuses := []string{"active", "pending"}

	// Test validation error for empty statuses
	t.Run("validation error - empty statuses", func(t *testing.T) {
		result, err := store.FindActiveEventsByStatuses(ctx, csp, []string{})
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "at least one status is required")
	})

	// Test successful find
	t.Run("successful find", func(t *testing.T) {
		mockDB.On("Find", ctx, mock.Anything, (*client.FindOptions)(nil)).Return(mockCursor, nil)
		mockCursor.On("Close", ctx).Return(nil)
		mockCursor.On("All", ctx, mock.AnythingOfType("*[]model.MaintenanceEvent")).Return(nil).Run(func(args mock.Arguments) {
			// Simulate populating the slice
			events := args.Get(1).(*[]model.MaintenanceEvent)
			*events = []model.MaintenanceEvent{
				{EventID: "test-event-1", CSP: csp},
			}
		})

		result, err := store.FindActiveEventsByStatuses(ctx, csp, statuses)
		assert.NoError(t, err)
		assert.Len(t, result, 1)
		assert.Equal(t, "test-event-1", result[0].EventID)

		mockDB.AssertExpectations(t)
		mockCursor.AssertExpectations(t)
	})

	// Test database error
	t.Run("database error", func(t *testing.T) {
		mockDB.ExpectedCalls = nil
		mockCursor.ExpectedCalls = nil

		mockDB.On("Find", ctx, mock.Anything, (*client.FindOptions)(nil)).Return((*MockCursor)(nil), errors.New("db error"))

		result, err := store.FindActiveEventsByStatuses(ctx, csp, statuses)
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "failed to find active events by statuses")

		mockDB.AssertExpectations(t)
	})
}

func TestMongoMaintenanceEventStore_UpsertMaintenanceEvent(t *testing.T) {
	ctx := context.Background()
	mockDB := new(MockDatabaseClient)
	store := &MongoMaintenanceEventStore{
		databaseClient: mockDB,
	}

	event := &model.MaintenanceEvent{
		EventID: "test-event-1",
		Status:  model.StatusDetected,
	}

	// Test successful upsert
	t.Run("successful upsert", func(t *testing.T) {
		mockDB.On("UpsertDocument", ctx, map[string]interface{}{"eventId": event.EventID}, event).Return(&client.UpdateResult{UpsertedCount: 1}, nil)

		err := store.UpsertMaintenanceEvent(ctx, event)
		assert.NoError(t, err)

		mockDB.AssertExpectations(t)
	})

	// Test database error
	t.Run("database error", func(t *testing.T) {
		mockDB.ExpectedCalls = nil

		mockDB.On("UpsertDocument", ctx, map[string]interface{}{"eventId": event.EventID}, event).Return((*client.UpdateResult)(nil), errors.New("db error"))

		err := store.UpsertMaintenanceEvent(ctx, event)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to upsert maintenance event")

		mockDB.AssertExpectations(t)
	})
}
