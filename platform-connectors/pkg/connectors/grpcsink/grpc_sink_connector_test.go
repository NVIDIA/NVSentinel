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

package grpcsink

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
)

type mockPlatformConnectorClient struct {
	mock.Mock
}

func (m *mockPlatformConnectorClient) HealthEventOccurredV1(
	ctx context.Context,
	in *protos.HealthEvents,
	opts ...grpc.CallOption,
) (*emptypb.Empty, error) {
	args := m.Called(ctx, in)
	return args.Get(0).(*emptypb.Empty), args.Error(1)
}

func newTestConnector(client *mockPlatformConnectorClient, rb *ringbuffer.RingBuffer, maxRetries int) *GRPCSinkConnector {
	return &GRPCSinkConnector{
		client:     client,
		ringBuffer: rb,
		maxRetries: maxRetries,
		rpcTimeout: 5 * time.Second,
	}
}

func TestSendHealthEvents_Success(t *testing.T) {
	mockClient := &mockPlatformConnectorClient{}
	rb := ringbuffer.NewRingBuffer("testSendSuccess", context.Background())

	connector := newTestConnector(mockClient, rb, 3)

	mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Return(&emptypb.Empty{}, nil)

	healthEvents := &protos.HealthEvents{
		Events: []*protos.HealthEvent{{
			NodeName:  "gpu-node-1",
			CheckName: "GpuMemWatch",
			IsFatal:   true,
			IsHealthy: false,
		}},
	}

	err := connector.sendHealthEvents(context.Background(), healthEvents)
	require.NoError(t, err)
	mockClient.AssertExpectations(t)
}

func TestSendHealthEvents_Failure(t *testing.T) {
	mockClient := &mockPlatformConnectorClient{}
	rb := ringbuffer.NewRingBuffer("testSendFailure", context.Background())

	connector := newTestConnector(mockClient, rb, 3)

	mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Return((*emptypb.Empty)(nil), errors.New("connection refused"))

	healthEvents := &protos.HealthEvents{
		Events: []*protos.HealthEvent{{
			NodeName:  "gpu-node-1",
			CheckName: "SysLogsXIDError",
			ErrorCode: []string{"79"},
			IsFatal:   true,
			IsHealthy: false,
		}},
	}

	err := connector.sendHealthEvents(context.Background(), healthEvents)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to forward health events to gRPC sink")
	mockClient.AssertExpectations(t)
}

func TestFetchAndProcessHealthMetric_Success(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rb := ringbuffer.NewRingBuffer("testFetchSuccess", ctx)
	mockClient := &mockPlatformConnectorClient{}

	mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Return(&emptypb.Empty{}, nil)

	connector := newTestConnector(mockClient, rb, 3)

	healthEvents := &protos.HealthEvents{
		Events: []*protos.HealthEvent{{
			NodeName:           "gpu-node-1",
			GeneratedTimestamp: timestamppb.New(time.Now()),
			CheckName:          "GpuMemWatch",
		}},
	}

	rb.Enqueue(ringbuffer.NewQueuedHealthEvents(healthEvents))
	require.Equal(t, 1, rb.CurrentLength())

	go connector.FetchAndProcessHealthMetric(ctx)

	require.Eventually(t, func() bool {
		return rb.CurrentLength() == 0
	}, 1*time.Second, 10*time.Millisecond, "event should be dequeued")

	time.Sleep(50 * time.Millisecond)

	cancel()
	mockClient.AssertExpectations(t)
}

func TestFetchAndProcessHealthMetric_RetryOnGRPCFailure_EventuallyProcessed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rb := ringbuffer.NewRingBuffer("testRetry", ctx,
		ringbuffer.WithRetryConfig(10*time.Millisecond, 50*time.Millisecond))
	mockClient := &mockPlatformConnectorClient{}

	// First 2 calls fail, 3rd succeeds
	mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Return((*emptypb.Empty)(nil), errors.New("gRPC sink target temporarily unavailable")).Times(2)
	mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Return(&emptypb.Empty{}, nil).Once()

	connector := newTestConnector(mockClient, rb, 3)

	healthEvents := &protos.HealthEvents{
		Events: []*protos.HealthEvent{{
			NodeName:           "gpu-node-1",
			GeneratedTimestamp: timestamppb.New(time.Now()),
			CheckName:          "SysLogsXIDError",
			ErrorCode:          []string{"74"},
			IsFatal:            true,
			IsHealthy:          false,
		}},
	}

	rb.Enqueue(ringbuffer.NewQueuedHealthEvents(healthEvents))
	require.Equal(t, 1, rb.CurrentLength())

	go connector.FetchAndProcessHealthMetric(ctx)

	require.Eventually(t, func() bool {
		return rb.CurrentLength() == 0
	}, 500*time.Millisecond, 10*time.Millisecond, "queue should be empty after successful retry")

	time.Sleep(100 * time.Millisecond)

	mockClient.AssertNumberOfCalls(t, "HealthEventOccurredV1", 3)
	cancel()
}

