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

package lambda

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testIdentityLRN = "lrn:iam:identity:3cd2d107c6a347eeb0ef9498820d637d"

// writeTokenFile drops a ServiceAccount token in a temp dir and returns its
// path, standing in for the volume the webhook projects.
func writeTokenFile(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	return path
}

// exchangeHandler serves the token-exchange endpoint, recording how many times
// it was called and asserting the request shape the API requires.
func exchangeHandler(t *testing.T, calls *atomic.Int64, accessToken string, expiresIn int) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		assert.Equal(t, "/api/v1/oidc/token", r.URL.Path)
		// The exchange is unauthenticated: the JWT in the body is the
		// credential, and sending a half-built header would be a bug.
		assert.Empty(t, r.Header.Get("Authorization"))

		var req exchangeRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "sa-jwt", req.Token)
		assert.Equal(t, testIdentityLRN, req.IdentityLRN)

		var resp exchangeResponse
		resp.Data.AccessToken = accessToken
		resp.Data.TokenType = "Bearer"
		resp.Data.ExpiresIn = expiresIn

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}
}

// newTestWorkloadIdentity wires a credential at srvURL against tokenFile,
// bypassing the webhook env so tests stay independent of process state.
func newTestWorkloadIdentity(srvURL, tokenFile string, httpClient *http.Client) *workloadIdentity {
	return &workloadIdentity{
		exchangeClient: &Client{
			endpoint: srvURL,
			http:     httpClient,
			retry:    retryPolicy{maxAttempts: 1, initialBackoff: time.Microsecond, factor: 2, jitter: 0},
			creds:    noCredential{},
		},
		identityLRN: testIdentityLRN,
		tokenFile:   tokenFile,
		clock:       time.Now,
	}
}

func TestWorkloadIdentity_TokenLifecycle_ExchangesCachesAndRemintsAfterInvalidation(t *testing.T) {
	var calls atomic.Int64

	srv := httptest.NewServer(exchangeHandler(t, &calls, "minted-key", 3600))
	defer srv.Close()

	w := newTestWorkloadIdentity(srv.URL, writeTokenFile(t, "sa-jwt\n"), srv.Client())

	first, err := w.token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "minted-key", first)

	// A key nowhere near expiry is reused rather than re-minted.
	second, err := w.token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "minted-key", second)
	assert.Equal(t, int64(1), calls.Load(), "second call must be served from cache")

	// Invalidate is what a 401 triggers; the next call must mint again.
	w.invalidate()

	third, err := w.token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "minted-key", third)
	assert.Equal(t, int64(2), calls.Load(), "invalidate must force a fresh exchange")
}

// TestWorkloadIdentity_TokenNearExpiry_RefreshesBeforeExpiry pins the point of the refresh
// window: a token still technically valid but inside the window is replaced, so
// no request rides one that lapses mid-flight.
func TestWorkloadIdentity_TokenNearExpiry_RefreshesBeforeExpiry(t *testing.T) {
	var calls atomic.Int64

	srv := httptest.NewServer(exchangeHandler(t, &calls, "minted-key", 3600))
	defer srv.Close()

	w := newTestWorkloadIdentity(srv.URL, writeTokenFile(t, "sa-jwt"), srv.Client())

	_, err := w.token(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), calls.Load())

	// Jump to one minute before expiry, inside the five minute window.
	expiry := w.cur.expiresAt
	w.clock = func() time.Time { return expiry.Add(-time.Minute) }

	_, err = w.token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(2), calls.Load(), "a token inside the refresh window must be replaced")
}

