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
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Mock DatabaseClient
type mockDatabaseClient struct {
	mock.Mock
}

func (m *mockDatabaseClient) InsertMany(ctx context.Context, documents []interface{}) (*client.InsertManyResult, error) {
	args := m.Called(ctx, documents)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*client.InsertManyResult), args.Error(1)
}

func (m *mockDatabaseClient) UpsertDocument(ctx context.Context, filter interface{}, document interface{}) (*client.UpdateResult, error) {
	args := m.Called(ctx, filter, document)
	return args.Get(0).(*client.UpdateResult), args.Error(1)
}

func (m *mockDatabaseClient) Close(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

// Additional methods to satisfy the DatabaseClient interface
func (m *mockDatabaseClient) UpdateDocumentStatus(ctx context.Context, documentID string, statusPath string, status interface{}) error {
	args := m.Called(ctx, documentID, statusPath, status)
	return args.Error(0)
}

func (m *mockDatabaseClient) CountDocuments(ctx context.Context, filter interface{}, options *client.CountOptions) (int64, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(int64), args.Error(1)
}

func (m *mockDatabaseClient) UpdateDocument(ctx context.Context, filter interface{}, update interface{}) (*client.UpdateResult, error) {
	args := m.Called(ctx, filter, update)
	return args.Get(0).(*client.UpdateResult), args.Error(1)
}

func (m *mockDatabaseClient) UpdateManyDocuments(ctx context.Context, filter interface{}, update interface{}) (*client.UpdateResult, error) {
	args := m.Called(ctx, filter, update)
	return args.Get(0).(*client.UpdateResult), args.Error(1)
}

func (m *mockDatabaseClient) FindOne(ctx context.Context, filter interface{}, options *client.FindOneOptions) (client.SingleResult, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(client.SingleResult), args.Error(1)
}

func (m *mockDatabaseClient) Find(ctx context.Context, filter interface{}, options *client.FindOptions) (client.Cursor, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(client.Cursor), args.Error(1)
}

func (m *mockDatabaseClient) Aggregate(ctx context.Context, pipeline interface{}) (client.Cursor, error) {
	args := m.Called(ctx, pipeline)
	return args.Get(0).(client.Cursor), args.Error(1)
}

func (m *mockDatabaseClient) NewChangeStreamWatcher(ctx context.Context, tokenConfig client.TokenConfig, pipeline interface{}) (client.ChangeStreamWatcher, error) {
	args := m.Called(ctx, tokenConfig, pipeline)
	return args.Get(0).(client.ChangeStreamWatcher), args.Error(1)
}

func (m *mockDatabaseClient) Ping(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func TestInsertHealthEvents(t *testing.T) {
	ringBuffer := ringbuffer.NewRingBuffer("testRingBuffer", context.Background())
	nodeName := "testNode"

	t.Run("successful insertion", func(t *testing.T) {
		mockClient := &mockDatabaseClient{}

		// Setup mock expectations
		mockClient.On("InsertMany", mock.Anything, mock.Anything).Return(&client.InsertManyResult{InsertedIDs: []interface{}{"id1"}}, nil)

		connector := &DatabaseStoreConnector{
			databaseClient: mockClient,
			ringBuffer:     ringBuffer,
			nodeName:       nodeName,
		}

		healthEvents := &protos.HealthEvents{
			Events: []*protos.HealthEvent{{ComponentClass: "abc"}},
		}

		err := connector.insertHealthEvents(context.Background(), healthEvents)
		require.NoError(t, err)
		mockClient.AssertExpectations(t)
	})

	t.Run("insertion failure", func(t *testing.T) {
		mockClient := &mockDatabaseClient{}

		// Setup mock expectations for insertion failure
		mockClient.On("InsertMany", mock.Anything, mock.Anything).Return((*client.InsertManyResult)(nil), errors.New("test error"))

		connector := &DatabaseStoreConnector{
			databaseClient: mockClient,
			ringBuffer:     ringBuffer,
			nodeName:       nodeName,
		}

		healthEvents := &protos.HealthEvents{
			Events: []*protos.HealthEvent{{ComponentClass: "abc"}},
		}

		err := connector.insertHealthEvents(context.Background(), healthEvents)
		require.Error(t, err)
		require.Contains(t, err.Error(), "insertMany failed")
		mockClient.AssertExpectations(t)
	})
}

func TestFetchAndProcessHealthMetric(t *testing.T) {
	t.Run("process health metrics", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ringBuffer := ringbuffer.NewRingBuffer("testRingBuffer1", ctx)
		nodeName := "testNode1"
		mockClient := &mockDatabaseClient{}

		// Setup mock expectations
		mockClient.On("InsertMany", mock.Anything, mock.Anything).Return(&client.InsertManyResult{InsertedIDs: []interface{}{"id1"}}, nil)

		connector := &DatabaseStoreConnector{
			databaseClient: mockClient,
			ringBuffer:     ringBuffer,
			nodeName:       nodeName,
		}

		healthEvent := &protos.HealthEvent{}

		healthEvents := &protos.HealthEvents{
			Events: []*protos.HealthEvent{healthEvent},
		}

		ringBuffer.Enqueue(healthEvents)

		require.Equal(t, 1, ringBuffer.CurrentLength())

		go connector.FetchAndProcessHealthMetric(ctx)

		// Wait for the event to be processed
		require.Eventually(t, func() bool {
			return ringBuffer.CurrentLength() == 0
		}, 1*time.Second, 10*time.Millisecond, "event should be dequeued")

		// Give a bit more time for the database operations to complete
		time.Sleep(50 * time.Millisecond)

		cancel()
		mockClient.AssertExpectations(t)
	})

	t.Run("process health metrics when insert fails", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ringBuffer := ringbuffer.NewRingBuffer("testRingBuffer2", ctx)
		nodeName := "testNode2"
		mockClient := &mockDatabaseClient{}

		// Setup mock expectations for failure
		mockClient.On("InsertMany", mock.Anything, mock.Anything).Return((*client.InsertManyResult)(nil), errors.New("test error"))

		connector := &DatabaseStoreConnector{
			databaseClient: mockClient,
			ringBuffer:     ringBuffer,
			nodeName:       nodeName,
		}

		healthEvent := &protos.HealthEvent{
			NodeName:           "test-node",
			GeneratedTimestamp: timestamppb.New(time.Now()),
			CheckName:          "test-check",
		}

		healthEvents := &protos.HealthEvents{
			Events: []*protos.HealthEvent{healthEvent},
		}

		ringBuffer.Enqueue(healthEvents)

		require.Equal(t, 1, ringBuffer.CurrentLength())

		go connector.FetchAndProcessHealthMetric(ctx)

		// Wait for the event to be processed
		require.Eventually(t, func() bool {
			return ringBuffer.CurrentLength() == 0
		}, 1*time.Second, 10*time.Millisecond, "event should be dequeued")

		// Give the goroutine time to complete processing
		time.Sleep(50 * time.Millisecond)

		cancel()
		mockClient.AssertExpectations(t)
	})
}

