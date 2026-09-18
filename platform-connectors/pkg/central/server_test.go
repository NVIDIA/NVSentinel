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
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/auth"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/server"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// scriptedVerifier answers each index check with the next result the test
// sends; a check blocks until the test provides one, which makes the loop's
// progress observable: when the n+1th check is waiting, the nth result has
// been applied.
type scriptedVerifier struct {
	results chan error
	calls   atomic.Int32
}

func (v *scriptedVerifier) VerifyIdempotencyIndex(ctx context.Context) error {
	v.calls.Add(1)

	select {
	case err := <-v.results:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (v *scriptedVerifier) awaitCall(t *testing.T, n int32) {
	t.Helper()
	require.Eventually(t, func() bool { return v.calls.Load() >= n }, 5*time.Second, time.Millisecond)
}

// TestVerifyIndexLoop_ReadinessFollowsDefinitiveAnswers: the replica becomes
// ready when the index verifies, stays ready through a datastore failure,
// turns unready (and so refuses writes) when the index is confirmed missing or
// changed, and becomes ready again once it verifies.
func TestVerifyIndexLoop_ReadinessFollowsDefinitiveAnswers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	verifier := &scriptedVerifier{results: make(chan error)}
	ready := &readiness{}

	go verifyIndexLoop(ctx, verifier, ready, time.Millisecond, time.Millisecond, 10*time.Second)

	verifier.awaitCall(t, 1)
	require.Error(t, ready.Ready(ctx), "unready until the first verification")

	verifier.results <- errors.New("connection refused")
	verifier.awaitCall(t, 2)
	require.Error(t, ready.Ready(ctx), "a datastore failure before the first verification keeps the replica unready")

	verifier.results <- nil
	verifier.awaitCall(t, 3)
	require.NoError(t, ready.Ready(ctx), "a verified index makes the replica ready")

	verifier.results <- errors.New("connection refused")
	verifier.awaitCall(t, 4)
	require.NoError(t, ready.Ready(ctx), "a datastore failure leaves a ready replica ready")

	verifier.results <- datastore.ErrIndexMissing
	verifier.awaitCall(t, 5)
	require.Error(t, ready.Ready(ctx), "a confirmed missing index turns the replica unready")

	verifier.results <- datastore.ErrIndexMismatch
	verifier.awaitCall(t, 6)
	require.Error(t, ready.Ready(ctx))

	verifier.results <- nil
	verifier.awaitCall(t, 7)
	require.NoError(t, ready.Ready(ctx), "the recreated index makes the replica ready again")
}

// TestVerifyIndexLoop_StalledCheckDoesNotStopTheLoop: a check that never
// returns ends with its own deadline, counts as a datastore failure (readiness
// unchanged) and the loop goes on to the next check.
func TestVerifyIndexLoop_StalledCheckDoesNotStopTheLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// No result is ever sent: every check blocks until its own deadline.
	verifier := &scriptedVerifier{results: make(chan error)}
	ready := &readiness{}
	ready.indexVerified.Store(true)

	go verifyIndexLoop(ctx, verifier, ready, time.Millisecond, time.Millisecond, 20*time.Millisecond)

	verifier.awaitCall(t, 3)
	require.NoError(t, ready.Ready(ctx), "a check that timed out leaves readiness unchanged")
}

func TestReadiness(t *testing.T) {
	r := &readiness{}
	require.Error(t, r.Ready(context.Background()), "not ready before the index is verified")

	r.indexVerified.Store(true)
	require.NoError(t, r.Ready(context.Background()))

	r.shuttingDown.Store(true)
	require.Error(t, r.Ready(context.Background()), "shutting down flips unready even with the index verified")
}

// recordingConnector keeps the last batch it was handed.
type recordingConnector struct {
	calls atomic.Int32
	mu    sync.Mutex
	last  *pb.HealthEvents
}

func (r *recordingConnector) ProcessBatch(_ context.Context, he *pb.HealthEvents) error {
	r.calls.Add(1)
	r.mu.Lock()
	r.last = he
	r.mu.Unlock()

	return nil
}

func (r *recordingConnector) lastBatch() *pb.HealthEvents {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.last
}

