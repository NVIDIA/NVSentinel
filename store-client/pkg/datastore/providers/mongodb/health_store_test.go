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
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// Mock implementations for testing
type MockDatabaseClient struct {
	mock.Mock
}

func (m *MockDatabaseClient) UpdateDocumentStatus(ctx context.Context, documentID string, statusPath string, status any) error {
	args := m.Called(ctx, documentID, statusPath, status)
	return args.Error(0)
}

func (m *MockDatabaseClient) UpdateDocumentStatusFields(ctx context.Context, documentID string, fields map[string]any) error {
	args := m.Called(ctx, documentID, fields)
	return args.Error(0)
}

func (m *MockDatabaseClient) UpdateDocument(ctx context.Context, filter any, update any) (*client.UpdateResult, error) {
	args := m.Called(ctx, filter, update)
	return args.Get(0).(*client.UpdateResult), args.Error(1)
}

func (m *MockDatabaseClient) UpdateManyDocuments(ctx context.Context, filter any, update any) (*client.UpdateResult, error) {
	args := m.Called(ctx, filter, update)
	return args.Get(0).(*client.UpdateResult), args.Error(1)
}

func (m *MockDatabaseClient) UpsertDocument(ctx context.Context, filter any, document any) (*client.UpdateResult, error) {
	args := m.Called(ctx, filter, document)
	return args.Get(0).(*client.UpdateResult), args.Error(1)
}

func (m *MockDatabaseClient) InsertMany(ctx context.Context, documents []any) (*client.InsertManyResult, error) {
	args := m.Called(ctx, documents)
	return args.Get(0).(*client.InsertManyResult), args.Error(1)
}

func (m *MockDatabaseClient) FindOne(ctx context.Context, filter any, options *client.FindOneOptions) (client.SingleResult, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(client.SingleResult), args.Error(1)
}

func (m *MockDatabaseClient) Find(ctx context.Context, filter any, options *client.FindOptions) (client.Cursor, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(client.Cursor), args.Error(1)
}

func (m *MockDatabaseClient) CountDocuments(ctx context.Context, filter any, options *client.CountOptions) (int64, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(int64), args.Error(1)
}

func (m *MockDatabaseClient) Aggregate(ctx context.Context, pipeline any) (client.Cursor, error) {
	args := m.Called(ctx, pipeline)
	return args.Get(0).(client.Cursor), args.Error(1)
}

