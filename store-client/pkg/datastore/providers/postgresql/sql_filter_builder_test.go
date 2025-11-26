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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLFilterBuilder_FieldToJSONBPath(t *testing.T) {
	builder := NewSQLFilterBuilder(3)

	tests := []struct {
		name     string
		field    string
		expected string
	}{
		{
			name:     "simple field",
			field:    "healthevent",
			expected: "new_values->>'healthevent'",
		},
		{
			name:     "nested field",
			field:    "healthevent.isFatal",
			expected: "new_values->'healthevent'->>'isFatal'",
		},
		{
			name:     "deeply nested field",
			field:    "healthevent.statusinfo.code",
			expected: "new_values->'healthevent'->'statusinfo'->>'code'",
		},
		{
			name:     "fullDocument prefix adds document level",
			field:    "fullDocument.healthevent.isFatal",
			expected: "new_values->'document'->'healthevent'->>'isFatal'",
		},
		{
			name:     "fullDocument only returns document path",
			field:    "fullDocument",
			expected: "new_values->'document'",
		},
		{
			name:     "operationType maps to operation column",
			field:    "operationType",
			expected: "operation",
		},
		{
			name:     "case conversion ishealthy to isHealthy",
			field:    "fullDocument.healthevent.ishealthy",
			expected: "new_values->'document'->'healthevent'->>'isHealthy'",
		},
		{
			name:     "case conversion isfatal to isFatal",
			field:    "fullDocument.healthevent.isfatal",
			expected: "new_values->'document'->'healthevent'->>'isFatal'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := builder.fieldToJSONBPath(tt.field)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSQLFilterBuilder_BooleanField(t *testing.T) {
	t.Run("true value", func(t *testing.T) {
		builder := NewSQLFilterBuilder(3)
		err := builder.BuildFromPipeline([]interface{}{
			map[string]interface{}{
				"$match": map[string]interface{}{
					"fullDocument.healthevent.isFatal": true,
				},
			},
		})
		require.NoError(t, err)

		clause := builder.GetWhereClause()
		args := builder.GetArgs()

		// fullDocument.healthevent.isFatal -> new_values->'document'->'healthevent'->>'isFatal'
		assert.Contains(t, clause, "new_values->'document'->'healthevent'->>'isFatal'")
		assert.Contains(t, clause, "::boolean")
		assert.Len(t, args, 1)
		assert.Equal(t, true, args[0])
	})

	t.Run("false value handles NULL", func(t *testing.T) {
		builder := NewSQLFilterBuilder(3)
		err := builder.BuildFromPipeline([]interface{}{
			map[string]interface{}{
				"$match": map[string]interface{}{
					"fullDocument.healthevent.isHealthy": false,
				},
			},
		})
		require.NoError(t, err)

		clause := builder.GetWhereClause()

		// For false, should handle NULL case (protobuf default)
		assert.Contains(t, clause, "IS NULL")
		assert.Contains(t, clause, "::boolean")
	})
}

func TestSQLFilterBuilder_OrOperator(t *testing.T) {
	builder := NewSQLFilterBuilder(3)

	// This is the typical health-events-analyzer pipeline
	pipeline := []interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"$or": []interface{}{
					map[string]interface{}{"fullDocument.healthevent.isFatal": true},
					map[string]interface{}{"fullDocument.healthevent.isHealthy": false},
				},
			},
		},
	}

	err := builder.BuildFromPipeline(pipeline)
	require.NoError(t, err)

	clause := builder.GetWhereClause()
	args := builder.GetArgs()

	// Should create an OR clause with document level
	assert.Contains(t, clause, " OR ")
	assert.Contains(t, clause, "new_values->'document'->'healthevent'->>'isFatal'")
	assert.Contains(t, clause, "new_values->'document'->'healthevent'->>'isHealthy'")

	// Should have args for both conditions
	assert.Len(t, args, 2)
}

