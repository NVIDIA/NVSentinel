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
	"fmt"
	"testing"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// TestMatchesIn_NullAndEmptyString tests the $in operator with NULL and empty string values
func TestMatchesIn_NullAndEmptyString(t *testing.T) {
	tests := []struct {
		name        string
		actualValue interface{}
		inArray     []interface{}
		expected    bool
		comment     string // optional explanation for test behavior
	}{
		{
			name:        "NULL value does not match non-empty strings",
			actualValue: nil,
			inArray:     []interface{}{"Succeeded", "AlreadyDrained"},
			expected:    false,
		},
		{
			name:        "NULL value does not match Quarantined",
			actualValue: nil,
			inArray:     []interface{}{"Quarantined", "AlreadyQuarantined"},
			expected:    false,
		},
		{
			name:        "NULL value does not match array with NULL",
			actualValue: nil,
			inArray:     []interface{}{nil, "Succeeded"},
			expected:    false,
			comment: "In MongoDB, { field: { $in: [null, \"value\"] } } matches documents " +
				"where field is null. However, in our PostgreSQL implementation, we explicitly " +
				"reject NULL values in $in checks to prevent incorrect matches. " +
				"This is intentional because in our use case, NULL means \"not set yet\" " +
				"and should never match valid enum values like \"Quarantined\", \"Succeeded\", etc.",
		},
		{
			name:        "empty string does not match non-empty strings",
			actualValue: "",
			inArray:     []interface{}{"Succeeded", "AlreadyDrained"},
			expected:    false,
		},
		{
			name:        "empty string matches if explicitly in array",
			actualValue: "",
			inArray:     []interface{}{"", "Succeeded"},
			expected:    true,
		},
		{
			name:        "non-empty string matches",
			actualValue: "Succeeded",
			inArray:     []interface{}{"Succeeded", "AlreadyDrained"},
			expected:    true,
		},
		{
			name:        "Quarantined matches",
			actualValue: "Quarantined",
			inArray:     []interface{}{"Quarantined", "AlreadyQuarantined"},
			expected:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := &PipelineFilter{}
			result := filter.matchesIn(tt.actualValue, tt.inArray)

			if result != tt.expected {
				msg := fmt.Sprintf("matchesIn(%v, %v) = %v, expected %v",
					tt.actualValue, tt.inArray, result, tt.expected)
				if tt.comment != "" {
					msg += "\nNote: " + tt.comment
				}
				t.Error(msg)
			}
		})
	}
}

// TestMatchesEqual_NullHandling tests equality comparison with NULL values
func TestMatchesEqual_NullHandling(t *testing.T) {
	tests := []struct {
		name     string
		actual   interface{}
		expected interface{}
		want     bool
	}{
		{
			name:     "NULL equals NULL",
			actual:   nil,
			expected: nil,
			want:     true,
		},
		{
			name:     "NULL does not equal string",
			actual:   nil,
			expected: "Quarantined",
			want:     false,
		},
		{
			name:     "string does not equal NULL",
			actual:   "Quarantined",
			expected: nil,
			want:     false,
		},
		{
			name:     "empty string does not equal NULL",
			actual:   "",
			expected: nil,
			want:     false,
		},
		{
			name:     "NULL does not equal empty string",
			actual:   nil,
			expected: "",
			want:     false,
		},
		{
			name:     "empty string equals empty string",
			actual:   "",
			expected: "",
			want:     true,
		},
		{
			name:     "non-empty strings equal",
			actual:   "Succeeded",
			expected: "Succeeded",
			want:     true,
		},
		{
			name:     "non-empty strings not equal",
			actual:   "Succeeded",
			expected: "Failed",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := &PipelineFilter{}
			result := filter.matchesEqual(tt.actual, tt.expected)

			if result != tt.want {
				t.Errorf("matchesEqual(%v, %v) = %v, want %v",
					tt.actual, tt.expected, result, tt.want)
			}
		})
	}
}

