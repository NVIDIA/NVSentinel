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
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// Mock DatabaseClient
type mockDatabaseClient struct {
	mock.Mock
}

func (m *mockDatabaseClient) InsertMany(ctx context.Context, documents []any) (*client.InsertManyResult, error) {
	args := m.Called(ctx, documents)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*client.InsertManyResult), args.Error(1)
}

func (m *mockDatabaseClient) UpsertDocument(ctx context.Context, filter any, document any) (*client.UpdateResult, error) {
	args := m.Called(ctx, filter, document)
	return args.Get(0).(*client.UpdateResult), args.Error(1)
}

func (m *mockDatabaseClient) Close(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func (m *mockDatabaseClient) DeleteResumeToken(ctx context.Context, tokenConfig client.TokenConfig) error {
	args := m.Called(ctx, tokenConfig)
	return args.Error(0)
}

// Additional methods to satisfy the DatabaseClient interface
func (m *mockDatabaseClient) UpdateDocumentStatus(ctx context.Context, documentID string, statusPath string, status any) error {
	args := m.Called(ctx, documentID, statusPath, status)
	return args.Error(0)
}

func (m *mockDatabaseClient) UpdateDocumentStatusFields(ctx context.Context, documentID string, fields map[string]any) error {
	args := m.Called(ctx, documentID, fields)
	return args.Error(0)
}

func (m *mockDatabaseClient) CountDocuments(ctx context.Context, filter any, options *client.CountOptions) (int64, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(int64), args.Error(1)
}

func (m *mockDatabaseClient) UpdateDocument(ctx context.Context, filter any, update any) (*client.UpdateResult, error) {
	args := m.Called(ctx, filter, update)
	return args.Get(0).(*client.UpdateResult), args.Error(1)
}

func (m *mockDatabaseClient) UpdateManyDocuments(ctx context.Context, filter any, update any) (*client.UpdateResult, error) {
	args := m.Called(ctx, filter, update)
	return args.Get(0).(*client.UpdateResult), args.Error(1)
}

func (m *mockDatabaseClient) FindOne(ctx context.Context, filter any, options *client.FindOneOptions) (client.SingleResult, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(client.SingleResult), args.Error(1)
}

func (m *mockDatabaseClient) Find(ctx context.Context, filter any, options *client.FindOptions) (client.Cursor, error) {
	args := m.Called(ctx, filter, options)
	return args.Get(0).(client.Cursor), args.Error(1)
}

func (m *mockDatabaseClient) Aggregate(ctx context.Context, pipeline any) (client.Cursor, error) {
	args := m.Called(ctx, pipeline)
	return args.Get(0).(client.Cursor), args.Error(1)
}

func (m *mockDatabaseClient) NewChangeStreamWatcher(ctx context.Context, tokenConfig client.TokenConfig, pipeline any) (client.ChangeStreamWatcher, error) {
	args := m.Called(ctx, tokenConfig, pipeline)
	return args.Get(0).(client.ChangeStreamWatcher), args.Error(1)
}

func (m *mockDatabaseClient) Ping(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func (m *mockDatabaseClient) InsertManyIdempotent(ctx context.Context, documents []any) (*client.InsertManyResult, error) {
	args := m.Called(ctx, documents)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*client.InsertManyResult), args.Error(1)
}

func (m *mockDatabaseClient) EnsureHealthEventIdempotencyIndex(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func (m *mockDatabaseClient) VerifyHealthEventIdempotencyIndex(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func TestInsertHealthEvents(t *testing.T) {
	ringBuffer := ringbuffer.NewRingBuffer("testRingBuffer", context.Background())

	t.Run("successful insertion", func(t *testing.T) {
		mockClient := &mockDatabaseClient{}

		// Setup mock expectations
		mockClient.On("InsertMany", mock.Anything, mock.Anything).Return(&client.InsertManyResult{InsertedIDs: []any{"id1"}}, nil)

		connector := &DatabaseStoreConnector{
			databaseClient: mockClient,
			ringBuffer:     ringBuffer,
		}

		healthEvents := &protos.HealthEvents{
			Events: []*protos.HealthEvent{{ComponentClass: "abc"}},
		}

		outcome, err := connector.insertHealthEvents(context.Background(), healthEvents, false)
		require.NoError(t, err)
		require.Equal(t, OutcomeStored, outcome)
		mockClient.AssertExpectations(t)
	})

	t.Run("insertion failure", func(t *testing.T) {
		mockClient := &mockDatabaseClient{}

		// Setup mock expectations for insertion failure
		mockClient.On("InsertMany", mock.Anything, mock.Anything).Return((*client.InsertManyResult)(nil), errors.New("test error"))

		connector := &DatabaseStoreConnector{
			databaseClient: mockClient,
			ringBuffer:     ringBuffer,
		}

		healthEvents := &protos.HealthEvents{
			Events: []*protos.HealthEvent{{ComponentClass: "abc"}},
		}

		_, err := connector.insertHealthEvents(context.Background(), healthEvents, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "insertMany failed")
		mockClient.AssertExpectations(t)
	})

	t.Run("socket path strips an inherited idempotency key", func(t *testing.T) {
		// A derived event cloned from a stored document carries that
		// document's key; stored as is on the socket path it would collide
		// with it under the unique index and be dropped after the ack.
		mockClient := &mockDatabaseClient{}
		mockClient.On("InsertMany", mock.Anything, mock.MatchedBy(func(docs []any) bool {
			doc, ok := docs[0].(model.HealthEventWithStatus)
			if !ok {
				return false
			}

			_, hasKey := doc.HealthEvent.GetMetadata()[datastore.HealthEventIdempotencyKeyMetadataField]

			return !hasKey && doc.HealthEvent.GetMetadata()["providerID"] == "aws:///i-1"
		})).Return(&client.InsertManyResult{InsertedIDs: []any{"id1"}}, nil)

		connector := &DatabaseStoreConnector{databaseClient: mockClient, ringBuffer: ringBuffer}

		healthEvents := &protos.HealthEvents{Events: []*protos.HealthEvent{{
			ComponentClass: "abc",
			Metadata: map[string]string{
				datastore.HealthEventIdempotencyKeyMetadataField: "pod-uid#key#0",
				"providerID": "aws:///i-1",
			},
		}}}

		outcome, err := connector.insertHealthEvents(context.Background(), healthEvents, false)
		require.NoError(t, err)
		require.Equal(t, OutcomeStored, outcome)
		require.Equal(t, "pod-uid#key#0",
			healthEvents.Events[0].Metadata[datastore.HealthEventIdempotencyKeyMetadataField],
			"the incoming event is left as it was; only the stored copy loses the key")
		mockClient.AssertExpectations(t)
	})
}

func TestFetchAndProcessHealthMetric(t *testing.T) {
	t.Run("process health metrics", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ringBuffer := ringbuffer.NewRingBuffer("testRingBuffer1", ctx)
		mockClient := &mockDatabaseClient{}

		// Setup mock expectations
		mockClient.On("InsertMany", mock.Anything, mock.Anything).Return(&client.InsertManyResult{InsertedIDs: []any{"id1"}}, nil)

		connector := &DatabaseStoreConnector{
			databaseClient: mockClient,
			ringBuffer:     ringBuffer,
		}

		healthEvent := &protos.HealthEvent{}

		healthEvents := &protos.HealthEvents{
			Events: []*protos.HealthEvent{healthEvent},
		}

		ringBuffer.Enqueue(ringbuffer.NewQueuedHealthEvents(healthEvents))

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
		mockClient := &mockDatabaseClient{}

		// Setup mock expectations for failure
		mockClient.On("InsertMany", mock.Anything, mock.Anything).Return((*client.InsertManyResult)(nil), errors.New("test error"))

		connector := &DatabaseStoreConnector{
			databaseClient: mockClient,
			ringBuffer:     ringBuffer,
		}

		healthEvent := &protos.HealthEvent{
			NodeName:           "test-node",
			GeneratedTimestamp: timestamppb.New(time.Now()),
			CheckName:          "test-check",
		}

		healthEvents := &protos.HealthEvents{
			Events: []*protos.HealthEvent{healthEvent},
		}

		ringBuffer.Enqueue(ringbuffer.NewQueuedHealthEvents(healthEvents))

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

// TestMessageRetriedOnMongoDBFailure verifies that
// messages are retried with exponential backoff when MongoDB write fails.
func TestMessageRetriedOnMongoDBFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ringBuffer := ringbuffer.NewRingBuffer("testRetryBehavior", ctx,
		ringbuffer.WithRetryConfig(10*time.Millisecond, 50*time.Millisecond))
	mockClient := &mockDatabaseClient{}

	// First 2 calls fail, 3rd call succeeds
	mockClient.On("InsertMany", mock.Anything, mock.Anything).
		Return(nil, errors.New("MongoDB temporarily unavailable")).Times(2)
	mockClient.On("InsertMany", mock.Anything, mock.Anything).
		Return(&client.InsertManyResult{InsertedIDs: []any{"id1"}}, nil).Once()

	connector := &DatabaseStoreConnector{
		databaseClient: mockClient,
		ringBuffer:     ringBuffer,
		maxRetries:     3,
	}

	healthEvent := &protos.HealthEvent{
		NodeName:           "gpu-node-1",
		GeneratedTimestamp: timestamppb.New(time.Now()),
		CheckName:          "GpuXidError",
		ErrorCode:          []string{"79"}, // GPU fell off the bus
		IsFatal:            true,
		IsHealthy:          false,
	}

	healthEvents := &protos.HealthEvents{
		Events: []*protos.HealthEvent{healthEvent},
	}

	ringBuffer.Enqueue(ringbuffer.NewQueuedHealthEvents(healthEvents))
	require.Equal(t, 1, ringBuffer.CurrentLength(), "Event should be in queue")
	go connector.FetchAndProcessHealthMetric(ctx)

	require.Eventually(t, func() bool {
		return ringBuffer.CurrentLength() == 0
	}, 500*time.Millisecond, 10*time.Millisecond, "Queue should be empty after successful retry")

	// Give a bit more time for all async operations to complete
	time.Sleep(100 * time.Millisecond)

	// Verify correct number of retry attempts
	mockClient.AssertNumberOfCalls(t, "InsertMany", 3)
	cancel()
}

// TestMessageDroppedAfterMaxRetries verifies that messages are eventually dropped
// after exceeding the maximum retry count to prevent unbounded memory growth.
func TestMessageDroppedAfterMaxRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ringBuffer := ringbuffer.NewRingBuffer("testMaxRetries", ctx,
		ringbuffer.WithRetryConfig(10*time.Millisecond, 50*time.Millisecond))
	mockClient := &mockDatabaseClient{}

	// Always fail to simulate persistent MongoDB outage
	mockClient.On("InsertMany", mock.Anything, mock.Anything).Return(
		(*client.InsertManyResult)(nil),
		errors.New("MongoDB permanently down"),
	)

	connector := &DatabaseStoreConnector{
		databaseClient: mockClient,
		ringBuffer:     ringBuffer,
		maxRetries:     3,
	}

	healthEvent := &protos.HealthEvent{
		NodeName:           "gpu-node-1",
		GeneratedTimestamp: timestamppb.New(time.Now()),
		CheckName:          "GpuXidError",
		ErrorCode:          []string{"79"},
		IsFatal:            true,
		IsHealthy:          false,
	}

	healthEvents := &protos.HealthEvents{
		Events: []*protos.HealthEvent{healthEvent},
	}

	ringBuffer.Enqueue(ringbuffer.NewQueuedHealthEvents(healthEvents))
	require.Equal(t, 1, ringBuffer.CurrentLength())

	go connector.FetchAndProcessHealthMetric(ctx)

	require.Eventually(t, func() bool {
		return ringBuffer.CurrentLength() == 0
	}, 500*time.Millisecond, 10*time.Millisecond, "Event should be dropped after max retries")

	// Give enough time for the final retry attempt to complete
	time.Sleep(100 * time.Millisecond)

	// Verify we attempted initial call plus 3 retries (4 total)
	mockClient.AssertNumberOfCalls(t, "InsertMany", 4)
	cancel()
}

// simpleHealthEvents builds a one-event batch for the tests below.
func simpleHealthEvents() *protos.HealthEvents {
	return &protos.HealthEvents{
		Events: []*protos.HealthEvent{{
			NodeName:           "gpu-node-1",
			GeneratedTimestamp: timestamppb.New(time.Now()),
			CheckName:          "GpuXidError",
		}},
	}
}

// TestDuplicateOnlyReplayIsSuccess verifies idempotency: a resend whose every
// document was already stored under the idempotency index inserted nothing
// and counts as success, with no retry.
func TestDuplicateOnlyReplayIsSuccess(t *testing.T) {
	mockClient := &mockDatabaseClient{}
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Return(&client.InsertManyResult{InsertedIDs: []any{}, DuplicateCount: 1}, nil).Once()

	connector := &DatabaseStoreConnector{databaseClient: mockClient, maxRetries: 3}

	outcome, err := connector.InsertBatch(context.Background(), simpleHealthEvents())
	require.NoError(t, err)
	require.Equal(t, OutcomeDuplicate, outcome)
	mockClient.AssertNumberOfCalls(t, "InsertManyIdempotent", 1)
}

// TestPartialReplayStoresMissingEvents pins partial replay: a resend that
// inserted some documents and skipped the rest as already stored is terminal
// success, no retry, and the outcome is "stored", not "duplicate", because
// something was written.
func TestPartialReplayStoresMissingEvents(t *testing.T) {
	mockClient := &mockDatabaseClient{}
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Return(&client.InsertManyResult{InsertedIDs: []any{"id-2"}, DuplicateCount: 1}, nil).Once()

	connector := &DatabaseStoreConnector{databaseClient: mockClient}

	outcome, err := connector.InsertBatch(context.Background(), simpleHealthEvents())
	require.NoError(t, err)
	require.Equal(t, OutcomeStored, outcome, "the resend inserted something, so it is a store")
	mockClient.AssertExpectations(t)
}

// TestEmptyBatchIsTerminalSuccess verifies an empty batch never reaches the
// datastore: on MongoDB the driver would return ErrEmptySlice, which
// classifies as retryable and would burn the whole retry budget.
func TestEmptyBatchIsTerminalSuccess(t *testing.T) {
	mockClient := &mockDatabaseClient{} // deliberately NO InsertManyIdempotent expectation

	connector := &DatabaseStoreConnector{databaseClient: mockClient, maxRetries: 3}

	outcome, err := connector.InsertBatch(context.Background(), &protos.HealthEvents{})
	require.NoError(t, err)
	require.Equal(t, OutcomeStored, outcome)
	mockClient.AssertNotCalled(t, "InsertManyIdempotent")
	mockClient.AssertExpectations(t)
}

// TestDuplicateOnOtherIndexStaysError verifies the boundary: a duplicate on
// any unique constraint other than the idempotency index remains a failure,
// and the server's answer about the document survives into the error.
func TestDuplicateOnOtherIndexStaysError(t *testing.T) {
	otherIndexFailure := &datastore.BulkWriteFailure{
		Failed: datastore.BulkDocumentError{
			DocumentIndex: 0,
			IndexName:     "some_other_unique_index",
			Duplicate:     true,
			Message:       "E11000 duplicate key error collection: db.HealthEvents index: some_other_unique_index",
		},
	}

	mockClient := &mockDatabaseClient{}
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Return(nil, error(otherIndexFailure))

	connector := &DatabaseStoreConnector{databaseClient: mockClient}

	_, err := connector.InsertBatch(context.Background(), simpleHealthEvents())
	require.Error(t, err)
	require.Contains(t, err.Error(), "insertMany failed")
	require.Contains(t, err.Error(), `duplicate on index "some_other_unique_index"`)
	require.Contains(t, err.Error(), "E11000", "the database's answer is kept for the log")
}