// TestInterceptorChain_AsWired: the deployment's chain over a real gRPC
// server, auth then idempotency then readiness then the handler. A caller
// without a token is refused first; a missing key is refused before the
// readiness gate answers; while the index is unverified a keyed batch gets a
// retryable refusal; once verified the handler sees the batch stamped with
// the caller's pod UID and pinned to its node.
func TestInterceptorChain_AsWired(t *testing.T) {
	validator := validatorReturning(t, authenticatedAs(testPublisher, podBoundExtras("node-a")))
	authInterceptor, err := newAuthInterceptor(context.Background(),
		auth.Settings{Enabled: true, Audience: testAudience}, validator)
	require.NoError(t, err)

	ready := &readiness{}
	recorder := &recordingConnector{}

	grpcServer := grpc.NewServer(grpc.ChainUnaryInterceptor(authInterceptor, idempotencyInterceptor(), readinessInterceptor(ready)))
	pb.RegisterPlatformConnectorServer(grpcServer, &server.PlatformConnectorServer{Pipeline: pipeline.New(), Connector: recorder})

	lis := bufconn.Listen(1 << 20)

	go func() { _ = grpcServer.Serve(lis) }()

	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	client := pb.NewPlatformConnectorClient(conn)

	send := func(token, key string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if token != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		}

		if key != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, healthpub.IdempotencyKeyHeader, key)
		}

		_, err := client.HealthEventOccurredV1(ctx, batchNaming("node-a", ""))

		return err
	}

	require.Equal(t, codes.Unauthenticated, status.Code(send("", "batch-1")), "no token")
	require.Equal(t, codes.InvalidArgument, status.Code(send("tok", "")), "a missing key is refused before readiness")
	require.Equal(t, codes.Unavailable, status.Code(send("tok", "batch-1")), "the index is not verified yet")
	require.Zero(t, recorder.calls.Load(), "nothing reached the connector")

	ready.indexVerified.Store(true)

	require.NoError(t, send("tok", "batch-1"))
	require.EqualValues(t, 1, recorder.calls.Load())

	got := recorder.lastBatch()
	require.Len(t, got.Events, 2)
	require.Equal(t, "pod-uid-1#batch-1#0", got.Events[0].Metadata[datastore.HealthEventIdempotencyKeyMetadataField])
	require.Equal(t, "pod-uid-1#batch-1#1", got.Events[1].Metadata[datastore.HealthEventIdempotencyKeyMetadataField])
	require.Equal(t, "node-a", got.Events[1].NodeName, "a blank node name is pinned to the token's node")
}

// failingConnector is a set member whose every batch fails.
type failingConnector struct{}

func (failingConnector) ProcessBatch(context.Context, *pb.HealthEvents) error {
	return errors.New("node update failed")
}

// TestMember_BestEffortOnlyNextToAStore: with a store connector the other
// members are best effort, so their failure never fails a stored batch;
// without one a member's failure is the reply, so the caller retries.
func TestMember_BestEffortOnlyNextToAStore(t *testing.T) {
	cfg := &config{conditionUpdateTimeout: time.Second}
	batch := &pb.HealthEvents{}

	withStore := &components{cfg: cfg, store: &store.DatabaseStoreConnector{}}
	require.NoError(t, withStore.member("kubernetes", failingConnector{}).ProcessBatch(context.Background(), batch))

	withoutStore := &components{cfg: cfg}
	require.EqualError(t, withoutStore.member("kubernetes", failingConnector{}).ProcessBatch(context.Background(), batch),
		"node update failed")
}

// TestNewReadiness_NoStoreIsReadyAtStart: with a store connector a replica is
// unready and refuses writes until the index loop verifies the index; without
// one there is no index, so it is ready and accepts writes from the start.
func TestNewReadiness_NoStoreIsReadyAtStart(t *testing.T) {
	withStore := newReadiness(true)
	require.False(t, withStore.writesAllowed())
	require.ErrorContains(t, withStore.Ready(context.Background()), "not verified")

	withoutStore := newReadiness(false)
	require.True(t, withoutStore.writesAllowed())
	require.NoError(t, withoutStore.Ready(context.Background()))
}
