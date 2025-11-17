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

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// Mock implementations for testing
type MockDatabaseClient struct {
	mock.Mock
}

func (m *MockDatabaseClient) UpdateDocumentStatus(ctx context.Context, documentID string, statusPath string, status interface{}) error {
	args := m.Called(ctx, documentID, statusPath, status)
	return args.Error(0)
}

func (m *MockDatabaseClient) UpdateDocument(ctx context.Context, filter interface{}, update interface{}) (*client.UpdateResult, error) {
	args := m.Called(ctx, filter, update)
	return args.Get(0).(*client.UpdateResult), args.Error(1)
}

func (m *MockDatabaseClient) UpdateManyDocuments(ctx context.Context, filter interface{}, update interface{}) (*client.UpdateResult, error) {
	args := m.Called(ctx, filter, update)
	return args.Get(0).(*client.UpdateResult), args.Error(1)
}

func (m *MockDatabaseClient) UpsertDocument(ctx context.Context, filter interface{}, document interface{}) (*client.UpdateResult, error) {
	args := m.Called(ctx, filter, document)
	return args.Get(0).(*client.UpdateResult), args.Error(1)
}

func (m *MockDatabaseClient) InsertMany(ctx context.Context, documents []interface{}) (*client.InsertManyResult, error) {
	args := m.Called(ctx, documents)
	return args.Get(0).(*client.InsertManyResult), args.Error(1)
}

func (m *MockDatabaseClient) FindOne(ctx context.Context, filter interface{}, options *client.FindOneOptions) (client.SingleResult, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(client.SingleResult), args.Error(1)
}

func (m *MockDatabaseClient) Find(ctx context.Context, filter interface{}, options *client.FindOptions) (client.Cursor, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(client.Cursor), args.Error(1)
}

func (m *MockDatabaseClient) CountDocuments(ctx context.Context, filter interface{}, options *client.CountOptions) (int64, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(int64), args.Error(1)
}

func (m *MockDatabaseClient) Aggregate(ctx context.Context, pipeline interface{}) (client.Cursor, error) {
	args := m.Called(ctx, pipeline)
	return args.Get(0).(client.Cursor), args.Error(1)
}

