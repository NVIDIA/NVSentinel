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

package client

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMongoDBPipelineBuilder_NodeQuarantineStatusPipeline(t *testing.T) {
	builder := NewMongoDBPipelineBuilder()
	pipeline := builder.BuildNodeQuarantineStatusPipeline()

	// Verify pipeline structure exists
	require.NotNil(t, pipeline)
	require.NotEmpty(t, pipeline)

	// Verify it has a $match stage with operationType=update
	// The actual filter testing is done in integration tests to avoid import cycles
	assert.Len(t, pipeline, 1, "Pipeline should have 1 stage")
}

func TestPostgreSQLPipelineBuilder_NodeQuarantineStatusPipeline(t *testing.T) {
	builder := NewPostgreSQLPipelineBuilder()
	pipeline := builder.BuildNodeQuarantineStatusPipeline()

	// Verify pipeline structure exists
	require.NotNil(t, pipeline)
	require.NotEmpty(t, pipeline)

	// Verify it has a $match stage with $or (supporting both INSERT and UPDATE)
	// The actual filter testing is done in integration tests to avoid import cycles
	assert.Len(t, pipeline, 1, "Pipeline should have 1 stage")
}

func TestAllHealthEventInsertsPipeline(t *testing.T) {
	testCases := []struct {
		name    string
		builder PipelineBuilder
	}{
		{"MongoDB", NewMongoDBPipelineBuilder()},
		{"PostgreSQL", NewPostgreSQLPipelineBuilder()},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pipeline := tc.builder.BuildAllHealthEventInsertsPipeline()
			require.NotNil(t, pipeline)
			require.NotEmpty(t, pipeline)
			assert.Len(t, pipeline, 1, "Pipeline should have 1 stage")
		})
	}
}

func TestNonFatalUnhealthyInsertsPipeline(t *testing.T) {
	testCases := []struct {
		name    string
		builder PipelineBuilder
	}{
		{"MongoDB", NewMongoDBPipelineBuilder()},
		{"PostgreSQL", NewPostgreSQLPipelineBuilder()},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pipeline := tc.builder.BuildNonFatalUnhealthyInsertsPipeline()
			require.NotNil(t, pipeline)
			require.NotEmpty(t, pipeline)
			assert.Len(t, pipeline, 1, "Pipeline should have 1 stage")
		})
	}
}

func TestQuarantinedAndDrainedNodesPipeline(t *testing.T) {
	testCases := []struct {
		name    string
		builder PipelineBuilder
	}{
		{"MongoDB", NewMongoDBPipelineBuilder()},
		{"PostgreSQL", NewPostgreSQLPipelineBuilder()},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pipeline := tc.builder.BuildQuarantinedAndDrainedNodesPipeline()
			require.NotNil(t, pipeline)
			require.NotEmpty(t, pipeline)
			assert.Len(t, pipeline, 1, "Pipeline should have 1 stage")
		})
	}
}

func TestGetPipelineBuilder_MongoDB(t *testing.T) {
	t.Setenv("DATASTORE_PROVIDER", "mongodb")

	builder := GetPipelineBuilder()
	require.NotNil(t, builder)

	// Verify it's the MongoDB implementation
	_, ok := builder.(*MongoDBPipelineBuilder)
	assert.True(t, ok, "Should return MongoDBPipelineBuilder for mongodb provider")
}

func TestGetPipelineBuilder_PostgreSQL(t *testing.T) {
	t.Setenv("DATASTORE_PROVIDER", "postgresql")

	builder := GetPipelineBuilder()
	require.NotNil(t, builder)

	// Verify it's the PostgreSQL implementation
	_, ok := builder.(*PostgreSQLPipelineBuilder)
	assert.True(t, ok, "Should return PostgreSQLPipelineBuilder for postgresql provider")
}

func TestGetPipelineBuilder_Default(t *testing.T) {
	t.Setenv("DATASTORE_PROVIDER", "")

	builder := GetPipelineBuilder()
	require.NotNil(t, builder)

	// Default should be MongoDB
	_, ok := builder.(*MongoDBPipelineBuilder)
	assert.True(t, ok, "Should return MongoDBPipelineBuilder by default")
}

func TestGetPipelineBuilder_UnknownProvider(t *testing.T) {
	t.Setenv("DATASTORE_PROVIDER", "unknown")

	builder := GetPipelineBuilder()
	require.NotNil(t, builder)

	// Unknown provider should fallback to MongoDB
	_, ok := builder.(*MongoDBPipelineBuilder)
	assert.True(t, ok, "Should fallback to MongoDBPipelineBuilder for unknown provider")
}

// TestBackwardCompatibility verifies that the deprecated functions still work
func TestBackwardCompatibility(t *testing.T) {
	// Test that deprecated functions still call the new interface
	t.Run("BuildNodeQuarantineStatusUpdatesPipeline", func(t *testing.T) {
		pipeline := BuildNodeQuarantineStatusUpdatesPipeline()
		require.NotNil(t, pipeline)
		require.NotEmpty(t, pipeline)
	})

	t.Run("BuildAllHealthEventInsertsPipeline", func(t *testing.T) {
		pipeline := BuildAllHealthEventInsertsPipeline()
		require.NotNil(t, pipeline)
		require.NotEmpty(t, pipeline)
	})

	t.Run("BuildNonFatalUnhealthyInsertsPipeline", func(t *testing.T) {
		pipeline := BuildNonFatalUnhealthyInsertsPipeline()
		require.NotNil(t, pipeline)
		require.NotEmpty(t, pipeline)
	})

	t.Run("BuildQuarantinedAndDrainedNodesPipeline", func(t *testing.T) {
		pipeline := BuildQuarantinedAndDrainedNodesPipeline()
		require.NotNil(t, pipeline)
		require.NotEmpty(t, pipeline)
	})
}
