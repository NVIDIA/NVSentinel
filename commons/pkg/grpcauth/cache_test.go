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

package grpcauth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	authv1 "k8s.io/api/authentication/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// makeToken builds a JWT-shaped token whose exp claim is real and whose
// signature is garbage — fine here, because expiry parsing is only ever
// applied to tokens the (stubbed) TokenReview has accepted.
func makeToken(exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))

	return "eyJhbGciOiJSUzI1NiJ9." + payload + ".sig"
}

// fastRetries shrinks the retry schedule so a test can observe retry behavior
// without spending the production window. Retrying itself is not optional, so
// there is no way to ask for "no retries" — only for quicker ones.
func fastRetries(v *Validator) {
	v.retryWindow = 200 * time.Millisecond
	v.backoff = wait.Backoff{Duration: 2 * time.Millisecond, Factor: 2, Jitter: 0.1, Steps: 8}
}

// countingClient returns a fake clientset that authenticates every token as
// username and counts TokenReview calls through the returned counter.
func countingClient(username string, audiences []string) (*fake.Clientset, *int) {
	calls := 0
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			calls++

			return true, &authv1.TokenReview{
				Status: authv1.TokenReviewStatus{
					Authenticated: true,
					Audiences:     audiences,
					User:          authv1.UserInfo{Username: username},
				},
			}, nil
		})

	return client, &calls
}

func TestVerdictCache_HitSkipsTokenReview(t *testing.T) {
	client, calls := countingClient(cspSA, []string{wantAudience})
	v, err := NewValidator(client, wantAudience)
	require.NoError(t, err)

	token := makeToken(time.Now().Add(time.Hour))

	for range 5 {
		identity, err := v.Authenticate(context.Background(), token)
		require.NoError(t, err)
		assert.Equal(t, cspSA, identity.Username)
	}

	assert.Equal(t, 1, *calls, "four of the five calls must be served from cache")
}

func TestVerdictCache_DistinctTokensAreDistinctEntries(t *testing.T) {
	client, calls := countingClient(cspSA, []string{wantAudience})
	v, err := NewValidator(client, wantAudience)
	require.NoError(t, err)

	_, err = v.Authenticate(context.Background(), makeToken(time.Now().Add(time.Hour)))
	require.NoError(t, err)
	_, err = v.Authenticate(context.Background(), makeToken(time.Now().Add(2*time.Hour)))
	require.NoError(t, err)

	assert.Equal(t, 2, *calls, "a rotated token is a new credential, not a cache hit")
}

func TestVerdictCache_EntryExpiresAfterFixedTTL(t *testing.T) {
	// The TTL is deliberately not the token's own expiry. TokenReview is the
	// only check that notices the bound pod being deleted, so this window is
	// how long a deleted pod's token keeps working — bounded for every caller
	// rather than stretching to the token lifetime (an hour by default).
	client, calls := countingClient(cspSA, []string{wantAudience})
	v, err := NewValidator(client, wantAudience)
	require.NoError(t, err)

	base := time.Now()
	v.now = func() time.Time { return base }

	token := makeToken(base.Add(time.Hour)) // long-lived token...
	_, err = v.Authenticate(context.Background(), token)
	require.NoError(t, err)

	v.now = func() time.Time { return base.Add(cacheTTL - time.Second) }
	_, err = v.Authenticate(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, 1, *calls, "still inside the TTL: served from cache")

	// ...but the verdict is re-checked once the fixed TTL elapses, even though
	// the token itself is valid for another hour.
	v.now = func() time.Time { return base.Add(cacheTTL + time.Second) }
	_, err = v.Authenticate(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, 2, *calls, "past the TTL: re-reviewed despite a live token")
}