// TestMatchesEqual_MissingBooleanField tests that missing boolean fields (nil) match expected false
// This is critical for protobuf/JSON serialization where false booleans are often omitted
func TestMatchesEqual_MissingBooleanField(t *testing.T) {
	tests := []struct {
		name     string
		actual   interface{}
		expected interface{}
		want     bool
		reason   string
	}{
		{
			name:     "nil matches expected false - protobuf default behavior",
			actual:   nil,
			expected: false,
			want:     true,
			reason:   "Protobuf omits false boolean fields from JSON, so nil should match false",
		},
		{
			name:     "nil does not match expected true",
			actual:   nil,
			expected: true,
			want:     false,
			reason:   "Missing field should not match expected true",
		},
		{
			name:     "false matches expected false",
			actual:   false,
			expected: false,
			want:     true,
			reason:   "Explicit false should match expected false",
		},
		{
			name:     "true matches expected true",
			actual:   true,
			expected: true,
			want:     true,
			reason:   "Explicit true should match expected true",
		},
		{
			name:     "true does not match expected false",
			actual:   true,
			expected: false,
			want:     false,
			reason:   "true should not match false",
		},
		{
			name:     "false does not match expected true",
			actual:   false,
			expected: true,
			want:     false,
			reason:   "false should not match true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := &PipelineFilter{}
			result := filter.matchesEqual(tt.actual, tt.expected)

			if result != tt.want {
				t.Errorf("matchesEqual(%v, %v) = %v, want %v\nReason: %s",
					tt.actual, tt.expected, result, tt.want, tt.reason)
			}
		})
	}
}

// TestMatchesEvent_QuarantinedAndDrainedPipeline tests the real fault-remediation pipeline
// with NULL and empty values to ensure proper filtering
func TestMatchesEvent_QuarantinedAndDrainedPipeline(t *testing.T) {
	// This is the actual pipeline used by fault-remediation
	pipeline := []interface{}{
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("$or", datastore.A(
					// Case 1: UPDATE with both fields set correctly
					datastore.D(
						datastore.E("operationType", "update"),
						datastore.E("$or", datastore.A(
							datastore.D(
								datastore.E("fullDocument.healtheventstatus.userpodsevictionstatus.status", datastore.D(
									datastore.E("$in", datastore.A("Succeeded", "AlreadyDrained")),
								)),
								datastore.E("fullDocument.healtheventstatus.nodequarantined", datastore.D(
									datastore.E("$in", datastore.A("Quarantined", "AlreadyQuarantined")),
								)),
							),
							datastore.D(
								datastore.E("fullDocument.healtheventstatus.nodequarantined", "UnQuarantined"),
								datastore.E("fullDocument.healtheventstatus.userpodsevictionstatus.status", "Succeeded"),
							),
							datastore.D(
								datastore.E("fullDocument.healtheventstatus.nodequarantined", "Cancelled"),
							),
						)),
					),
					// Case 2: INSERT with both fields set correctly
					datastore.D(
						datastore.E("operationType", "insert"),
						datastore.E("$or", datastore.A(
							datastore.D(
								datastore.E("fullDocument.healtheventstatus.userpodsevictionstatus.status", datastore.D(
									datastore.E("$in", datastore.A("Succeeded", "AlreadyDrained")),
								)),
								datastore.E("fullDocument.healtheventstatus.nodequarantined", datastore.D(
									datastore.E("$in", datastore.A("Quarantined", "AlreadyQuarantined")),
								)),
							),
							datastore.D(
								datastore.E("fullDocument.healtheventstatus.nodequarantined", "UnQuarantined"),
								datastore.E("fullDocument.healtheventstatus.userpodsevictionstatus.status", "Succeeded"),
							),
							datastore.D(
								datastore.E("fullDocument.healtheventstatus.nodequarantined", "Cancelled"),
							),
						)),
					),
				)),
			)),
		),
	}

	tests := []struct {
		name     string
		event    datastore.EventWithToken
		expected bool
		reason   string
	}{
		{
			name: "SHOULD MATCH: UPDATE with Quarantined and Succeeded",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "update",
					"fullDocument": map[string]interface{}{
						"healtheventstatus": map[string]interface{}{
							"nodequarantined": "Quarantined",
							"userpodsevictionstatus": map[string]interface{}{
								"status": "Succeeded",
							},
						},
					},
				},
				ResumeToken: []byte("1"),
			},
			expected: true,
			reason:   "Both fields have valid values",
		},
		{
			name: "SHOULD NOT MATCH: INSERT with NULL nodequarantined and empty status",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "insert",
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"agent":            "health-events-analyzer",
							"checkName":        "MultipleRemediations",
							"recommendedAction": 5,
						},
						"healtheventstatus": map[string]interface{}{
							"nodequarantined": nil,
							"userpodsevictionstatus": map[string]interface{}{
								"status": "",
							},
						},
					},
				},
				ResumeToken: []byte("2"),
			},
			expected: false,
			reason:   "NULL nodequarantined and empty status should not match",
		},
		{
			name: "SHOULD NOT MATCH: UPDATE with NULL nodequarantined",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "update",
					"fullDocument": map[string]interface{}{
						"healtheventstatus": map[string]interface{}{
							"nodequarantined": nil,
							"userpodsevictionstatus": map[string]interface{}{
								"status": "Succeeded",
							},
						},
					},
				},
				ResumeToken: []byte("3"),
			},
			expected: false,
			reason:   "NULL nodequarantined should not match even if status is Succeeded",
		},
		{
			name: "SHOULD NOT MATCH: UPDATE with empty status",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "update",
					"fullDocument": map[string]interface{}{
						"healtheventstatus": map[string]interface{}{
							"nodequarantined": "Quarantined",
							"userpodsevictionstatus": map[string]interface{}{
								"status": "",
							},
						},
					},
				},
				ResumeToken: []byte("4"),
			},
			expected: false,
			reason:   "Empty status should not match even if nodequarantined is valid",
		},
		{
			name: "SHOULD NOT MATCH: UPDATE with InProgress status",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "update",
					"fullDocument": map[string]interface{}{
						"healtheventstatus": map[string]interface{}{
							"nodequarantined": "Quarantined",
							"userpodsevictionstatus": map[string]interface{}{
								"status": "InProgress",
							},
						},
					},
				},
				ResumeToken: []byte("5"),
			},
			expected: false,
			reason:   "InProgress status should not match",
		},
		{
			name: "SHOULD MATCH: UPDATE with UnQuarantined and Succeeded",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "update",
					"fullDocument": map[string]interface{}{
						"healtheventstatus": map[string]interface{}{
							"nodequarantined": "UnQuarantined",
							"userpodsevictionstatus": map[string]interface{}{
								"status": "Succeeded",
							},
						},
					},
				},
				ResumeToken: []byte("6"),
			},
			expected: true,
			reason:   "UnQuarantined with Succeeded status is valid for cleanup",
		},
		{
			name: "SHOULD MATCH: UPDATE with Cancelled",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "update",
					"fullDocument": map[string]interface{}{
						"healtheventstatus": map[string]interface{}{
							"nodequarantined": "Cancelled",
						},
					},
				},
				ResumeToken: []byte("7"),
			},
			expected: true,
			reason:   "Cancelled nodequarantined alone is sufficient",
		},
		{
			name: "SHOULD MATCH: INSERT with AlreadyQuarantined and AlreadyDrained",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "insert",
					"fullDocument": map[string]interface{}{
						"healtheventstatus": map[string]interface{}{
							"nodequarantined": "AlreadyQuarantined",
							"userpodsevictionstatus": map[string]interface{}{
								"status": "AlreadyDrained",
							},
						},
					},
				},
				ResumeToken: []byte("8"),
			},
			expected: true,
			reason:   "AlreadyQuarantined and AlreadyDrained are valid values",
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
				t.Errorf("MatchesEvent() = %v, expected %v\nReason: %s\nEvent: %+v",
					result, tt.expected, tt.reason, tt.event.Event)
			}
		})
	}
}

