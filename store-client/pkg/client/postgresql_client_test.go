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
	"context"
	"strings"
	"testing"
)

// TestPostgreSQLClient_BasicOperations tests basic CRUD operations
// This is a smoke test to verify the implementation compiles and has correct signatures
func TestPostgreSQLClient_BasicOperations(t *testing.T) {
	// This is a compile-time test to verify all methods exist and have correct signatures
	// Actual integration tests would require a PostgreSQL instance

	t.Skip("Skipping integration test - requires PostgreSQL instance")

	// The following code verifies that all required methods exist with correct signatures
	var client DatabaseClient
	var _ DatabaseClient = (*PostgreSQLClient)(nil) // Compile-time interface check

	_ = client // Avoid unused variable warning
}

// TestPostgreSQLClient_InterfaceCompliance verifies PostgreSQLClient implements DatabaseClient
func TestPostgreSQLClient_InterfaceCompliance(t *testing.T) {
	var _ DatabaseClient = (*PostgreSQLClient)(nil)
}

// TestPostgreSQLChangeStreamWatcher_InterfaceCompliance verifies watcher implements ChangeStreamWatcher
func TestPostgreSQLChangeStreamWatcher_InterfaceCompliance(t *testing.T) {
	var _ ChangeStreamWatcher = (*PostgreSQLChangeStreamWatcher)(nil)
}

// TestBuildWhereClause tests filter translation logic
func TestBuildWhereClause(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}

	tests := []struct {
		name           string
		filter         interface{}
		expectedClause string
		expectedArgs   int
		expectError    bool
	}{
		{
			name:           "nil filter",
			filter:         nil,
			expectedClause: "TRUE",
			expectedArgs:   0,
			expectError:    false,
		},
		{
			name:           "empty filter",
			filter:         map[string]interface{}{},
			expectedClause: "TRUE",
			expectedArgs:   0,
			expectError:    false,
		},
		{
			name: "simple equality",
			filter: map[string]interface{}{
				"nodeName": "node-1",
			},
			expectedClause: "document->>'nodeName' = $1",
			expectedArgs:   1,
			expectError:    false,
		},
		{
			name: "nested field",
			filter: map[string]interface{}{
				"healthevent.nodename": "node-1",
			},
			expectedClause: "document->'healthevent'->>'nodeName' = $1", // nodename is normalized to nodeName
			expectedArgs:   1,
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clause, args, err := client.buildWhereClause(tt.filter)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if clause != tt.expectedClause {
				t.Errorf("expected clause %q, got %q", tt.expectedClause, clause)
			}

			if len(args) != tt.expectedArgs {
				t.Errorf("expected %d args, got %d", tt.expectedArgs, len(args))
			}
		})
	}
}

