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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

func (m *mockDatabaseClient) VerifyHealthEventIdempotencyIndex(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func TestInsertHealthEvents(t *testing.T) {
	ringBuffer := ringbuffer.NewRingBuffer("testRingBuffer", context.Background())

	t.Run("successful insertion", func(t *testing.T) {
		mockClient := &mockDatabaseClient{}

		// Setup mock expectations
		mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).Return(&client.InsertManyResult{InsertedIDs: []any{"id1"}}, nil)

		connector := &DatabaseStoreConnector{
			databaseClient: mockClient,
			ringBuffer:     ringBuffer,
		}

		healthEvents := &protos.HealthEvents{
			Events: []*protos.HealthEvent{{ComponentClass: "abc"}},
		}

		outcome, err := connector.insertHealthEvents(context.Background(), healthEvents, "inst#1")
		require.NoError(t, err)
		require.Equal(t, outcomeStored, outcome)
		mockClient.AssertExpectations(t)
	})

	t.Run("insertion failure", func(t *testing.T) {
		mockClient := &mockDatabaseClient{}

		// Setup mock expectations for insertion failure
		mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).Return((*client.InsertManyResult)(nil), errors.New("test error"))

		connector := &DatabaseStoreConnector{
			databaseClient: mockClient,
			ringBuffer:     ringBuffer,
		}

		healthEvents := &protos.HealthEvents{
			Events: []*protos.HealthEvent{{ComponentClass: "abc"}},
		}

		_, err := connector.insertHealthEvents(context.Background(), healthEvents, "inst#1")
		require.Error(t, err)
		require.Contains(t, err.Error(), "insertMany failed")
		mockClient.AssertExpectations(t)
	})

	t.Run("the ring buffer loop keys the stored copy and overwrites an inherited key", func(t *testing.T) {
		// A derived event cloned from a stored document carries that
		// document's key; stored as is it would collide with that document
		// under the unique index and be dropped after the ack.
		mockClient := &mockDatabaseClient{}
		mockClient.On("InsertManyIdempotent", mock.Anything, mock.MatchedBy(func(docs []any) bool {
			doc, ok := docs[0].(model.HealthEventWithStatus)
			if !ok {
				return false
			}

			return doc.HealthEvent.GetMetadata()[datastore.HealthEventIdempotencyKeyMetadataField] == "inst#1#0" &&
				doc.HealthEvent.GetMetadata()["providerID"] == "aws:///i-1"
		})).Return(&client.InsertManyResult{InsertedIDs: []any{"id1"}}, nil)

		connector := &DatabaseStoreConnector{databaseClient: mockClient, ringBuffer: ringBuffer}

		healthEvents := &protos.HealthEvents{Events: []*protos.HealthEvent{{
			ComponentClass: "abc",
			Metadata: map[string]string{
				datastore.HealthEventIdempotencyKeyMetadataField: "pod-uid#key#0",
				"providerID": "aws:///i-1",
			},
		}}}

		outcome, err := connector.insertHealthEvents(context.Background(), healthEvents, "inst#1")
		require.NoError(t, err)
		require.Equal(t, outcomeStored, outcome)
		require.Equal(t, "pod-uid#key#0",
			healthEvents.Events[0].Metadata[datastore.HealthEventIdempotencyKeyMetadataField],
			"the incoming event is left as it was; only the stored copy is keyed")
		mockClient.AssertExpectations(t)
	})

	t.Run("without a batch key the events keep the keys they arrived with", func(t *testing.T) {
		// The deployment platform connector keys its batches before the
		// pipeline runs and calls ProcessBatch, which passes no batch key.
		mockClient := &mockDatabaseClient{}
		mockClient.On("InsertManyIdempotent", mock.Anything, mock.MatchedBy(func(docs []any) bool {
			doc, ok := docs[0].(model.HealthEventWithStatus)

			return ok && doc.HealthEvent.GetMetadata()[datastore.HealthEventIdempotencyKeyMetadataField] == "pod-uid#key#0"
		})).Return(&client.InsertManyResult{InsertedIDs: []any{"id1"}}, nil)

		connector := &DatabaseStoreConnector{databaseClient: mockClient}

		healthEvents := &protos.HealthEvents{Events: []*protos.HealthEvent{{
			ComponentClass: "abc",
			Metadata:       map[string]string{datastore.HealthEventIdempotencyKeyMetadataField: "pod-uid#key#0"},
		}}}

		outcome, err := connector.insertHealthEvents(context.Background(), healthEvents, "")
		require.NoError(t, err)
		require.Equal(t, outcomeStored, outcome)
		mockClient.AssertExpectations(t)
	})
}

