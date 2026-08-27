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
	"maps"
	"testing"

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

// fakeSource returns pre-canned events without hitting HTTP; used to exercise
// pollEvents in isolation.
type fakeSource struct{ events []lambdaapi.Event }

func (f *fakeSource) fetchEvents(_ context.Context) ([]lambdaapi.Event, error) {
	return f.events, nil
}

// newInformerForTest builds a NodeInformer around the given uuid → node map
// without needing a real Kubernetes client. GetNodeName only reads the map.
func newInformerForTest(entries map[string]string) *NodeInformer {
	ni := &NodeInformer{instanceToNodeName: map[string]string{}}
	maps.Copy(ni.instanceToNodeName, entries)
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
			got := c.resolveLRNs(lambdaapi.Event{ID: "e1", EntityLRNs: tc.lrns})
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestPollEvents_LRNFanOut is the end-to-end check for the LRN fan-out:
// pollEvents must emit one internal MaintenanceEvent per resolved LRN, using
// EventID = raw.ID + "-" + instanceUUID; unresolvable LRNs (bad UUID or absent
// from the informer) are skipped individually and never cause a resolvable
// sibling to be dropped.
func TestPollEvents_LRNFanOut(t *testing.T) {
	tests := []struct {
		name       string
		informer   map[string]string
		entityLRNs []string
		want       []struct{ eventID, nodeName string }
	}{
		{
			name:       "multiple valid LRNs emit one event per node in order",
			informer:   map[string]string{"uuid-a": "node-a", "uuid-b": "node-b"},
			entityLRNs: []string{"lrn:cloud:instance:uuid-a", "lrn:cloud:instance:uuid-b"},
			want: []struct{ eventID, nodeName string }{
				{"evt-1-uuid-a", "node-a"},
				{"evt-1-uuid-b", "node-b"},
			},
		},
		{
			name:     "LRN[0] non-instance entity still emits resolved LRN[1]",
			informer: map[string]string{"uuid-b": "node-b"},
			entityLRNs: []string{
				"lrn:cloud:server:non-instance", // unresolvable
				"lrn:cloud:instance:uuid-b",     // resolves
			},
			want: []struct{ eventID, nodeName string }{{"evt-1-uuid-b", "node-b"}},
		},
		{
			name:       "all LRNs unresolvable emits nothing",
			informer:   map[string]string{"uuid-a": "node-a"},
			entityLRNs: []string{"lrn:cloud:instance:uuid-unknown", "lrn:cloud:server:x"},
			want:       nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{
				clusterName:  "test-cluster",
				nodeInformer: newInformerForTest(tc.informer),
				normalizer:   &eventpkg.LambdaNormalizer{},
				source: &fakeSource{events: []lambdaapi.Event{{
					ID:         "evt-1",
					Urgency:    "emergency",
					Status:     "scheduled",
					EntityLRNs: tc.entityLRNs,
				}}},
			}

			ch := make(chan model.MaintenanceEvent, 8)
			require.NoError(t, c.pollEvents(context.Background(), ch))
			close(ch)

			var got []model.MaintenanceEvent
			for e := range ch {
				got = append(got, e)
			}

			require.Len(t, got, len(tc.want))

			for i, w := range tc.want {
				assert.Equal(t, w.eventID, got[i].EventID)
				assert.Equal(t, w.nodeName, got[i].NodeName)
			}
		})
	}
}
