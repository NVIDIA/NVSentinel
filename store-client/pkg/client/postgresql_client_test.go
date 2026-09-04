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
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
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
		filter         any
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
			filter:         map[string]any{},
			expectedClause: "TRUE",
			expectedArgs:   0,
			expectError:    false,
		},
		{
			name: "simple equality",
			filter: map[string]any{
				"nodeName": "node-1",
			},
			expectedClause: "document->>'nodeName' = $1",
			expectedArgs:   1,
			expectError:    false,
		},
		{
			name: "nested field",
			filter: map[string]any{
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
		{
			name:     "createdAt legacy document field",
			field:    "createdAt",
			expected: "document->>'createdAt'",
		},
		{
			name:     "updatedAt legacy document field",
			field:    "updatedAt",
			expected: "document->>'updatedAt'",
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

	if got := client.buildJSONPathWithOptions("createdAt", true); got != "created_at" {
		t.Fatalf("extended createdAt path = %q", got)
	}
	if got := client.buildJSONPathWithOptions("updatedAt", true); got != "updated_at" {
		t.Fatalf("extended updatedAt path = %q", got)
	}
}

func TestRecoveryQueryLogicalFilters(t *testing.T) {
	client := &PostgreSQLClient{}

	for name, value := range map[string]any{
		"generic slice": []any{map[string]any{"nodeName": "node-a"}},
		"map slice":     []map[string]any{{"nodeName": "node-a"}},
		"datastore array": datastore.Array{
			map[string]any{"nodeName": "node-a"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			clause, args, err := client.buildLogicalWhereClause(opOr, value, 3, true)
			if err != nil {
				t.Fatalf("buildLogicalWhereClause() error = %v", err)
			}
			if clause != "(document->>'nodeName' = $3)" {
				t.Fatalf("clause = %q", clause)
			}
			if !reflect.DeepEqual(args, []any{"node-a"}) {
				t.Fatalf("args = %#v", args)
			}
		})
	}

	clause, args, err := client.buildLogicalWhereClause(opAnd, []any{
		map[string]any{"nodeName": "node-a"},
		map[string]any{"createdAt": map[string]any{opGT: time.Unix(10, 0)}},
	}, 1, true)
	if err != nil {
		t.Fatalf("buildLogicalWhereClause() error = %v", err)
	}
	if clause != "(document->>'nodeName' = $1 AND created_at > $2)" || len(args) != 2 {
		t.Fatalf("clause = %q, args = %#v", clause, args)
	}
}

func TestRecoveryQueryLogicalFilterValidation(t *testing.T) {
	client := &PostgreSQLClient{}

	for name, value := range map[string]any{
		"wrong type": "node-a",
		"empty":      []any{},
		"non-map":    []any{"node-a"},
		"nested error": []any{
			map[string]any{"createdAt": map[string]any{"$unsupported": 1}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := client.buildLogicalWhereClause(opOr, value, 1, true)
			if err == nil {
				t.Fatal("buildLogicalWhereClause() expected an error")
			}
		})
	}

	_, _, err := client.buildWhereClauseMapWithOptions(map[string]any{opOr: "node-a"}, 1, true)
	if err == nil {
		t.Fatal("root logical filter accepted an invalid value")
	}
	_, _, err = client.buildWhereClauseMapWithOptions(map[string]any{
		"status.value": map[string]any{opExists: "true"},
	}, 1, true)
	if err == nil {
		t.Fatal("field comparison accepted an invalid $exists value")
	}
}

func TestRecoveryQueryComparisonOperators(t *testing.T) {
	for operator, expected := range map[string]string{
		opGTE: ">=", opGT: ">", opLTE: "<=", opLT: "<", opEQ: "=", opNE: "IS DISTINCT FROM",
	} {
		actual, ok := sqlComparisonOperator(operator)
		if !ok || actual != expected {
			t.Fatalf("sqlComparisonOperator(%q) = %q, %v", operator, actual, ok)
		}
	}
	if _, ok := sqlComparisonOperator("$unsupported"); ok {
		t.Fatal("unsupported operator accepted")
	}

	for _, test := range []struct {
		value          any
		wantExpression string
		wantArgument   any
	}{
		{true, "CASE WHEN document->>'count' IS NULL THEN false WHEN document->>'count' IN ('true', 'false') THEN (document->>'count')::boolean END", true},
		{int32(-2), "CASE WHEN document->>'count' ~ '^-?[0-9]+([.][0-9]+)?([eE][+-]?[0-9]+)?$' THEN (document->>'count')::numeric END", "-2"},
		{uint64(3), "CASE WHEN document->>'count' ~ '^-?[0-9]+([.][0-9]+)?([eE][+-]?[0-9]+)?$' THEN (document->>'count')::numeric END", "3"},
		{float32(1.5), "CASE WHEN document->>'count' ~ '^-?[0-9]+([.][0-9]+)?([eE][+-]?[0-9]+)?$' THEN (document->>'count')::numeric END", "1.5"},
	} {
		expression, argument := typedComparisonOperand("document->>'count'", test.value)
		if expression != test.wantExpression || argument != test.wantArgument {
			t.Fatalf("typedComparisonOperand(%#v) = %q, %#v", test.value, expression, argument)
		}
	}
	cutoff := time.Unix(20, 0)
	expression, argument := typedComparisonOperand("updated_at", cutoff)
	if expression != "updated_at" || argument != cutoff {
		t.Fatalf("updated_at operand = %q, %#v", expression, argument)
	}
	expression, argument = typedComparisonOperand("document->>'value'", "raw")
	if expression != "document->>'value'" || argument != "raw" {
		t.Fatalf("string operand = %q, %#v", expression, argument)
	}
	if condition, args := buildScalarComparison("document->>'value'", "=", nil, 1, true); condition != "document->>'value' IS NULL" || len(args) != 0 {
		t.Fatalf("nil equality = %q, %#v", condition, args)
	}
	if condition, args := buildScalarComparison("document->>'value'", "!=", nil, 1, true); condition != "document->>'value' IS NOT NULL" || len(args) != 0 {
		t.Fatalf("nil inequality = %q, %#v", condition, args)
	}
	if condition, args := buildScalarComparison("document->>'value'", "IS DISTINCT FROM", "analyzer", 1, true); condition != "document->>'value' IS DISTINCT FROM $1" || !reflect.DeepEqual(args, []any{"analyzer"}) {
		t.Fatalf("null-safe inequality = %q, %#v", condition, args)
	}
	if condition, args := buildScalarComparison("document->>'value'", "IS DISTINCT FROM", false, 1, true); condition != "CASE WHEN document->>'value' IN ('true', 'false') THEN (document->>'value')::boolean END IS DISTINCT FROM $1" || !reflect.DeepEqual(args, []any{false}) {
		t.Fatalf("null-safe boolean inequality = %q, %#v", condition, args)
	}
}

func TestRecoveryQueryExistsOperator(t *testing.T) {
	path := "document->>'value'"
	for value, expected := range map[bool]string{true: path + " IS NOT NULL", false: path + " IS NULL"} {
		actual, err := buildExistsCondition(path, value)
		if err != nil || actual != expected {
			t.Fatalf("buildExistsCondition(%v) = %q, %v", value, actual, err)
		}
	}
	if _, err := buildExistsCondition(path, "true"); err == nil {
		t.Fatal("non-boolean $exists value accepted")
	}

	client := &PostgreSQLClient{}
	clause, args, err := client.buildWhereClauseWithOptions(map[string]any{
		"healthevent.processingstrategy": map[string]any{opExists: false},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if clause != "document->'healthevent'->'processingStrategy' IS NULL" || len(args) != 0 {
		t.Fatalf("$exists clause = %q, args = %#v", clause, args)
	}
}

func TestExtendedQueryTranslationRequiresOptIn(t *testing.T) {
	client := &PostgreSQLClient{}
	filter := map[string]any{opOr: []any{
		map[string]any{"healthevent.processingstrategy": int32(1)},
		map[string]any{"healthevent.processingstrategy": map[string]any{opExists: false}},
	}}

	legacyClause, _, err := client.buildWhereClause(filter)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(legacyClause, " OR ") || strings.Contains(legacyClause, "processingStrategy") {
		t.Fatalf("unscoped filter enabled extended translation: %s", legacyClause)
	}

	extendedClause, _, err := client.buildWhereClauseWithOptions(filter, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(extendedClause, " OR ") || !strings.Contains(extendedClause, "processingStrategy") {
		t.Fatalf("scoped filter did not enable extended translation: %s", extendedClause)
	}

	legacyNilClause, legacyNilArgs, err := client.buildWhereClause(map[string]any{"value": nil})
	if err != nil {
		t.Fatal(err)
	}
	if legacyNilClause != "document->>'value' = $1" || len(legacyNilArgs) != 1 || legacyNilArgs[0] != nil {
		t.Fatalf("unscoped nil comparison changed semantics: %s, %#v", legacyNilClause, legacyNilArgs)
	}
}

func TestAggregationExtendedFilterPrefix(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}
	stages := []map[string]any{
		{"$match": map[string]any{opOr: []any{
			map[string]any{"healthevent.processingstrategy": int32(0)},
			map[string]any{"healthevent.processingstrategy": int32(1)},
			map[string]any{"healthevent.processingstrategy": map[string]any{opExists: false}},
		}}},
		{"$match": map[string]any{"createdAt": "legacy-custom-rule"}},
	}

	rawPipeline, options := ResolvePipelineStageOptions(WithExtendedFilterPrefix(stages, 1))
	query, args, err := client.buildAggregationQuery(rawPipeline.([]map[string]any), options)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "processingStrategy") || !strings.Contains(query, " OR ") {
		t.Fatalf("mandatory stage did not use extended translation: %s", query)
	}
	if !strings.Contains(query, "document->>'createdAt' = $3") {
		t.Fatalf("configured stage did not retain legacy translation: %s", query)
	}
	if len(args) != 3 {
		t.Fatalf("args = %#v, want three legacy-compatible parameters", args)
	}
}

func buildResolvedAggregationQueryForTest(
	t *testing.T,
	client *PostgreSQLClient,
	pipeline any,
) (string, []any, error) {
	t.Helper()

	rawPipeline, options := ResolvePipelineStageOptions(pipeline)
	stages, ok := rawPipeline.([]map[string]any)
	if !ok {
		t.Fatalf("resolved pipeline has type %T, want []map[string]any", rawPipeline)
	}

	return client.buildAggregationQuery(stages, options)
}

func TestAggregationExtendedFilterPrefixRejectsLaterLogicalOperators(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}
	stages := []map[string]any{
		{"$match": map[string]any{"healthevent.nodename": "node-a"}},
		{"$match": map[string]any{opOr: []any{
			map[string]any{"healthevent.checkname": "legacy-custom-rule"},
		}}},
	}

	_, _, err := buildResolvedAggregationQueryForTest(t, client, WithExtendedFilterPrefix(stages, 1))
	if err == nil {
		t.Fatal("extended-only configured operator accepted outside the enabled prefix")
	}
	if !datastore.IsDeterministicError(err) {
		t.Fatalf("configured operator classified as transient: %v", err)
	}
}

func TestAnalyzerAggregationRejectsUnsupportedMatchShapes(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}
	tests := map[string]any{
		"extended nor": WithExtendedFilters([]map[string]any{
			{"$match": map[string]any{"$nor": []any{
				map[string]any{"healthevent.checkname": "custom-rule"},
			}}},
		}),
		"nested nor": WithExtendedFilters([]map[string]any{
			{"$match": map[string]any{opOr: []any{
				map[string]any{"$nor": []any{
					map[string]any{"healthevent.checkname": "custom-rule"},
				}},
			}}},
		}),
		"later nor": WithExtendedFilterPrefix([]map[string]any{
			{"$match": map[string]any{"healthevent.nodename": "node-a"}},
			{"$match": map[string]any{"$nor": []any{
				map[string]any{"healthevent.checkname": "custom-rule"},
			}}},
		}, 1),
		"later array equality": WithExtendedFilterPrefix([]map[string]any{
			{"$match": map[string]any{"healthevent.nodename": "node-a"}},
			{"$match": map[string]any{"healthevent.errorcode": []any{"94"}}},
		}, 1),
	}

	for name, pipeline := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := buildResolvedAggregationQueryForTest(t, client, pipeline)
			if err == nil {
				t.Fatal("unsupported PostgreSQL $match shape accepted")
			}
			if !datastore.IsDeterministicError(err) {
				t.Fatalf("unsupported shape classified as transient: %v", err)
			}
		})
	}
}