// TestMatchesEvent_HealthEventsAnalyzerPipeline tests the health-events-analyzer pipeline
// with events where isHealthy field is missing (protobuf default behavior)
func TestMatchesEvent_HealthEventsAnalyzerPipeline(t *testing.T) {
	// This is the actual pipeline used by health-events-analyzer
	// It filters for: operationType in [insert, update], agent != health-events-analyzer, isHealthy = false
	pipeline := []interface{}{
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", datastore.D(datastore.E("$in", datastore.A("insert", "update")))),
				datastore.E("fullDocument.healthevent.agent", datastore.D(datastore.E("$ne", "health-events-analyzer"))),
				datastore.E("fullDocument.healthevent.ishealthy", false),
			)),
		),
	}

	tests := []struct {
		name     string
		event    datastore.EventWithToken
		expected bool
		reason   string
	}{
		{
			name: "SHOULD MATCH: INSERT with isFatal=true and isHealthy missing (protobuf default)",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "insert",
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"agent":          "gpu-health-monitor",
							"checkName":      "GpuXidError",
							"componentClass": "GPU",
							"isFatal":        true,
							"nodeName":       "kwok-node-23",
							// isHealthy is NOT present - this is protobuf default behavior
							// when isHealthy=false, it's omitted from JSON
						},
					},
				},
				ResumeToken: []byte("100"),
			},
			expected: true,
			reason:   "Missing isHealthy field should be treated as false (protobuf default)",
		},
		{
			name: "SHOULD MATCH: UPDATE with isHealthy explicitly false",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "update",
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"agent":     "gpu-health-monitor",
							"checkName": "GpuXidError",
							"isFatal":   true,
							"isHealthy": false, // explicitly set to false
							"nodeName":  "kwok-node-23",
						},
					},
				},
				ResumeToken: []byte("101"),
			},
			expected: true,
			reason:   "Explicit isHealthy=false should match",
		},
		{
			name: "SHOULD NOT MATCH: INSERT with isHealthy=true",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "insert",
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"agent":     "gpu-health-monitor",
							"checkName": "GpuXidError",
							"isFatal":   false,
							"isHealthy": true, // healthy event
							"nodeName":  "kwok-node-23",
						},
					},
				},
				ResumeToken: []byte("102"),
			},
			expected: false,
			reason:   "isHealthy=true should not match filter for isHealthy=false",
		},
		{
			name: "SHOULD NOT MATCH: INSERT from health-events-analyzer agent",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "insert",
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"agent":     "health-events-analyzer", // self-generated event
							"checkName": "MultipleRemediations",
							"isFatal":   true,
							// isHealthy missing (false)
							"nodeName": "kwok-node-23",
						},
					},
				},
				ResumeToken: []byte("103"),
			},
			expected: false,
			reason:   "Events from health-events-analyzer should be filtered out to prevent infinite loops",
		},
		{
			name: "SHOULD NOT MATCH: DELETE operation",
			event: datastore.EventWithToken{
				Event: map[string]interface{}{
					"operationType": "delete",
					"fullDocument": map[string]interface{}{
						"healthevent": map[string]interface{}{
							"agent":     "gpu-health-monitor",
							"checkName": "GpuXidError",
							"isFatal":   true,
							"nodeName":  "kwok-node-23",
						},
					},
				},
				ResumeToken: []byte("104"),
			},
			expected: false,
			reason:   "DELETE operations should not match (only insert/update)",
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
				t.Errorf("MatchesEvent() = %v, expected %v\nReason: %s\nEvent: %+v",
					result, tt.expected, tt.reason, tt.event.Event)
			}
		})
	}
}

