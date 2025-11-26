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
	"encoding/json"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// TestHealthEventStatusSerialization verifies that HealthEventStatus serializes
// correctly with both JSON (PostgreSQL) and BSON (MongoDB), especially handling
// nil pointer fields consistently.
//
// This test ensures that the fix for nil NodeQuarantined works correctly:
// - With omitempty tags, nil pointers are omitted during serialization
// - After deserialization, nil pointers remain nil (not becoming explicit nulls)
// - Both JSON and BSON behave consistently
func TestHealthEventStatusSerialization(t *testing.T) {
	tests := []struct {
		name              string
		status            model.HealthEventStatus
		expectJSONOmit    []string // Fields that should be omitted in JSON
		expectBSONOmit    []string // Fields that should be omitted in BSON
		expectJSONInclude []string // Fields that should be included in JSON
		expectBSONInclude []string // Fields that should be included in BSON
	}{
		{
			name: "nil NodeQuarantined - should be omitted in both JSON and BSON",
			status: model.HealthEventStatus{
				NodeQuarantined: nil,
				UserPodsEvictionStatus: model.OperationStatus{
					Status:  model.StatusNotStarted,
					Message: "test",
				},
				FaultRemediated:          nil,
				LastRemediationTimestamp: nil,
			},
			expectJSONOmit:    []string{"nodequarantined", "faultremediated", "lastremediationtimestamp"},
			expectJSONInclude: []string{"userpodsevictionstatus"},
			expectBSONInclude: []string{"userpodsevictionstatus"},
		},
		{
			name: "non-nil NodeQuarantined - should be included",
			status: model.HealthEventStatus{
				NodeQuarantined: func() *model.Status { s := model.Quarantined; return &s }(),
				UserPodsEvictionStatus: model.OperationStatus{
					Status: model.StatusInProgress,
				},
				FaultRemediated: func() *bool { b := true; return &b }(),
			},
			expectJSONInclude: []string{"nodequarantined", "userpodsevictionstatus", "faultremediated"},
			expectBSONInclude: []string{"nodequarantined", "userpodsevictionstatus", "faultremediated"},
		},
		{
			name: "all fields populated",
			status: model.HealthEventStatus{
				NodeQuarantined: func() *model.Status { s := model.UnQuarantined; return &s }(),
				UserPodsEvictionStatus: model.OperationStatus{
					Status:  model.StatusSucceeded,
					Message: "completed",
				},
				FaultRemediated: func() *bool { b := false; return &b }(),
				LastRemediationTimestamp: func() *time.Time {
					t := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
					return &t
				}(),
			},
			expectJSONInclude: []string{
				"nodequarantined",
				"userpodsevictionstatus",
				"faultremediated",
				"lastremediationtimestamp",
			},
			expectBSONInclude: []string{
				"nodequarantined",
				"userpodsevictionstatus",
				"faultremediated",
				"lastremediationtimestamp",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test JSON serialization (PostgreSQL)
			t.Run("JSON_PostgreSQL", func(t *testing.T) {
				jsonData, err := json.Marshal(tt.status)
				require.NoError(t, err, "JSON marshal should succeed")

				var jsonMap map[string]interface{}
				err = json.Unmarshal(jsonData, &jsonMap)
				require.NoError(t, err, "JSON unmarshal to map should succeed")

				// Check omitted fields
				for _, field := range tt.expectJSONOmit {
					assert.NotContains(t, jsonMap, field,
						"Field %s should be omitted from JSON", field)
				}

				// Check included fields
				for _, field := range tt.expectJSONInclude {
					assert.Contains(t, jsonMap, field,
						"Field %s should be included in JSON", field)
				}

				// Verify round-trip: unmarshal and check nil fields remain nil
				var unmarshaled model.HealthEventStatus
				err = json.Unmarshal(jsonData, &unmarshaled)
				require.NoError(t, err, "JSON unmarshal should succeed")

				// For nil pointer fields, both should be nil after round-trip
				if tt.status.NodeQuarantined == nil {
					assert.Nil(t, unmarshaled.NodeQuarantined,
						"NodeQuarantined should remain nil after JSON round-trip")
				} else {
					require.NotNil(t, unmarshaled.NodeQuarantined)
					assert.Equal(t, *tt.status.NodeQuarantined, *unmarshaled.NodeQuarantined)
				}

				if tt.status.FaultRemediated == nil {
					assert.Nil(t, unmarshaled.FaultRemediated,
						"FaultRemediated should remain nil after JSON round-trip")
				} else {
					require.NotNil(t, unmarshaled.FaultRemediated)
					assert.Equal(t, *tt.status.FaultRemediated, *unmarshaled.FaultRemediated)
				}
			})

			// Test BSON serialization (MongoDB)
			t.Run("BSON_MongoDB", func(t *testing.T) {
				bsonData, err := bson.Marshal(tt.status)
				require.NoError(t, err, "BSON marshal should succeed")

				var bsonMap bson.M
				err = bson.Unmarshal(bsonData, &bsonMap)
				require.NoError(t, err, "BSON unmarshal to map should succeed")

				// Check omitted fields
				for _, field := range tt.expectBSONOmit {
					assert.NotContains(t, bsonMap, field,
						"Field %s should be omitted from BSON", field)
				}

				// Check included fields
				for _, field := range tt.expectBSONInclude {
					assert.Contains(t, bsonMap, field,
						"Field %s should be included in BSON", field)
				}

				// Verify round-trip: unmarshal and check nil fields remain nil
				var unmarshaled model.HealthEventStatus
				err = bson.Unmarshal(bsonData, &unmarshaled)
				require.NoError(t, err, "BSON unmarshal should succeed")

				// For nil pointer fields, both should be nil after round-trip
				if tt.status.NodeQuarantined == nil {
					assert.Nil(t, unmarshaled.NodeQuarantined,
						"NodeQuarantined should remain nil after BSON round-trip")
				} else {
					require.NotNil(t, unmarshaled.NodeQuarantined)
					assert.Equal(t, *tt.status.NodeQuarantined, *unmarshaled.NodeQuarantined)
				}

				if tt.status.FaultRemediated == nil {
					assert.Nil(t, unmarshaled.FaultRemediated,
						"FaultRemediated should remain nil after BSON round-trip")
				} else {
					require.NotNil(t, unmarshaled.FaultRemediated)
					assert.Equal(t, *tt.status.FaultRemediated, *unmarshaled.FaultRemediated)
				}
			})
		})
	}
}