func TestPostCountMatchCombinesAllOperators(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}
	query, args, err := client.buildAggregationQuery([]map[string]any{
		{"$count": "count"},
		{"$match": map[string]any{"count": map[string]any{"$gte": 2, "$lte": 5}}},
	}, PipelineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "(document->>'count')::bigint >= $1 AND (document->>'count')::bigint <= $2") {
		t.Fatalf("post-count bounds were not combined: %s", query)
	}
	if !reflect.DeepEqual(args, []any{2, 5}) {
		t.Fatalf("args = %#v, want [2 5]", args)
	}
}

func TestPostCountMatch_MultipleFields_OrdersFieldsDeterministically(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}
	wantQuery := "(document->>'critical')::bigint >= $1 AND (document->>'total')::bigint <= $2"
	wantArgs := []any{2, 5}

	for range 20 {
		query, args, err := client.buildAggregationQuery([]map[string]any{
			{"$count": "count"},
			{"$match": map[string]any{
				"total":    map[string]any{"$lte": 5},
				"critical": map[string]any{"$gte": 2},
			}},
		}, PipelineOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(query, wantQuery) {
			t.Fatalf("post-count fields are not ordered: %s", query)
		}
		if !reflect.DeepEqual(args, wantArgs) {
			t.Fatalf("args = %#v, want %#v", args, wantArgs)
		}
	}
}