func TestVerdictCache_RejectionsAreNotCached(t *testing.T) {
	calls := 0
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			calls++

			return true, &authv1.TokenReview{
				Status: authv1.TokenReviewStatus{Authenticated: false, Error: "expired"},
			}, nil
		})

	v, err := NewValidator(client, wantAudience)
	require.NoError(t, err)

	token := makeToken(time.Now().Add(time.Hour))
	_, err = v.Authenticate(context.Background(), token)
	require.Error(t, err)
	_, err = v.Authenticate(context.Background(), token)
	require.Error(t, err)

	assert.Equal(t, 2, calls, "each attempt with a rejected token is re-reviewed")
}

func TestVerdictCache_EvictsLeastRecentlyUsedWhenFull(t *testing.T) {
	// The cache is bounded so that a caller replaying distinct tokens cannot
	// grow it without limit. Past the bound the least recently used entry is
	// dropped; dropping a verdict only costs a TokenReview, never correctness.
	c, err := newVerdictCache()
	require.NoError(t, err)

	now := time.Now()
	tokens := make([]string, cacheMaxEntries+1)

	for i := range tokens {
		tokens[i] = fmt.Sprintf("token-%d", i)
		c.put(tokens[i], &Identity{Username: cspSA}, now)
	}

	_, ok := c.get(tokens[0], now)
	assert.False(t, ok, "the least recently used entry is evicted once the cache is full")

	_, ok = c.get(tokens[len(tokens)-1], now)
	assert.True(t, ok, "the newest entry is retained")
}

func TestVerdictCache_CallersCannotCorruptCachedEntries(t *testing.T) {
	// A cached Identity is handed out repeatedly. Every field is a value type,
	// so the copy handed back is fully detached; if it were not, one caller
	// rewriting a field would silently change what every later caller sees.
	client, _ := countingClient(cspSA, []string{wantAudience})
	v, err := NewValidator(client, wantAudience)
	require.NoError(t, err)

	token := makeToken(time.Now().Add(time.Hour))

	first, err := v.Authenticate(context.Background(), token)
	require.NoError(t, err)

	first.Username = "tampered"
	first.NodeName = "tampered"

	second, err := v.Authenticate(context.Background(), token)
	require.NoError(t, err)

	assert.Equal(t, cspSA, second.Username, "cached identity must survive a caller mutating its copy")
	assert.Empty(t, second.NodeName)
}

func TestVerdictCache_ConcurrentAuthenticate(t *testing.T) {
	// One Validator serves every in-flight request on a node, so the cache is
	// read and written from many goroutines at once. Run under -race.
	client := fake.NewSimpleClientset()

	var mu sync.Mutex

	calls := 0

	client.PrependReactor("create", "tokenreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			mu.Lock()
			calls++
			mu.Unlock()

			tr := action.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
			tr.Status = authv1.TokenReviewStatus{
				Authenticated: true,
				Audiences:     []string{wantAudience},
				User: authv1.UserInfo{
					Username: cspSA,
					Extra: map[string]authv1.ExtraValue{
						"authentication.kubernetes.io/node-name": {"gpu-node-01"},
					},
				},
			}

			return true, tr, nil
		})

	v, err := NewValidator(client, wantAudience)
	require.NoError(t, err)

	// A handful of distinct tokens, each hit by many goroutines: exercises
	// concurrent hits, misses and evictions against a deliberately small cache.
	tokens := make([]string, 6)
	for i := range tokens {
		tokens[i] = makeToken(time.Now().Add(time.Duration(i+1) * time.Hour))
	}

	var wg sync.WaitGroup

	const goroutines = 48

	wg.Add(goroutines)

	for i := range goroutines {
		go func() {
			defer wg.Done()

			identity, err := v.Authenticate(context.Background(), tokens[i%len(tokens)])
			assert.NoError(t, err)

			if identity != nil {
				// Mutating a returned copy must never be visible to anyone else.
				assert.Equal(t, cspSA, identity.Username)
				assert.Equal(t, "gpu-node-01", identity.NodeName)
				identity.NodeName = "scribble"
			}
		}()
	}

	wg.Wait()

	// Every token must still authenticate cleanly with its original identity.
	for _, tok := range tokens {
		identity, err := v.Authenticate(context.Background(), tok)
		require.NoError(t, err)
		assert.Equal(t, "gpu-node-01", identity.NodeName)
	}
}