// TestJSONAndBSONConsistency verifies that JSON (PostgreSQL) and BSON (MongoDB)
// produce consistent results for nil pointer fields.
//
// This is critical because:
// - fault-remediation checks if NodeQuarantined is nil
// - Without omitempty, JSON would serialize nil as {"nodequarantined": null}
// - When deserialized, this would remain as nil pointer (not the same as omitted field)
// - This test ensures both backends behave identically
func TestJSONAndBSONConsistency(t *testing.T) {
	status := model.HealthEventStatus{
		NodeQuarantined: nil, // This is the key test case - nil should behave the same
		UserPodsEvictionStatus: model.OperationStatus{
			Status:  model.StatusNotStarted,
			Message: "",
		},
		FaultRemediated:          nil,
		LastRemediationTimestamp: nil,
	}

	// Marshal and unmarshal with JSON (PostgreSQL path)
	jsonData, err := json.Marshal(status)
	require.NoError(t, err)

	var jsonUnmarshaled model.HealthEventStatus
	err = json.Unmarshal(jsonData, &jsonUnmarshaled)
	require.NoError(t, err)

	// Marshal and unmarshal with BSON (MongoDB path)
	bsonData, err := bson.Marshal(status)
	require.NoError(t, err)

	var bsonUnmarshaled model.HealthEventStatus
	err = bson.Unmarshal(bsonData, &bsonUnmarshaled)
	require.NoError(t, err)

	// Both should have nil NodeQuarantined after round-trip
	assert.Nil(t, jsonUnmarshaled.NodeQuarantined,
		"JSON: NodeQuarantined should be nil after round-trip")
	assert.Nil(t, bsonUnmarshaled.NodeQuarantined,
		"BSON: NodeQuarantined should be nil after round-trip")

	// Both should have nil FaultRemediated after round-trip
	assert.Nil(t, jsonUnmarshaled.FaultRemediated,
		"JSON: FaultRemediated should be nil after round-trip")
	assert.Nil(t, bsonUnmarshaled.FaultRemediated,
		"BSON: FaultRemediated should be nil after round-trip")

	// Verify the serialized data doesn't contain the nil fields
	var jsonMap map[string]interface{}
	err = json.Unmarshal(jsonData, &jsonMap)
	require.NoError(t, err)
	assert.NotContains(t, jsonMap, "nodequarantined",
		"JSON should not contain nodequarantined field when nil")
	assert.NotContains(t, jsonMap, "faultremediated",
		"JSON should not contain faultremediated field when nil")
}