// TestWorkloadIdentity_RefreshFailure_UsesValidCacheAndRejectsExpiredToken covers the case where
// the refresh fails but the cached key has not actually expired: the API call
// should still go out rather than fail on a credential that is still good.
func TestWorkloadIdentity_RefreshFailure_UsesValidCacheAndRejectsExpiredToken(t *testing.T) {
	var calls atomic.Int64

	srv := httptest.NewServer(func() http.HandlerFunc {
		good := exchangeHandler(t, &calls, "minted-key", 3600)

		return func(w http.ResponseWriter, r *http.Request) {
			if calls.Load() == 0 {
				good(w, r)
				return
			}

			calls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}())
	defer srv.Close()

	w := newTestWorkloadIdentity(srv.URL, writeTokenFile(t, "sa-jwt"), srv.Client())

	_, err := w.token(context.Background())
	require.NoError(t, err)

	expiry := w.cur.expiresAt
	w.clock = func() time.Time { return expiry.Add(-time.Minute) }

	token, err := w.token(context.Background())
	require.NoError(t, err, "a still-valid key must outlive a failed refresh")
	assert.Equal(t, "minted-key", token)

	// Past the hard deadline the error surfaces instead.
	w.clock = func() time.Time { return expiry.Add(time.Minute) }

	_, err = w.token(context.Background())
	assert.Error(t, err, "an expired key must not be served")
}

func TestWorkloadIdentity_MissingOrEmptyTokenFile_ReturnsError(t *testing.T) {
	var calls atomic.Int64

	srv := httptest.NewServer(exchangeHandler(t, &calls, "minted-key", 3600))
	defer srv.Close()

	tests := []struct {
		name        string
		tokenFile   func(t *testing.T) string
		wantErrPart string
	}{
		{
			name:        "missing file names the path",
			tokenFile:   func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
			wantErrPart: "read workload identity token",
		},
		{
			name:        "empty file is refused",
			tokenFile:   func(t *testing.T) string { return writeTokenFile(t, "  \n") },
			wantErrPart: "is empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkloadIdentity(srv.URL, tc.tokenFile(t), srv.Client())

			_, err := w.token(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrPart)
		})
	}
}

// TestWorkloadIdentity_ConcurrentColdCacheRequests_MintsTokenOnce covers the single-flight guard:
// a burst of callers on a cold cache must produce one exchange, not one each.
func TestWorkloadIdentity_ConcurrentColdCacheRequests_MintsTokenOnce(t *testing.T) {
	var calls atomic.Int64

	srv := httptest.NewServer(exchangeHandler(t, &calls, "minted-key", 3600))
	defer srv.Close()

	w := newTestWorkloadIdentity(srv.URL, writeTokenFile(t, "sa-jwt"), srv.Client())

	var wg sync.WaitGroup

	for range 16 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			token, err := w.token(context.Background())
			assert.NoError(t, err)
			assert.Equal(t, "minted-key", token)
		}()
	}

	wg.Wait()
	assert.Equal(t, int64(1), calls.Load(), "concurrent callers must share one exchange")
}

// TestClient_WorkloadIdentityAndStaticKeyConfigured_UsesExchangedBearerToken drives a real Client through the whole
// path: mint on the first call, then send the minted key as the bearer token.
func TestClient_WorkloadIdentityAndStaticKeyConfigured_UsesExchangedBearerToken(t *testing.T) {
	var (
		exchanges atomic.Int64
		gotAuth   = make(chan string, 1)
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/oidc/token", exchangeHandler(t, &exchanges, "minted-key", 3600))
	mux.HandleFunc("/api/v1/things", func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv(IdentityLRNEnvVar, testIdentityLRN)
	t.Setenv(TokenFileEnvVar, writeTokenFile(t, "sa-jwt"))
	// A static key must be ignored entirely when a workload identity is
	// injected, otherwise the pod silently keeps using the secret it was
	// supposed to stop needing.
	t.Setenv(APIKeyEnvVar, "static-key-that-must-not-be-used")

	require.Equal(t, AuthWorkloadIdentity, DetectAuthMode())

	c := NewClient(srv.URL, WithHTTPClient(srv.Client()))

	require.NoError(t, c.Get(context.Background(), "/api/v1/things", nil, nil))
	assert.Equal(t, "Bearer minted-key", <-gotAuth)
	assert.Equal(t, int64(1), exchanges.Load())
}

// TestExpiryOf_ExpiryMetadataVariants_UsesAbsoluteExpiryOrFallback pins parsing against what the API actually sends. The server
// renders expires_at with Python's datetime.isoformat(), which emits a numeric
// "+00:00" offset rather than "Z", and includes microseconds only when the
// value has them. A form we cannot parse must fall back to expires_in rather
// than silently yielding a zero time, which would re-mint on every request.
func TestExpiryOf_ExpiryMetadataVariants_UsesAbsoluteExpiryOrFallback(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		expiresAt string
		expiresIn int
		want      time.Time
	}{
		{
			name:      "isoformat with microseconds and offset",
			expiresAt: "2026-08-14T11:09:12.479653+00:00",
			want:      time.Date(2026, 8, 14, 11, 9, 12, 479653000, time.UTC),
		},
		{
			name:      "isoformat with offset, no microseconds",
			expiresAt: "2026-08-14T11:09:12+00:00",
			want:      time.Date(2026, 8, 14, 11, 9, 12, 0, time.UTC),
		},
		{
			name:      "RFC3339 with Z",
			expiresAt: "2026-08-14T11:09:12Z",
			want:      time.Date(2026, 8, 14, 11, 9, 12, 0, time.UTC),
		},
		{
			name:      "a naive timestamp falls back to expires_in",
			expiresAt: "2026-08-14T11:09:12.479653",
			expiresIn: 3600,
			want:      now.Add(time.Hour),
		},
		{
			name:      "expires_in alone",
			expiresIn: 900,
			want:      now.Add(15 * time.Minute),
		},
		{
			name: "neither is treated as already expired",
			want: now,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var r exchangeResponse
			r.Data.ExpiresAt = tc.expiresAt
			r.Data.ExpiresIn = tc.expiresIn

			assert.True(t, expiryOf(r, now).Equal(tc.want),
				"expiresAt=%q expiresIn=%d: got %s, want %s",
				tc.expiresAt, tc.expiresIn, expiryOf(r, now), tc.want)
		})
	}
}

