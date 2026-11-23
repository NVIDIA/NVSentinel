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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_ListMaintenance_SingleEvent_ReturnsParsedFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/account/maintenance", r.URL.Path)
		assert.Equal(t, "500", r.URL.Query().Get("page_size"))

		_, _ = w.Write([]byte(`{"page":1,"pages":1,"results":1,"data":[{
			"id": 7,
			"entity": {"id": 11111111, "label": "lke123456-gpu", "type": "linode", "url": "/v4/linode/instances/11111111"},
			"type": "power_off_on",
			"status": "scheduled",
			"reason": "Host upgrade",
			"description": "scheduled",
			"source": "platform",
			"maintenance_policy_set": "linode/power_off_on",
			"when": "2026-10-20T12:00:00",
			"not_before": "2026-10-20T12:00:00Z",
			"start_time": null,
			"complete_time": null
		}]}`))
	}))
	defer srv.Close()

	events, err := newTestClient(t, srv).ListMaintenance(context.Background())
	require.NoError(t, err)
	require.Len(t, events, 1)

	got := events[0]
	assert.Equal(t, 7, got.ID)
	assert.Equal(t, 11111111, got.Entity.ID)
	assert.Equal(t, MaintenanceEntityLinode, got.Entity.Type)
	assert.Equal(t, "power_off_on", got.Type)
	assert.Equal(t, "linode/power_off_on", got.MaintenancePolicySet)
	assert.Nil(t, got.StartTime)

	// Both the zone-less form and RFC 3339 land on the same instant, in UTC.
	want := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	require.NotNil(t, got.When)
	assert.True(t, got.When.Equal(want))
	require.NotNil(t, got.NotBefore)
	assert.True(t, got.NotBefore.Equal(want))
}

func TestTimestamp_UnmarshalJSON_Garbage_ReturnsError(t *testing.T) {
	var ts Timestamp

	require.Error(t, ts.UnmarshalJSON([]byte(`"yesterday"`)))
}

func TestClient_ListMaintenance_MultiplePages_ReturnsAllEvents(t *testing.T) {
	var pages []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("page")
		pages = append(pages, p)

		if p == "1" {
			_, _ = w.Write([]byte(`{"page":1,"pages":2,"results":2,"data":[{"id":1}]}`))

			return
		}

		_, _ = w.Write([]byte(`{"page":2,"pages":2,"results":2,"data":[{"id":2}]}`))
	}))
	defer srv.Close()

	events, err := newTestClient(t, srv).ListMaintenance(context.Background())
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, []string{"1", "2"}, pages)
}

// TestClient_ListMaintenance_NoEvents_ReturnsEmptyList pins that an account with no events is an empty
// list, not an error.
func TestClient_ListMaintenance_NoEvents_ReturnsEmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"page":1,"pages":1,"results":0,"data":[]}`))
	}))
	defer srv.Close()

	events, err := newTestClient(t, srv).ListMaintenance(context.Background())
	require.NoError(t, err)
	assert.Empty(t, events)
}

func serveBody(body string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}
}

func TestClient_ListMaintenance_ResultsExceedItems_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(serveBody(
		`{"page":1,"pages":1,"results":5,"data":[{"id":1},{"id":2}]}`)))
	defer srv.Close()

	events, err := newTestClient(t, srv).ListMaintenance(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned 2 items but the API reports 5")
	assert.Nil(t, events)
}

func TestClient_ListMaintenance_ResultsAbsent_ReturnsItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(serveBody(`{"page":1,"pages":1,"data":[{"id":1},{"id":2}]}`)))
	defer srv.Close()

	events, err := newTestClient(t, srv).ListMaintenance(context.Background())
	require.NoError(t, err)
	assert.Len(t, events, 2)
}

func TestClient_ListMaintenance_ZeroPagesWithResults_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(serveBody(`{"page":1,"pages":0,"results":3,"data":[]}`)))
	defer srv.Close()

	events, err := newTestClient(t, srv).ListMaintenance(context.Background())
	require.Error(t, err)
	assert.Nil(t, events)
}
