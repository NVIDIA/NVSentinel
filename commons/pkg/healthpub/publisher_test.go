// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package healthpub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// fakePCClient stands in for pb.PlatformConnectorClient. responseFn
// receives a 1-based call number; nil return is treated as success.
type fakePCClient struct {
	calls      atomic.Int64
	responseFn func(call int) error
}

func (f *fakePCClient) HealthEventOccurredV1(
	_ context.Context, _ *pb.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	n := int(f.calls.Add(1))
	if f.responseFn != nil {
		if err := f.responseFn(n); err != nil {
			return nil, err
		}
	}

	return &emptypb.Empty{}, nil
}

func sampleEvents() *pb.HealthEvents {
	return &pb.HealthEvents{
		Version: 1,
		Events: []*pb.HealthEvent{{
			Version:            1,
			Agent:              "test",
			CheckName:          "TestCheck",
			ComponentClass:     "Test",
			GeneratedTimestamp: timestamppb.New(time.Unix(0, 0)),
			NodeName:           "test-node",
		}},
	}
}

// fastRetryOpt makes "all retries exhausted" tests complete in ms.
func fastRetryOpt() Option {
	return WithRetryPolicy(3, 5*time.Millisecond, 1.0, 0)
}

// touchSocket creates an empty file at path; the publisher only stats.
func touchSocket(t *testing.T, path string) {
	t.Helper()

	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// TestPublish_SkipsWhenSocketMissing: with the socket absent, Publish
// must skip without invoking gRPC, increment the skip counter, and
// return ErrPlatformConnectorUnavailable.
func TestPublish_SkipsWhenSocketMissing(t *testing.T) {
	tmp := t.TempDir()
	target := "unix://" + filepath.Join(tmp, "nvsentinel.sock") // path does NOT exist

	monitor := "test-skip-when-missing"

	skippedBefore := testutil.ToFloat64(sendsSkippedPCUnavailable.WithLabelValues(monitor))
	successBefore := testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor))

	fc := &fakePCClient{}

	p := New(fc, target, monitor, fastRetryOpt())

	start := time.Now()
	err := p.Publish(context.Background(), sampleEvents())
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrPlatformConnectorUnavailable,
		"expected ErrPlatformConnectorUnavailable when socket file is absent")
	assert.Equal(t, int64(0), fc.calls.Load(),
		"gRPC must NOT be invoked when socket is missing — no buffering, no retries")
	assert.Less(t, elapsed, 100*time.Millisecond,
		"skip must be fast; if elapsed approaches the retry budget the gate isn't firing")

	skippedAfter := testutil.ToFloat64(sendsSkippedPCUnavailable.WithLabelValues(monitor))
	successAfter := testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor))
	assert.Equal(t, skippedBefore+1, skippedAfter,
		"skip counter must increment exactly once per skipped Publish")
	assert.Equal(t, successBefore, successAfter,
		"success counter must not move when the call was skipped")
}

// TestPublish_SuccessWhenSocketPresent: happy path — gRPC accepts on
// first attempt, success counter +1, skip counter unchanged.
func TestPublish_SuccessWhenSocketPresent(t *testing.T) {
	tmp := t.TempDir()
	socket := filepath.Join(tmp, "nvsentinel.sock")
	touchSocket(t, socket)

	monitor := "test-success-when-present"

	skippedBefore := testutil.ToFloat64(sendsSkippedPCUnavailable.WithLabelValues(monitor))
	successBefore := testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor))

	fc := &fakePCClient{}

	p := New(fc, "unix://"+socket, monitor)

	require.NoError(t, p.Publish(context.Background(), sampleEvents()))
	assert.Equal(t, int64(1), fc.calls.Load(),
		"gRPC must be invoked exactly once on a clean send")

	skippedAfter := testutil.ToFloat64(sendsSkippedPCUnavailable.WithLabelValues(monitor))
	successAfter := testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor))
	assert.Equal(t, skippedBefore, skippedAfter,
		"skip counter must NOT move on a successful send")
	assert.Equal(t, successBefore+1, successAfter,
		"success counter must increment on a successful send")
}

// TestPublish_RetriesOnTransientErrorThenSucceeds: a transient
// Unavailable with the socket present must be retried.
func TestPublish_RetriesOnTransientErrorThenSucceeds(t *testing.T) {
	tmp := t.TempDir()
	socket := filepath.Join(tmp, "nvsentinel.sock")
	touchSocket(t, socket)

	monitor := "test-retry-transient"

	fc := &fakePCClient{
		responseFn: func(call int) error {
			if call < 2 {
				return status.Error(codes.Unavailable, "transient")
			}

			return nil
		},
	}

	p := New(fc, "unix://"+socket, monitor, fastRetryOpt())

	require.NoError(t, p.Publish(context.Background(), sampleEvents()))
	assert.Equal(t, int64(2), fc.calls.Load(),
		"first call should fail Unavailable, second should succeed")
}

