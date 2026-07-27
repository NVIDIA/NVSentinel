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

	lambdaapi "github.com/nvidia/nvsentinel/commons/pkg/lambda"
)

func TestExtractUUIDFromLRN(t *testing.T) {
	tests := []struct {
		lrn  string
		want string
	}{
		{"lrn:cloud:instance:06c1e2f8a20042be8d4617c83fa18b39", "06c1e2f8a20042be8d4617c83fa18b39"},
		{"lrn:cloud:instance:abc-def-123", "abc-def-123"},
		{"lrn:cloud:server:abc123", ""},
		{"lrn:cloud:instance:", ""},
		{"lrn:cloud:instance", ""},
		{"", ""},
		{"no-colons", ""},
	}

	for _, tc := range tests {
		t.Run(tc.lrn, func(t *testing.T) {
			assert.Equal(t, tc.want, extractUUIDFromLRN(tc.lrn))
		})
	}
}

func TestAPISourceFetchEventsPagination(t *testing.T) {
	page1Token := "token-page2"
	page1 := apiResponse{}
	page1.Data.MaintenanceEvents = []Event{
		{ID: "event-1", Urgency: "emergency", Status: "scheduled"},
	}
	page1.Data.PageToken = &page1Token

	page2 := apiResponse{}
	page2.Data.MaintenanceEvents = []Event{
		{ID: "event-2", Urgency: "critical_with_deadline", Status: "scheduled"},
	}
	page2.Data.PageToken = nil

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		var resp apiResponse
		if r.URL.Query().Get("page_token") == page1Token {
			resp = page2
		} else {
			resp = page1
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer srv.Close()

	t.Setenv(lambdaapi.APIKeyEnvVar, "test-key")

	src := &apiSource{
		client: lambdaapi.NewClient(srv.URL, lambdaapi.WithHTTPClient(srv.Client())),
	}

	events, err := src.fetchEvents(context.Background())
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, "event-1", events[0].ID)
	assert.Equal(t, "event-2", events[1].ID)
}

func TestAPISourceFetchEventsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()

	t.Setenv(lambdaapi.APIKeyEnvVar, "bad-key")

	src := &apiSource{
		client: lambdaapi.NewClient(srv.URL, lambdaapi.WithHTTPClient(srv.Client())),
	}

	_, err := src.fetchEvents(context.Background())
	assert.ErrorContains(t, err, "401")
}