// TestBuildJSONPath tests JSONB path generation
func TestBuildJSONPath(t *testing.T) {
	client := &PostgreSQLClient{}

	tests := []struct {
		name     string
		field    string
		expected string
	}{
		{
			name:     "simple field",
			field:    "nodeName",
			expected: "document->>'nodeName'",
		},
		{
			name:     "nested field (2 levels)",
			field:    "healthevent.nodename",
			expected: "document->'healthevent'->>'nodeName'", // nodename is normalized to nodeName
		},
		{
			name:     "deeply nested field",
			field:    "healthevent.status.message",
			expected: "document->'healthevent'->'status'->>'message'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := client.buildJSONPath(tt.field)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestBuildUpdateClause tests update translation logic
func TestBuildUpdateClause(t *testing.T) {
	client := &PostgreSQLClient{}

	tests := []struct {
		name        string
		update      interface{}
		expectError bool
	}{
		{
			name: "simple $set",
			update: map[string]interface{}{
				"$set": map[string]interface{}{
					"status": "Active",
				},
			},
			expectError: false,
		},
		{
			name: "nested $set",
			update: map[string]interface{}{
				"$set": map[string]interface{}{
					"healthevent.status": "Resolved",
				},
			},
			expectError: false,
		},
		{
			name: "missing $set",
			update: map[string]interface{}{
				"status": "Active",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := client.buildUpdateClause(tt.update)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestAggregationPipelineConversion tests aggregation pipeline parsing
func TestAggregationPipelineConversion(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}

	tests := []struct {
		name        string
		stages      []map[string]interface{}
		expectError bool
		errorOp     string
	}{
		{
			name: "basic $match and $sort",
			stages: []map[string]interface{}{
				{"$match": map[string]interface{}{"nodeName": "node-1"}},
				{"$sort": map[string]interface{}{"created_at": -1}},
			},
			expectError: false,
		},
		{
			name: "$limit and $skip",
			stages: []map[string]interface{}{
				{"$limit": 10},
				{"$skip": 5},
			},
			expectError: false,
		},
		{
			name: "basic $group support",
			stages: []map[string]interface{}{
				{"$group": map[string]interface{}{"_id": "$nodeName"}},
			},
			expectError: false,
		},
		{
			name: "supported $setWindowFields with basic spec",
			stages: []map[string]interface{}{
				{
					"$setWindowFields": map[string]interface{}{
						"sortBy": map[string]interface{}{
							"healthevent.generatedtimestamp.seconds": 1,
						},
						"output": map[string]interface{}{
							"allPreviousEvents": map[string]interface{}{
								"$push":  "$$ROOT",
								"window": map[string]interface{}{"documents": []interface{}{"unbounded", -1}},
							},
						},
					},
				},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := client.buildAggregationQuery(tt.stages)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
					return
				}
				// Optionally verify error message contains the operator name
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestSetWindowFieldsQueryGeneration verifies SQL query generation for $setWindowFields
func TestSetWindowFieldsQueryGeneration(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}

	tests := []struct {
		name            string
		stages          []map[string]interface{}
		expectedInQuery []string // Substrings expected in generated SQL
	}{
		{
			name: "$push with unbounded preceding",
			stages: []map[string]interface{}{
				{
					"$setWindowFields": map[string]interface{}{
						"sortBy": map[string]interface{}{
							"healthevent.generatedtimestamp.seconds": 1,
						},
						"output": map[string]interface{}{
							"allPreviousEvents": map[string]interface{}{
								"$push":  "$$ROOT",
								"window": map[string]interface{}{"documents": []interface{}{"unbounded", -1}},
							},
						},
					},
				},
			},
			expectedInQuery: []string{
				"jsonb_agg(document)",
				"OVER",
				"ORDER BY",
				"ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING",
				"jsonb_set",
				"allPreviousEvents",
			},
		},
		{
			name: "$sum with conditional expression",
			stages: []map[string]interface{}{
				{
					"$setWindowFields": map[string]interface{}{
						"sortBy": map[string]interface{}{
							"healthevent.generatedtimestamp.seconds": 1,
						},
						"output": map[string]interface{}{
							"burstId": map[string]interface{}{
								"$sum":   1,
								"window": map[string]interface{}{"documents": []interface{}{"unbounded", "current"}},
							},
						},
					},
				},
			},
			expectedInQuery: []string{
				"SUM",
				"OVER",
				"ORDER BY",
				"ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW",
				"jsonb_set",
				"burstId",
			},
		},
		{
			name: "$max with field reference",
			stages: []map[string]interface{}{
				{
					"$setWindowFields": map[string]interface{}{
						"sortBy": map[string]interface{}{
							"_id.burstId": 1,
						},
						"output": map[string]interface{}{
							"maxBurstId": map[string]interface{}{
								"$max": "$_id.burstId",
							},
						},
					},
				},
			},
			expectedInQuery: []string{
				"MAX",
				"OVER",
				"ORDER BY",
				"jsonb_set",
				"maxBurstId",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, _, err := client.buildAggregationQuery(tt.stages)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify expected substrings are present in the query
			for _, expected := range tt.expectedInQuery {
				if !strings.Contains(query, expected) {
					t.Errorf("expected query to contain '%s', but it doesn't.\nQuery: %s", expected, query)
				}
			}

			t.Logf("Generated query: %s", query)
		})
	}
}

// Example of how to use the PostgreSQL client (documentation)
func ExamplePostgreSQLClient() {
	// This example shows how components will use the PostgreSQL client
	// Actual execution requires a PostgreSQL instance

	ctx := context.Background()

	// Components get the client through the factory
	// The factory routes to PostgreSQL based on DATASTORE_PROVIDER environment variable
	//
	// Example usage:
	// os.Setenv("DATASTORE_PROVIDER", "postgresql")
	// factory, err := factory.NewClientFactoryFromEnv()
	// client, err := factory.CreateDatabaseClient(ctx)
	//
	// // Insert documents
	// docs := []interface{}{
	//     map[string]interface{}{"nodeName": "node-1", "status": "healthy"},
	// }
	// result, err := client.InsertMany(ctx, docs)
	//
	// // Query documents
	// filter := map[string]interface{}{"nodeName": "node-1"}
	// cursor, err := client.Find(ctx, filter, nil)
	//
	// // Update documents
	// update := map[string]interface{}{
	//     "$set": map[string]interface{}{"status": "unhealthy"},
	// }
	// result, err := client.UpdateDocument(ctx, filter, update)
	//
	// // Aggregate
	// pipeline := []interface{}{
	//     map[string]interface{}{"$match": filter},
	//     map[string]interface{}{"$sort": map[string]interface{}{"created_at": -1}},
	//     map[string]interface{}{"$limit": 10},
	// }
	// cursor, err := client.Aggregate(ctx, pipeline)

	_ = ctx // Avoid unused variable
}

// TestBuildExprValue_NewOperators tests the newly implemented expression operators
func TestBuildExprValue_NewOperators(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}

	tests := []struct {
		name        string
		expr        interface{}
		expectedSQL string
		expectError bool
	}{
		// Test $and operator
		{
			name: "$and with two boolean fields",
			expr: map[string]interface{}{
				"$and": []interface{}{
					"$isStickyXid",
					"$stickyXidWithin3Hours",
				},
			},
			expectedSQL: "((document->>'isStickyXid')::bigint AND (document->>'stickyXidWithin3Hours')::bigint)",
			expectError: false,
		},
		{
			name: "$and with comparison expressions",
			expr: map[string]interface{}{
				"$and": []interface{}{
					map[string]interface{}{
						"$eq": []interface{}{"$status", "$expectedStatus"},
					},
					map[string]interface{}{
						"$eq": []interface{}{"$count", 5},
					},
				},
			},
			expectedSQL: "(((document->>'status')::bigint = (document->>'expectedStatus')::bigint) AND ((document->>'count')::bigint = 5))",
			expectError: false,
		},
		{
			name: "$and with single expression",
			expr: map[string]interface{}{
				"$and": []interface{}{
					"$isStickyXid",
				},
			},
			expectedSQL: "((document->>'isStickyXid')::bigint)",
			expectError: false,
		},
		{
			name: "$and with empty array",
			expr: map[string]interface{}{
				"$and": []interface{}{},
			},
			expectedSQL: "",
			expectError: true,
		},

		// Test $anyElementTrue operator
		{
			name: "$anyElementTrue with map result",
			expr: map[string]interface{}{
				"$anyElementTrue": map[string]interface{}{
					"$map": map[string]interface{}{
						"input": "$arrayField",
						"in":    "$$this.isActive",
					},
				},
			},
			// Complex expression - just verify it contains the key components
			expectedSQL: "bool_or",
			expectError: false,
		},

		// Test $lte operator
		{
			name: "$lte with numeric comparison",
			expr: map[string]interface{}{
				"$lte": []interface{}{
					map[string]interface{}{
						"$subtract": []interface{}{
							"$healthevent.generatedtimestamp.seconds",
							"$healthevent.prevtimestamp.seconds",
						},
					},
					20,
				},
			},
			// generatedtimestamp is normalized to generatedTimestamp
			expectedSQL: "(((document->'healthevent'->'generatedTimestamp'->>'seconds')::bigint - (document->'healthevent'->'prevtimestamp'->>'seconds')::bigint) <= 20)",
			expectError: false,
		},
		{
			name: "$lte with two fields",
			expr: map[string]interface{}{
				"$lte": []interface{}{
					"$value1",
					"$value2",
				},
			},
			expectedSQL: "((document->>'value1')::bigint <= (document->>'value2')::bigint)",
			expectError: false,
		},
		{
			name: "$lte with single operand - should fail",
			expr: map[string]interface{}{
				"$lte": []interface{}{
					"$value1",
				},
			},
			expectedSQL: "",
			expectError: true,
		},
		{
			name: "$lte with three operands - should fail",
			expr: map[string]interface{}{
				"$lte": []interface{}{
					"$value1",
					"$value2",
					"$value3",
				},
			},
			expectedSQL: "",
			expectError: true,
		},

		// Test $subtract operator
		{
			name: "$subtract with two field references",
			expr: map[string]interface{}{
				"$subtract": []interface{}{
					"$healthevent.generatedtimestamp.seconds",
					"$healthevent.prevtimestamp.seconds",
				},
			},
			// generatedtimestamp is normalized to generatedTimestamp
			expectedSQL: "((document->'healthevent'->'generatedTimestamp'->>'seconds')::bigint - (document->'healthevent'->'prevtimestamp'->>'seconds')::bigint)",
			expectError: false,
		},
		{
			name: "$subtract with field and literal",
			expr: map[string]interface{}{
				"$subtract": []interface{}{
					"$count",
					5,
				},
			},
			expectedSQL: "((document->>'count')::bigint - 5)",
			expectError: false,
		},
		{
			name: "$subtract with nested expressions",
			expr: map[string]interface{}{
				"$subtract": []interface{}{
					map[string]interface{}{
						"$subtract": []interface{}{
							"$value1",
							"$value2",
						},
					},
					10,
				},
			},
			expectedSQL: "(((document->>'value1')::bigint - (document->>'value2')::bigint) - 10)",
			expectError: false,
		},
		{
			name: "$subtract with single operand - should fail",
			expr: map[string]interface{}{
				"$subtract": []interface{}{
					"$value1",
				},
			},
			expectedSQL: "",
			expectError: true,
		},

		// Test combined operators
		{
			name: "$and with $lte and $subtract",
			expr: map[string]interface{}{
				"$and": []interface{}{
					"$isStickyXid",
					map[string]interface{}{
						"$lte": []interface{}{
							map[string]interface{}{
								"$subtract": []interface{}{
									"$currentTime",
									"$eventTime",
								},
							},
							3600,
						},
					},
				},
			},
			expectedSQL: "((document->>'isStickyXid')::bigint AND (((document->>'currentTime')::bigint - (document->>'eventTime')::bigint) <= 3600))",
			expectError: false,
		},

		// Test $in with string literal (RepeatedXidError rule pattern)
		// This tests the case where a resolved "this.healthevent.errorcode.0" becomes a literal like "79"
		{
			name: "$in with string literal and field reference",
			expr: map[string]interface{}{
				"$in": []interface{}{
					"79", // Literal string value (resolved from this.healthevent.errorcode.0)
					"$uniqueXidsInBurst",
				},
			},
			expectedSQL: "@> to_jsonb('79')", // Uses PostgreSQL containment operator
			expectError: false,
		},
		{
			name: "$in with string literal containing special chars",
			expr: map[string]interface{}{
				"$in": []interface{}{
					"test'value", // String with single quote (should be escaped)
					"$arrayField",
				},
			},
			expectedSQL: "@> to_jsonb('test''value')", // Single quote is escaped
			expectError: false,
		},
		{
			name: "$in with field reference and literal array",
			expr: map[string]interface{}{
				"$in": []interface{}{
					map[string]interface{}{
						"$arrayElemAt": []interface{}{
							"$healthevent.errorcode",
							0,
						},
					},
					[]interface{}{"74", "79", "95", "109", "119"},
				},
			},
			expectedSQL: "jsonb_build_array('74', '79', '95', '109', '119')", // Literal array
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := client.buildExprValue(tt.expr)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if !strings.Contains(result, tt.expectedSQL) {
				t.Errorf("expected SQL to contain %q, got %q", tt.expectedSQL, result)
			}

			t.Logf("Generated SQL: %s", result)
		})
	}
}

// TestAddFieldsWithNewOperators tests $addFields stage with the new operators
func TestAddFieldsWithNewOperators(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}

	tests := []struct {
		name            string
		stages          []map[string]interface{}
		expectedInQuery []string
	}{
		{
			name: "$addFields with $and operator",
			stages: []map[string]interface{}{
				{
					"$addFields": map[string]interface{}{
						"isValid": map[string]interface{}{
							"$and": []interface{}{
								"$isStickyXid",
								"$stickyXidWithin3Hours",
							},
						},
					},
				},
			},
			expectedInQuery: []string{
				"jsonb_set",
				"isValid",
				"AND",
			},
		},
		{
			name: "$addFields with $lte and $subtract",
			stages: []map[string]interface{}{
				{
					"$addFields": map[string]interface{}{
						"timeDiffOk": map[string]interface{}{
							"$lte": []interface{}{
								map[string]interface{}{
									"$subtract": []interface{}{
										"$healthevent.generatedtimestamp.seconds",
										"$healthevent.prevtimestamp.seconds",
									},
								},
								20,
							},
						},
					},
				},
			},
			expectedInQuery: []string{
				"jsonb_set",
				"timeDiffOk",
				"<=",
				"-",
			},
		},
		{
			name: "$addFields with $anyElementTrue",
			stages: []map[string]interface{}{
				{
					"$addFields": map[string]interface{}{
						"hasActiveElement": map[string]interface{}{
							"$anyElementTrue": map[string]interface{}{
								"$map": map[string]interface{}{
									"input": "$items",
									"in":    "$$this.isActive",
								},
							},
						},
					},
				},
			},
			expectedInQuery: []string{
				"jsonb_set",
				"hasActiveElement",
				"bool_or",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, _, err := client.buildAggregationQuery(tt.stages)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify expected substrings are present in the query
			for _, expected := range tt.expectedInQuery {
				if !strings.Contains(query, expected) {
					t.Errorf("expected query to contain '%s', but it doesn't.\nQuery: %s", expected, query)
				}
			}

			t.Logf("Generated query: %s", query)
		})
	}
}

// TestCountWithPostMatchFilter tests the MultipleRemediations pattern:
// $match -> $match -> $count -> $match (filter on count result)
// This is a regression test for a bug where $match after $count was ignored,
// causing rules to incorrectly match when count was 0.
func TestCountWithPostMatchFilter(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}

	// This is the exact pipeline pattern from MultipleRemediations rule:
	// 1. $match: time filter (recent events)
	// 2. $match: nodename/isfatal filter
	// 3. $count: "count"
	// 4. $match: {count: {$gte: 5}} - FILTER ON COUNT RESULT
	stages := []map[string]interface{}{
		{"$match": map[string]interface{}{"healthevent.nodename": "test-node"}},
		{"$match": map[string]interface{}{"healthevent.isfatal": true}},
		{"$count": "count"},
		{"$match": map[string]interface{}{"count": map[string]interface{}{"$gte": 5}}},
	}

	query, args, err := client.buildAggregationQuery(stages)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Logf("Generated query for MultipleRemediations pattern: %s", query)
	t.Logf("Query args: %v", args)

	// The query MUST wrap the count in a subquery and filter on the result
	// The structure should be: SELECT * FROM (count_query) WHERE count >= $N

	// Check that the query has a wrapper that filters the count result
	if !strings.Contains(query, "count_result") {
		t.Errorf("Query does not wrap count in a subquery for filtering. Post-count $match is ignored!\nQuery: %s", query)
	}

	// Check that there's a WHERE clause filtering on the count field
	if !strings.Contains(query, "(document->>'count')::bigint >=") {
		t.Errorf("Query does not filter on count result. Post-count $match is ignored!\nQuery: %s", query)
	}

	// Verify the threshold value (5) is in the args
	found := false
	for _, arg := range args {
		if v, ok := arg.(int); ok && v == 5 {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("Query args do not contain the count threshold 5. Args: %v", args)
	}
}

// TestCountWithPostMatchFilter_ZeroCount verifies that when count is 0,
// the $match {count: {$gte: 5}} should filter it out (return no rows)
func TestCountWithPostMatchFilter_ZeroCount(t *testing.T) {
	// This test documents the expected behavior:
	// When there are 0 matching documents, the count should be 0,
	// and the post-count $match {count: {$gte: 5}} should filter it out.
	//
	// MongoDB behavior:
	// - $count returns {count: 0} when no documents match
	// - $match {count: {$gte: 5}} filters this out, returning empty result
	//
	// PostgreSQL should have the same behavior.
	t.Log("This test documents expected behavior for count=0 case")

	client := &PostgreSQLClient{table: "health_events"}

	stages := []map[string]interface{}{
		{"$match": map[string]interface{}{"healthevent.nodename": "nonexistent-node"}},
		{"$count": "count"},
		{"$match": map[string]interface{}{"count": map[string]interface{}{"$gte": 5}}},
	}

	query, args, err := client.buildAggregationQuery(stages)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Logf("Generated query for zero count case: %s", query)
	t.Logf("Query args: %v", args)

	// Verify the query structure properly handles the post-count filter
	if !strings.Contains(query, "count_result") {
		t.Errorf("Query does not wrap count for filtering. Post-count $match is ignored!\nQuery: %s", query)
	}

	// Verify the threshold value (5) is in the args
	found := false
	for _, arg := range args {
		if v, ok := arg.(int); ok && v == 5 {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("Query args do not contain the count threshold 5. Args: %v", args)
	}
}
