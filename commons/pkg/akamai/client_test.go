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

package akamai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "test-token"

// fastRetry is a retry policy with negligible sleeps for tests. Keeps the
// exponential-backoff code path exercised without actually waiting.
func fastRetry(maxAttempts int) Option {
	return WithRetryPolicy(maxAttempts, time.Microsecond, 2.0, 0.0)
}

// newTestClient returns a client pointed at srv with a token in the
// environment, as the real deployment provides it.
func newTestClient(t *testing.T, srv *httptest.Server, opts ...Option) *Client {
	t.Helper()
	t.Setenv(APITokenEnvVar, testToken)

	return NewClient(srv.URL, append([]Option{WithHTTPClient(srv.Client()), fastRetry(3)}, opts...)...)
}

// TestClientGetSuccess pins the request shape on which every Akamai call
// depends: bearer auth, a JSON Accept header, query params carried through, and
// no Content-Type on a body-less GET.
func TestClientGetSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// assert, not require: this runs on the server goroutine, and require's
		// FailNow is only valid on the test's own.
		assert.Equal(t, "Bearer "+testToken, r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		assert.Empty(t, r.Header.Get("Content-Type"))
		assert.Equal(t, "/things", r.URL.Path)
		assert.Equal(t, "2", r.URL.Query().Get("page"))

		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var out struct {
		OK bool `json:"ok"`
	}

	err := newTestClient(t, srv).Get(context.Background(), "/things", map[string][]string{"page": {"2"}}, &out)
	require.NoError(t, err)
	assert.True(t, out.OK)
}

// TestClientNoToken pins that a missing credential fails before any request
// leaves the process.
func TestClientNoToken(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	t.Setenv(APITokenEnvVar, "")

	err := NewClient(srv.URL, WithHTTPClient(srv.Client())).Get(context.Background(), "/things", nil, nil)
	require.ErrorContains(t, err, APITokenEnvVar)
	assert.Zero(t, calls.Load())
}

// TestClientTokenReadPerRequest pins that rotating the Secret takes effect
// without a restart.
func TestClientTokenReadPerRequest(t *testing.T) {
	var seen []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	require.NoError(t, c.Get(context.Background(), "/a", nil, nil))

	t.Setenv(APITokenEnvVar, "rotated")
	require.NoError(t, c.Get(context.Background(), "/a", nil, nil))

	assert.Equal(t, []string{"Bearer " + testToken, "Bearer rotated"}, seen)
}

// TestClientRetryScope pins which failures repeat: reads retry 5xx, writes do
// not, because a 5xx on a write may have already taken effect.
func TestClientRetryScope(t *testing.T) {
	tests := []struct {
		name      string
		call      func(c *Client) error
		status    int
		wantCalls int32
	}{
		{"get retries 500", func(c *Client) error {
			return c.Get(context.Background(), "/x", nil, nil)
		}, http.StatusInternalServerError, 3},
		{"get does not retry 404", func(c *Client) error {
			return c.Get(context.Background(), "/x", nil, nil)
		}, http.StatusNotFound, 1},
		{"post does not retry 500", func(c *Client) error {
			return c.Post(context.Background(), "/x", struct{}{}, nil)
		}, http.StatusInternalServerError, 1},
		{"post retries 429", func(c *Client) error {
			return c.Post(context.Background(), "/x", struct{}{}, nil)
		}, http.StatusTooManyRequests, 3},
		{"delete does not retry 500", func(c *Client) error {
			return c.Delete(context.Background(), "/x", nil)
		}, http.StatusInternalServerError, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"errors":[{"reason":"nope"}]}`))
			}))
			defer srv.Close()

			err := tt.call(newTestClient(t, srv))
			require.Error(t, err)
			assert.Equal(t, tt.wantCalls, calls.Load())
		})
	}
}

// TestClientRefusesInsecureRedirect pins that the token is never replayed to a
// plaintext listener after a redirect.
func TestClientRefusesInsecureRedirect(t *testing.T) {
	var plaintextHits atomic.Int32

	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		plaintextHits.Add(1)
	}))
	defer plain.Close()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer srv.Close()

	err := newTestClient(t, srv).Get(context.Background(), "/x", nil, nil)
	require.ErrorIs(t, err, errRedirect)
	assert.Zero(t, plaintextHits.Load())
}

// TestClientMalformedBody pins that a 200 with a body that is not JSON is an
// error, not an empty result.
func TestClientMalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	var out map[string]any

	err := newTestClient(t, srv).Get(context.Background(), "/x", nil, &out)
	require.ErrorContains(t, err, "unmarshal response")
}