func (m *MockDatabaseClient) Ping(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func (m *MockDatabaseClient) NewChangeStreamWatcher(ctx context.Context, tokenConfig client.TokenConfig, pipeline any) (client.ChangeStreamWatcher, error) {
	args := m.Called(ctx, tokenConfig, pipeline)
	return args.Get(0).(client.ChangeStreamWatcher), args.Error(1)
}

func (m *MockDatabaseClient) Close(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func (m *MockDatabaseClient) DeleteResumeToken(ctx context.Context, tokenConfig client.TokenConfig) error {
	args := m.Called(ctx, tokenConfig)
	return args.Error(0)
}

type MockCursor struct {
	mock.Mock
}

func (m *MockCursor) Next(ctx context.Context) bool {
	args := m.Called(ctx)
	return args.Bool(0)
}

func (m *MockCursor) Decode(v any) error {
	args := m.Called(v)
	return args.Error(0)
}

func (m *MockCursor) Close(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func (m *MockCursor) All(ctx context.Context, results any) error {
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

func (m *MockSingleResult) Decode(v any) error {
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
	expectedFilter := map[string]any{
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
	expectedFilter := map[string]any{
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
	expectedFilter := map[string]any{
		"healthevent.nodename": nodeName,
	}
	expectedOptions := &client.FindOneOptions{
		Sort: map[string]any{"createdAt": -1},
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

// mockQueryBuilder is a minimal QueryBuilder for tests.
type mockQueryBuilder struct{}

func (mockQueryBuilder) ToMongo() map[string]any {
	return map[string]any{"status": "active"}
}

func (mockQueryBuilder) ToSQL() (string, []any) { return "", nil }

func TestMongoHealthEventStore_FindHealthEventsByQueryBatched(t *testing.T) {
	ctx := context.Background()

	t.Run("delivers events in batches", func(t *testing.T) {
		mockDB := new(MockDatabaseClient)
		mockCursor := new(MockCursor)
		store := &MongoHealthEventStore{databaseClient: mockDB}

		mockDB.On("Find", ctx, mock.Anything, (*client.FindOptions)(nil)).Return(mockCursor, nil)
		mockCursor.On("Close", ctx).Return(nil)

		// Simulate 3 documents; batch size = 2 → two callback invocations (2 + 1).
		docIdx := 0
		mockCursor.On("Next", ctx).Return(true).Times(3)
		mockCursor.On("Next", ctx).Return(false).Once()
		mockCursor.On("Decode", mock.AnythingOfType("*map[string]interface {}")).Return(nil).Run(func(args mock.Arguments) {
			docIdx++
			doc := args.Get(0).(*map[string]any)
			*doc = map[string]any{
				"_id": fmt.Sprintf("event-%d", docIdx),
			}
		})
		mockCursor.On("Err").Return(nil)

		var batches [][]datastore.HealthEventWithStatus

		err := store.FindHealthEventsByQueryBatched(ctx, mockQueryBuilder{}, 2,
			func(batch []datastore.HealthEventWithStatus) error {
				cp := make([]datastore.HealthEventWithStatus, len(batch))
				copy(cp, batch)
				batches = append(batches, cp)

				return nil
			})

		assert.NoError(t, err)
		assert.Len(t, batches, 2, "should have 2 callback invocations (batch of 2 + batch of 1)")
		assert.Len(t, batches[0], 2)
		assert.Len(t, batches[1], 1)

		mockDB.AssertExpectations(t)
		mockCursor.AssertExpectations(t)
	})

	t.Run("find error", func(t *testing.T) {
		mockDB := new(MockDatabaseClient)
		store := &MongoHealthEventStore{databaseClient: mockDB}

		mockDB.On("Find", ctx, mock.Anything, (*client.FindOptions)(nil)).
			Return((*MockCursor)(nil), errors.New("db error"))

		err := store.FindHealthEventsByQueryBatched(ctx, mockQueryBuilder{}, 10,
			func([]datastore.HealthEventWithStatus) error { return nil })
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to find health events for batched query")

		mockDB.AssertExpectations(t)
	})

	t.Run("callback error stops iteration", func(t *testing.T) {
		mockDB := new(MockDatabaseClient)
		mockCursor := new(MockCursor)
		store := &MongoHealthEventStore{databaseClient: mockDB}

		mockDB.On("Find", ctx, mock.Anything, (*client.FindOptions)(nil)).Return(mockCursor, nil)
		mockCursor.On("Close", ctx).Return(nil)

		mockCursor.On("Next", ctx).Return(true).Once()
		mockCursor.On("Decode", mock.AnythingOfType("*map[string]interface {}")).Return(nil).Run(func(args mock.Arguments) {
			doc := args.Get(0).(*map[string]any)
			*doc = map[string]any{"_id": "evt"}
		})

		callbackErr := errors.New("stop processing")

		err := store.FindHealthEventsByQueryBatched(ctx, mockQueryBuilder{}, 1,
			func([]datastore.HealthEventWithStatus) error { return callbackErr })
		assert.ErrorIs(t, err, callbackErr)

		mockDB.AssertExpectations(t)
	})
}

func TestNormalizeValue(t *testing.T) {
	tests := []struct {
		name     string
		input    any
		expected any
	}{
		{
			name: "bson.D with nested values",
			input: bson.D{
				{Key: "nodename", Value: "test-node"},
				{Key: "status", Value: "healthy"},
				{Key: "metadata", Value: bson.D{
					{Key: "region", Value: "us-west"},
					{Key: "zone", Value: "a"},
				}},
			},
			expected: map[string]any{
				"nodename": "test-node",
				"status":   "healthy",
				"metadata": map[string]any{
					"region": "us-west",
					"zone":   "a",
				},
			},
		},
		{
			name: "bson.D with array containing bson.D",
			input: bson.D{
				{Key: "items", Value: []any{
					bson.D{
						{Key: "name", Value: "item1"},
						{Key: "value", Value: int32(100)},
					},
					bson.D{
						{Key: "name", Value: "item2"},
						{Key: "value", Value: int32(200)},
					},
				}},
			},
			expected: map[string]any{
				"items": []any{
					map[string]any{
						"name":  "item1",
						"value": int32(100),
					},
					map[string]any{
						"name":  "item2",
						"value": int32(200),
					},
				},
			},
		},
		{
			name: "already normalized map",
			input: map[string]any{
				"key1": "value1",
				"key2": map[string]any{
					"nested": "value2",
				},
			},
			expected: map[string]any{
				"key1": "value1",
				"key2": map[string]any{
					"nested": "value2",
				},
			},
		},
		{
			name: "array with mixed types",
			input: []any{
				"string",
				123,
				bson.D{{Key: "key", Value: "value"}},
				map[string]any{"already": "normalized"},
			},
			expected: []any{
				"string",
				123,
				map[string]any{"key": "value"},
				map[string]any{"already": "normalized"},
			},
		},
		{
			// The collection client sets DefaultDocumentM, so documents decode
			// into bson.M rather than bson.D. bson.M is a defined type, so it
			// does not match the map[string]interface{} case.
			name: "top-level bson.M with nested values",
			input: bson.M{
				"nodename": "test-node",
				"metadata": bson.M{
					"region": "us-west",
				},
				"nested_d": bson.D{{Key: "zone", Value: "a"}},
				"items": bson.A{
					bson.M{"name": "item1"},
					bson.D{{Key: "name", Value: "item2"}},
				},
			},
			expected: map[string]any{
				"nodename": "test-node",
				"metadata": map[string]any{
					"region": "us-west",
				},
				"nested_d": map[string]any{"zone": "a"},
				"items": []any{
					map[string]any{"name": "item1"},
					map[string]any{"name": "item2"},
				},
			},
		},
		{
			name:     "primitive string",
			input:    "test-string",
			expected: "test-string",
		},
		{
			name:     "primitive int",
			input:    42,
			expected: 42,
		},
		{
			name:     "nil value",
			input:    nil,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeValue(tt.input)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("normalizeValue() = %v (type: %T), expected %v (type: %T)",
					result, result, tt.expected, tt.expected)
			}
		})
	}
}