// TestRefreshAt_VariedTokenLifetimes_SchedulesRefreshWithinExpectedBounds pins the margin refreshAt leaves before expiry, since
// randFrac draws from crypto/rand and can't be substituted: each case checks
// invariants across repeated draws instead of a single deterministic value.
func TestRefreshAt_VariedTokenLifetimes_SchedulesRefreshWithinExpectedBounds(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	maxMargin := refreshWindow + time.Duration(jitterFrac*float64(refreshWindow))

	tests := []struct {
		name  string
		life  time.Duration
		check func(t *testing.T, expiresAt, at time.Time)
	}{
		{
			name: "life longer than the window: jitter only pulls refresh earlier",
			life: time.Hour,
			check: func(t *testing.T, expiresAt, at time.Time) {
				t.Helper()

				margin := expiresAt.Sub(at)
				assert.GreaterOrEqual(t, margin, refreshWindow, "margin=%s fell short of refreshWindow", margin)
				assert.LessOrEqual(t, margin, maxMargin, "margin=%s exceeded refreshWindow+jitter", margin)
			},
		},
		{
			name: "life exactly at the window: aims for its half-life",
			life: refreshWindow,
			check: func(t *testing.T, expiresAt, at time.Time) {
				t.Helper()

				assert.True(t, at.After(now), "refresh time must not be in the past")
				assert.True(t, at.Before(expiresAt), "refresh time must be before expiry")
			},
		},
		{
			name: "life shorter than the window: aims for its half-life",
			life: 2 * time.Minute,
			check: func(t *testing.T, expiresAt, at time.Time) {
				t.Helper()

				assert.True(t, at.After(now), "refresh time must not be in the past")
				assert.True(t, at.Before(expiresAt), "refresh time must be before expiry")
			},
		},
		{
			name: "already expired: immediately due for refresh",
			life: -time.Minute,
			check: func(t *testing.T, _, at time.Time) {
				t.Helper()

				assert.False(t, now.Before(at), "refresh must be immediately due")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expiresAt := now.Add(tc.life)

			for i := 0; i < 100; i++ {
				tc.check(t, expiresAt, refreshAt(expiresAt, now))
			}
		})
	}
}

func TestDetectAuthMode_CredentialEnvironmentCombinations_SelectsExpectedAuthMode(t *testing.T) {
	tests := []struct {
		name        string
		identityLRN string
		apiKey      string
		want        AuthMode
	}{
		{name: "webhook injection wins", identityLRN: testIdentityLRN, apiKey: "key", want: AuthWorkloadIdentity},
		{name: "identity alone", identityLRN: testIdentityLRN, want: AuthWorkloadIdentity},
		{name: "static key alone", apiKey: "key", want: AuthAPIKey},
		{name: "neither configured", want: AuthNone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(IdentityLRNEnvVar, tc.identityLRN)
			t.Setenv(APIKeyEnvVar, tc.apiKey)

			assert.Equal(t, tc.want, DetectAuthMode())
		})
	}
}