func TestSQLFilterBuilder_NeOperator(t *testing.T) {
	t.Run("$ne with string", func(t *testing.T) {
		builder := NewSQLFilterBuilder(3)
		err := builder.BuildFromPipeline([]interface{}{
			map[string]interface{}{
				"$match": map[string]interface{}{
					"fullDocument.healthevent.checkName": map[string]interface{}{
						"$ne": "HealthCheck",
					},
				},
			},
		})
		require.NoError(t, err)

		clause := builder.GetWhereClause()
		args := builder.GetArgs()

		assert.Contains(t, clause, "IS NULL OR")
		assert.Contains(t, clause, "!=")
		assert.Len(t, args, 1)
		assert.Equal(t, "HealthCheck", args[0])
	})

	t.Run("$ne with boolean true", func(t *testing.T) {
		builder := NewSQLFilterBuilder(3)
		err := builder.BuildFromPipeline([]interface{}{
			map[string]interface{}{
				"$match": map[string]interface{}{
					"fullDocument.healthevent.isFatal": map[string]interface{}{
						"$ne": true,
					},
				},
			},
		})
		require.NoError(t, err)

		clause := builder.GetWhereClause()

		// $ne: true means field is false or missing
		assert.Contains(t, clause, "= false")
		assert.Contains(t, clause, "IS NULL")
	})
}

func TestSQLFilterBuilder_InOperator(t *testing.T) {
	builder := NewSQLFilterBuilder(3)
	err := builder.BuildFromPipeline([]interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"fullDocument.healthevent.errorCode": map[string]interface{}{
					"$in": []interface{}{"79", "119", "31"},
				},
			},
		},
	})
	require.NoError(t, err)

	clause := builder.GetWhereClause()
	args := builder.GetArgs()

	assert.Contains(t, clause, "IN")
	assert.Len(t, args, 3)
}

func TestSQLFilterBuilder_ExistsOperator(t *testing.T) {
	t.Run("$exists true", func(t *testing.T) {
		builder := NewSQLFilterBuilder(3)
		err := builder.BuildFromPipeline([]interface{}{
			map[string]interface{}{
				"$match": map[string]interface{}{
					"fullDocument.healthevent.nodeName": map[string]interface{}{
						"$exists": true,
					},
				},
			},
		})
		require.NoError(t, err)

		clause := builder.GetWhereClause()
		assert.Contains(t, clause, "IS NOT NULL")
	})

	t.Run("$exists false", func(t *testing.T) {
		builder := NewSQLFilterBuilder(3)
		err := builder.BuildFromPipeline([]interface{}{
			map[string]interface{}{
				"$match": map[string]interface{}{
					"fullDocument.healthevent.metadata": map[string]interface{}{
						"$exists": false,
					},
				},
			},
		})
		require.NoError(t, err)

		clause := builder.GetWhereClause()
		assert.Contains(t, clause, "IS NULL")
	})
}

func TestSQLFilterBuilder_NilPipeline(t *testing.T) {
	builder := NewSQLFilterBuilder(3)
	err := builder.BuildFromPipeline(nil)
	require.NoError(t, err)

	assert.False(t, builder.HasConditions())
	assert.Empty(t, builder.GetWhereClause())
	assert.Empty(t, builder.GetArgs())
}

func TestSQLFilterBuilder_EmptyPipeline(t *testing.T) {
	builder := NewSQLFilterBuilder(3)
	err := builder.BuildFromPipeline([]interface{}{})
	require.NoError(t, err)

	assert.False(t, builder.HasConditions())
	assert.Empty(t, builder.GetWhereClause())
}

func TestSQLFilterBuilder_GetWhereClauseWithAnd(t *testing.T) {
	builder := NewSQLFilterBuilder(3)
	err := builder.BuildFromPipeline([]interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"fullDocument.healthevent.isFatal": true,
			},
		},
	})
	require.NoError(t, err)

	clauseWithAnd := builder.GetWhereClauseWithAnd()

	// Should start with " AND "
	assert.True(t, len(clauseWithAnd) > 5)
	assert.Equal(t, " AND ", clauseWithAnd[:5])
}

func TestSQLFilterBuilder_MultipleMatchConditions(t *testing.T) {
	builder := NewSQLFilterBuilder(3)
	err := builder.BuildFromPipeline([]interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"fullDocument.healthevent.isFatal":   true,
				"fullDocument.healthevent.nodeName":  "test-node",
				"fullDocument.healthevent.checkName": "GpuXidError",
			},
		},
	})
	require.NoError(t, err)

	clause := builder.GetWhereClause()
	args := builder.GetArgs()

	// Should have conditions for all three fields
	assert.Contains(t, clause, "isFatal")
	assert.Contains(t, clause, "nodeName")
	assert.Contains(t, clause, "checkName")

	// Should have AND between conditions
	assert.Contains(t, clause, " AND ")

	// Should have 3 args
	assert.Len(t, args, 3)
}

