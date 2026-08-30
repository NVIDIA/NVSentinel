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

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func TestRecoveryPipelineOperators(t *testing.T) {
	filter := &PipelineFilter{extendedFilters: true}

	if !matchesExists(true, true) || !matchesExists(false, false) {
		t.Fatal("$exists did not match field presence")
	}
	if matchesExists(true, false) || matchesExists(false, true) || matchesExists(true, "true") {
		t.Fatal("$exists accepted a mismatched or invalid operand")
	}
	if !filter.matchesOperators(2, map[string]any{opGt: 1, opLt: 3}) ||
		filter.matchesOperators(2, map[string]any{opGt: 1, opLt: 2}) {
		t.Fatal("multiple comparison operators were not combined with AND")
	}

	for operator, expected := range map[string]bool{
		opGt: true, opGte: true, opLt: false, opLte: false,
	} {
		if actual := filter.matchesOrderedOperator(2, operator, 1); actual != expected {
			t.Fatalf("matchesOrderedOperator(%q) = %v, want %v", operator, actual, expected)
		}
	}
	if filter.matchesOrderedOperator(2, "$unsupported", 1) {
		t.Fatal("unsupported ordered operator matched")
	}
	if !filter.matchesOperators(1, map[string]any{}) {
		t.Fatal("empty operator set should match")
	}
	if filter.matchesOperators(1, map[string]any{"$unsupported": 1}) {
		t.Fatal("unsupported operator matched")
	}
}

func TestExistsDistinguishesMissingAndExplicitNull(t *testing.T) {
	for name, test := range map[string]struct {
		event    map[string]any
		exists   bool
		expected bool
	}{
		"missing matches false": {
			event:    map[string]any{"fullDocument": map[string]any{"healthevent": map[string]any{}}},
			expected: true,
		},
		"null does not match false": {
			event: map[string]any{"fullDocument": map[string]any{"healthevent": map[string]any{
				"processingstrategy": nil,
			}}},
		},
		"null matches true": {
			event: map[string]any{"fullDocument": map[string]any{"healthevent": map[string]any{
				"processingstrategy": nil,
			}}},
			exists:   true,
			expected: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			filter, err := NewPipelineFilter(client.WithExtendedFilters([]any{map[string]any{"$match": map[string]any{
				"fullDocument.healthevent.processingstrategy": map[string]any{"$exists": test.exists},
			}}}))
			if err != nil {
				t.Fatal(err)
			}
			if got := filter.MatchesEvent(datastore.EventWithToken{Event: test.event}); got != test.expected {
				t.Fatalf("MatchesEvent() = %t, want %t", got, test.expected)
			}
		})
	}
}

func TestExistsCombinesWithOtherOperators(t *testing.T) {
	filter, err := NewPipelineFilter(client.WithExtendedFilters([]any{map[string]any{"$match": map[string]any{
		"fullDocument.count": map[string]any{"$exists": true, "$gt": 3},
	}}}))
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		event map[string]any
		want  bool
	}{
		{event: map[string]any{"fullDocument": map[string]any{}}, want: false},
		{event: map[string]any{"fullDocument": map[string]any{"count": 2}}, want: false},
		{event: map[string]any{"fullDocument": map[string]any{"count": 4}}, want: true},
	} {
		if got := filter.MatchesEvent(datastore.EventWithToken{Event: test.event}); got != test.want {
			t.Fatalf("MatchesEvent(%v) = %t, want %t", test.event, got, test.want)
		}
	}
}