func TestFetchAndProcessHealthMetric_MaxRetries_EventDropped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rb := ringbuffer.NewRingBuffer("testMaxRetries", ctx,
		ringbuffer.WithRetryConfig(10*time.Millisecond, 50*time.Millisecond))
	mockClient := &mockPlatformConnectorClient{}

	// Always fail
	mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Return((*emptypb.Empty)(nil), errors.New("gRPC sink target permanently unreachable"))

	connector := newTestConnector(mockClient, rb, 3)

	healthEvents := &protos.HealthEvents{
		Events: []*protos.HealthEvent{{
			NodeName:           "gpu-node-1",
			GeneratedTimestamp: timestamppb.New(time.Now()),
			CheckName:          "SysLogsXIDError",
			ErrorCode:          []string{"79"},
			IsFatal:            true,
			IsHealthy:          false,
		}},
	}

	rb.Enqueue(ringbuffer.NewQueuedHealthEvents(healthEvents))
	require.Equal(t, 1, rb.CurrentLength())

	go connector.FetchAndProcessHealthMetric(ctx)

	require.Eventually(t, func() bool {
		return rb.CurrentLength() == 0
	}, 500*time.Millisecond, 10*time.Millisecond, "event should be dropped after max retries")

	time.Sleep(100 * time.Millisecond)

	// Initial call + 3 retries = 4 total
	mockClient.AssertNumberOfCalls(t, "HealthEventOccurredV1", 4)
	cancel()
}

func TestClose_NilConn(t *testing.T) {
	connector := &GRPCSinkConnector{conn: nil}
	err := connector.Close()
	require.NoError(t, err)
}

// sentMessages records, in call order, the message of the first event of each
// batch the mock client is asked to send.
type sentMessages struct {
	mu       sync.Mutex
	messages []string
}

func (m *sentMessages) record(args mock.Arguments) {
	events := args.Get(1).(*protos.HealthEvents)

	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = append(m.messages, events.GetEvents()[0].GetMessage())
}

func (m *sentMessages) get() []string {
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
func runLoop(ctx context.Context, connector *GRPCSinkConnector) <-chan struct{} {
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

// TestFetchAndProcessHealthMetric_SendFailingAfterQueueShutDown_IsRetried: a
// send that fails after the queue is shut down, while Shutdown waits for the
// loop, is retried and the batch reaches the sink before the one behind it. A
// queue that is shut down takes no batch back, so a failed batch handed back
// to it would be lost without a log line.
func TestFetchAndProcessHealthMetric_SendFailingAfterQueueShutDown_IsRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rb := ringbuffer.NewRingBuffer("testSendFailingAfterShutDown", ctx,
		ringbuffer.WithRetryConfig(10*time.Millisecond, 10*time.Millisecond))
	sent := &sentMessages{}
	inFlight := make(chan struct{})
	release := make(chan struct{})

	mockClient := &mockPlatformConnectorClient{}
	mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			sent.record(args)
			close(inFlight)
			<-release
		}).Return((*emptypb.Empty)(nil), errors.New("sink unavailable")).Once()
	mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Run(sent.record).Return(&emptypb.Empty{}, nil)

	connector := newTestConnector(mockClient, rb, 3)

	rb.Enqueue(batchWithMessage("first", false))
	rb.Enqueue(batchWithMessage("second", false))

	exited := runLoop(ctx, connector)

	requireClosed(t, inFlight, "the first send was never attempted")
	rb.ShutDownHealthMetricQueue()
	close(release)
	requireClosed(t, exited, "the loop did not return after the queue was shut down")

	require.Equal(t, []string{"first", "first", "second"}, sent.get(),
		"the batch whose send failed after the shutdown was sent again, before the one behind it")
}

