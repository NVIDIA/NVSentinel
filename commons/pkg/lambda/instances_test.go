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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClient_GetInstance_ExistingInstance_ReturnsInstance pins the path and
// verb used to read one instance, and that the response envelope is unwrapped.
func TestClient_GetInstance_ExistingInstance_ReturnsInstance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, instancesPath+"/i-123", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(instanceResponse{
			Data: Instance{ID: "i-123", Status: InstanceStatusActive},
		}))
	}))
	defer srv.Close()

	t.Setenv(APIKeyEnvVar, "test-key")

	inst, err := NewClient(srv.URL, WithHTTPClient(srv.Client())).GetInstance(context.Background(), "i-123")
	require.NoError(t, err)
	assert.Equal(t, "i-123", inst.ID)
	assert.Equal(t, InstanceStatusActive, inst.Status)
}

// TestClient_GetInstance_EmptyID_ReturnsError checks an empty ID is rejected
// locally rather than becoming a request for the whole instance collection.
func TestClient_GetInstance_EmptyID_ReturnsError(t *testing.T) {
	_, err := NewClient("http://example.invalid").GetInstance(context.Background(), "")
	assert.ErrorContains(t, err, "instance id is empty")
}

// TestClient_GetInstance_NotFound_ReturnsErrorWithoutRetrying checks a 404 is
// terminal and that the API's error code survives into the returned error.
func TestClient_GetInstance_NotFound_ReturnsErrorWithoutRetrying(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"global/object-does-not-exist","message":"not found"}}`)
	}))
	defer srv.Close()

	t.Setenv(APIKeyEnvVar, "test-key")

	_, err := NewClient(srv.URL, WithHTTPClient(srv.Client()), fastRetry(4)).
		GetInstance(context.Background(), "i-gone")
	assert.ErrorContains(t, err, "404")
	assert.ErrorContains(t, err, "global/object-does-not-exist")
	assert.EqualValues(t, 1, calls.Load(), "4xx should short-circuit the retry loop")
}

// instanceOperationCase drives the two instance-operations endpoints, which
// differ only in path and response envelope key.
type instanceOperationCase struct {
	name     string
	wantPath string
	respBody string
	call     func(context.Context, *Client) error
}

// instanceOperationCases returns both instance-operation endpoints so every
// test below runs against power-cycle and terminate alike.
func instanceOperationCases() []instanceOperationCase {
	return []instanceOperationCase{
		{
			name:     "power cycle",
			wantPath: powerCycleInstancePath,
			respBody: `{"data":{"power_cycled_instances":[{"id":"i-123","status":"active"}]}}`,
			call: func(ctx context.Context, c *Client) error {
				return c.PowerCycleInstance(ctx, "i-123")
			},
		},
		{
			name:     "terminate",
			wantPath: terminateInstancePath,
			respBody: `{"data":{"terminated_instances":[{"id":"i-123","status":"terminating"}]}}`,
			call: func(ctx context.Context, c *Client) error {
				return c.TerminateInstance(ctx, "i-123")
			},
		},
	}
}

// TestClient_InstanceOperations_Success_PostsInstanceID pins the wire contract
// of both operations: the right path, and the single instance ID wrapped in the
// batch body the API expects.
func TestClient_InstanceOperations_Success_PostsInstanceID(t *testing.T) {
	for _, tc := range instanceOperationCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, tc.wantPath, r.URL.Path)
				assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				assert.JSONEq(t, `{"instance_ids":["i-123"]}`, string(body))

				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.respBody)
			}))
			defer srv.Close()

			t.Setenv(APIKeyEnvVar, "test-key")

			require.NoError(t, tc.call(context.Background(), NewClient(srv.URL, WithHTTPClient(srv.Client()))))
		})
	}
}

// The API re-reads the IDs it was given, so a response missing ours means the
// operation did not apply to our instance.
func TestClient_InstanceOperations_UnacknowledgedInstance_ReturnsError(t *testing.T) {
	for _, tc := range instanceOperationCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, strings.Replace(tc.respBody, `"id":"i-123"`, `"id":"i-999"`, 1))
			}))
			defer srv.Close()

			t.Setenv(APIKeyEnvVar, "test-key")

			err := tc.call(context.Background(), NewClient(srv.URL, WithHTTPClient(srv.Client())))
			assert.ErrorContains(t, err, "not acknowledged")
			assert.ErrorContains(t, err, "i-123")
		})
	}
}

// An empty ID is caught locally, matching GetInstance. Left through, it posts
// {"instance_ids":[""]} and burns a round-trip to learn nothing matched. The
// table above bakes in a valid ID, so these take the methods directly.
func TestClient_InstanceOperations_EmptyID_ReturnsErrorWithoutRequesting(t *testing.T) {
	ops := []struct {
		name string
		call func(*Client, context.Context, string) error
	}{
		{"power cycle", (*Client).PowerCycleInstance},
		{"terminate", (*Client).TerminateInstance},
	}

	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			var calls atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				fmt.Fprint(w, `{"data":{}}`)
			}))
			defer srv.Close()

			t.Setenv(APIKeyEnvVar, "test-key")

			c := NewClient(srv.URL, WithHTTPClient(srv.Client()))
			assert.ErrorContains(t, op.call(c, context.Background(), ""), "instance id is empty")
			assert.Zero(t, calls.Load(), "an empty ID must not reach the API")
		})
	}
}

// 429 means the rate limiter rejected the request without acting on it, so it
// is the one outcome a non-idempotent POST may retry. The retry must resend the
// body, not an already-drained reader.
func TestClient_InstanceOperations_RateLimited_RetriesWithSameBody(t *testing.T) {
	for _, tc := range instanceOperationCases() {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				assert.JSONEq(t, `{"instance_ids":["i-123"]}`, string(body))

				if calls.Add(1) < 3 {
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, `{"error":"slow down"}`)

					return
				}

				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.respBody)
			}))
			defer srv.Close()

			t.Setenv(APIKeyEnvVar, "test-key")

			require.NoError(t, tc.call(context.Background(), NewClient(srv.URL, WithHTTPClient(srv.Client()), fastRetry(5))))
			assert.EqualValues(t, 3, calls.Load())
		})
	}
}

// A 5xx is ambiguous: the operation may already have landed. Resubmitting could
// power cycle a host that is already booting, so these POSTs must not retry.
func TestClient_InstanceOperations_ServerError_DoesNotRetry(t *testing.T) {
	for _, tc := range instanceOperationCases() {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"error":"boom"}`)
			}))
			defer srv.Close()

			t.Setenv(APIKeyEnvVar, "test-key")

			err := tc.call(context.Background(), NewClient(srv.URL, WithHTTPClient(srv.Client()), fastRetry(5)))
			assert.ErrorContains(t, err, "500")
			assert.ErrorContains(t, err, "i-123")
			assert.EqualValues(t, 1, calls.Load(), "a 5xx POST may already have taken effect")
		})
	}
}

