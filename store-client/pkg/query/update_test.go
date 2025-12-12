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

package query

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpdateBuilder_Set_SimpleField(t *testing.T) {
	update := NewUpdate().Set("status", "active")

	// Test MongoDB output
	mongoUpdate := update.ToMongo()
	expectedMongo := map[string]interface{}{
		"$set": map[string]interface{}{
			"status": "active",
		},
	}
	assert.Equal(t, expectedMongo, mongoUpdate)

	// Test SQL output
	sql, args := update.ToSQL()
	assert.Equal(t, "document = jsonb_set(document, '{status}', $1::jsonb)", sql)
	assert.Equal(t, []interface{}{"\"active\""}, args)
}

func TestUpdateBuilder_Set_NestedField(t *testing.T) {
	update := NewUpdate().Set("healtheventstatus.nodequarantined", "Quarantined")

	// Test MongoDB output
	mongoUpdate := update.ToMongo()
	expectedMongo := map[string]interface{}{
		"$set": map[string]interface{}{
			"healtheventstatus.nodequarantined": "Quarantined",
		},
	}
	assert.Equal(t, expectedMongo, mongoUpdate)

	// Test SQL output
	sql, args := update.ToSQL()
	assert.Equal(t, "document = jsonb_set(document, '{healtheventstatus,nodequarantined}', $1::jsonb)", sql)
	assert.Equal(t, []interface{}{"\"Quarantined\""}, args)
}

func TestUpdateBuilder_Set_ColumnField(t *testing.T) {
	update := NewUpdate().Set("updatedAt", "2025-01-15T10:00:00Z")

	// Test MongoDB output
	mongoUpdate := update.ToMongo()
	expectedMongo := map[string]interface{}{
		"$set": map[string]interface{}{
			"updatedAt": "2025-01-15T10:00:00Z",
		},
	}
	assert.Equal(t, expectedMongo, mongoUpdate)

	// Test SQL output
	sql, args := update.ToSQL()
	assert.Equal(t, "updatedAt = $1", sql)
	assert.Equal(t, []interface{}{"2025-01-15T10:00:00Z"}, args)
}

func TestUpdateBuilder_SetMultiple_Fields(t *testing.T) {
	update := NewUpdate().
		Set("status", "active").
		Set("type", "critical")

	// Test MongoDB output
	mongoUpdate := update.ToMongo()
	setMap := mongoUpdate["$set"].(map[string]interface{})
	assert.Len(t, setMap, 2)
	assert.Equal(t, "active", setMap["status"])
	assert.Equal(t, "critical", setMap["type"])

	// Test SQL output
	sql, args := update.ToSQL()
	assert.Contains(t, sql, "document = jsonb_set(document, '{status}', $1::jsonb)")
	assert.Contains(t, sql, "document = jsonb_set(document, '{type}', $2::jsonb)")
	assert.Len(t, args, 2)
}

func TestUpdateBuilder_SetMultiple_Map(t *testing.T) {
	updates := map[string]interface{}{
		"status": "active",
		"count":  10,
	}
	update := NewUpdate().SetMultiple(updates)

	// Test MongoDB output
	mongoUpdate := update.ToMongo()
	setMap := mongoUpdate["$set"].(map[string]interface{})
	assert.Len(t, setMap, 2)
	assert.Equal(t, "active", setMap["status"])
	assert.Equal(t, 10, setMap["count"])

	// Test SQL output
	sql, args := update.ToSQL()
	assert.Contains(t, sql, "document = jsonb_set(document")
	assert.Len(t, args, 2)
}

func TestUpdateBuilder_Set_DeeplyNestedField(t *testing.T) {
	update := NewUpdate().Set("healtheventstatus.userpodsevictionstatus.status", "InProgress")

	// Test MongoDB output
	mongoUpdate := update.ToMongo()
	expectedMongo := map[string]interface{}{
		"$set": map[string]interface{}{
			"healtheventstatus.userpodsevictionstatus.status": "InProgress",
		},
	}
	assert.Equal(t, expectedMongo, mongoUpdate)

	// Test SQL output
	sql, args := update.ToSQL()
	assert.Equal(t, "document = jsonb_set(document, '{healtheventstatus,userpodsevictionstatus,status}', $1::jsonb)", sql)
	assert.Equal(t, []interface{}{"\"InProgress\""}, args)
}