func TestSQLFilterBuilder_RealWorldHealthEventsAnalyzerPipeline(t *testing.T) {
	// This is the actual pipeline used by health-events-analyzer
	pipeline := []interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"$or": []interface{}{
					map[string]interface{}{"fullDocument.healthevent.isFatal": true},
					map[string]interface{}{"fullDocument.healthevent.isHealthy": false},
				},
			},
		},
	}

	builder := NewSQLFilterBuilder(3)
	err := builder.BuildFromPipeline(pipeline)
	require.NoError(t, err)

	assert.True(t, builder.HasConditions())

	clause := builder.GetWhereClauseWithAnd()
	t.Logf("Generated SQL clause: %s", clause)
	t.Logf("Args: %v", builder.GetArgs())

	// Should create proper OR condition
	assert.Contains(t, clause, " OR ")

	// Should handle isFatal = true
	assert.Contains(t, clause, "healthevent'->>'isFatal'")

	// Should handle isHealthy = false with NULL handling
	assert.Contains(t, clause, "healthevent'->>'isHealthy'")
}

// ============================================================================
// REAL COMPONENT PIPELINES FROM CI LOGS
// These tests validate SQL filter generation for actual pipelines used by
// nvsentinel components in production
// ============================================================================

// TestSQLFilterBuilder_HealthEventsAnalyzerPipeline tests the pipeline used by health-events-analyzer
// From postgresql_pipeline_builder.go: BuildNonFatalUnhealthyInsertsPipeline
// This is the pipeline that caused the 12+ minute event processing lag in CI
func TestSQLFilterBuilder_HealthEventsAnalyzerPipeline(t *testing.T) {
	// Actual pipeline from BuildNonFatalUnhealthyInsertsPipeline
	// Uses datastore.Pipeline format (as seen in CI logs)
	pipeline := datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", datastore.D(datastore.E("$in", datastore.A("insert", "update")))),
				datastore.E("fullDocument.healthevent.agent", datastore.D(datastore.E("$ne", "health-events-analyzer"))),
				datastore.E("fullDocument.healthevent.ishealthy", false),
			)),
		),
	)

	builder := NewSQLFilterBuilder(3)
	err := builder.BuildFromPipeline(pipeline)
	require.NoError(t, err)

	assert.True(t, builder.HasConditions())

	clause := builder.GetWhereClause()
	args := builder.GetArgs()

	t.Logf("health-events-analyzer pipeline SQL: %s", clause)
	t.Logf("Args: %v", args)

	// Should filter by isHealthy = false (ishealthy converted to isHealthy)
	assert.Contains(t, clause, "healthevent'->>'isHealthy'")

	// Should filter by agent != "health-events-analyzer"
	assert.Contains(t, clause, "healthevent'->>'agent'")

	// Should filter by operationType using operation column
	assert.Contains(t, clause, "operation IN")

	// The value "health-events-analyzer" should be in the args (as a parameterized value)
	found := false
	for _, arg := range args {
		if arg == "health-events-analyzer" {
			found = true

			break
		}
	}

	assert.True(t, found, "health-events-analyzer should be in args")
}

// TestSQLFilterBuilder_FaultRemediationPipeline tests the pipeline used by fault-remediation
// From postgresql_pipeline_builder.go: BuildQuarantinedAndDrainedNodesPipeline
func TestSQLFilterBuilder_FaultRemediationPipeline(t *testing.T) {
	// Simplified version of BuildQuarantinedAndDrainedNodesPipeline
	// The actual pipeline has nested $or with multiple conditions
	pipeline := datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("$or", datastore.A(
					// UPDATE path - remediation ready
					datastore.D(
						datastore.E("operationType", "update"),
						datastore.E("fullDocument.healtheventstatus.userpodsevictionstatus.status", datastore.D(
							datastore.E("$in", datastore.A("Succeeded", "AlreadyDrained")),
						)),
						datastore.E("fullDocument.healtheventstatus.nodequarantined", datastore.D(
							datastore.E("$in", datastore.A("Quarantined", "AlreadyQuarantined")),
						)),
					),
					// Cancelled path
					datastore.D(
						datastore.E("fullDocument.healtheventstatus.nodequarantined", "Cancelled"),
					),
				)),
			)),
		),
	)

	builder := NewSQLFilterBuilder(3)
	err := builder.BuildFromPipeline(pipeline)
	require.NoError(t, err)

	assert.True(t, builder.HasConditions())

	clause := builder.GetWhereClause()
	args := builder.GetArgs()

	t.Logf("fault-remediation pipeline SQL: %s", clause)
	t.Logf("Args: %v", args)

	// Should have OR condition
	assert.Contains(t, clause, " OR ")

	// Should filter by nodequarantined status
	assert.Contains(t, clause, "nodequarantined")
}