// TestBatchKey_StableAcrossRequeues: a queued batch gets its key on the first
// write attempt and keeps it, so a batch requeued after a failed insert stamps
// the same per-event keys; the next batch gets the next key.
func TestBatchKey_StableAcrossRequeues(t *testing.T) {
	connector := &DatabaseStoreConnector{instanceID: "inst"}

	first := ringbuffer.NewQueuedHealthEvents(simpleHealthEvents())
	require.Equal(t, "inst#1", connector.batchKey(first))
	require.Equal(t, "inst#1", connector.batchKey(first), "a requeued batch keeps its key")
	require.Equal(t, "inst#1#0", EventIdempotencyKey(connector.batchKey(first), 0))

	second := ringbuffer.NewQueuedHealthEvents(simpleHealthEvents())
	require.Equal(t, "inst#2", connector.batchKey(second), "the next batch gets the next key")
}

func TestFetchAndProcessHealthMetric(t *testing.T) {
	t.Run("process health metrics", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ringBuffer := ringbuffer.NewRingBuffer("testRingBuffer1", ctx)
		mockClient := &mockDatabaseClient{}

		// Setup mock expectations
		mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).Return(&client.InsertManyResult{InsertedIDs: []any{"id1"}}, nil)

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
		mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).Return((*client.InsertManyResult)(nil), errors.New("test error"))

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

// TestMessageRetriedOnMongoDBFailure verifies that
// messages are retried with exponential backoff when MongoDB write fails.
func TestMessageRetriedOnMongoDBFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ringBuffer := ringbuffer.NewRingBuffer("testRetryBehavior", ctx,
		ringbuffer.WithRetryConfig(10*time.Millisecond, 50*time.Millisecond))
	mockClient := &mockDatabaseClient{}

	// First 2 calls fail, 3rd call succeeds
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Return(nil, errors.New("MongoDB temporarily unavailable")).Times(2)
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
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
	mockClient.AssertNumberOfCalls(t, "InsertManyIdempotent", 3)
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
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).Return(
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
	mockClient.AssertNumberOfCalls(t, "InsertManyIdempotent", 4)
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

	outcome, err := connector.insertHealthEvents(context.Background(), simpleHealthEvents(), "")
	require.NoError(t, err)
	require.Equal(t, outcomeDuplicate, outcome)
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

	outcome, err := connector.insertHealthEvents(context.Background(), simpleHealthEvents(), "")
	require.NoError(t, err)
	require.Equal(t, outcomeStored, outcome, "the resend inserted something, so it is a store")
	mockClient.AssertExpectations(t)
}