func TestDisconnect(t *testing.T) {
	t.Run("successful disconnect", func(t *testing.T) {
		mockClient := &mockDatabaseClient{}
		mockClient.On("Close", mock.Anything).Return(nil)

		connector := &DatabaseStoreConnector{
			databaseClient: mockClient,
		}

		err := connector.Disconnect(context.Background())
		require.NoError(t, err)
		mockClient.AssertExpectations(t)
	})

	t.Run("disconnect with nil client", func(t *testing.T) {
		connector := &DatabaseStoreConnector{
			databaseClient: nil,
		}

		err := connector.Disconnect(context.Background())
		require.NoError(t, err)
	})

	t.Run("disconnect error", func(t *testing.T) {
		mockClient := &mockDatabaseClient{}
		mockClient.On("Close", mock.Anything).Return(errors.New("test error"))

		connector := &DatabaseStoreConnector{
			databaseClient: mockClient,
		}

		err := connector.Disconnect(context.Background())
		require.NoError(t, err) // Should not return error, just log
		mockClient.AssertExpectations(t)
	})
}

func TestGenerateRandomObjectID(t *testing.T) {
	objectID := GenerateRandomObjectID()
	require.NotEmpty(t, objectID)
	require.Len(t, objectID, 36) // UUID string is 36 characters
}

func TestInitializeDatabaseStoreConnector(t *testing.T) {
	t.Run("missing NODE_NAME environment variable", func(t *testing.T) {
		// Unset NODE_NAME if it exists
		originalNodeName := os.Getenv("NODE_NAME")
		os.Unsetenv("NODE_NAME")
		defer func() {
			if originalNodeName != "" {
				os.Setenv("NODE_NAME", originalNodeName)
			}
		}()

		ringBuffer := ringbuffer.NewRingBuffer("test", context.Background())
		connector, err := InitializeDatabaseStoreConnector(context.Background(), ringBuffer, "")

		require.Error(t, err)
		require.Nil(t, connector)
		require.Contains(t, err.Error(), "NODE_NAME is not set")
	})
}