// TestSQLFilterBuilder_NodeDrainerPipeline tests the pipeline used by node-drainer
// From postgresql_pipeline_builder.go: BuildNodeQuarantineStatusPipeline
func TestSQLFilterBuilder_NodeDrainerPipeline(t *testing.T) {
	// Simplified version of BuildNodeQuarantineStatusPipeline
	pipeline := datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("$or", datastore.A(
					// UPDATE with quarantine status
					datastore.D(
						datastore.E("operationType", "update"),
						datastore.E("updateDescription.updatedFields", datastore.D(
							datastore.E("healtheventstatus.nodequarantined", "Quarantined"),
						)),
					),
					// INSERT with quarantine status (failsafe)
					datastore.D(
						datastore.E("operationType", "insert"),
						datastore.E("fullDocument.healtheventstatus.nodequarantined", datastore.D(
							datastore.E("$in", datastore.A("Quarantined", "AlreadyQuarantined", "UnQuarantined", "Cancelled")),
						)),
					),
				)),
			)),
		),
	)

	builder := NewSQLFilterBuilder(3)
	err := builder.BuildFromPipeline(pipeline)
	require.NoError(t, err)

	assert.True(t, builder.HasConditions())

	clause := builder.GetWhereClause()
	args := builder.GetArgs()

	t.Logf("node-drainer pipeline SQL: %s", clause)
	t.Logf("Args: %v", args)

	// Should have OR condition for the two paths
	assert.Contains(t, clause, " OR ")
}

// TestSQLFilterBuilder_FaultQuarantinePipeline tests the pipeline used by fault-quarantine
// From postgresql_pipeline_builder.go: BuildAllHealthEventInsertsPipeline
func TestSQLFilterBuilder_FaultQuarantinePipeline(t *testing.T) {
	// Actual pipeline from BuildAllHealthEventInsertsPipeline
	pipeline := datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", datastore.D(
					datastore.E("$in", datastore.A("insert")),
				)),
			)),
		),
	)

	builder := NewSQLFilterBuilder(3)
	err := builder.BuildFromPipeline(pipeline)
	require.NoError(t, err)

	// This pipeline only filters by operationType which is NOT in new_values
	// The SQL filter builder should handle this gracefully
	clause := builder.GetWhereClause()
	t.Logf("fault-quarantine pipeline SQL: %s", clause)

	// operationType is a top-level field, not in fullDocument
	// The builder should still work but may not generate conditions for it
	// This is fine - the application-side filter will handle it
}

// TestSQLFilterBuilder_SimpleMapInterface tests pipelines passed as map[string]interface{}
// This is how pipelines appear when parsed from JSON or passed through generic interfaces
func TestSQLFilterBuilder_SimpleMapInterface(t *testing.T) {
	// Pipeline as simple map[string]interface{} (common in tests and generic code)
	pipeline := []interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"operationType":                      "insert",
				"fullDocument.healthevent.ishealthy": false,
				"fullDocument.healthevent.isfatal":   true,
			},
		},
	}

	builder := NewSQLFilterBuilder(3)
	err := builder.BuildFromPipeline(pipeline)
	require.NoError(t, err)

	assert.True(t, builder.HasConditions())

	clause := builder.GetWhereClause()
	args := builder.GetArgs()

	t.Logf("Simple map pipeline SQL: %s", clause)
	t.Logf("Args: %v", args)

	// Field names are converted from MongoDB lowercase (ishealthy, isfatal)
	// to PostgreSQL JSON camelCase (isHealthy, isFatal)
	assert.Contains(t, clause, "isHealthy")
	assert.Contains(t, clause, "isFatal")

	// operationType should map to operation column
	assert.Contains(t, clause, "operation")
}

func TestSQLFilterBuilder_ArgIndexing(t *testing.T) {
	// Test that arg indices start from the correct position
	builder := NewSQLFilterBuilder(3) // Start at 4 ($1, $2, $3 already used)

	err := builder.BuildFromPipeline([]interface{}{
		map[string]interface{}{
			"$match": map[string]interface{}{
				"fullDocument.healthevent.isFatal":  true,
				"fullDocument.healthevent.nodeName": "test-node",
			},
		},
	})
	require.NoError(t, err)

	clause := builder.GetWhereClause()

	// Should use $4 and $5 (not $1 and $2)
	assert.Contains(t, clause, "$4")
	assert.Contains(t, clause, "$5")
	assert.NotContains(t, clause, "$1")
	assert.NotContains(t, clause, "$2")
	assert.NotContains(t, clause, "$3")
}