// TestMatchesEvent_RealWorldFailingScenario reproduces the exact failing scenario
// from TestFatalUnsupportedHealthEvent
func TestMatchesEvent_RealWorldFailingScenario(t *testing.T) {
	// The actual pipeline fault-remediation uses
	pipeline := []interface{}{
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("$or", datastore.A(
					datastore.D(
						datastore.E("operationType", "update"),
						datastore.E("$or", datastore.A(
							datastore.D(
								datastore.E("fullDocument.healtheventstatus.userpodsevictionstatus.status", datastore.D(
									datastore.E("$in", datastore.A("Succeeded", "AlreadyDrained")),
								)),
								datastore.E("fullDocument.healtheventstatus.nodequarantined", datastore.D(
									datastore.E("$in", datastore.A("Quarantined", "AlreadyQuarantined")),
								)),
							),
						)),
					),
				)),
			)),
		),
	}

	// The exact event that was incorrectly passing through the filter
	failingEvent := datastore.EventWithToken{
		Event: map[string]interface{}{
			"operationType": "insert",
			"fullDocument": map[string]interface{}{
				"healthevent": map[string]interface{}{
					"agent":            "health-events-analyzer",
					"checkName":        "MultipleRemediations",
					"componentClass":   "GPU",
					"recommendedAction": 5, // CONTACT_SUPPORT
					"isFatal":           true,
					"nodeName":          "kwok-node-29",
				},
				"healtheventstatus": map[string]interface{}{
					"faultremediated": nil,
					"nodequarantined": nil,
					"userpodsevictionstatus": map[string]interface{}{
						"status": "",
					},
				},
			},
		},
		ResumeToken: []byte("78"),
	}

	filter, err := NewPipelineFilter(pipeline)
	if err != nil {
		t.Fatalf("NewPipelineFilter() error = %v", err)
	}

	result := filter.MatchesEvent(failingEvent)
	if result {
		t.Errorf("MatchesEvent() = true, expected false\n"+
			"This event should have been FILTERED OUT because:\n"+
			"- nodequarantined is NULL (not in [Quarantined, AlreadyQuarantined])\n"+
			"- userpodsevictionstatus.status is empty string (not in [Succeeded, AlreadyDrained])\n"+
			"- This is the exact event that was causing TestFatalUnsupportedHealthEvent to fail")
	}
}

