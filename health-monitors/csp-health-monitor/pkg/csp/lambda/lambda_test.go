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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lambdaapi "github.com/nvidia/nvsentinel/commons/pkg/lambda"
	eventpkg "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/event"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
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

// fakeSource returns pre-canned events without hitting HTTP; used to exercise
// pollEvents in isolation.
type fakeSource struct{ events []Event }

func (f *fakeSource) fetchEvents(_ context.Context) ([]Event, error) {
	return f.events, nil
}

// newInformerForTest builds a NodeInformer around the given uuid → node map
// without needing a real Kubernetes client. GetNodeName only reads the map.
func newInformerForTest(entries map[string]string) *NodeInformer {
	ni := &NodeInformer{instanceToNodeName: map[string]string{}}
	for k, v := range entries {
		ni.instanceToNodeName[k] = v
	}
	return ni
}

func TestResolveLRNs(t *testing.T) {
	c := &Client{
		nodeInformer: newInformerForTest(map[string]string{
			"uuid-a": "node-a",
			"uuid-b": "node-b",
		}),
	}

	tests := []struct {
		name string
		lrns []string
		want []resolvedLRN
	}{
		{
			name: "single valid LRN",
			lrns: []string{"lrn:cloud:instance:uuid-a"},
			want: []resolvedLRN{{uuid: "uuid-a", nodeName: "node-a"}},
		},
		{
			name: "multiple valid LRNs — all fan out",
			lrns: []string{"lrn:cloud:instance:uuid-a", "lrn:cloud:instance:uuid-b"},
			want: []resolvedLRN{
				{uuid: "uuid-a", nodeName: "node-a"},
				{uuid: "uuid-b", nodeName: "node-b"},
			},
		},
		{
			name: "LRN[0] is non-instance entity — LRN[1] still resolves",
			lrns: []string{"lrn:cloud:server:whatever", "lrn:cloud:instance:uuid-a"},
			want: []resolvedLRN{{uuid: "uuid-a", nodeName: "node-a"}},
		},
		{
			name: "LRN[0] unknown UUID — LRN[1] still resolves",
			lrns: []string{"lrn:cloud:instance:uuid-unknown", "lrn:cloud:instance:uuid-b"},
			want: []resolvedLRN{{uuid: "uuid-b", nodeName: "node-b"}},
		},
		{
			name: "all LRNs unresolvable — empty result",
			lrns: []string{"lrn:cloud:instance:uuid-unknown", "lrn:cloud:server:x"},
			want: nil,
		},
		{
			name: "empty entity_lrns list",
			lrns: nil,
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := c.resolveLRNs(Event{ID: "e1", EntityLRNs: tc.lrns})
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestPollEventsFanOutsAcrossLRNs is the end-to-end check for fix 2:
// a single Lambda event with multiple entity_lrns produces one internal
// MaintenanceEvent per resolved node, each with a suffixed EventID.
func TestPollEventsFanOutsAcrossLRNs(t *testing.T) {
	c := &Client{
		clusterName:  "test-cluster",
		nodeInformer: newInformerForTest(map[string]string{"uuid-a": "node-a", "uuid-b": "node-b"}),
		normalizer:   &eventpkg.LambdaNormalizer{},
		source: &fakeSource{events: []Event{{
			ID:         "evt-1",
			Urgency:    "emergency",
			Status:     "scheduled",
			EntityLRNs: []string{"lrn:cloud:instance:uuid-a", "lrn:cloud:instance:uuid-b"},
		}}},
		triggerTimeLimit: 30 * time.Minute,
	}

	ch := make(chan model.MaintenanceEvent, 4)
	require.NoError(t, c.pollEvents(context.Background(), ch))
	close(ch)

	var got []model.MaintenanceEvent
	for e := range ch {
		got = append(got, e)
	}

	require.Len(t, got, 2, "one internal event per resolved LRN")

	// Order matches EntityLRNs order.
	assert.Equal(t, "evt-1-uuid-a", got[0].EventID)
	assert.Equal(t, "node-a", got[0].NodeName)
	assert.Equal(t, "evt-1-uuid-b", got[1].EventID)
	assert.Equal(t, "node-b", got[1].NodeName)
}

// TestPollEventsPartialLRNResolution regresses the specific bug called out in
// review — LRN[0] being unresolvable used to drop the entire event even if
// LRN[1] resolved.
func TestPollEventsPartialLRNResolution(t *testing.T) {
	c := &Client{
		clusterName:  "test-cluster",
		nodeInformer: newInformerForTest(map[string]string{"uuid-b": "node-b"}),
		normalizer:   &eventpkg.LambdaNormalizer{},
		source: &fakeSource{events: []Event{{
			ID:      "evt-1",
			Urgency: "emergency",
			Status:  "scheduled",
			EntityLRNs: []string{
				"lrn:cloud:server:non-instance", // unresolvable
				"lrn:cloud:instance:uuid-b",     // resolves
			},
		}}},
		triggerTimeLimit: 30 * time.Minute,
	}

	ch := make(chan model.MaintenanceEvent, 4)
	require.NoError(t, c.pollEvents(context.Background(), ch))
	close(ch)

	var got []model.MaintenanceEvent
	for e := range ch {
		got = append(got, e)
	}

	require.Len(t, got, 1, "the resolved LRN should emit even though LRN[0] was bad")
	assert.Equal(t, "evt-1-uuid-b", got[0].EventID)
	assert.Equal(t, "node-b", got[0].NodeName)
}
