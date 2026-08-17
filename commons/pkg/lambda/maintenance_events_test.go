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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_ListMaintenanceEvents_PaginatedResponse_ReturnsAllEvents(t *testing.T) {
	page1Token := "token-page2"
	page1 := apiResponse{
		Data: []Event{
			{ID: "event-1", Urgency: "emergency", Status: "scheduled"},
		},
		PageToken: &page1Token,
	}

	page2 := apiResponse{
		Data: []Event{
			{ID: "event-2", Urgency: "critical_with_deadline", Status: "scheduled"},
		},
		PageToken: nil,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// testify's require.* calls t.FailNow() → runtime.Goexit(), which is
		// unsafe from a non-test goroutine (like httptest handlers). Use
		// assert.* so failures record on t without exiting this goroutine.
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		var resp apiResponse
		if r.URL.Query().Get("page_token") == page1Token {
			resp = page2
		} else {
			resp = page1
		}

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer srv.Close()

	t.Setenv(APIKeyEnvVar, "test-key")

	client := NewClient(srv.URL, WithHTTPClient(srv.Client()))

	events, err := client.ListMaintenanceEvents(context.Background())
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, "event-1", events[0].ID)
	assert.Equal(t, "event-2", events[1].ID)
}

// TestClient_ListMaintenanceEvents_WorkspaceID checks the workspace scope is
// sent on every page, and left off entirely when none was configured so the API
// keeps falling back to the key's own workspace.
func TestClient_ListMaintenanceEvents_WorkspaceID(t *testing.T) {
	tests := []struct {
		name        string
		workspaceID string
	}{
		{name: "configured workspace is sent", workspaceID: "c4d291f47f9d436fa39f58493ce3b50d"},
		{name: "unset workspace omits the parameter", workspaceID: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nextToken := "token-page2"

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query := r.URL.Query()

				// Presence is asserted separately from the value: Get returns ""
				// both for an absent parameter and for a bare "workspace_id=",
				// so dropping the guard in ListMaintenanceEvents would otherwise
				// go unnoticed here. The API answers 400 to an empty one.
				_, present := query["workspace_id"]
				assert.Equal(t, tc.workspaceID != "", present, "workspace_id present in query")
				assert.Equal(t, tc.workspaceID, query.Get("workspace_id"))

				resp := apiResponse{Data: []Event{{ID: "event-2"}}}
				if query.Get("page_token") == "" {
					resp = apiResponse{Data: []Event{{ID: "event-1"}}, PageToken: &nextToken}
				}

				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.NewEncoder(w).Encode(resp))
			}))
			defer srv.Close()

			t.Setenv(APIKeyEnvVar, "test-key")

			client := NewClient(srv.URL, WithHTTPClient(srv.Client()), WithWorkspaceID(tc.workspaceID))

			events, err := client.ListMaintenanceEvents(context.Background())
			require.NoError(t, err)
			require.Len(t, events, 2)
		})
	}
}

func TestClient_ListMaintenanceEvents_ServerError_ReturnsWrappedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()

	t.Setenv(APIKeyEnvVar, "bad-key")

	client := NewClient(srv.URL, WithHTTPClient(srv.Client()))

	_, err := client.ListMaintenanceEvents(context.Background())
	assert.ErrorContains(t, err, "401")
}

// TestClient_ListMaintenanceEvents_PaginationErrors covers the two guards on
// the pagination loop: refuse to spin on a non-advancing page_token, and cap
// the total number of pages so a runaway server can't grow memory without
// bound.
func TestClient_ListMaintenanceEvents_PaginationErrors(t *testing.T) {
	stuckToken := "stuck-token"

	tests := []struct {
		name        string
		handler     http.HandlerFunc
		wantErrText string
	}{
		{
			name: "non-advancing page_token returns error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				resp := apiResponse{
					Data:      []Event{{ID: "event-stuck", Urgency: "emergency", Status: "scheduled"}},
					PageToken: &stuckToken,
				}
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.NewEncoder(w).Encode(resp))
			},
			wantErrText: "did not advance",
		},
		{
			// Ever-changing tokens defeat the same-token guard, so the page cap
			// must be what terminates the loop.
			name: "unbounded pagination hits page cap",
			handler: func() http.HandlerFunc {
				page := 0
				return func(w http.ResponseWriter, _ *http.Request) {
					page++
					next := fmt.Sprintf("token-%d", page)
					resp := apiResponse{
						Data:      []Event{{ID: fmt.Sprintf("event-%d", page)}},
						PageToken: &next,
					}
					w.Header().Set("Content-Type", "application/json")
					assert.NoError(t, json.NewEncoder(w).Encode(resp))
				}
			}(),
			wantErrText: "exceeded",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			t.Setenv(APIKeyEnvVar, "test-key")

			client := NewClient(srv.URL, WithHTTPClient(srv.Client()))

			_, err := client.ListMaintenanceEvents(context.Background())
			assert.ErrorContains(t, err, tc.wantErrText)
		})
	}
}