func TestUpdateBuilder_EmptyUpdate(t *testing.T) {
	update := NewUpdate()

	// Test MongoDB output
	mongoUpdate := update.ToMongo()
	assert.Equal(t, map[string]interface{}{}, mongoUpdate)

	// Test SQL output
	sql, args := update.ToSQL()
	assert.Equal(t, "", sql)
	assert.Nil(t, args)
}

func TestUpdateBuilder_NilUpdate(t *testing.T) {
	var update *UpdateBuilder

	// Test MongoDB output
	mongoUpdate := update.ToMongo()
	assert.Equal(t, map[string]interface{}{}, mongoUpdate)

	// Test SQL output
	sql, args := update.ToSQL()
	assert.Equal(t, "", sql)
	assert.Nil(t, args)
}

func TestToJSONBValue(t *testing.T) {
	tests := []struct {
		name     string
		value    interface{}
		expected string
	}{
		{
			name:     "string value",
			value:    "active",
			expected: "\"active\"",
		},
		{
			name:     "boolean true",
			value:    true,
			expected: "true",
		},
		{
			name:     "boolean false",
			value:    false,
			expected: "false",
		},
		{
			name:     "integer",
			value:    42,
			expected: "42",
		},
		{
			name:     "float",
			value:    3.14,
			expected: "3.140000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := toJSONBValue(tt.value)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestMongoFieldToJSONBPath(t *testing.T) {
	tests := []struct {
		name         string
		mongoField   string
		expectedPath string
	}{
		{
			name:         "simple field",
			mongoField:   "status",
			expectedPath: "status",
		},
		{
			name:         "two levels",
			mongoField:   "healthevent.isfatal",
			expectedPath: "healthevent,isfatal",
		},
		{
			name:         "three levels",
			mongoField:   "healtheventstatus.userpodsevictionstatus.status",
			expectedPath: "healtheventstatus,userpodsevictionstatus,status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mongoFieldToJSONBPath(tt.mongoField)
			assert.Equal(t, tt.expectedPath, result)
		})
	}
}

func TestUpdateBuilder_RealWorldUpdates(t *testing.T) {
	t.Run("node-drainer status update", func(t *testing.T) {
		update := NewUpdate().Set("healtheventstatus.userpodsevictionstatus.status", "Succeeded")

		// MongoDB update should work
		mongoUpdate := update.ToMongo()
		require.NotNil(t, mongoUpdate)
		assert.Contains(t, mongoUpdate, "$set")

		// SQL should generate correctly
		sql, args := update.ToSQL()
		assert.Contains(t, sql, "jsonb_set")
		assert.Contains(t, sql, "healtheventstatus,userpodsevictionstatus,status")
		assert.Len(t, args, 1)
	})

	t.Run("fault-quarantine cancellation", func(t *testing.T) {
		update := NewUpdate().Set("healtheventstatus.nodequarantined", "Cancelled")

		// MongoDB update should work
		mongoUpdate := update.ToMongo()
		require.NotNil(t, mongoUpdate)
		assert.Contains(t, mongoUpdate, "$set")

		// SQL should generate correctly
		sql, args := update.ToSQL()
		assert.Contains(t, sql, "jsonb_set")
		assert.Contains(t, sql, "healtheventstatus,nodequarantined")
		assert.Equal(t, []interface{}{"\"Cancelled\""}, args)
	})

	t.Run("multiple field update", func(t *testing.T) {
		update := NewUpdate().
			Set("healtheventstatus.nodequarantined", "Quarantined").
			Set("healtheventstatus.userpodsevictionstatus.status", "InProgress")

		// MongoDB update should work
		mongoUpdate := update.ToMongo()
		require.NotNil(t, mongoUpdate)
		setMap := mongoUpdate["$set"].(map[string]interface{})
		assert.Len(t, setMap, 2)

		// SQL should generate correctly
		sql, args := update.ToSQL()
		assert.Contains(t, sql, "jsonb_set")
		assert.Len(t, args, 2)
	})
}

// TestToJSONBValue_ComplexTypes tests the critical fix for struct marshaling
// This test ensures that Go structs are properly marshaled to JSON instead of
// being formatted as strings like "{Succeeded }"
func TestToJSONBValue_ComplexTypes(t *testing.T) {
	// Define test struct matching node-drainer's OperationStatus
	type OperationStatus struct {
		Status  string `json:"status"`
		Message string `json:"message,omitempty"`
	}

	tests := []struct {
		name     string
		value    interface{}
		expected string
	}{
		{
			name: "struct with json tags - full fields",
			value: OperationStatus{
				Status:  "Succeeded",
				Message: "All pods evicted",
			},
			expected: `{"status":"Succeeded","message":"All pods evicted"}`,
		},
		{
			name: "struct with json tags - empty message",
			value: OperationStatus{
				Status:  "Succeeded",
				Message: "",
			},
			expected: `{"status":"Succeeded"}`,
		},
		{
			name: "struct with json tags - InProgress",
			value: OperationStatus{
				Status:  "InProgress",
				Message: "",
			},
			expected: `{"status":"InProgress"}`,
		},
		{
			name: "struct with json tags - Failed with message",
			value: OperationStatus{
				Status:  "Failed",
				Message: "Timeout waiting for pods",
			},
			expected: `{"status":"Failed","message":"Timeout waiting for pods"}`,
		},
		{
			name: "map[string]interface{}",
			value: map[string]interface{}{
				"status":  "Succeeded",
				"message": "Done",
			},
			expected: `{"message":"Done","status":"Succeeded"}`, // Note: map order may vary
		},
		{
			name:     "slice of strings",
			value:    []string{"a", "b", "c"},
			expected: `["a","b","c"]`,
		},
		{
			name:     "slice of integers",
			value:    []int{1, 2, 3},
			expected: `[1,2,3]`,
		},
		{
			name: "nested struct",
			value: struct {
				Name   string          `json:"name"`
				Status OperationStatus `json:"status"`
			}{
				Name: "test",
				Status: OperationStatus{
					Status:  "Succeeded",
					Message: "",
				},
			},
			expected: `{"name":"test","status":{"status":"Succeeded"}}`,
		},
		{
			name:     "nil value",
			value:    nil,
			expected: "null",
		},
		{
			name:     "empty map",
			value:    map[string]interface{}{},
			expected: `{}`,
		},
		{
			name:     "empty slice",
			value:    []string{},
			expected: `[]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := toJSONBValue(tt.value)

			// For maps, compare as JSON to handle key ordering
			if _, isMap := tt.value.(map[string]interface{}); isMap {
				var expectedJSON, resultJSON map[string]interface{}
				err := json.Unmarshal([]byte(tt.expected), &expectedJSON)
				require.NoError(t, err, "expected value should be valid JSON")
				err = json.Unmarshal([]byte(result), &resultJSON)
				require.NoError(t, err, "result should be valid JSON")
				assert.Equal(t, expectedJSON, resultJSON)
			} else {
				assert.Equal(t, tt.expected, result)
			}

			// Verify result is valid JSON (except for primitive types)
			switch tt.value.(type) {
			case string, bool, int, int32, int64, uint, uint32, uint64, float32, float64:
				// Primitive types may not be complete JSON documents
			default:
				var jsonCheck interface{}
				err := json.Unmarshal([]byte(result), &jsonCheck)
				assert.NoError(t, err, "result should be valid JSON")
			}
		})
	}
}

// TestToJSONBValue_RegressionTest_StructAsString tests the specific bug
// where structs were being formatted as strings like "{Succeeded }"
func TestToJSONBValue_RegressionTest_StructAsString(t *testing.T) {
	type OperationStatus struct {
		Status  string `json:"status"`
		Message string `json:"message,omitempty"`
	}

	// This is the exact scenario that was failing in CI
	status := OperationStatus{
		Status:  "Succeeded",
		Message: "",
	}

	result := toJSONBValue(status)

	// Should NOT be a string representation like "{Succeeded }" or "\"{Succeeded }\""
	assert.NotContains(t, result, "{Succeeded }", "should not format struct as string")
	assert.NotContains(t, result, "\\\"", "should not have escaped quotes")

	// Should be valid JSON object
	var parsed map[string]interface{}
	err := json.Unmarshal([]byte(result), &parsed)
	require.NoError(t, err, "should be valid JSON object")

	// Should have correct fields
	assert.Equal(t, "Succeeded", parsed["status"])
	assert.NotContains(t, parsed, "message") // omitempty should exclude empty string
}

// TestUpdateBuilder_Set_StructValue tests updating with a struct value
// This is the real-world scenario from node-drainer
func TestUpdateBuilder_Set_StructValue(t *testing.T) {
	type OperationStatus struct {
		Status  string `json:"status"`
		Message string `json:"message,omitempty"`
	}

	status := OperationStatus{
		Status:  "Succeeded",
		Message: "",
	}

	update := NewUpdate().Set("healtheventstatus.userpodsevictionstatus", status)

	// Test MongoDB output
	mongoUpdate := update.ToMongo()
	setMap := mongoUpdate["$set"].(map[string]interface{})
	assert.Contains(t, setMap, "healtheventstatus.userpodsevictionstatus")

	statusValue := setMap["healtheventstatus.userpodsevictionstatus"]
	assert.IsType(t, OperationStatus{}, statusValue, "should preserve struct type for MongoDB")

	// Test SQL output
	sql, args := update.ToSQL()
	assert.Contains(t, sql, "jsonb_set")
	assert.Contains(t, sql, "healtheventstatus,userpodsevictionstatus")
	require.Len(t, args, 1)

	// The argument should be valid JSON object, not a string like "{Succeeded }"
	argStr := args[0].(string)
	assert.NotContains(t, argStr, "{Succeeded }", "should not format struct as string")

	var parsed map[string]interface{}
	err := json.Unmarshal([]byte(argStr), &parsed)
	require.NoError(t, err, "SQL argument should be valid JSON")
	assert.Equal(t, "Succeeded", parsed["status"])
}

// TestUpdateBuilder_Set_MapValue tests updating with a map value
func TestUpdateBuilder_Set_MapValue(t *testing.T) {
	statusMap := map[string]interface{}{
		"status":  "InProgress",
		"message": "Evicting pods",
	}

	update := NewUpdate().Set("healtheventstatus.userpodsevictionstatus", statusMap)

	// Test MongoDB output
	mongoUpdate := update.ToMongo()
	setMap := mongoUpdate["$set"].(map[string]interface{})
	assert.Contains(t, setMap, "healtheventstatus.userpodsevictionstatus")

	// Test SQL output
	sql, args := update.ToSQL()
	assert.Contains(t, sql, "jsonb_set")
	require.Len(t, args, 1)

	// The argument should be valid JSON object
	argStr := args[0].(string)
	var parsed map[string]interface{}
	err := json.Unmarshal([]byte(argStr), &parsed)
	require.NoError(t, err, "SQL argument should be valid JSON")
	assert.Equal(t, "InProgress", parsed["status"])
	assert.Equal(t, "Evicting pods", parsed["message"])
}

// TestUpdateBuilder_Set_SliceValue tests updating with a slice value
func TestUpdateBuilder_Set_SliceValue(t *testing.T) {
	events := []string{"event1", "event2", "event3"}

	update := NewUpdate().Set("recentEvents", events)

	// Test SQL output
	sql, args := update.ToSQL()
	assert.Contains(t, sql, "jsonb_set")
	require.Len(t, args, 1)

	// The argument should be valid JSON array
	argStr := args[0].(string)
	var parsed []string
	err := json.Unmarshal([]byte(argStr), &parsed)
	require.NoError(t, err, "SQL argument should be valid JSON array")
	assert.Equal(t, events, parsed)
}

// TestUpdateBuilder_RealCICorruptionScenario reproduces the exact data corruption
// seen in CI where userpodsevictionstatus was written as "{Succeeded }" instead of proper JSON
// This is a regression test based on the actual failure from CI logs
func TestUpdateBuilder_RealCICorruptionScenario(t *testing.T) {
	type OperationStatus struct {
		Status  string `json:"status"`
		Message string `json:"message,omitempty"`
	}

	// This is the exact scenario from CI logs:
	// node-drainer sets userpodsevictionstatus after completing pod eviction
	status := OperationStatus{
		Status:  "Succeeded",
		Message: "", // Empty message, omitempty should exclude it
	}

	// Create the update that node-drainer would create
	update := NewUpdate().Set("healtheventstatus.userpodsevictionstatus", status)

	// Test SQL output (PostgreSQL path)
	sql, args := update.ToSQL()
	assert.Contains(t, sql, "document = jsonb_set(document, '{healtheventstatus,userpodsevictionstatus}', $1::jsonb)")
	require.Len(t, args, 1)

	argStr := args[0].(string)

	// CRITICAL: Before the fix, this would be:
	//   "{Succeeded }"  (string representation of struct)
	// After the fix, it should be:
	//   {"status":"Succeeded"}  (proper JSON object)

	// Verify it's NOT the corrupted format
	assert.NotEqual(t, "\"{Succeeded }\"", argStr, "BUG: struct formatted as string!")
	assert.NotContains(t, argStr, "{Succeeded }", "BUG: struct toString() used instead of JSON!")

	// Verify it IS proper JSON
	var parsed map[string]interface{}
	err := json.Unmarshal([]byte(argStr), &parsed)
	require.NoError(t, err, "MUST be valid JSON object, not string")

	// Verify the structure matches what the pipeline filter expects
	assert.Equal(t, "Succeeded", parsed["status"], "status field must be accessible")
	assert.NotContains(t, parsed, "message", "empty message should be omitted (omitempty)")

	// This is what the OLD (buggy) code would have produced:
	oldBuggyFormat := fmt.Sprintf("\"%v\"", status) // Results in "{Succeeded }"
	assert.NotEqual(t, oldBuggyFormat, argStr, "Should NOT match old buggy format")

	// Additional verification: simulate what PostgreSQL stores and what the pipeline reads
	t.Run("verify_pipeline_can_read_status", func(t *testing.T) {
		// After jsonb_set, PostgreSQL stores: {"status": "Succeeded"}
		// The pipeline filter needs to evaluate: userpodsevictionstatus.status == "Succeeded"

		// Parse what would be stored in the database
		var storedData map[string]interface{}
		err := json.Unmarshal([]byte(argStr), &storedData)
		require.NoError(t, err)

		// The pipeline filter should be able to access nested fields
		statusValue, exists := storedData["status"]
		assert.True(t, exists, "status field must exist for pipeline $match")
		assert.Equal(t, "Succeeded", statusValue, "status must equal 'Succeeded'")

		// With the OLD buggy format "{Succeeded }", this would fail:
		// storedData would be the string "{Succeeded }" itself, not a map
		// There would be no "status" field accessible
	})

	t.Run("compare_with_corrupted_data", func(t *testing.T) {
		// What the OLD code produced (the bug):
		corruptedValue := "\"{Succeeded }\""

		// Try to parse it as JSON object - this would FAIL or give wrong result
		var attempt map[string]interface{}
		_ = json.Unmarshal([]byte(corruptedValue), &attempt)

		// With the old format, either:
		// 1. Unmarshal succeeds and gives us a STRING value (not a map)
		// 2. Or it fails to parse

		// The corrupted format would be stored as a JSON string in PostgreSQL:
		// "userpodsevictionstatus": "{Succeeded }"
		// This means the pipeline filter trying to access .status would fail

		// Verify our fix produces different output
		assert.NotEqual(t, corruptedValue, argStr, "Fixed version must differ from corrupted")
	})

	t.Run("mongodb_compatibility", func(t *testing.T) {
		// Verify MongoDB path still works correctly
		mongoUpdate := update.ToMongo()
		setMap := mongoUpdate["$set"].(map[string]interface{})

		statusValue := setMap["healtheventstatus.userpodsevictionstatus"]
		assert.IsType(t, OperationStatus{}, statusValue, "MongoDB should get struct directly")

		// MongoDB can handle the struct natively
		typedStatus := statusValue.(OperationStatus)
		assert.Equal(t, "Succeeded", typedStatus.Status)
	})
}
