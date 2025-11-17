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

package factory

import (
	"testing"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertToMongoPipeline(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    interface{}
		expectError bool
		description string
	}{
		{
			name:        "datastore.Pipeline conversion",
			pipeline:    datastore.Pipeline{},
			expectError: false,
			description: "Should convert datastore.Pipeline using ConvertAgnosticPipelineToMongo",
		},
		{
			name: "[]interface{} pipeline conversion (from NewPipelineBuilder)",
			pipeline: []interface{}{
				map[string]interface{}{
					"$match": map[string]interface{}{
						"operationType": "insert",
					},
				},
			},
			expectError: false,
			description: "Should convert []interface{} pipeline created by client.NewPipelineBuilder() to MongoDB format",
		},
		{
			name: "[]interface{} with invalid stage type",
			pipeline: []interface{}{
				"invalid-stage", // Not a map[string]interface{}
			},
			expectError: true,
			description: "Should return error for invalid stage types in []interface{} pipeline",
		},
		{
			name:        "Already MongoDB pipeline (backward compatibility)",
			pipeline:    map[string]interface{}{"$match": map[string]interface{}{"field": "value"}},
			expectError: false,
			description: "Should pass through already MongoDB-compatible pipelines unchanged",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := convertToMongoPipeline(tt.pipeline)

			if tt.expectError {
				assert.Error(t, err, tt.description)
				assert.Nil(t, result)
			} else {
				assert.NoError(t, err, tt.description)
				assert.NotNil(t, result)

				// For []interface{} conversion, verify the result format
				if originalSlice, ok := tt.pipeline.([]interface{}); ok {
					resultSlice, ok := result.([]map[string]interface{})
					require.True(t, ok, "Result should be []map[string]interface{}")
					assert.Len(t, resultSlice, len(originalSlice), "Result should have same length as input")
				}
			}
		})
	}
}

func TestConvertToMongoPipeline_Integration(t *testing.T) {
	// Test the real-world scenario from fault-remediation
	t.Run("fault-remediation pipeline integration", func(t *testing.T) {
		// Simulate what fault-remediation does:
		// client.BuildChangeStreamPipeline(matchCondition)

		// Build a filter like fault-remediation does
		matchCondition := client.NewFilterBuilder().
			Eq("operationType", "update").
			Build()

		// Build a pipeline like fault-remediation does (manually)
		pipeline := []interface{}{
			map[string]interface{}{
				"$match": matchCondition,
			},
		}

		// This should not fail in convertToMongoPipeline
		result, err := convertToMongoPipeline(pipeline)

		require.NoError(t, err, "Pipeline conversion should succeed for fault-remediation-style pipeline")
		require.NotNil(t, result, "Result should not be nil")

		// Verify the result is in the expected format for MongoDB
		resultSlice, ok := result.([]map[string]interface{})
		require.True(t, ok, "Result should be []map[string]interface{} for MongoDB compatibility")
		require.Len(t, resultSlice, 1, "Should have one stage (the $match stage)")

		// Verify the stage structure
		stage := resultSlice[0]
		require.Contains(t, stage, "$match", "Stage should contain $match operator")
	})
}