// TestFetchAndProcessHealthMetric_FailedSend_RetriedBeforeTheNextBatch: a batch
// whose send fails is retried before the loop takes the next one, so the sink
// gets a fault and the recovery queued behind it in the order the monitor sent
// them. The retry counts in the same two series a requeue counted in.
func TestFetchAndProcessHealthMetric_FailedSend_RetriedBeforeTheNextBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rb := ringbuffer.NewRingBuffer("testSendRetryBeforeNextBatch", ctx,
		ringbuffer.WithRetryConfig(20*time.Millisecond, 20*time.Millisecond))
	sent := &sentMessages{}

	mockClient := &mockPlatformConnectorClient{}
	mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Run(sent.record).Return((*emptypb.Empty)(nil), errors.New("sink unavailable")).Once()
	mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Run(sent.record).Return(&emptypb.Empty{}, nil)

	connector := newTestConnector(mockClient, rb, 3)
	queueRetriesBefore := queueRetries(t, "testSendRetryBeforeNextBatch")
	sinkRetriesBefore := testutil.ToFloat64(grpcSinkRetryCounter)

	rb.Enqueue(batchWithMessage("fault", false))
	rb.Enqueue(batchWithMessage("recovery", true))

	exited := runLoop(ctx, connector)

	require.Eventually(t, func() bool { return len(sent.get()) == 3 }, 5*time.Second, 5*time.Millisecond)

	rb.ShutDownHealthMetricQueue()
	requireClosed(t, exited, "the loop did not return after the queue was shut down")

	require.Equal(t, []string{"fault", "fault", "recovery"}, sent.get(),
		"the failed fault is sent again before the recovery queued behind it")
	require.Equal(t, 1.0, queueRetries(t, "testSendRetryBeforeNextBatch")-queueRetriesBefore,
		"the retry counts in the queue's retries series, as the requeue did")
	require.Equal(t, 1.0, testutil.ToFloat64(grpcSinkRetryCounter)-sinkRetriesBefore,
		"the retry counts in grpc_sink_platform_connector_retry_total, as the requeue did")
}