// TestEmptyBatchIsTerminalSuccess verifies an empty batch never reaches the
// datastore: on MongoDB the driver would return ErrEmptySlice, which
// classifies as retryable and would burn the whole retry budget.
func TestEmptyBatchIsTerminalSuccess(t *testing.T) {
	mockClient := &mockDatabaseClient{} // deliberately NO InsertManyIdempotent expectation

	connector := &DatabaseStoreConnector{databaseClient: mockClient, maxRetries: 3}

	outcome, err := connector.insertHealthEvents(context.Background(), &protos.HealthEvents{}, "")
	require.NoError(t, err)
	require.Equal(t, outcomeStored, outcome)
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

	_, err := connector.insertHealthEvents(context.Background(), simpleHealthEvents(), "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "insertMany failed")
	require.Contains(t, err.Error(), `duplicate on index "some_other_unique_index"`)
	require.Contains(t, err.Error(), "E11000", "the database's answer is kept for the log")
}

// TestProcessBatch_ReportsOnlyTheErrorAndCountsTheOutcome: inside a request
// the connector answers with the error alone; stored and duplicate outcomes
// are both successes, told apart in the store's own counter.
func TestProcessBatch_ReportsOnlyTheErrorAndCountsTheOutcome(t *testing.T) {
	storedBefore := testutil.ToFloat64(batchesWritten.WithLabelValues(outcomeStored))
	duplicateBefore := testutil.ToFloat64(batchesWritten.WithLabelValues(outcomeDuplicate))

	mockClient := &mockDatabaseClient{}
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Return(&client.InsertManyResult{InsertedIDs: []any{"id-1"}}, nil).Once()
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Return(&client.InsertManyResult{InsertedIDs: []any{}, DuplicateCount: 1}, nil).Once()
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Return(nil, errors.New("primary stepped down")).Once()

	connector := &DatabaseStoreConnector{databaseClient: mockClient}

	require.NoError(t, connector.ProcessBatch(context.Background(), simpleHealthEvents()))
	require.NoError(t, connector.ProcessBatch(context.Background(), simpleHealthEvents()), "a resend is a success")
	require.Error(t, connector.ProcessBatch(context.Background(), simpleHealthEvents()))

	require.Equal(t, storedBefore+1, testutil.ToFloat64(batchesWritten.WithLabelValues(outcomeStored)))
	require.Equal(t, duplicateBefore+1, testutil.ToFloat64(batchesWritten.WithLabelValues(outcomeDuplicate)))
	mockClient.AssertExpectations(t)
}

// TestProcessBatch_OnlyABatchThatCannotBeStoredIsNotRetried: a duplicate on
// another unique index, or a document the datastore rejects for what it
// contains, gets the same answer on every resend, so the caller gets a status
// it does not retry, naming the event. Any other document error (the server
// stepping down mid-batch is reported per document too) and any transport
// failure stay plain errors the caller retries.
func TestProcessBatch_OnlyABatchThatCannotBeStoredIsNotRetried(t *testing.T) {
	mockClient := &mockDatabaseClient{}
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Return(nil, error(&datastore.BulkWriteFailure{
			InsertedCount: 1,
			Failed: datastore.BulkDocumentError{
				DocumentIndex: 1, Duplicate: true, IndexName: "some_other_unique_index", Message: "E11000 duplicate key",
			},
		})).Once()
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Return(nil, error(&datastore.BulkWriteFailure{
			InsertedCount: 2,
			Failed: datastore.BulkDocumentError{
				DocumentIndex: 2, Permanent: true, Message: "value too long for type character varying(100)",
			},
		})).Once()
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Return(nil, error(&datastore.BulkWriteFailure{
			Failed: datastore.BulkDocumentError{DocumentIndex: 0, Message: "InterruptedDueToReplStateChange"},
		})).Once()
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Return(nil, errors.New("connection reset")).Once()

	connector := &DatabaseStoreConnector{databaseClient: mockClient}

	err := connector.ProcessBatch(context.Background(), simpleHealthEvents())
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "event 1")
	require.Contains(t, status.Convert(err).Message(), "some_other_unique_index")

	err = connector.ProcessBatch(context.Background(), simpleHealthEvents())
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "event 2")
	require.Contains(t, status.Convert(err).Message(), "value too long for type character varying(100)")

	err = connector.ProcessBatch(context.Background(), simpleHealthEvents())
	require.Error(t, err)
	require.Equal(t, codes.Unknown, status.Code(err), "a document error that is not a duplicate or permanent is retried")

	err = connector.ProcessBatch(context.Background(), simpleHealthEvents())
	require.Error(t, err)
	require.Equal(t, codes.Unknown, status.Code(err), "a transport failure is a plain error, retried by the caller")
	mockClient.AssertExpectations(t)
}

// insertedMessages records, in call order, the message of the first event of
// each batch the mock datastore is asked to insert.
type insertedMessages struct {
	mu       sync.Mutex
	messages []string
}

func (m *insertedMessages) record(args mock.Arguments) {
	docs := args.Get(1).([]any)

	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = append(m.messages, docs[0].(model.HealthEventWithStatus).HealthEvent.GetMessage())
}

func (m *insertedMessages) get() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.messages...)
}

// batchWithMessage builds a one-event batch told apart by its message.
func batchWithMessage(message string, healthy bool) *ringbuffer.QueuedHealthEvents {
	return ringbuffer.NewQueuedHealthEvents(&protos.HealthEvents{Events: []*protos.HealthEvent{{
		NodeName:           "gpu-node-1",
		CheckName:          "GpuXidError",
		Message:            message,
		IsHealthy:          healthy,
		IsFatal:            !healthy,
		GeneratedTimestamp: timestamppb.Now(),
	}}})
}

