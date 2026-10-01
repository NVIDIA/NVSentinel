// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package central

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcauth"
	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/auth"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func TestClientIdempotencyKey(t *testing.T) {
	cases := []struct {
		name     string
		md       metadata.MD
		wantKey  string
		wantCode codes.Code
	}{
		{name: "valid key", md: metadata.Pairs(healthpub.IdempotencyKeyHeader, "batch-001.a:b_c"), wantKey: "batch-001.a:b_c"},
		{name: "absent key is rejected", md: metadata.MD{}, wantCode: codes.InvalidArgument},
		{name: "empty key is rejected", md: metadata.Pairs(healthpub.IdempotencyKeyHeader, ""), wantCode: codes.InvalidArgument},
		{
			name:     "multiple headers rejected",
			md:       metadata.Pairs(healthpub.IdempotencyKeyHeader, "a", healthpub.IdempotencyKeyHeader, "b"),
			wantCode: codes.InvalidArgument,
		},
		{name: "illegal character rejected", md: metadata.Pairs(healthpub.IdempotencyKeyHeader, "no spaces"), wantCode: codes.InvalidArgument},
		{name: "hash separator rejected", md: metadata.Pairs(healthpub.IdempotencyKeyHeader, "a#b"), wantCode: codes.InvalidArgument},
		{
			name:     "overlong key rejected",
			md:       metadata.Pairs(healthpub.IdempotencyKeyHeader, strings.Repeat("k", 129)),
			wantCode: codes.InvalidArgument,
		},
		{name: "max length key accepted", md: metadata.Pairs(healthpub.IdempotencyKeyHeader, strings.Repeat("k", 128)), wantKey: strings.Repeat("k", 128)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, err := clientIdempotencyKey(tc.md)
			if tc.wantCode != codes.OK {
				require.Error(t, err)
				require.Equal(t, tc.wantCode, status.Code(err))

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantKey, key)
		})
	}
}

func TestStampIdempotencyKeys(t *testing.T) {
	const podUID = "8b9e6c1a-pod-uid"

	he := &pb.HealthEvents{Events: []*pb.HealthEvent{
		{NodeName: "n1"},
		{NodeName: "n1", Metadata: map[string]string{
			datastore.HealthEventIdempotencyKeyMetadataField: "forged-by-caller",
			"other": "kept",
		}},
	}}

	stampIdempotencyKeys(he, podUID, "batch-1")
	require.Equal(t, podUID+"#batch-1#0", he.Events[0].Metadata[datastore.HealthEventIdempotencyKeyMetadataField])
	require.Equal(t, podUID+"#batch-1#1", he.Events[1].Metadata[datastore.HealthEventIdempotencyKeyMetadataField],
		"an inbound idempotencyKey value must be overwritten, never trusted")
	require.Equal(t, "kept", he.Events[1].Metadata["other"])
}

var unaryInfo = &grpc.UnaryServerInfo{FullMethod: "/PlatformConnector/HealthEventOccurredV1"}

// requestFrom is a request context as the auth interceptor leaves it for a
// node-pinned publisher, plus the idempotency-key header when withKey is set.
func requestFrom(withKey bool) context.Context {
	ctx := context.Background()
	if withKey {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(healthpub.IdempotencyKeyHeader, "batch-1"))
	}

	return auth.ContextWithCaller(ctx, &grpcauth.Identity{Username: testPublisher, PodUID: "pod-uid-1", NodeName: "node-a"})
}

// countingHandler records how many times it ran.
type countingHandler struct {
	calls int
}

func (h *countingHandler) handle(context.Context, any) (any, error) {
	h.calls++

	return &emptypb.Empty{}, nil
}

// TestIdempotencyInterceptor: the key is required and stamped as the
// caller's pod UID, the client key and the event index; a missing key, or a
// missing caller identity, stops the request before the handler.
func TestIdempotencyInterceptor(t *testing.T) {
	interceptor := idempotencyInterceptor

	t.Run("stamps every event", func(t *testing.T) {
		h := &countingHandler{}
		he := batchNaming("node-a", "node-a")

		_, err := interceptor(requestFrom(true), he, unaryInfo, h.handle)
		require.NoError(t, err)
		require.Equal(t, 1, h.calls)
		require.Equal(t, "pod-uid-1#batch-1#0", he.Events[0].Metadata[datastore.HealthEventIdempotencyKeyMetadataField])
		require.Equal(t, "pod-uid-1#batch-1#1", he.Events[1].Metadata[datastore.HealthEventIdempotencyKeyMetadataField])
	})

	t.Run("missing key is InvalidArgument and counted", func(t *testing.T) {
		before := testutil.ToFloat64(refusals.WithLabelValues(refusalIdempotencyKey))
		h := &countingHandler{}

		_, err := interceptor(requestFrom(false), batchNaming("node-a"), unaryInfo, h.handle)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Zero(t, h.calls)
		require.Equal(t, before+1, testutil.ToFloat64(refusals.WithLabelValues(refusalIdempotencyKey)))
	})

	t.Run("missing caller identity is Internal", func(t *testing.T) {
		h := &countingHandler{}
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs(healthpub.IdempotencyKeyHeader, "batch-1"))

		_, err := interceptor(ctx, batchNaming("node-a"), unaryInfo, h.handle)
		require.Equal(t, codes.Internal, status.Code(err))
		require.Zero(t, h.calls)
	})

	t.Run("other requests pass through", func(t *testing.T) {
		h := &countingHandler{}

		_, err := interceptor(context.Background(), &emptypb.Empty{}, unaryInfo, h.handle)
		require.NoError(t, err)
		require.Equal(t, 1, h.calls)
	})
}

// TestReadinessInterceptor: a replica whose idempotency index is not verified
// refuses batches with a retryable status, so an established connection
// cannot make it store a resend twice; once verified, batches pass.
func TestReadinessInterceptor(t *testing.T) {
	gate := &indexGate{}
	interceptor := readinessInterceptor(gate)
	h := &countingHandler{}

	before := testutil.ToFloat64(refusals.WithLabelValues(refusalIndexUnverified))

	_, err := interceptor(requestFrom(true), batchNaming("node-a"), unaryInfo, h.handle)
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Zero(t, h.calls, "nothing reaches the handler while the index is unverified")
	require.Equal(t, before+1, testutil.ToFloat64(refusals.WithLabelValues(refusalIndexUnverified)))

	gate.verified.Store(true)

	_, err = interceptor(requestFrom(true), batchNaming("node-a"), unaryInfo, h.handle)
	require.NoError(t, err)
	require.Equal(t, 1, h.calls)
}
