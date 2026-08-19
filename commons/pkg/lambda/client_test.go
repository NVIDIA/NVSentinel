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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fastRetry is a retry policy with negligible sleeps for tests. Keeps the
// exponential-backoff code path exercised without actually waiting.
func fastRetry(maxAttempts int) Option {
	return WithRetryPolicy(maxAttempts, time.Microsecond, 2.0, 0.0)
}

// TestClientGetSuccess pins the request shape every Lambda call depends on:
// bearer auth, a JSON Accept header, query params carried through, and no
// Content-Type on a body-less GET.
func TestClientGetSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// assert, not require: this runs on the server goroutine, and require's
		// FailNow is only valid on the test's own.
		if !assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization")) {
			return
		}

		if !assert.Equal(t, "application/json", r.Header.Get("Accept")) {
			return
		}

		if !assert.Empty(t, r.Header.Get("Content-Type")) {
			return
		}

		if !assert.Equal(t, "abc", r.URL.Query().Get("page_token")) {
			return
		}

		fmt.Fprint(w, `{"data":{"id":"i-1"}}`)
	}))
	defer srv.Close()

	t.Setenv(APIKeyEnvVar, "test-key")

	c := NewClient(srv.URL, WithHTTPClient(srv.Client()))
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	q := url.Values{}
	q.Set("page_token", "abc")
	require.NoError(t, c.Get(context.Background(), "/api/v1/things", q, &out))
	assert.Equal(t, "i-1", out.Data.ID)
}

// TestClientGetMissingAPIKey checks an unset key fails before any request is
// built, and that the error names the env var an operator has to set.
func TestClientGetMissingAPIKey(t *testing.T) {
	c := NewClient("http://example.invalid")
	t.Setenv(APIKeyEnvVar, "")
	err := c.Get(context.Background(), "/api/v1/things", nil, nil)
	assert.ErrorContains(t, err, "LAMBDA_API_KEY")
}

// TestClientGetPermanent4xxNoRetry checks a client error short-circuits the
// backoff loop. Retrying a 401 burns the whole retry budget on a credential
// problem that cannot fix itself.
func TestClientGetPermanent4xxNoRetry(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()

	t.Setenv(APIKeyEnvVar, "bad-key")

	c := NewClient(srv.URL, WithHTTPClient(srv.Client()), fastRetry(4))
	err := c.Get(context.Background(), "/api/v1/things", nil, nil)
	assert.ErrorContains(t, err, "401")
	assert.EqualValues(t, 1, calls.Load(), "4xx should short-circuit the retry loop")
}

// TestClientGetRetriesOn5xxThenSucceeds covers the idempotent-GET path: a
// server error is transient, so the call recovers instead of failing a poll.
func TestClientGetRetriesOn5xxThenSucceeds(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"boom"}`)

			return
		}
		fmt.Fprint(w, `{"data":{"id":"i-ok"}}`)
	}))
	defer srv.Close()

	t.Setenv(APIKeyEnvVar, "test-key")

	c := NewClient(srv.URL, WithHTTPClient(srv.Client()), fastRetry(5))
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, c.Get(context.Background(), "/api/v1/things", nil, &out))
	assert.Equal(t, "i-ok", out.Data.ID)
	assert.EqualValues(t, 3, calls.Load())
}

// TestClientGetRetriesOn429ThenGivesUp checks the retry budget is bounded and
// that the surfaced error reports the attempts actually made, not the
// configured maximum.
func TestClientGetRetriesOn429ThenGivesUp(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"slow down"}`)
	}))
	defer srv.Close()

	t.Setenv(APIKeyEnvVar, "test-key")

	c := NewClient(srv.URL, WithHTTPClient(srv.Client()), fastRetry(3))
	err := c.Get(context.Background(), "/api/v1/things", nil, nil)
	assert.ErrorContains(t, err, "429")
	assert.ErrorContains(t, err, "after 3 attempts")
	assert.EqualValues(t, 3, calls.Load())
}

// TestClientGetContextCancelStopsRetries checks a cancelled context stops the
// backoff loop, so a shutting-down process does not keep hitting the API.
func TestClientGetContextCancelStopsRetries(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	t.Setenv(APIKeyEnvVar, "test-key")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := NewClient(srv.URL, WithHTTPClient(srv.Client()), fastRetry(5))
	err := c.Get(ctx, "/api/v1/things", nil, nil)
	require.Error(t, err)
	assert.LessOrEqual(t, calls.Load(), int32(1), "cancelled ctx should not hit the server multiple times")
}

// TestClient_Post_Success_SendsJSONBodyAndDecodesResponse pins the POST request
// shape: a marshalled JSON body plus the Content-Type header a GET must not
// carry.
func TestClient_Post_Success_SendsJSONBodyAndDecodesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.JSONEq(t, `{"ids":["i-1"]}`, string(body))

		fmt.Fprint(w, `{"data":{"id":"i-1"}}`)
	}))
	defer srv.Close()

	t.Setenv(APIKeyEnvVar, "test-key")

	c := NewClient(srv.URL, WithHTTPClient(srv.Client()))
	in := struct {
		IDs []string `json:"ids"`
	}{IDs: []string{"i-1"}}

	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	require.NoError(t, c.Post(context.Background(), "/api/v1/things", in, &out))
	assert.Equal(t, "i-1", out.Data.ID)
}