func TestValidator_ExpiredRetryWindowDoesNotPanic(t *testing.T) {
	// If the first TokenReview consumes the whole retry window, the backoff loop
	// starts with an already-expired context and never runs its callback. The
	// validator must still report a retryable failure: returning a nil result
	// with a nil error would be dereferenced by the caller, and this runs inside
	// a gRPC server with no panic recovery.
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			time.Sleep(120 * time.Millisecond) // outlives the retry window below

			return true, nil, context.DeadlineExceeded
		})

	v, err := NewValidator(client, wantAudience)
	require.NoError(t, err)

	fastRetries(v)

	// The caller's own context has no deadline, so only our window expires.
	require.NotPanics(t, func() {
		identity, err := v.Authenticate(context.Background(), "some-token")

		assert.Nil(t, identity)
		require.Error(t, err, "a total failure must never look like a successful review")
		assert.Equal(t, codes.Unavailable, status.Code(err),
			"our own window expiring is a retryable outage, not a rejected credential")
	})
}

func TestValidator_RetriesUnavailableWithinWindow(t *testing.T) {
	calls := 0
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			calls++
			if calls < 3 {
				return true, nil, k8serrors.NewInternalError(errors.New("etcd unreachable"))
			}

			return true, &authv1.TokenReview{
				Status: authv1.TokenReviewStatus{
					Authenticated: true,
					Audiences:     []string{wantAudience},
					User:          authv1.UserInfo{Username: cspSA},
				},
			}, nil
		})

	v, err := NewValidator(client, wantAudience)
	require.NoError(t, err)

	fastRetries(v)

	identity, err := v.Authenticate(context.Background(), "some-token")

	require.NoError(t, err, "two transient failures must be absorbed by in-call retries")
	assert.Equal(t, cspSA, identity.Username)
	assert.Equal(t, 3, calls)
}

func TestValidator_DoesNotRetryPermanentFaults(t *testing.T) {
	gr := schema.GroupResource{Group: "authentication.k8s.io", Resource: "tokenreviews"}
	calls := 0
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			calls++

			return true, nil, k8serrors.NewForbidden(gr, "", errors.New("no RBAC"))
		})

	v, err := NewValidator(client, wantAudience)
	require.NoError(t, err)

	fastRetries(v)

	_, err = v.Authenticate(context.Background(), "some-token")

	require.Error(t, err)
	assert.Equal(t, 1, calls, "an RBAC failure must not be retried")
}

func TestValidator_RetryIsUnconditional(t *testing.T) {
	// Retrying a retryable failure is not opt-in: a control-plane blip must not
	// depend on how a particular server happened to build its validator.
	calls := 0
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			calls++

			return true, nil, k8serrors.NewInternalError(errors.New("etcd unreachable"))
		})

	v, err := NewValidator(client, wantAudience)
	require.NoError(t, err)

	fastRetries(v)

	_, err = v.Authenticate(context.Background(), "some-token")

	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Greater(t, calls, 1, "a retryable failure must be retried without any opt-in")
}

func TestValidator_SurfacesPodAndNodeExtras(t *testing.T) {
	client := fakeClientWithStatus(authv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{wantAudience},
		User: authv1.UserInfo{
			Username: cspSA,
			UID:      "sa-uid",
			Extra: map[string]authv1.ExtraValue{
				"authentication.kubernetes.io/pod-name":  {"csp-health-monitor-abc"},
				"authentication.kubernetes.io/pod-uid":   {"pod-uid-1"},
				"authentication.kubernetes.io/node-name": {"gpu-node-01"},
				"authentication.kubernetes.io/node-uid":  {"node-uid-1"},
			},
		},
	})

	v, err := NewValidator(client, wantAudience)
	require.NoError(t, err)

	identity, err := v.Authenticate(context.Background(), "some-token")
	require.NoError(t, err)

	assert.Equal(t, "csp-health-monitor-abc", identity.PodName)
	assert.Equal(t, "pod-uid-1", identity.PodUID)
	assert.Equal(t, "gpu-node-01", identity.NodeName)
	assert.Equal(t, "node-uid-1", identity.NodeUID)
}

