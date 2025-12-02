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

package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractDocumentID(t *testing.T) {
	tests := []struct {
		name        string
		event       map[string]interface{}
		expectedID  string
		expectError bool
	}{
		{
			name: "PostgreSQL changestream event with fullDocument",
			event: map[string]interface{}{
				"_id": map[string]interface{}{
					"_data": "219", // Resume token, should be ignored
				},
				"operationType": "update",
				"fullDocument": map[string]interface{}{
					"id":         "79e4bbe9-1fb4-43d3-be8a-0e40c197c332",
					"healthevent": map[string]interface{}{
						"nodeName": "test-node",
					},
				},
			},
			expectedID:  "79e4bbe9-1fb4-43d3-be8a-0e40c197c332",
			expectError: false,
		},
		{
			name: "PostgreSQL changestream INSERT event",
			event: map[string]interface{}{
				"_id": map[string]interface{}{
					"_data": "123",
				},
				"operationType": "insert",
				"fullDocument": map[string]interface{}{
					"id":        "a1b2c3d4-e5f6-4789-0123-456789abcdef",
					"createdAt": "2025-11-22T00:00:00Z",
				},
			},
			expectedID:  "a1b2c3d4-e5f6-4789-0123-456789abcdef",
			expectError: false,
		},
		{
			name: "MongoDB changestream event with ObjectID in fullDocument",
			event: map[string]interface{}{
				"_id": map[string]interface{}{
					"_data": "somebase64token",
				},
				"operationType": "update",
				"fullDocument": map[string]interface{}{
					"_id":  "507f1f77bcf86cd799439011",
					"name": "test",
				},
			},
			expectedID:  "507f1f77bcf86cd799439011",
			expectError: false,
		},
		{
			name: "MongoDB direct query with simple _id",
			event: map[string]interface{}{
				"_id":  "507f1f77bcf86cd799439011",
				"name": "test",
			},
			expectedID:  "507f1f77bcf86cd799439011",
			expectError: false,
		},
		{
			name: "PostgreSQL direct query with top-level id",
			event: map[string]interface{}{
				"id":        "12345678-1234-1234-1234-123456789abc",
				"createdAt": "2025-11-22T00:00:00Z",
			},
			expectedID:  "12345678-1234-1234-1234-123456789abc",
			expectError: false,
		},
		{
			name: "Event with no ID fields",
			event: map[string]interface{}{
				"operationType": "delete",
				"deletedCount":  1,
			},
			expectedID:  "",
			expectError: true,
		},
		{
			name: "Event with only resume token (should fail)",
			event: map[string]interface{}{
				"_id": map[string]interface{}{
					"_data": "456",
				},
				"operationType": "update",
			},
			expectedID:  "",
			expectError: true,
		},
		{
			name: "MongoDB changestream with nested _id map (not a resume token)",
			event: map[string]interface{}{
				"_id": map[string]interface{}{
					"subfield": "value", // Not a _data field, so it's a valid ID
				},
				"operationType": "insert",
			},
			expectedID:  "map[subfield:value]",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := ExtractDocumentID(tt.event)

			if tt.expectError {
				assert.Error(t, err, "Expected error for event: %v", tt.event)
				assert.Empty(t, id, "Expected empty ID on error")
			} else {
				require.NoError(t, err, "Unexpected error for event: %v", tt.event)
				assert.Equal(t, tt.expectedID, id, "ID mismatch")
			}
		})
	}
}

func TestExtractDocumentID_PostgreSQLResumeTokenRegression(t *testing.T) {
	// This is a regression test for the bug where PostgreSQL changestream
	// resume tokens in event["_id"]["_data"] were being incorrectly used as
	// document IDs, resulting in invalid CR names like "map[_data:219]"
	event := map[string]interface{}{
		"_id": map[string]interface{}{
			"_data": "219", // Resume token
		},
		"clusterTime":   "2025-11-22T04:22:13.73271Z",
		"operationType": "update",
		"fullDocument": map[string]interface{}{
			"RawEvent":  nil,
			"createdAt": "2025-11-22T04:22:13.299021864Z",
			"healthevent": map[string]interface{}{
				"agent":       "gpu-health-monitor",
				"checkName":   "GpuXidError",
				"nodeName":    "kwok-kata-test-node-0",
				"isFatal":     true,
				"message":     "XID 79 fatal error",
				"errorCode":   []interface{}{"79"},
				"componentClass": "GPU",
			},
			"healtheventstatus": map[string]interface{}{
				"nodequarantined": "Quarantined",
				"userpodsevictionstatus": map[string]interface{}{
					"status": "InProgress",
				},
			},
			"id": "79e4bbe9-1fb4-43d3-be8a-0e40c197c332",
		},
		"updateDescription": map[string]interface{}{
			"updatedFields": map[string]interface{}{
				"healtheventstatus.userpodsevictionstatus.status": "InProgress",
			},
		},
	}

	id, err := ExtractDocumentID(event)

	require.NoError(t, err, "Should extract ID from fullDocument")
	assert.Equal(t, "79e4bbe9-1fb4-43d3-be8a-0e40c197c332", id,
		"Should use fullDocument.id, not _id._data resume token")

	// Verify the ID would be valid in a Kubernetes CR name
	assert.NotContains(t, id, "map[", "ID should not contain 'map[' prefix")
	assert.NotContains(t, id, "_data", "ID should not contain '_data' field")
}

func TestExtractDocumentID_MongoDBBackwardsCompatibility(t *testing.T) {
	// Ensure we don't break MongoDB changestreams that use _id properly
	event := map[string]interface{}{
		"_id": map[string]interface{}{
			"_data": "base64encodedtoken",
		},
		"operationType": "insert",
		"fullDocument": map[string]interface{}{
			"_id":     "507f1f77bcf86cd799439011",
			"field1":  "value1",
		},
	}

	id, err := ExtractDocumentID(event)

	require.NoError(t, err)
	assert.Equal(t, "507f1f77bcf86cd799439011", id,
		"Should extract MongoDB ObjectID from fullDocument._id")
}