// runLoop starts the processing loop; the channel is closed when it returns.
func runLoop(ctx context.Context, connector *DatabaseStoreConnector) <-chan struct{} {
	exited := make(chan struct{})

	go func() {
		defer close(exited)

		connector.FetchAndProcessHealthMetric(ctx)
	}()

	return exited
}

func requireClosed(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}

// queueRetries reads the retries series of the ring buffer named queue.
func queueRetries(t *testing.T, queue string) float64 {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() == "platform_connector_workqueue_retries_total_"+queue {
			return family.GetMetric()[0].GetCounter().GetValue()
		}
	}

	return 0
}

// syncBuffer is a log sink a test can read while a loop may still write to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// captureLogs sends the default logger to a buffer until the test ends.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()

	logs := &syncBuffer{}
	previous := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return logs
}

// TestFetchAndProcessHealthMetric_FailedInsert_RetriesBeforeTheNextBatch: a
// batch whose insert fails is retried before the loop takes the next one, so
// a fault and the recovery queued behind it are stored in the order the
// monitor sent them. Requeued behind the recovery, the fault would be stored
// last, and the consumers of the store would see the node fail again after it
// recovered.
func TestFetchAndProcessHealthMetric_FailedInsert_RetriesBeforeTheNextBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ringBuffer := ringbuffer.NewRingBuffer("testRetryBeforeNextBatch", ctx,
		ringbuffer.WithRetryConfig(20*time.Millisecond, 20*time.Millisecond))
	inserts := &insertedMessages{}

	mockClient := &mockDatabaseClient{}
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Run(inserts.record).Return(nil, errors.New("not primary")).Once()
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Run(inserts.record).Return(&client.InsertManyResult{InsertedIDs: []any{"id"}}, nil)

	connector := &DatabaseStoreConnector{databaseClient: mockClient, ringBuffer: ringBuffer, maxRetries: 3}
	retriesBefore := queueRetries(t, "testRetryBeforeNextBatch")

	ringBuffer.Enqueue(batchWithMessage("fault", false))
	ringBuffer.Enqueue(batchWithMessage("recovery", true))

	exited := runLoop(ctx, connector)

	require.Eventually(t, func() bool { return len(inserts.get()) == 3 }, 5*time.Second, 5*time.Millisecond)

	ringBuffer.ShutDownHealthMetricQueue()
	requireClosed(t, exited, "the loop did not return after the queue was shut down")

	require.Equal(t, []string{"fault", "fault", "recovery"}, inserts.get(),
		"the failed fault is retried before the recovery queued behind it is stored")
	require.Equal(t, 1.0, queueRetries(t, "testRetryBeforeNextBatch")-retriesBefore,
		"the retry counts in the queue's retries series, as the requeue did")
}

// TestFetchAndProcessHealthMetric_QueueShutDownDuringRetry_StoresTheBatch: a
// batch waiting for its retry when the queue is shut down is still retried and
// stored, and the loop returns only once the batch queued behind it is stored
// too. Both were acknowledged to the monitor when they were queued.
func TestFetchAndProcessHealthMetric_QueueShutDownDuringRetry_StoresTheBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ringBuffer := ringbuffer.NewRingBuffer("testShutDownDuringRetry", ctx,
		ringbuffer.WithRetryConfig(50*time.Millisecond, 50*time.Millisecond))
	inserts := &insertedMessages{}
	firstFailed := make(chan struct{})

	mockClient := &mockDatabaseClient{}
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			inserts.record(args)
			close(firstFailed)
		}).Return(nil, errors.New("not primary")).Once()
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Run(inserts.record).Return(&client.InsertManyResult{InsertedIDs: []any{"id"}}, nil)

	connector := &DatabaseStoreConnector{databaseClient: mockClient, ringBuffer: ringBuffer, maxRetries: 3}

	ringBuffer.Enqueue(batchWithMessage("first", false))
	ringBuffer.Enqueue(batchWithMessage("second", false))

	exited := runLoop(ctx, connector)

	requireClosed(t, firstFailed, "the first insert was never attempted")
	ringBuffer.ShutDownHealthMetricQueue()
	requireClosed(t, exited, "the loop did not return after the queue was shut down")

	require.Equal(t, []string{"first", "first", "second"}, inserts.get(),
		"the batch waiting for its retry at shutdown is stored, and so is the one behind it")
}