func TestGetFieldValue_CaseInsensitive(t *testing.T) {
	tests := []struct {
		name      string
		event     map[string]any
		fieldPath string
		expected  any
	}{
		{
			name: "exact case match - lowercase",
			event: map[string]any{
				"fullDocument": map[string]any{
					"healthevent": map[string]any{
						"ishealthy": false,
					},
				},
			},
			fieldPath: "fullDocument.healthevent.ishealthy",
			expected:  false,
		},
		{
			name: "exact case match - camelCase",
			event: map[string]any{
				"fullDocument": map[string]any{
					"healthevent": map[string]any{
						"isHealthy": true,
					},
				},
			},
			fieldPath: "fullDocument.healthevent.isHealthy",
			expected:  true,
		},
		{
			name: "case insensitive match - query lowercase, data camelCase",
			event: map[string]any{
				"fullDocument": map[string]any{
					"healthevent": map[string]any{
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
			event: map[string]any{
				"fullDocument": map[string]any{
					"healthevent": map[string]any{
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
			event: map[string]any{
				"fullDocument": map[string]any{
					"healthevent": map[string]any{
						"isFatal": true,
					},
				},
			},
			fieldPath: "fullDocument.healthevent.isfatal",
			expected:  true,
		},
		{
			name: "case insensitive match - nested agent field",
			event: map[string]any{
				"fullDocument": map[string]any{
					"healthevent": map[string]any{
						"agent": "simple-health-client",
					},
				},
			},
			fieldPath: "fullDocument.healthevent.Agent",
			expected:  "simple-health-client",
		},
		{
			name: "non-existent field",
			event: map[string]any{
				"fullDocument": map[string]any{
					"healthevent": map[string]any{
						"isHealthy": true,
					},
				},
			},
			fieldPath: "fullDocument.healthevent.nonexistent",
			expected:  nil,
		},
		{
			name: "top-level field",
			event: map[string]any{
				"operationType": "insert",
			},
			fieldPath: "operationType",
			expected:  "insert",
		},
		{
			name: "top-level field case insensitive",
			event: map[string]any{
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
		pipeline any
		event    datastore.EventWithToken
		expected bool
	}{
		{
			name: "matches with lowercase filter and camelCase data",
			pipeline: []any{
				map[string]any{
					"$match": map[string]any{
						"operationType":                      "insert",
						"fullDocument.healthevent.agent":     map[string]any{"$ne": "health-events-analyzer"},
						"fullDocument.healthevent.ishealthy": false,
					},
				},
			},
			event: datastore.EventWithToken{
				Event: map[string]any{
					"operationType": "insert",
					"fullDocument": map[string]any{
						"healthevent": map[string]any{
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
			pipeline: []any{
				map[string]any{
					"$match": map[string]any{
						"operationType":                      "insert",
						"fullDocument.healthevent.ishealthy": false,
					},
				},
			},
			event: datastore.EventWithToken{
				Event: map[string]any{
					"operationType": "insert",
					"fullDocument": map[string]any{
						"healthevent": map[string]any{
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
			pipeline: []any{
				map[string]any{
					"$match": map[string]any{
						"fullDocument.healthevent.isHealthy": true,
					},
				},
			},
			event: datastore.EventWithToken{
				Event: map[string]any{
					"fullDocument": map[string]any{
						"healthevent": map[string]any{
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
			pipeline: []any{
				map[string]any{
					"$match": map[string]any{
						"fullDocument.healthevent.isfatal": true,
					},
				},
			},
			event: datastore.EventWithToken{
				Event: map[string]any{
					"fullDocument": map[string]any{
						"healthevent": map[string]any{
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
			pipeline: []any{
				map[string]any{
					"$match": map[string]any{
						"fullDocument.healthevent.agent": map[string]any{
							"$ne": "health-events-analyzer",
						},
					},
				},
			},
			event: datastore.EventWithToken{
				Event: map[string]any{
					"fullDocument": map[string]any{
						"healthevent": map[string]any{
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
			pipeline: []any{
				map[string]any{
					"$match": map[string]any{
						"fullDocument.healthevent.agent": map[string]any{
							"$ne": "health-events-analyzer",
						},
					},
				},
			},
			event: datastore.EventWithToken{
				Event: map[string]any{
					"fullDocument": map[string]any{
						"healthevent": map[string]any{
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
	pipeline := []any{
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
				Event: map[string]any{
					"operationType": "insert",
					"fullDocument": map[string]any{
						"healthevent": map[string]any{
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
				Event: map[string]any{
					"operationType": "insert",
					"fullDocument": map[string]any{
						"healthevent": map[string]any{
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
				Event: map[string]any{
					"operationType": "insert",
					"fullDocument": map[string]any{
						"healthevent": map[string]any{
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
				Event: map[string]any{
					"operationType": "update",
					"fullDocument": map[string]any{
						"healthevent": map[string]any{
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