func TestPostCountMatchRejectsUnsupportedShapes(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}
	tests := map[string]map[string]any{
		"logical operator": {
			opOr: []any{map[string]any{"count": map[string]any{opGTE: 5}}},
		},
		"expression": {
			"$expr": map[string]any{opGTE: []any{"$count", 5}},
		},
		"array equality": {
			"count": []any{5, 6},
		},
		"array comparison": {
			"count": map[string]any{opGTE: []any{5}},
		},
		"unsupported in": {
			"count": map[string]any{opIn: []any{5, 6}},
		},
		"invalid exists": {
			"count": map[string]any{opExists: "yes"},
		},
	}

	for name, match := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := client.buildAggregationQuery([]map[string]any{
				{"$count": "count"},
				{"$match": match},
			}, PipelineOptions{})
			if err == nil {
				t.Fatal("unsupported post-count $match accepted")
			}
			if !datastore.IsDeterministicError(err) {
				t.Fatalf("post-count validation error classified as transient: %v", err)
			}
		})
	}
}

func TestPostCountMatchNeverDropsNonEmptyFilter(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}
	query, _, err := client.buildAggregationQuery([]map[string]any{
		{"$count": "count"},
		{"$match": map[string]any{"count": map[string]any{opGTE: 5}}},
	}, PipelineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "count_result WHERE") {
		t.Fatalf("post-count filter produced an unfiltered count query: %s", query)
	}
}

