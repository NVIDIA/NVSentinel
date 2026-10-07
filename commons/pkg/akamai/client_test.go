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
	"errors"
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

// TestClient_Get_ReturnsDecodedBodyWithAuthHeaders pins the request shape on which every Akamai call
// depends: bearer auth, a JSON Accept header, query params carried through, and
// no Content-Type on a body-less GET.
func TestClient_Get_ReturnsDecodedBodyWithAuthHeaders(t *testing.T) {
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

// TestClient_Get_NoToken_FailsBeforeSendingRequest pins that a missing credential fails before any request
// leaves the process.
func TestClient_Get_NoToken_FailsBeforeSendingRequest(t *testing.T) {
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

// TestClient_Get_TokenRotated_UsesNewTokenOnNextRequest pins that rotating the Secret takes effect
// without a restart.
func TestClient_Get_TokenRotated_UsesNewTokenOnNextRequest(t *testing.T) {
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

// TestClient_Request_ServerFailure_RetriesOnlyWhereSafe pins which failures repeat: reads retry 5xx, writes do
// not, because a 5xx on a write may have already taken effect.
func TestClient_Request_ServerFailure_RetriesOnlyWhereSafe(t *testing.T) {
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

// TestClient_Get_InsecureRedirect_ReturnsErrorWithoutReplayingToken pins that the token is never replayed to a
// plaintext listener after a redirect.
func TestClient_Get_InsecureRedirect_ReturnsErrorWithoutReplayingToken(t *testing.T) {
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

// TestClient_Get_MalformedBody_ReturnsUnmarshalError pins that a 200 with a body that is not JSON is an
// error, not an empty result.
func TestClient_Get_MalformedBody_ReturnsUnmarshalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	var out map[string]any

	err := newTestClient(t, srv).Get(context.Background(), "/x", nil, &out)
	require.ErrorContains(t, err, "unmarshal response")
}

// serveSequence answers each request with the next status in statuses and the
// last one for every request after that.
func serveSequence(t *testing.T, body string, statuses ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := int(calls.Add(1)) - 1
		if n >= len(statuses) {
			n = len(statuses) - 1
		}

		w.WriteHeader(statuses[n])
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv, &calls
}

// TestClient_Get_TransientFailureThenPermanent_ReportsPermanentError pins that the error returned is the
// one of the last attempt. A retried 503 must not hide the 4xx that ended the call.
func TestClient_Get_TransientFailureThenPermanent_ReportsPermanentError(t *testing.T) {
	tests := []struct {
		name     string
		statuses []int
		want     int
	}{
		{"503 then 404", []int{http.StatusServiceUnavailable, http.StatusNotFound}, http.StatusNotFound},
		{"503 then 401", []int{http.StatusServiceUnavailable, http.StatusUnauthorized}, http.StatusUnauthorized},
		{"429 then 400", []int{http.StatusTooManyRequests, http.StatusBadRequest}, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, calls := serveSequence(t, `{"errors":[{"reason":"final reason"}]}`, tt.statuses...)

			err := newTestClient(t, srv).Get(context.Background(), "/x", nil, nil)

			require.Error(t, err)
			assert.Equal(t, int32(2), calls.Load())
			assert.ErrorContains(t, err, "after 2 attempts")
			assert.ErrorContains(t, err, "final reason")

			var statusErr *StatusError
			require.ErrorAs(t, err, &statusErr)
			assert.Equal(t, tt.want, statusErr.StatusCode, "the last response, not the retried one, is reported")
		})
	}
}

// TestClient_Get_PermanentFailureOnFirstAttempt_ReturnsStatusErrorWithoutAttemptCount pins that a call that
// is refused at once returns the typed error unwrapped by an attempt count.
func TestClient_Get_PermanentFailureOnFirstAttempt_ReturnsStatusErrorWithoutAttemptCount(t *testing.T) {
	srv, calls := serveSequence(t, `{"errors":[{"reason":"nope"}]}`, http.StatusUnauthorized)

	err := newTestClient(t, srv).Get(context.Background(), "/x", nil, nil)

	require.Error(t, err)
	assert.Equal(t, int32(1), calls.Load())
	assert.NotContains(t, err.Error(), "after")

	var statusErr *StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusUnauthorized, statusErr.StatusCode)
	assert.Equal(t, http.MethodGet, statusErr.Method)
}

// TestClient_Get_TransientFailureThenMalformedBody_ReportsDecodeError pins that a decode failure after a
// retry is not replaced by the retried status error.
func TestClient_Get_TransientFailureThenMalformedBody_ReportsDecodeError(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	var out struct{}

	err := newTestClient(t, srv).Get(context.Background(), "/x", nil, &out)

	require.Error(t, err)
	assert.Equal(t, int32(2), calls.Load())

	var statusErr *StatusError
	assert.False(t, errors.As(err, &statusErr), "the retried 503 must not be the reported error")
}

// TestNewClient_InjectedHTTPClient_LeavesCallersRedirectPolicyAlone pins that the scheme check is added to a
// copy, so the client the caller passed in is not changed.
func TestNewClient_InjectedHTTPClient_LeavesCallersRedirectPolicyAlone(t *testing.T) {
	injected := &http.Client{Timeout: time.Second}

	c := NewClient("https://api.example.test/v4", WithHTTPClient(injected))

	assert.Nil(t, injected.CheckRedirect, "the caller's client must not be modified")
	assert.NotSame(t, injected, c.http)
	assert.NotNil(t, c.http.CheckRedirect, "the copy carries the scheme check")
	assert.Equal(t, time.Second, c.http.Timeout, "other settings of the injected client are kept")
}
