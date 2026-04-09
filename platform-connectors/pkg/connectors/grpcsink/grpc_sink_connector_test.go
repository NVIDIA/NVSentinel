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
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
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

func TestShutdownRingBuffer_AfterShutdown_EnqueueIsNoOp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rb := ringbuffer.NewRingBuffer("testShutdown", ctx)
	connector := &GRPCSinkConnector{ringBuffer: rb}

	done := make(chan struct{})
	go func() {
		connector.ShutdownRingBuffer()
		close(done)
	}()

	select {
	case <-done:
		// ShutdownRingBuffer completed successfully
	case <-time.After(1 * time.Second):
		t.Fatal("ShutdownRingBuffer did not complete within timeout")
	}

	// Enqueue after shutdown is a no-op; queue length stays 0
	rb.Enqueue(ringbuffer.NewQueuedHealthEvents(&protos.HealthEvents{}))
	require.Equal(t, 0, rb.CurrentLength())
}

func TestClose_NilConn(t *testing.T) {
	connector := &GRPCSinkConnector{conn: nil}
	err := connector.Close()
	require.NoError(t, err)
}

// --- tokenInterceptor tests ---

func writeTestToken(t *testing.T, content string) string {
	t.Helper()

	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte(content), 0o600))

	return tokenPath
}

func TestTokenInterceptor_AttachesToken(t *testing.T) {
	tokenPath := writeTestToken(t, "test-token-value")
	interceptor := tokenInterceptor(tokenPath)

	var capturedCtx context.Context

	fakeInvoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		capturedCtx = ctx
		return nil
	}

	err := interceptor(context.Background(), "/test.Method", nil, nil, nil, fakeInvoker)
	require.NoError(t, err)

	md, ok := metadata.FromOutgoingContext(capturedCtx)
	require.True(t, ok, "outgoing metadata should be present")

	authValues := md.Get("authorization")
	require.Len(t, authValues, 1)
	assert.Equal(t, "Bearer test-token-value", authValues[0])
}

func TestTokenInterceptor_ReReadsToken(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("token-v1"), 0o600))

	interceptor := tokenInterceptor(tokenPath)

	var firstCtx context.Context

	fakeInvoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		firstCtx = ctx
		return nil
	}

	require.NoError(t, interceptor(context.Background(), "/test", nil, nil, nil, fakeInvoker))

	md1, _ := metadata.FromOutgoingContext(firstCtx)
	assert.Equal(t, "Bearer token-v1", md1.Get("authorization")[0])

	// Rotate token
	require.NoError(t, os.WriteFile(tokenPath, []byte("token-v2"), 0o600))

	var secondCtx context.Context

	fakeInvoker2 := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		secondCtx = ctx
		return nil
	}

	require.NoError(t, interceptor(context.Background(), "/test", nil, nil, nil, fakeInvoker2))

	md2, _ := metadata.FromOutgoingContext(secondCtx)
	assert.Equal(t, "Bearer token-v2", md2.Get("authorization")[0])
}

func TestTokenInterceptor_MissingTokenFile(t *testing.T) {
	interceptor := tokenInterceptor("/nonexistent/token")

	fakeInvoker := func(_ context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		t.Fatal("invoker should not be called when token file is missing")
		return nil
	}

	err := interceptor(context.Background(), "/test", nil, nil, nil, fakeInvoker)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "reading SA token")
}

func TestTokenInterceptor_TrimsWhitespace(t *testing.T) {
	tokenPath := writeTestToken(t, "  my-token\n")
	interceptor := tokenInterceptor(tokenPath)

	var capturedCtx context.Context

	fakeInvoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		capturedCtx = ctx
		return nil
	}

	require.NoError(t, interceptor(context.Background(), "/test", nil, nil, nil, fakeInvoker))

	md, _ := metadata.FromOutgoingContext(capturedCtx)
	assert.Equal(t, "Bearer my-token", md.Get("authorization")[0])
}