// TestClient_Post_UnmarshalableBody_ReturnsErrorWithoutRequesting checks a body
// that cannot be marshalled fails before anything reaches the network. The
// endpoint is deliberately unroutable so a regression shows up as a test error
// rather than a hang.
func TestClient_Post_UnmarshalableBody_ReturnsErrorWithoutRequesting(t *testing.T) {
	t.Setenv(APIKeyEnvVar, "test-key")

	c := NewClient("http://example.invalid")
	err := c.Post(context.Background(), "/api/v1/things", make(chan int), nil)
	assert.ErrorContains(t, err, "marshal request body")
}

// net/http copies Authorization to the same host or a subdomain without looking
// at the scheme, so an https endpoint redirecting to http would hand the API key
// to a plaintext listener. Both servers here are on 127.0.0.1, which is what
// makes the stdlib treat them as the same host.
func TestClient_HTTPSToHTTPRedirect_RefusedWithoutSendingAPIKey(t *testing.T) {
	var gotAuth atomic.Value

	gotAuth.Store("")

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		fmt.Fprint(w, `{}`)
	}))
	defer plain.Close()

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer secure.Close()

	t.Setenv(APIKeyEnvVar, "test-key")

	c := NewClient(secure.URL, WithHTTPClient(secure.Client()), fastRetry(1))
	err := c.Get(context.Background(), "/api/v1/things", nil, nil)

	assert.ErrorContains(t, err, "redirect refused")
	assert.Empty(t, gotAuth.Load(), "the API key must never reach the plaintext server")
}

// TestClient_HTTPSRedirectLoop_StopsAtLimitWithoutRetrying guards the cap that
// setting CheckRedirect silently drops. Left unhandled an https to https loop
// runs until the client timeout, and the timeout then looks transient enough to
// retry, replaying the whole chain.
func TestClient_HTTPSRedirectLoop_StopsAtLimitWithoutRetrying(t *testing.T) {
	var calls atomic.Int32

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, r.URL.String(), http.StatusFound)
	}))
	defer secure.Close()

	t.Setenv(APIKeyEnvVar, "test-key")

	client := secure.Client()
	client.Timeout = 10 * time.Second // bounds the damage if the cap regresses

	c := NewClient(secure.URL, WithHTTPClient(client), fastRetry(4))
	err := c.Get(context.Background(), "/api/v1/things", nil, nil)

	assert.ErrorContains(t, err, "stopped after 10 redirects")
	assert.LessOrEqual(t, calls.Load(), int32(maxRedirects),
		"one capped chain: neither an uncapped loop nor a retried one")
}

// TestClient_InjectedCheckRedirect_RunsAfterTheSchemeCheck covers the wrapping:
// a caller's own policy still applies, but only for redirects that survive the
// https check.
func TestClient_InjectedCheckRedirect_RunsAfterTheSchemeCheck(t *testing.T) {
	var injectedCalls atomic.Int32

	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{"id":"i-1"}}`)
	}))
	defer target.Close()

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer secure.Close()

	t.Setenv(APIKeyEnvVar, "test-key")

	client := secure.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		injectedCalls.Add(1)

		return fmt.Errorf("injected policy says no")
	}

	c := NewClient(secure.URL, WithHTTPClient(client), fastRetry(1))
	err := c.Get(context.Background(), "/api/v1/things", nil, nil)

	assert.ErrorContains(t, err, "injected policy says no")
	assert.EqualValues(t, 1, injectedCalls.Load(), "the injected policy must still be consulted")
}

// TestClient_Post_Permanent4xx_ErrorNamesTheMethod checks the error carries the
// verb and status, so a failed remediation can be diagnosed from the message
// alone.
func TestClient_Post_Permanent4xx_ErrorNamesTheMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"global/not-found"}}`)
	}))
	defer srv.Close()

	t.Setenv(APIKeyEnvVar, "test-key")

	c := NewClient(srv.URL, WithHTTPClient(srv.Client()), fastRetry(3))
	err := c.Post(context.Background(), "/api/v1/things", struct{}{}, nil)
	assert.ErrorContains(t, err, "POST")
	assert.ErrorContains(t, err, "404")
}

// TestValidWorkspaceID pins the two forms the API accepts, dashed and undashed,
// against the near-misses a hand-copied ID turns into.
func TestValidWorkspaceID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "undashed", id: "c4d291f47f9d436fa39f58493ce3b50d", want: true},
		{name: "dashed", id: "c4d291f4-7f9d-436f-a39f-58493ce3b50d", want: true},
		{name: "uppercase", id: "C4D291F47F9D436FA39F58493CE3B50D", want: true},
		{name: "empty", id: ""},
		{name: "a name, not an ID", id: "my-workspace"},
		{name: "one digit short", id: "c4d291f47f9d436fa39f58493ce3b50"},
		{name: "one digit long", id: "c4d291f47f9d436fa39f58493ce3b50da"},
		{name: "non-hex digit", id: "g4d291f47f9d436fa39f58493ce3b50d"},
		// Each dash is optional independently, matching the API, which strips
		// every dash before checking the length.
		{name: "some dashes missing", id: "c4d291f4-7f9d436f-a39f-58493ce3b50d", want: true},
		{name: "surrounding whitespace", id: " c4d291f47f9d436fa39f58493ce3b50d "},
		{name: "an LRN, not an ID", id: "lrn:iam:workspace:c4d291f47f9d436fa39f58493ce3b50d"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, ValidWorkspaceID(tc.id))
		})
	}
}