func TestExprBuilderClassifiesMalformedShapesDeterministically(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}
	tests := map[string]any{
		"bad filter condition": map[string]any{
			opEQ: []any{
				map[string]any{"$filter": map[string]any{"input": "$items", "cond": "invalid"}},
				1,
			},
		},
		"bad map expression": map[string]any{
			opEQ: []any{
				map[string]any{"$map": map[string]any{"input": "$items", "in": []any{"invalid"}}},
				1,
			},
		},
		"multiple operators": map[string]any{
			opGT: []any{"$count", 1},
			opLT: []any{"$count", 9},
		},
	}

	for name, expr := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := client.buildExprCondition(expr)
			if err == nil {
				t.Fatal("malformed $expr accepted")
			}
			if !datastore.IsDeterministicError(err) {
				t.Fatalf("malformed $expr classified as transient: %v", err)
			}
		})
	}
}

func TestUnsupportedAggregationStageIsDeterministic(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}
	_, _, err := client.buildAggregationQuery([]map[string]any{
		{"$project": map[string]any{"healthevent": 1}},
	}, PipelineOptions{})
	if err == nil {
		t.Fatal("unsupported PostgreSQL aggregation stage accepted")
	}
	if !datastore.IsDeterministicError(err) {
		t.Fatalf("unsupported stage classified as transient: %v", err)
	}
}