// Same reasoning as the 5xx case: a lost response does not mean a lost request.
func TestClient_InstanceOperations_TransportError_DoesNotRetry(t *testing.T) {
	for _, tc := range instanceOperationCases() {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)

				// assert, not require: this runs on the server goroutine, and
				// require's FailNow is only valid on the test's own.
				hj, ok := w.(http.Hijacker)
				if !assert.True(t, ok) {
					return
				}

				conn, _, err := hj.Hijack()
				if !assert.NoError(t, err) {
					return
				}

				conn.Close()
			}))
			defer srv.Close()

			t.Setenv(APIKeyEnvVar, "test-key")

			err := tc.call(context.Background(), NewClient(srv.URL, WithHTTPClient(srv.Client()), fastRetry(5)))
			assert.Error(t, err)
			assert.EqualValues(t, 1, calls.Load(), "the request may have reached the API")
		})
	}
}

// TestClient_InstanceOperations_Forbidden_ErrorNamesInstance checks a rejected
// operation names the instance it was for, so one failure in a fleet-wide
// remediation can be traced back to a node.
func TestClient_InstanceOperations_Forbidden_ErrorNamesInstance(t *testing.T) {
	for _, tc := range instanceOperationCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"error":{"code":"global/account-inactive","message":"nope"}}`)
			}))
			defer srv.Close()

			t.Setenv(APIKeyEnvVar, "test-key")

			err := tc.call(context.Background(), NewClient(srv.URL, WithHTTPClient(srv.Client()), fastRetry(3)))
			assert.ErrorContains(t, err, "i-123")
			assert.ErrorContains(t, err, "403")
		})
	}
}