// TestPublish_AbortsMidRetryWhenSocketDisappears: socket vanishing
// between attempts must abort the loop and charge the skip counter.
func TestPublish_AbortsMidRetryWhenSocketDisappears(t *testing.T) {
	tmp := t.TempDir()
	socket := filepath.Join(tmp, "nvsentinel.sock")
	touchSocket(t, socket)

	monitor := "test-abort-mid-retry"

	fc := &fakePCClient{
		responseFn: func(call int) error {
			// First attempt fails transiently AND removes the socket.
			if call == 1 {
				_ = os.Remove(socket)
				return status.Error(codes.Unavailable, "transient")
			}

			return nil
		},
	}

	p := New(fc, "unix://"+socket, monitor,
		WithRetryPolicy(5, 5*time.Millisecond, 1.0, 0))

	skippedBefore := testutil.ToFloat64(sendsSkippedPCUnavailable.WithLabelValues(monitor))

	err := p.Publish(context.Background(), sampleEvents())
	require.ErrorIs(t, err, ErrPlatformConnectorUnavailable)
	assert.Equal(t, int64(1), fc.calls.Load(),
		"gRPC must be invoked exactly once before the in-loop probe catches the disappearance")

	skippedAfter := testutil.ToFloat64(sendsSkippedPCUnavailable.WithLabelValues(monitor))
	assert.Equal(t, skippedBefore+1, skippedAfter,
		"in-loop disappearance must charge the skip counter (not the error counter)")
}

// TestPublish_NonRetryableErrorReturnsImmediately: permanent errors
// like InvalidArgument must not consume the retry budget.
func TestPublish_NonRetryableErrorReturnsImmediately(t *testing.T) {
	tmp := t.TempDir()
	socket := filepath.Join(tmp, "nvsentinel.sock")
	touchSocket(t, socket)

	monitor := "test-non-retryable"

	fc := &fakePCClient{
		responseFn: func(_ int) error {
			return status.Error(codes.InvalidArgument, "bad request")
		},
	}

	p := New(fc, "unix://"+socket, monitor, fastRetryOpt())

	err := p.Publish(context.Background(), sampleEvents())
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPlatformConnectorUnavailable)
	assert.Equal(t, int64(1), fc.calls.Load(),
		"non-retryable errors must NOT trigger retries")
}

// TestUnixSocketPathFromTarget covers both Unix-socket URI variants
// gRPC accepts, plus the negative TCP/dns/empty cases.
func TestUnixSocketPathFromTarget(t *testing.T) {
	cases := []struct {
		target string
		want   string
	}{
		{"unix:///var/run/nvsentinel.sock", "/var/run/nvsentinel.sock"},
		{"unix:/var/run/nvsentinel.sock", "/var/run/nvsentinel.sock"},
		{"unix:relative/path", ""},
		{"127.0.0.1:5555", ""},
		{"dns:///host:5555", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			assert.Equal(t, tc.want, unixSocketPathFromTarget(tc.target))
		})
	}
}

// TestPublish_TCPTargetSkipsGate: non-unix targets must bypass the
// socket-existence probe entirely.
func TestPublish_TCPTargetSkipsGate(t *testing.T) {
	monitor := "test-tcp-target"

	fc := &fakePCClient{}

	p := New(fc, "127.0.0.1:5555", monitor)

	assert.Empty(t, p.SocketPath(),
		"non-unix:// targets must not derive a socketPath")
	require.NoError(t, p.Publish(context.Background(), sampleEvents()))
	assert.Equal(t, int64(1), fc.calls.Load(),
		"TCP target must reach the gRPC call without gating on a file")
}

// TestPublish_NilOrEmptyEventsIsNoOp: empty batches short-circuit
// before any gRPC call.
func TestPublish_NilOrEmptyEventsIsNoOp(t *testing.T) {
	monitor := "test-noop"

	fc := &fakePCClient{}

	p := New(fc, "unix:///nonexistent.sock", monitor)

	assert.NoError(t, p.Publish(context.Background(), nil),
		"nil HealthEvents must be a no-op error-free")
	assert.NoError(t, p.Publish(context.Background(), &pb.HealthEvents{}),
		"empty HealthEvents must be a no-op error-free")
	assert.Equal(t, int64(0), fc.calls.Load(),
		"empty input must short-circuit before any gRPC call")
}

// TestPublish_ContextCancellationStopsRetries: a cancelled context
// must abort the retry loop early.
func TestPublish_ContextCancellationStopsRetries(t *testing.T) {
	tmp := t.TempDir()
	socket := filepath.Join(tmp, "nvsentinel.sock")
	touchSocket(t, socket)

	monitor := "test-ctx-cancel"

	fc := &fakePCClient{
		responseFn: func(_ int) error {
			return status.Error(codes.Unavailable, "transient")
		},
	}

	p := New(fc, "unix://"+socket, monitor,
		WithRetryPolicy(20, 50*time.Millisecond, 1.0, 0))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	err := p.Publish(ctx, sampleEvents())
	require.Error(t, err)
	// Accept either context.DeadlineExceeded or wait.ErrWaitTimeout.
	assert.True(t,
		errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, context.Canceled) ||
			err.Error() == "timed out waiting for the condition" ||
			err.Error() == "context deadline exceeded",
		"expected a context-related error, got %v", err)
	assert.Less(t, fc.calls.Load(), int64(20),
		"retries must stop when the context is cancelled, not exhaust the full budget")
}