// TestFetchAndProcessHealthMetric_ContextCanceledDuringRetry_DropsTheBatchAtError:
// canceling the loop's context ends the wait for the next retry at once, as
// Shutdown does at the drain timeout, and the batch that was not sent is
// logged at Error rather than dropped silently.
func TestFetchAndProcessHealthMetric_ContextCanceledDuringRetry_DropsTheBatchAtError(t *testing.T) {
	logs := captureLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The buffer outlives the loop's context, as the process context
	// outlives the loop at shutdown.
	rb := ringbuffer.NewRingBuffer("testSendCancelDuringRetry", context.Background(),
		ringbuffer.WithRetryConfig(time.Minute, time.Minute))
	t.Cleanup(rb.ShutDownHealthMetricQueue)

	sent := &sentMessages{}

	mockClient := &mockPlatformConnectorClient{}
	mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Run(sent.record).Return((*emptypb.Empty)(nil), errors.New("sink unavailable"))

	connector := newTestConnector(mockClient, rb, 3)

	rb.Enqueue(batchWithMessage("fault", false))

	exited := runLoop(ctx, connector)

	require.Eventually(t, func() bool { return len(sent.get()) == 1 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	requireClosed(t, exited, "the loop kept waiting out the retry delay after its context was canceled")

	require.Len(t, sent.get(), 1, "no retry after the context was canceled")
	require.Contains(t, logs.String(),
		`level=ERROR msg="Stopped before the batch was sent, dropping health events permanently"`)
}

// TestFetchAndProcessHealthMetric_StoppedWithBatchesQueued_LogsThemAtError: a
// loop stopped before it emptied its queue, as Shutdown stops one still
// running at its drain timeout, logs at Error how many acknowledged batches it
// leaves behind, besides the one it was retrying, as the store connector does.
func TestFetchAndProcessHealthMetric_StoppedWithBatchesQueued_LogsThemAtError(t *testing.T) {
	logs := captureLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rb := ringbuffer.NewRingBuffer("testSendStoppedWithBatchesQueued", context.Background(),
		ringbuffer.WithRetryConfig(time.Minute, time.Minute))
	t.Cleanup(rb.ShutDownHealthMetricQueue)

	sent := &sentMessages{}

	mockClient := &mockPlatformConnectorClient{}
	mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Run(sent.record).Return((*emptypb.Empty)(nil), errors.New("sink unavailable"))

	connector := newTestConnector(mockClient, rb, 3)

	for _, message := range []string{"first", "second", "third"} {
		rb.Enqueue(batchWithMessage(message, false))
	}

	exited := runLoop(ctx, connector)

	require.Eventually(t, func() bool { return len(sent.get()) == 1 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	requireClosed(t, exited, "the loop did not return after its context was canceled")

	require.Equal(t, []string{"first"}, sent.get())
	require.Contains(t, logs.String(),
		`level=ERROR msg="Stopped before the batch was sent, dropping health events permanently"`)
	require.Contains(t, logs.String(),
		`level=ERROR msg="Context canceled with batches still queued, dropping them permanently" batchCount=2`)
}

// TestFetchAndProcessHealthMetric_StatusTheTargetRepeats_IsNotRetried: a batch
// the target refuses with a status it gives every resend of it
// (InvalidArgument, PermissionDenied, Unimplemented) is dropped at once, at
// Error, so the batch behind it goes out without waiting out a backoff. Every
// other status, Unavailable, Unauthenticated and ResourceExhausted among them,
// is still retried.
func TestFetchAndProcessHealthMetric_StatusTheTargetRepeats_IsNotRetried(t *testing.T) {
	for _, test := range []struct {
		code    codes.Code
		retried bool
	}{
		{code: codes.InvalidArgument},
		{code: codes.PermissionDenied},
		{code: codes.Unimplemented},
		{code: codes.Unavailable, retried: true},
		{code: codes.Unauthenticated, retried: true},
		{code: codes.ResourceExhausted, retried: true},
	} {
		t.Run(test.code.String(), func(t *testing.T) {
			logs := captureLogs(t)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// A retry of a refused batch would hold the one behind it for a
			// minute; a status that is retried gets a short backoff.
			delay := time.Minute
			if test.retried {
				delay = 10 * time.Millisecond
			}

			rb := ringbuffer.NewRingBuffer("testStatusTheTargetRepeats", ctx, ringbuffer.WithRetryConfig(delay, delay))
			sent := &sentMessages{}

			mockClient := &mockPlatformConnectorClient{}
			mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
				Run(sent.record).Return((*emptypb.Empty)(nil), status.Error(test.code, "the target answers "+test.code.String())).
				Once()
			mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
				Run(sent.record).Return(&emptypb.Empty{}, nil)

			connector := newTestConnector(mockClient, rb, 3)

			rb.Enqueue(batchWithMessage("first", false))
			rb.Enqueue(batchWithMessage("next", false))

			exited := runLoop(ctx, connector)

			want := []string{"first", "next"}
			if test.retried {
				want = []string{"first", "first", "next"}
			}

			require.Eventually(t, func() bool { return len(sent.get()) == len(want) }, 5*time.Second, 5*time.Millisecond,
				"the batch behind the first one was held for a retry")

			rb.ShutDownHealthMetricQueue()
			requireClosed(t, exited, "the loop did not return after the queue was shut down")

			require.Equal(t, want, sent.get())

			if !test.retried {
				require.Regexp(t, `level=ERROR msg="gRPC sink refused the batch, dropping health events permanently"`+
					` error="[^"]*code = `+test.code.String()+`[^"]*" retryCount=0 maxRetries=3`, logs.String())
			}
		})
	}
}

// TestSendWithRetry_DefaultBackoff_KeepsRetryCountAndTiming: with the default
// backoff a failing batch is sent once and then GRPCSinkConnectorMaxRetries
// more times, 0.5, 1, 2 and then 3 s apart, the schedule the ring buffer's
// rate limiter gave a requeued batch. The fake clock checks the timing without
// waiting for it.
func TestSendWithRetry_DefaultBackoff_KeepsRetryCountAndTiming(t *testing.T) {
	rb := ringbuffer.NewRingBuffer("testSendDefaultBackoff", context.Background())

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
				mockClient := &mockPlatformConnectorClient{}
				mockClient.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
					Return((*emptypb.Empty)(nil), errors.New("sink unavailable"))

				connector := newTestConnector(mockClient, rb, test.maxRetries)

				start := time.Now()
				retries, err := connector.sendWithRetry(context.Background(), batchWithMessage("fault", false).Events)

				require.Error(t, err)
				require.Equal(t, test.maxRetries, retries)
				mockClient.AssertNumberOfCalls(t, "HealthEventOccurredV1", test.wantCalls)
				require.Equal(t, test.wantElapsed, time.Since(start))
			})
		})
	}
}
