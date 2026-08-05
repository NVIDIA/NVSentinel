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

func TestClientGetSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Accept"))
		require.Equal(t, "abc", r.URL.Query().Get("page_token"))
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

func TestClientGetMissingAPIKey(t *testing.T) {
	c := NewClient("http://example.invalid")
	t.Setenv(APIKeyEnvVar, "")
	err := c.Get(context.Background(), "/api/v1/things", nil, nil)
	assert.ErrorContains(t, err, "LAMBDA_API_KEY")
}

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
