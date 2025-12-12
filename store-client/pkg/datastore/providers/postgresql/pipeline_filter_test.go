// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package postgresql

import (
	"testing"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func TestGetFieldValue_CaseInsensitive(t *testing.T) {
	tests := []struct {
		name      string
		event     map[string]interface{}
		fieldPath string
		expected  interface{}
	}{
		{
			name: "exact case match - lowercase",
			event: map[string]interface{}{
				"fullDocument": map[string]interface{}{
					"healthevent": map[string]interface{}{
						"ishealthy": false,
					},
				},
			},
			fieldPath: "fullDocument.healthevent.ishealthy",
			expected:  false,
		},
		{
			name: "exact case match - camelCase",
			event: map[string]interface{}{
				"fullDocument": map[string]interface{}{
					"healthevent": map[string]interface{}{
						"isHealthy": true,
					},
				},
			},
			fieldPath: "fullDocument.healthevent.isHealthy",
			expected:  true,
		},
		{
			name: "case insensitive match - query lowercase, data camelCase",
			event: map[string]interface{}{
				"fullDocument": map[string]interface{}{
					"healthevent": map[string]interface{}{
						"isHealthy": false,
						"isFatal":   true,
					},
				},
			},
			fieldPath: "fullDocument.healthevent.ishealthy",
			expected:  false,
		},
		{
			name: "case insensitive match - query camelCase, data lowercase",
			event: map[string]interface{}{
				"fullDocument": map[string]interface{}{
					"healthevent": map[string]interface{}{
						"ishealthy": true,
						"isfatal":   false,
					},
				},
			},
			fieldPath: "fullDocument.healthevent.isHealthy",
			expected:  true,
		},
		{
			name: "case insensitive match - isFatal",
			event: map[string]interface{}{
				"fullDocument": map[string]interface{}{
					"healthevent": map[string]interface{}{
						"isFatal": true,
					},
				},
			},
			fieldPath: "fullDocument.healthevent.isfatal",
			expected:  true,
		},
		{
			name: "case insensitive match - nested agent field",
			event: map[string]interface{}{
				"fullDocument": map[string]interface{}{
					"healthevent": map[string]interface{}{
						"agent": "simple-health-client",
					},
				},
			},
			fieldPath: "fullDocument.healthevent.Agent",
			expected:  "simple-health-client",
		},
		{
			name: "non-existent field",
			event: map[string]interface{}{
				"fullDocument": map[string]interface{}{
					"healthevent": map[string]interface{}{
						"isHealthy": true,
					},
				},
			},
			fieldPath: "fullDocument.healthevent.nonexistent",
			expected:  nil,
		},
		{
			name: "top-level field",
			event: map[string]interface{}{
				"operationType": "insert",
			},
			fieldPath: "operationType",
			expected:  "insert",
		},
		{
			name: "top-level field case insensitive",
			event: map[string]interface{}{
				"operationType": "update",
			},
			fieldPath: "OperationType",
			expected:  "update",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := &PipelineFilter{}
			result := filter.getFieldValue(tt.event, tt.fieldPath)

			if result != tt.expected {
				t.Errorf("getFieldValue() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestMatchesEvent_CaseInsensitive(t *testing.T) {
	tests := []struct {
		name     string
		pipeline interface{}
		event    datastore.EventWithToken
		expected bool
	}{
		{
			name: "matches with lowercase filter and camelCase data",
			pipeline: []interface{}{
				map[string]interface{}{
					"$match": map[string]interface{}{
						"operationType":                    "insert",
						"fullDocument.healthevent.agent":   map[string]interface{}{"$ne": "health-events-analyzer"},
						"fullDocument.healthevent.ishealthy": false,
					},
				},
			},
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "insert",
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"agent":     "simple-health-client",
							"isHealthy": false,
						},
					},
				},
				ResumeToken: []byte("123"),
			},
			expected: true,
		},
		{
			name: "does not match when isHealthy is true",
			pipeline: []interface{}{
				map[string]interface{}{
					"$match": map[string]interface{}{
						"operationType":                    "insert",
						"fullDocument.healthevent.ishealthy": false,
					},
				},
			},
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "insert",
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"isHealthy": true,
						},
					},
				},
				ResumeToken: []byte("124"),
			},
			expected: false,
		},
		{
			name: "matches with camelCase filter and lowercase data",
			pipeline: []interface{}{
				map[string]interface{}{
					"$match": map[string]interface{}{
						"fullDocument.healthevent.isHealthy": true,
					},
				},
			},
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"ishealthy": true,
						},
					},
				},
				ResumeToken: []byte("125"),
			},
			expected: true,
		},
		{
			name: "matches isFatal case insensitive",
			pipeline: []interface{}{
				map[string]interface{}{
					"$match": map[string]interface{}{
						"fullDocument.healthevent.isfatal": true,
					},
				},
			},
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"isFatal": true,
						},
					},
				},
				ResumeToken: []byte("126"),
			},
			expected: true,
		},
		{
			name: "matches with $ne operator case insensitive",
			pipeline: []interface{}{
				map[string]interface{}{
					"$match": map[string]interface{}{
						"fullDocument.healthevent.agent": map[string]interface{}{
							"$ne": "health-events-analyzer",
						},
					},
				},
			},
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"Agent": "simple-health-client",
						},
					},
				},
				ResumeToken: []byte("127"),
			},
			expected: true,
		},
		{
			name: "does not match when agent is health-events-analyzer",
			pipeline: []interface{}{
				map[string]interface{}{
					"$match": map[string]interface{}{
						"fullDocument.healthevent.agent": map[string]interface{}{
							"$ne": "health-events-analyzer",
						},
					},
				},
			},
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"agent": "health-events-analyzer",
						},
					},
				},
				ResumeToken: []byte("128"),
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter, err := NewPipelineFilter(tt.pipeline)
			if err != nil {
				t.Fatalf("NewPipelineFilter() error = %v", err)
			}

			result := filter.MatchesEvent(tt.event)
			if result != tt.expected {
				t.Errorf("MatchesEvent() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestMatchesEvent_RealWorldHealthEventsAnalyzer(t *testing.T) {
	// This test simulates the actual pipeline used by health-events-analyzer
	// and ensures it works with PostgreSQL's camelCase field names
	pipeline := []interface{}{
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", "insert"),
				datastore.E("fullDocument.healthevent.agent", datastore.D(datastore.E("$ne", "health-events-analyzer"))),
				datastore.E("fullDocument.healthevent.ishealthy", false),
			)),
		),
	}

	tests := []struct {
		name     string
		event    datastore.EventWithToken
		expected bool
	}{
		{
			name: "should match - unhealthy insert from simple-health-client",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "insert",
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"agent":     "simple-health-client",
							"isHealthy": false,
							"isFatal":   true,
							"nodename":  "kwok-node-0",
						},
					},
				},
				ResumeToken: []byte("200"),
			},
			expected: true,
		},
		{
			name: "should NOT match - healthy insert",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "insert",
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"agent":     "simple-health-client",
							"isHealthy": true,
							"nodename":  "kwok-node-0",
						},
					},
				},
				ResumeToken: []byte("201"),
			},
			expected: false,
		},
		{
			name: "should NOT match - from health-events-analyzer itself",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "insert",
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"agent":     "health-events-analyzer",
							"isHealthy": false,
							"nodename":  "kwok-node-0",
						},
					},
				},
				ResumeToken: []byte("202"),
			},
			expected: false,
		},
		{
			name: "should NOT match - update operation",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "update",
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"agent":     "simple-health-client",
							"isHealthy": false,
							"nodename":  "kwok-node-0",
						},
					},
				},
				ResumeToken: []byte("203"),
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter, err := NewPipelineFilter(pipeline)
			if err != nil {
				t.Fatalf("NewPipelineFilter() error = %v", err)
			}

			result := filter.MatchesEvent(tt.event)
			if result != tt.expected {
				t.Errorf("MatchesEvent() = %v, expected %v", result, tt.expected)
			}
		})
	}
}