func TestExtendedEmptyMatchUsesTrueClause(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}
	query, args, err := client.buildAggregationQuery([]map[string]any{
		{"$match": map[string]any{}},
	}, PipelineOptions{EnableExtendedFilters: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "WHERE TRUE") || len(args) != 0 {
		t.Fatalf("query = %s, args = %#v", query, args)
	}
}

func TestAggregationRecoveryBoundaryUsesCreatedAtColumn(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}
	cutoff := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	query, args, err := client.buildAggregationQuery([]map[string]any{
		{"$match": map[string]any{"createdAt": map[string]any{"$gt": cutoff}}},
	}, PipelineOptions{EnableExtendedFilters: true})
	if err != nil {
		t.Fatalf("buildAggregationQuery() error = %v", err)
	}

	if !strings.Contains(query, "created_at > $1") {
		t.Fatalf("query does not use typed created_at boundary: %s", query)
	}

	if len(args) != 1 || !args[0].(time.Time).Equal(cutoff) {
		t.Fatalf("args = %v, want [%s]", args, cutoff)
	}
}

func TestAggregationRecoveryBoundarySupportsNanosecondEventTime(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}

	query, _, err := client.buildAggregationQuery([]map[string]any{
		{"$match": map[string]any{
			"$expr": map[string]any{
				"$or": []any{
					map[string]any{
						"$gt": []any{"$healthevent.generatedtimestamp.seconds", int64(100)},
					},
					map[string]any{
						"$and": []any{
							map[string]any{
								"$eq": []any{"$healthevent.generatedtimestamp.seconds", int64(100)},
							},
							map[string]any{
								"$gt": []any{"$healthevent.generatedtimestamp.nanos", int64(250)},
							},
						},
					},
				},
			},
		}},
	}, PipelineOptions{EnableExtendedFilters: true})
	if err != nil {
		t.Fatalf("buildAggregationQuery() error = %v", err)
	}

	for _, expected := range []string{"generatedTimestamp'->>'seconds')::bigint > 100",
		"generatedTimestamp'->>'nanos')::bigint > 250", " OR ", " AND "} {
		if !strings.Contains(query, expected) {
			t.Fatalf("query does not contain %q: %s", expected, query)
		}
	}
}

