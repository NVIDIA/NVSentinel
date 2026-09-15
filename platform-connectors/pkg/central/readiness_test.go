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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