func (m *MockDatabaseClient) Ping(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func (m *MockDatabaseClient) NewChangeStreamWatcher(ctx context.Context, tokenConfig client.TokenConfig, pipeline interface{}) (client.ChangeStreamWatcher, error) {
	args := m.Called(ctx, tokenConfig, pipeline)
	return args.Get(0).(client.ChangeStreamWatcher), args.Error(1)
}

func (m *MockDatabaseClient) Close(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

type MockCursor struct {
	mock.Mock
}

func (m *MockCursor) Next(ctx context.Context) bool {
	args := m.Called(ctx)
	return args.Bool(0)
}

func (m *MockCursor) Decode(v interface{}) error {
	args := m.Called(v)
	return args.Error(0)
}

func (m *MockCursor) Close(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func (m *MockCursor) All(ctx context.Context, results interface{}) error {
	args := m.Called(ctx, results)
	return args.Error(0)
}

func (m *MockCursor) Err() error {
	args := m.Called()
	return args.Error(0)
}

type MockSingleResult struct {
	mock.Mock
}

func (m *MockSingleResult) Decode(v interface{}) error {
	args := m.Called(v)
	return args.Error(0)
}

func (m *MockSingleResult) Err() error {
	args := m.Called()
	return args.Error(0)
}

func TestMongoHealthEventStore_FindHealthEventsByNode(t *testing.T) {
	ctx := context.Background()
	mockDB := new(MockDatabaseClient)
	mockCursor := new(MockCursor)
	store := &MongoHealthEventStore{
		databaseClient: mockDB,
	}

	nodeName := "test-node"
	expectedFilter := map[string]interface{}{
		"healthevent.nodename": nodeName,
	}

	// Test successful find
	t.Run("successful find", func(t *testing.T) {
		mockDB.On("Find", ctx, expectedFilter, (*client.FindOptions)(nil)).Return(mockCursor, nil)
		mockCursor.On("Close", ctx).Return(nil)
		mockCursor.On("All", ctx, mock.AnythingOfType("*[]datastore.HealthEventWithStatus")).Return(nil).Run(func(args mock.Arguments) {
			// Simulate populating the slice
			events := args.Get(1).(*[]datastore.HealthEventWithStatus)
			*events = []datastore.HealthEventWithStatus{
				{CreatedAt: time.Now()},
			}
		})

		result, err := store.FindHealthEventsByNode(ctx, nodeName)
		assert.NoError(t, err)
		assert.Len(t, result, 1)

		mockDB.AssertExpectations(t)
		mockCursor.AssertExpectations(t)
	})

	// Test database error
	t.Run("database error", func(t *testing.T) {
		mockDB.ExpectedCalls = nil
		mockCursor.ExpectedCalls = nil

		mockDB.On("Find", ctx, expectedFilter, (*client.FindOptions)(nil)).Return((*MockCursor)(nil), errors.New("db error"))

		result, err := store.FindHealthEventsByNode(ctx, nodeName)
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "failed to find health events by node")

		mockDB.AssertExpectations(t)
	})
}

func TestMongoHealthEventStore_CheckIfNodeAlreadyDrained(t *testing.T) {
	ctx := context.Background()
	mockDB := new(MockDatabaseClient)
	store := &MongoHealthEventStore{
		databaseClient: mockDB,
	}

	nodeName := "test-node"
	expectedFilter := map[string]interface{}{
		"healthevent.nodename":                            nodeName,
		"healtheventstatus.userpodsevictionstatus.status": datastore.StatusSucceeded,
	}

	// Test node is drained (count > 0)
	t.Run("node is drained", func(t *testing.T) {
		mockDB.On("CountDocuments", ctx, expectedFilter, (*client.CountOptions)(nil)).Return(int64(1), nil)

		result, err := store.CheckIfNodeAlreadyDrained(ctx, nodeName)
		assert.NoError(t, err)
		assert.True(t, result)

		mockDB.AssertExpectations(t)
	})

	// Test node is not drained (count = 0)
	t.Run("node is not drained", func(t *testing.T) {
		mockDB.ExpectedCalls = nil

		mockDB.On("CountDocuments", ctx, expectedFilter, (*client.CountOptions)(nil)).Return(int64(0), nil)

		result, err := store.CheckIfNodeAlreadyDrained(ctx, nodeName)
		assert.NoError(t, err)
		assert.False(t, result)

		mockDB.AssertExpectations(t)
	})

	// Test database error
	t.Run("database error", func(t *testing.T) {
		mockDB.ExpectedCalls = nil

		mockDB.On("CountDocuments", ctx, expectedFilter, (*client.CountOptions)(nil)).Return(int64(0), errors.New("db error"))

		result, err := store.CheckIfNodeAlreadyDrained(ctx, nodeName)
		assert.Error(t, err)
		assert.False(t, result)
		assert.Contains(t, err.Error(), "failed to check if node is already drained")

		mockDB.AssertExpectations(t)
	})
}

func TestMongoHealthEventStore_FindLatestEventForNode(t *testing.T) {
	ctx := context.Background()
	mockDB := new(MockDatabaseClient)
	mockResult := new(MockSingleResult)
	store := &MongoHealthEventStore{
		databaseClient: mockDB,
	}

	nodeName := "test-node"
	expectedFilter := map[string]interface{}{
		"healthevent.nodename": nodeName,
	}
	expectedOptions := &client.FindOneOptions{
		Sort: map[string]interface{}{"createdAt": -1},
	}

	// Test successful find
	t.Run("successful find", func(t *testing.T) {
		mockDB.On("FindOne", ctx, expectedFilter, expectedOptions).Return(mockResult, nil)
		mockResult.On("Decode", mock.AnythingOfType("*datastore.HealthEventWithStatus")).Return(nil).Run(func(args mock.Arguments) {
			// Simulate decoding
			event := args.Get(0).(*datastore.HealthEventWithStatus)
			event.CreatedAt = time.Now()
		})

		result, err := store.FindLatestEventForNode(ctx, nodeName)
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.False(t, result.CreatedAt.IsZero())

		mockDB.AssertExpectations(t)
		mockResult.AssertExpectations(t)
	})

	// Test no document found
	t.Run("no document found", func(t *testing.T) {
		mockDB.ExpectedCalls = nil
		mockResult.ExpectedCalls = nil

		mockDB.On("FindOne", ctx, expectedFilter, expectedOptions).Return(mockResult, nil)
		mockResult.On("Decode", mock.AnythingOfType("*datastore.HealthEventWithStatus")).Return(errors.New("no documents in result"))

		// Need to mock IsNoDocumentsError function behavior
		result, err := store.FindLatestEventForNode(ctx, nodeName)
		assert.NoError(t, err)
		assert.Nil(t, result)

		mockDB.AssertExpectations(t)
		mockResult.AssertExpectations(t)
	})
}