func TestAggregationSupportsAnalyzerMandatoryLogicalFilter(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}

	query, args, err := client.buildAggregationQuery([]map[string]any{
		{"$match": map[string]any{
			"healthevent.agent":     map[string]any{"$ne": "health-events-analyzer"},
			"healthevent.ishealthy": false,
			"$or": []any{
				map[string]any{"healthevent.processingstrategy": int32(1)},
				map[string]any{"healthevent.processingstrategy": int32(2)},
				map[string]any{
					"healthevent.processingstrategy": map[string]any{"$exists": false},
				},
			},
		}},
	}, PipelineOptions{EnableExtendedFilters: true})
	if err != nil {
		t.Fatalf("buildAggregationQuery() error = %v", err)
	}

	for _, expected := range []string{" OR ", "IS NULL", "document->'healthevent'->>'processingStrategy'"} {
		if !strings.Contains(query, expected) {
			t.Fatalf("query does not contain %q: %s", expected, query)
		}
	}
	if !strings.Contains(query, "document->'healthevent'->>'agent' IS DISTINCT FROM") {
		t.Fatalf("agent exclusion is not null-safe: %s", query)
	}

	for _, arg := range args {
		if _, isMap := arg.(map[string]any); isMap {
			t.Fatalf("SQL argument must not contain a logical-filter map: %#v", args)
		}
	}

	wantArgs := []any{"1", "2", "health-events-analyzer", false}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", args, wantArgs)
	}
}