func TestValidator_ExtraEdgeCases(t *testing.T) {
	// Every shape an authenticator might return for the node claim must resolve
	// to either a usable value or a clean empty string — never a panic, never a
	// blank-but-present value that would be mistaken for a real node.
	tests := []struct {
		name     string
		extra    map[string]authv1.ExtraValue
		wantNode string
		wantPod  string
		wantErr  bool
	}{
		{name: "nil extra map", extra: nil, wantNode: "", wantPod: ""},
		{
			name:     "node present, pod absent (partial claims)",
			extra:    map[string]authv1.ExtraValue{"authentication.kubernetes.io/node-name": {"gpu-node-01"}},
			wantNode: "gpu-node-01",
			wantPod:  "",
		},
		{
			name: "node claim present but empty is treated as absent",
			extra: map[string]authv1.ExtraValue{
				"authentication.kubernetes.io/node-name": {""},
			},
			wantNode: "",
		},
		{
			// A node name is a DNS subdomain, so the API server cannot emit
			// this. Recording it verbatim means the interceptor compares it
			// against its own node, does not match, and fails closed — which is
			// the right answer for a claim we cannot make sense of.
			name: "whitespace-only node claim is recorded verbatim",
			extra: map[string]authv1.ExtraValue{
				"authentication.kubernetes.io/node-name": {"   "},
			},
			wantNode: "   ",
		},
		{
			// A present key with no value is not "absent" — it is an
			// authenticator saying something this validator cannot read. These
			// fields decide which node a caller may speak for, so an unreadable
			// answer is refused rather than resolved.
			name: "present key with no value is rejected as ambiguous",
			extra: map[string]authv1.ExtraValue{
				"authentication.kubernetes.io/node-name": {},
			},
			wantErr: true,
		},
		{
			name: "multiple values for one key is rejected as ambiguous",
			extra: map[string]authv1.ExtraValue{
				"authentication.kubernetes.io/node-name": {"gpu-node-01", "gpu-node-02"},
			},
			wantErr: true,
		},
		{
			// Likewise: the claim is what the authenticator said it is. Trimming
			// would mean this validator, not the API server, decides which node
			// a token attests to.
			name: "padded node claim is recorded verbatim",
			extra: map[string]authv1.ExtraValue{
				"authentication.kubernetes.io/node-name": {" gpu-node-01\n"},
			},
			wantNode: " gpu-node-01\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fakeClientWithStatus(authv1.TokenReviewStatus{
				Authenticated: true,
				Audiences:     []string{wantAudience},
				User:          authv1.UserInfo{Username: cspSA, Extra: tt.extra},
			})

			v, err := NewValidator(client, wantAudience)
			require.NoError(t, err)

			identity, err := v.Authenticate(context.Background(), "some-token")

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, codes.Unauthenticated, status.Code(err))

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantNode, identity.NodeName)
			assert.Equal(t, tt.wantPod, identity.PodName)
		})
	}
}

func TestValidator_ExtrasAbsentIsNotAnError(t *testing.T) {
	// Clusters that predate node info in tokens return no extras; the identity
	// simply has no node claim and downstream falls back accordingly.
	client := fakeClientWithStatus(authv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{wantAudience},
		User:          authv1.UserInfo{Username: cspSA},
	})

	v, err := NewValidator(client, wantAudience)
	require.NoError(t, err)

	identity, err := v.Authenticate(context.Background(), "some-token")
	require.NoError(t, err)

	assert.Empty(t, identity.NodeName)
	assert.Empty(t, identity.PodName)
}