// TestFetchAndProcessHealthMetric_ContextCanceledDuringRetry_DropsTheBatchAtError:
// canceling the loop's context ends the wait for the next retry at once, as
// shutdown does when the datastore stays down, and the batch that was not
// stored is logged at Error rather than dropped silently.
func TestFetchAndProcessHealthMetric_ContextCanceledDuringRetry_DropsTheBatchAtError(t *testing.T) {
	logs := captureLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The buffer outlives the loop's context, as the process context
	// outlives the loop at shutdown.
	ringBuffer := ringbuffer.NewRingBuffer("testCancelDuringRetry", context.Background(),
		ringbuffer.WithRetryConfig(time.Minute, time.Minute))
	t.Cleanup(ringBuffer.ShutDownHealthMetricQueue)

	inserts := &insertedMessages{}

	mockClient := &mockDatabaseClient{}
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Run(inserts.record).Return(nil, errors.New("no reachable servers"))

	connector := &DatabaseStoreConnector{databaseClient: mockClient, ringBuffer: ringBuffer, maxRetries: 3}

	ringBuffer.Enqueue(batchWithMessage("fault", false))

	exited := runLoop(ctx, connector)

	require.Eventually(t, func() bool { return len(inserts.get()) == 1 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	requireClosed(t, exited, "the loop kept waiting out the retry delay after its context was canceled")

	require.Len(t, inserts.get(), 1, "no retry after the context was canceled")
	require.Contains(t, logs.String(), `level=ERROR msg="Stopped before the batch was stored`)
}

// TestFetchAndProcessHealthMetric_WriteThatCannotSucceed_IsNotRetried: a batch
// that duplicates a document under another unique index, or one the datastore
// rejects for what it contains, such as a check name longer than its
// PostgreSQL column, gets the same answer on every attempt, so it is dropped
// at once, at Error, instead of holding the batches behind it through the
// whole backoff. A replay whose events are already stored under the
// idempotency index is a success and is not retried either.
func TestFetchAndProcessHealthMetric_WriteThatCannotSucceed_IsNotRetried(t *testing.T) {
	logs := captureLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ringBuffer := ringbuffer.NewRingBuffer("testWriteThatCannotSucceed", ctx,
		ringbuffer.WithRetryConfig(time.Minute, time.Minute))
	inserts := &insertedMessages{}

	mockClient := &mockDatabaseClient{}
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Run(inserts.record).Return(nil, error(&datastore.BulkWriteFailure{
		Failed: datastore.BulkDocumentError{
			DocumentIndex: 0, Duplicate: true, IndexName: "some_other_unique_index", Message: "E11000 duplicate key",
		},
	})).Once()
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Run(inserts.record).Return(nil, error(&datastore.BulkWriteFailure{
		Failed: datastore.BulkDocumentError{
			DocumentIndex: 0, Permanent: true, Message: "value too long for type character varying(100)",
		},
	})).Once()
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Run(inserts.record).Return(&client.InsertManyResult{InsertedIDs: []any{}, DuplicateCount: 1}, nil).Once()
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Run(inserts.record).Return(&client.InsertManyResult{InsertedIDs: []any{"id"}}, nil)

	connector := &DatabaseStoreConnector{databaseClient: mockClient, ringBuffer: ringBuffer, maxRetries: 3}

	for _, message := range []string{"foreign duplicate", "rejected", "replay", "next"} {
		ringBuffer.Enqueue(batchWithMessage(message, false))
	}

	exited := runLoop(ctx, connector)

	require.Eventually(t, func() bool { return len(inserts.get()) == 4 }, 5*time.Second, 5*time.Millisecond,
		"a batch was held for a retry")

	ringBuffer.ShutDownHealthMetricQueue()
	requireClosed(t, exited, "the loop did not return after the queue was shut down")

	require.Equal(t, []string{"foreign duplicate", "rejected", "replay", "next"}, inserts.get())
	require.Equal(t, 2, strings.Count(logs.String(),
		`level=ERROR msg="Batch cannot be stored as sent, dropping health events permanently"`))
	require.Contains(t, logs.String(), `value too long for type character varying(100)`)
}

// insertedDocuments records, in call order, the message and idempotency key of
// every document the mock datastore is asked to insert.
type insertedDocuments struct {
	mu    sync.Mutex
	calls [][]string
}

func (d *insertedDocuments) record(args mock.Arguments) {
	docs := args.Get(1).([]any)
	call := make([]string, 0, len(docs))

	for _, doc := range docs {
		event := doc.(model.HealthEventWithStatus).HealthEvent
		call = append(call, event.GetMessage()+" "+event.GetMetadata()[datastore.HealthEventIdempotencyKeyMetadataField])
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.calls = append(d.calls, call)
}

func (d *insertedDocuments) get() [][]string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([][]string(nil), d.calls...)
}

func batchWithMessages(messages ...string) *protos.HealthEvents {
	events := make([]*protos.HealthEvent, 0, len(messages))
	for _, message := range messages {
		events = append(events, &protos.HealthEvent{
			NodeName: "gpu-node-1", CheckName: "GpuXidError", Message: message, IsFatal: true,
			GeneratedTimestamp: timestamppb.Now(),
		})
	}

	return &protos.HealthEvents{Events: events}
}

// TestInsertWithRetry_RejectedEvent_TheEventsAfterItAreStored: the insert is
// ordered and stops at an event the datastore rejects for what it contains, so
// the events after it were never attempted. Only the rejected event is dropped,
// at Error; the events after it are stored with the keys of their positions in
// the batch, without a retry and without dropping the batch.
func TestInsertWithRetry_RejectedEvent_TheEventsAfterItAreStored(t *testing.T) {
	logs := captureLogs(t)

	ringBuffer := ringbuffer.NewRingBuffer("testRejectedEventMidBatch", context.Background(),
		ringbuffer.WithRetryConfig(time.Minute, time.Minute))
	t.Cleanup(ringBuffer.ShutDownHealthMetricQueue)

	inserts := &insertedDocuments{}

	mockClient := &mockDatabaseClient{}
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Run(inserts.record).Return(nil, error(&datastore.BulkWriteFailure{
		InsertedCount: 1,
		Failed: datastore.BulkDocumentError{
			DocumentIndex: 1, Permanent: true, Message: "value too long for type character varying(100)",
		},
	})).Once()
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Run(inserts.record).Return(&client.InsertManyResult{InsertedIDs: []any{"id-c"}}, nil).Once()

	connector := &DatabaseStoreConnector{databaseClient: mockClient, ringBuffer: ringBuffer, maxRetries: 3}

	outcome, retries, err := connector.insertWithRetry(context.Background(), batchWithMessages("a", "b", "c"), "inst#7")
	require.NoError(t, err)
	require.Equal(t, outcomeStored, outcome)
	require.Zero(t, retries, "a rejected event is not a reason to retry")

	require.Equal(t, [][]string{
		{"a inst#7#0", "b inst#7#1", "c inst#7#2"},
		{"c inst#7#2"},
	}, inserts.get(), "only the events after the rejected one are written again, under the keys of their positions")
	require.Contains(t, logs.String(),
		`level=ERROR msg="Event cannot be stored as sent, dropping it permanently and storing the rest of its batch"`)
	require.Contains(t, logs.String(), "eventIndex=1 eventCount=3")
	require.NotContains(t, logs.String(), "Batch cannot be stored as sent")
	mockClient.AssertExpectations(t)
}

// TestInsertWithRetry_LastEventRejected_TheBatchIsStoredWithoutIt: when the
// rejected event is the last one, the events before it are already stored, so
// the batch counts as stored and only that event is dropped.
func TestInsertWithRetry_LastEventRejected_TheBatchIsStoredWithoutIt(t *testing.T) {
	logs := captureLogs(t)

	ringBuffer := ringbuffer.NewRingBuffer("testRejectedLastEvent", context.Background(),
		ringbuffer.WithRetryConfig(time.Minute, time.Minute))
	t.Cleanup(ringBuffer.ShutDownHealthMetricQueue)

	mockClient := &mockDatabaseClient{}
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Return(nil, error(&datastore.BulkWriteFailure{
			InsertedCount: 2,
			Failed: datastore.BulkDocumentError{
				DocumentIndex: 2, Permanent: true, Message: "invalid byte sequence for encoding \"UTF8\": 0x00",
			},
		})).Once()

	connector := &DatabaseStoreConnector{databaseClient: mockClient, ringBuffer: ringBuffer, maxRetries: 3}

	outcome, retries, err := connector.insertWithRetry(context.Background(), batchWithMessages("a", "b", "c"), "inst#8")
	require.NoError(t, err)
	require.Equal(t, outcomeStored, outcome)
	require.Zero(t, retries)
	require.Contains(t, logs.String(), "eventIndex=2 eventCount=3")
	require.NotContains(t, logs.String(), "Batch cannot be stored as sent")
	mockClient.AssertExpectations(t)
}

// TestFetchAndProcessHealthMetric_StoppedWithBatchesQueued_LogsThemAtError: a
// loop stopped before it emptied its queue, as Shutdown stops one still
// running at its drain timeout, logs at Error how many acknowledged batches it
// leaves behind, besides the one it was retrying.
func TestFetchAndProcessHealthMetric_StoppedWithBatchesQueued_LogsThemAtError(t *testing.T) {
	logs := captureLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ringBuffer := ringbuffer.NewRingBuffer("testStoppedWithBatchesQueued", context.Background(),
		ringbuffer.WithRetryConfig(time.Minute, time.Minute))
	t.Cleanup(ringBuffer.ShutDownHealthMetricQueue)

	inserts := &insertedMessages{}

	mockClient := &mockDatabaseClient{}
	mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
		Run(inserts.record).Return(nil, errors.New("no reachable servers"))

	connector := &DatabaseStoreConnector{databaseClient: mockClient, ringBuffer: ringBuffer, maxRetries: 3}

	for _, message := range []string{"first", "second", "third"} {
		ringBuffer.Enqueue(batchWithMessage(message, false))
	}

	exited := runLoop(ctx, connector)

	require.Eventually(t, func() bool { return len(inserts.get()) == 1 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	requireClosed(t, exited, "the loop did not return after its context was canceled")

	require.Equal(t, []string{"first"}, inserts.get())
	require.Contains(t, logs.String(), `level=ERROR msg="Stopped before the batch was stored`)
	require.Contains(t, logs.String(),
		`level=ERROR msg="Context canceled with batches still queued, dropping them permanently" batchCount=2`)
}

// TestInsertWithRetry_DefaultBackoff_KeepsRetryCountAndTiming: with the
// default backoff a failing batch is tried once and then StoreConnectorMaxRetries
// more times, 0.5, 1, 2 and then 3 s apart, the schedule the ring buffer's rate
// limiter gave a requeued batch. The fake clock checks the timing without
// waiting for it.
func TestInsertWithRetry_DefaultBackoff_KeepsRetryCountAndTiming(t *testing.T) {
	ringBuffer := ringbuffer.NewRingBuffer("testDefaultBackoff", context.Background())

	for _, test := range []struct {
		name        string
		maxRetries  int
		wantCalls   int
		wantElapsed time.Duration
	}{
		{name: "no retries", maxRetries: 0, wantCalls: 1, wantElapsed: 0},
		{name: "chart default", maxRetries: 3, wantCalls: 4, wantElapsed: 3500 * time.Millisecond},
		{name: "past the longest delay", maxRetries: 5, wantCalls: 6, wantElapsed: 9500 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				mockClient := &mockDatabaseClient{}
				mockClient.On("InsertManyIdempotent", mock.Anything, mock.Anything).
					Return(nil, errors.New("no reachable servers"))

				connector := &DatabaseStoreConnector{
					databaseClient: mockClient, ringBuffer: ringBuffer, maxRetries: test.maxRetries,
				}

				start := time.Now()
				_, retries, err := connector.insertWithRetry(context.Background(), simpleHealthEvents(), "inst#1")

				require.Error(t, err)
				require.Equal(t, test.maxRetries, retries)
				mockClient.AssertNumberOfCalls(t, "InsertManyIdempotent", test.wantCalls)
				require.Equal(t, test.wantElapsed, time.Since(start))
			})
		})
	}
}