// TestBuildUpdateClause tests update translation logic
func TestBuildUpdateClause(t *testing.T) {
	client := &PostgreSQLClient{}

	tests := []struct {
		name        string
		update      any
		expectError bool
	}{
		{
			name: "simple $set",
			update: map[string]any{
				"$set": map[string]any{
					"status": "Active",
				},
			},
			expectError: false,
		},
		{
			name: "nested $set",
			update: map[string]any{
				"$set": map[string]any{
					"healthevent.status": "Resolved",
				},
			},
			expectError: false,
		},
		{
			name: "missing $set",
			update: map[string]any{
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

func TestUpdateDocumentOffsetsWherePlaceholdersAfterUpdateArgs(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sql mock: %v", err)
	}
	defer db.Close()

	client := &PostgreSQLClient{
		db:    db,
		table: "health_events",
	}

	filter := map[string]any{
		"status": "old",
	}
	update := map[string]any{
		"$set": map[string]any{
			"status": "new",
		},
	}

	expectedSQL := "UPDATE health_events SET document = jsonb_set(document, '{status}', $1), updated_at = NOW() " +
		"WHERE document->>'status' = $2"
	mock.ExpectExec(regexp.QuoteMeta(expectedSQL)).
		WithArgs(`"new"`, "old").
		WillReturnResult(sqlmock.NewResult(0, 1))

	result, err := client.UpdateDocument(context.Background(), filter, update)
	if err != nil {
		t.Fatalf("UpdateDocument returned error: %v", err)
	}

	if result.MatchedCount != 1 || result.ModifiedCount != 1 {
		t.Fatalf("expected one matched and modified document, got matched=%d modified=%d",
			result.MatchedCount, result.ModifiedCount)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestAdjustParameterNumbersHandlesMultiDigitPlaceholders(t *testing.T) {
	client := &PostgreSQLClient{}

	clause := "document->>'field1' = $1 AND document->>'field10' = $10"
	expected := "document->>'field1' = $3 AND document->>'field10' = $12"

	if got := client.adjustParameterNumbers(clause, 2); got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}
}

// TestUpdateDocumentStatusFields_SharedParent_InitializesParentOnce verifies
// that sibling status updates share one bounded parent initialization chain.
func TestUpdateDocumentStatusFields_SharedParent_InitializesParentOnce(t *testing.T) {
	var executedSQL string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(
		func(_ string, actual string) error {
			executedSQL = actual

			return nil
		},
	)))
	require.NoError(t, err)
	defer db.Close()

	client := &PostgreSQLClient{db: db, table: "health_events"}
	mock.ExpectExec("capture").WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, client.UpdateDocumentStatusFields(context.Background(), "event-id", map[string]any{
		"healtheventstatus.alpha": "a",
		"healtheventstatus.beta":  "b",
		"healtheventstatus.gamma": "c",
	}))
	require.NoError(t, mock.ExpectationsWereMet())
	assert.Equal(t, 3, strings.Count(executedSQL, "'{healtheventstatus}'"), executedSQL)
	assert.Less(t, len(executedSQL), 1000, executedSQL)
}

// TestAggregationPipelineConversion tests aggregation pipeline parsing
func TestAggregationPipelineConversion(t *testing.T) {
	client := &PostgreSQLClient{table: "health_events"}

	tests := []struct {
		name        string
		stages      []map[string]any
		expectError bool
		errorOp     string
	}{
		{
			name: "basic $match and $sort",
			stages: []map[string]any{
				{"$match": map[string]any{"nodeName": "node-1"}},
				{"$sort": map[string]any{"created_at": -1}},
			},
			expectError: false,
		},
		{
			name: "$limit and $skip",
			stages: []map[string]any{
				{"$limit": 10},
				{"$skip": 5},
			},
			expectError: false,
		},
		{
			name: "basic $group support",
			stages: []map[string]any{
				{"$group": map[string]any{"_id": "$nodeName"}},
			},
			expectError: false,
		},
		{
			name: "supported $setWindowFields with basic spec",
			stages: []map[string]any{
				{
					"$setWindowFields": map[string]any{
						"sortBy": map[string]any{
							"healthevent.generatedtimestamp.seconds": 1,
						},
						"output": map[string]any{
							"allPreviousEvents": map[string]any{
								"$push":  "$$ROOT",
								"window": map[string]any{"documents": []any{"unbounded", -1}},
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
			_, _, err := client.buildAggregationQuery(tt.stages, PipelineOptions{})

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
		stages          []map[string]any
		expectedInQuery []string // Substrings expected in generated SQL
	}{
		{
			name: "$push with unbounded preceding",
			stages: []map[string]any{
				{
					"$setWindowFields": map[string]any{
						"sortBy": map[string]any{
							"healthevent.generatedtimestamp.seconds": 1,
						},
						"output": map[string]any{
							"allPreviousEvents": map[string]any{
								"$push":  "$$ROOT",
								"window": map[string]any{"documents": []any{"unbounded", -1}},
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
			stages: []map[string]any{
				{
					"$setWindowFields": map[string]any{
						"sortBy": map[string]any{
							"healthevent.generatedtimestamp.seconds": 1,
						},
						"output": map[string]any{
							"burstId": map[string]any{
								"$sum":   1,
								"window": map[string]any{"documents": []any{"unbounded", "current"}},
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
			stages: []map[string]any{
				{
					"$setWindowFields": map[string]any{
						"sortBy": map[string]any{
							"_id.burstId": 1,
						},
						"output": map[string]any{
							"maxBurstId": map[string]any{
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
			query, _, err := client.buildAggregationQuery(tt.stages, PipelineOptions{})
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
		expr        any
		expectedSQL string
		expectError bool
	}{
		// Test $and operator
		{
			name: "$and with two boolean fields",
			expr: map[string]any{
				"$and": []any{
					"$isStickyXid",
					"$stickyXidWithin3Hours",
				},
			},
			expectedSQL: "((document->>'isStickyXid')::bigint AND (document->>'stickyXidWithin3Hours')::bigint)",
			expectError: false,
		},
		{
			name: "$and with comparison expressions",
			expr: map[string]any{
				"$and": []any{
					map[string]any{
						"$eq": []any{"$status", "$expectedStatus"},
					},
					map[string]any{
						"$eq": []any{"$count", 5},
					},
				},
			},
			expectedSQL: "(((document->>'status')::bigint = (document->>'expectedStatus')::bigint) AND ((document->>'count')::bigint = 5))",
			expectError: false,
		},
		{
			name: "$and with single expression",
			expr: map[string]any{
				"$and": []any{
					"$isStickyXid",
				},
			},
			expectedSQL: "((document->>'isStickyXid')::bigint)",
			expectError: false,
		},
		{
			name: "$and with empty array",
			expr: map[string]any{
				"$and": []any{},
			},
			expectedSQL: "",
			expectError: true,
		},

		// Test $anyElementTrue operator
		{
			name: "$anyElementTrue with map result",
			expr: map[string]any{
				"$anyElementTrue": map[string]any{
					"$map": map[string]any{
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
			expr: map[string]any{
				"$lte": []any{
					map[string]any{
						"$subtract": []any{
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
			expr: map[string]any{
				"$lte": []any{
					"$value1",
					"$value2",
				},
			},
			expectedSQL: "((document->>'value1')::bigint <= (document->>'value2')::bigint)",
			expectError: false,
		},
		{
			name: "$lte with single operand - should fail",
			expr: map[string]any{
				"$lte": []any{
					"$value1",
				},
			},
			expectedSQL: "",
			expectError: true,
		},
		{
			name: "$lte with three operands - should fail",
			expr: map[string]any{
				"$lte": []any{
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
			expr: map[string]any{
				"$subtract": []any{
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
			expr: map[string]any{
				"$subtract": []any{
					"$count",
					5,
				},
			},
			expectedSQL: "((document->>'count')::bigint - 5)",
			expectError: false,
		},
		{
			name: "$subtract with nested expressions",
			expr: map[string]any{
				"$subtract": []any{
					map[string]any{
						"$subtract": []any{
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
			expr: map[string]any{
				"$subtract": []any{
					"$value1",
				},
			},
			expectedSQL: "",
			expectError: true,
		},

		// Test combined operators
		{
			name: "$and with $lte and $subtract",
			expr: map[string]any{
				"$and": []any{
					"$isStickyXid",
					map[string]any{
						"$lte": []any{
							map[string]any{
								"$subtract": []any{
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
			expr: map[string]any{
				"$in": []any{
					"79", // Literal string value (resolved from this.healthevent.errorcode.0)
					"$uniqueXidsInBurst",
				},
			},
			expectedSQL: "@> to_jsonb('79')", // Uses PostgreSQL containment operator
			expectError: false,
		},
		{
			name: "$in with string literal containing special chars",
			expr: map[string]any{
				"$in": []any{
					"test'value", // String with single quote (should be escaped)
					"$arrayField",
				},
			},
			expectedSQL: "@> to_jsonb('test''value')", // Single quote is escaped
			expectError: false,
		},
		{
			name: "$in with field reference and literal array",
			expr: map[string]any{
				"$in": []any{
					map[string]any{
						"$arrayElemAt": []any{
							"$healthevent.errorcode",
							0,
						},
					},
					[]any{"74", "79", "95", "109", "119"},
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
		stages          []map[string]any
		expectedInQuery []string
	}{
		{
			name: "$addFields with $and operator",
			stages: []map[string]any{
				{
					"$addFields": map[string]any{
						"isValid": map[string]any{
							"$and": []any{
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
			stages: []map[string]any{
				{
					"$addFields": map[string]any{
						"timeDiffOk": map[string]any{
							"$lte": []any{
								map[string]any{
									"$subtract": []any{
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
			stages: []map[string]any{
				{
					"$addFields": map[string]any{
						"hasActiveElement": map[string]any{
							"$anyElementTrue": map[string]any{
								"$map": map[string]any{
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
			query, _, err := client.buildAggregationQuery(tt.stages, PipelineOptions{})
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
	stages := []map[string]any{
		{"$match": map[string]any{"healthevent.nodename": "test-node"}},
		{"$match": map[string]any{"healthevent.isfatal": true}},
		{"$count": "count"},
		{"$match": map[string]any{"count": map[string]any{"$gte": 5}}},
	}

	query, args, err := client.buildAggregationQuery(stages, PipelineOptions{})
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

	stages := []map[string]any{
		{"$match": map[string]any{"healthevent.nodename": "nonexistent-node"}},
		{"$count": "count"},
		{"$match": map[string]any{"count": map[string]any{"$gte": 5}}},
	}

	query, args, err := client.buildAggregationQuery(stages, PipelineOptions{})
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
