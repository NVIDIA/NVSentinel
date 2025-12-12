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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuilder_Eq(t *testing.T) {
	tests := []struct {
		name          string
		field         string
		value         interface{}
		expectedMongo map[string]interface{}
		expectedSQL   string
		expectedArgs  []interface{}
	}{
		{
			name:  "simple field equality",
			field: "status",
			value: "active",
			expectedMongo: map[string]interface{}{
				"status": "active",
			},
			expectedSQL:  "document->>'status' = $1",
			expectedArgs: []interface{}{"active"},
		},
		{
			name:  "nested field equality",
			field: "healtheventstatus.nodequarantined",
			value: "Quarantined",
			expectedMongo: map[string]interface{}{
				"healtheventstatus.nodequarantined": "Quarantined",
			},
			expectedSQL:  "COALESCE(document->'healtheventstatus'->>'nodequarantined', document->'healtheventstatus'->>'nodeQuarantined') = $1",
			expectedArgs: []interface{}{"Quarantined"},
		},
		{
			name:  "column field (id)",
			field: "id",
			value: "123",
			expectedMongo: map[string]interface{}{
				"id": "123",
			},
			expectedSQL:  "id = $1",
			expectedArgs: []interface{}{"123"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := New().Build(Eq(tt.field, tt.value))

			// Test MongoDB output
			mongoFilter := builder.ToMongo()
			assert.Equal(t, tt.expectedMongo, mongoFilter)

			// Test SQL output
			sql, args := builder.ToSQL()
			assert.Equal(t, tt.expectedSQL, sql)
			assert.Equal(t, tt.expectedArgs, args)
		})
	}
}

func TestBuilder_Ne(t *testing.T) {
	builder := New().Build(Ne("agent", "health-events-analyzer"))

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]interface{}{
		"agent": map[string]interface{}{
			"$ne": "health-events-analyzer",
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "document->>'agent' != $1", sql)
	assert.Equal(t, []interface{}{"health-events-analyzer"}, args)
}

func TestBuilder_In(t *testing.T) {
	builder := New().Build(In("status", []interface{}{"active", "pending"}))

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]interface{}{
		"status": map[string]interface{}{
			"$in": []interface{}{"active", "pending"},
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "document->>'status' IN ($1, $2)", sql)
	assert.Equal(t, []interface{}{"active", "pending"}, args)
}

func TestBuilder_Gt(t *testing.T) {
	builder := New().Build(Gt("count", 10))

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]interface{}{
		"count": map[string]interface{}{
			"$gt": 10,
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "document->>'count' > $1", sql)
	assert.Equal(t, []interface{}{10}, args)
}

func TestBuilder_Gte(t *testing.T) {
	builder := New().Build(Gte("count", 10))

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]interface{}{
		"count": map[string]interface{}{
			"$gte": 10,
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "document->>'count' >= $1", sql)
	assert.Equal(t, []interface{}{10}, args)
}

func TestBuilder_Lt(t *testing.T) {
	builder := New().Build(Lt("count", 100))

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]interface{}{
		"count": map[string]interface{}{
			"$lt": 100,
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "document->>'count' < $1", sql)
	assert.Equal(t, []interface{}{100}, args)
}

func TestBuilder_Lte(t *testing.T) {
	builder := New().Build(Lte("count", 100))

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]interface{}{
		"count": map[string]interface{}{
			"$lte": 100,
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "document->>'count' <= $1", sql)
	assert.Equal(t, []interface{}{100}, args)
}

func TestBuilder_And(t *testing.T) {
	builder := New().Build(
		And(
			Eq("status", "active"),
			Eq("type", "critical"),
		),
	)

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]interface{}{
		"status": "active",
		"type":   "critical",
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "(document->>'status' = $1) AND (document->>'type' = $2)", sql)
	assert.Equal(t, []interface{}{"active", "critical"}, args)
}

func TestBuilder_And_WithConflictingFields(t *testing.T) {
	// When AND has conditions on the same field, must use $and operator
	builder := New().Build(
		And(
			Gt("count", 10),
			Lt("count", 100),
		),
	)

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]interface{}{
		"$and": []interface{}{
			map[string]interface{}{
				"count": map[string]interface{}{"$gt": 10},
			},
			map[string]interface{}{
				"count": map[string]interface{}{"$lt": 100},
			},
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "(document->>'count' > $1) AND (document->>'count' < $2)", sql)
	assert.Equal(t, []interface{}{10, 100}, args)
}

func TestBuilder_Or(t *testing.T) {
	builder := New().Build(
		Or(
			Eq("status", "active"),
			Eq("status", "pending"),
		),
	)

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]interface{}{
		"$or": []interface{}{
			map[string]interface{}{"status": "active"},
			map[string]interface{}{"status": "pending"},
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "(document->>'status' = $1) OR (document->>'status' = $2)", sql)
	assert.Equal(t, []interface{}{"active", "pending"}, args)
}

func TestBuilder_ComplexOr(t *testing.T) {
	// Simulate node-drainer cold start query
	builder := New().Build(
		Or(
			// Case 1: In-progress events
			Eq("healtheventstatus.userpodsevictionstatus.status", "InProgress"),
			// Case 2: Quarantined not started
			And(
				Eq("healtheventstatus.nodequarantined", "Quarantined"),
				In("healtheventstatus.userpodsevictionstatus.status", []interface{}{"", "NotStarted"}),
			),
			// Case 3: AlreadyQuarantined not started
			And(
				Eq("healtheventstatus.nodequarantined", "AlreadyQuarantined"),
				In("healtheventstatus.userpodsevictionstatus.status", []interface{}{"", "NotStarted"}),
			),
		),
	)

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	require.NotNil(t, mongoFilter)
	assert.Contains(t, mongoFilter, "$or")
	orArray := mongoFilter["$or"].([]interface{})
	assert.Len(t, orArray, 3)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Contains(t, sql, "OR")
	assert.Contains(t, sql, "AND")
	assert.Len(t, args, 7) // InProgress + Quarantined + "" + "NotStarted" + AlreadyQuarantined + "" + "NotStarted"
}

func TestBuilder_NestedFieldPaths(t *testing.T) {
	tests := []struct {
		name         string
		field        string
		expectedPath string
	}{
		{
			name:         "single level",
			field:        "status",
			expectedPath: "document->>'status'",
		},
		{
			name:         "two levels",
			field:        "healthevent.isfatal",
			expectedPath: "document->'healthevent'->>'isfatal'",
		},
		{
			name:         "three levels",
			field:        "healtheventstatus.userpodsevictionstatus.status",
			expectedPath: "document->'healtheventstatus'->'userpodsevictionstatus'->>'status'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := New().Build(Eq(tt.field, "test"))
			sql, _ := builder.ToSQL()
			assert.Contains(t, sql, tt.expectedPath)
		})
	}
}

func TestBuilder_EmptyBuilder(t *testing.T) {
	builder := New()

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	assert.Equal(t, map[string]interface{}{}, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "", sql)
	assert.Nil(t, args)
}

func TestBuilder_NilBuilder(t *testing.T) {
	var builder *Builder

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	assert.Equal(t, map[string]interface{}{}, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "", sql)
	assert.Nil(t, args)
}

func TestMongoFieldToJSONB(t *testing.T) {
	tests := []struct {
		name         string
		mongoField   string
		expectedPath string
	}{
		{
			name:         "simple field",
			mongoField:   "status",
			expectedPath: "document->>'status'",
		},
		{
			name:         "column field (id)",
			mongoField:   "id",
			expectedPath: "id",
		},
		{
			name:         "column field (createdAt)",
			mongoField:   "createdAt",
			expectedPath: "created_at",
		},
		{
			name:         "nested two levels",
			mongoField:   "healthevent.isfatal",
			expectedPath: "document->'healthevent'->>'isfatal'",
		},
		{
			name:         "nested three levels",
			mongoField:   "healtheventstatus.nodequarantined",
			expectedPath: "COALESCE(document->'healtheventstatus'->>'nodequarantined', document->'healtheventstatus'->>'nodeQuarantined')",
		},
		{
			name:         "deeply nested",
			mongoField:   "healtheventstatus.userpodsevictionstatus.status",
			expectedPath: "document->'healtheventstatus'->'userpodsevictionstatus'->>'status'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mongoFieldToJSONB(tt.mongoField)
			assert.Equal(t, tt.expectedPath, result)
		})
	}
}

func TestBuilder_RealWorldQueries(t *testing.T) {
	t.Run("node-drainer cold start", func(t *testing.T) {
		builder := New().Build(
			Or(
				Eq("healtheventstatus.userpodsevictionstatus.status", "InProgress"),
				And(
					Eq("healtheventstatus.nodequarantined", "Quarantined"),
					In("healtheventstatus.userpodsevictionstatus.status", []interface{}{"", "NotStarted"}),
				),
			),
		)

		// MongoDB filter should work
		mongoFilter := builder.ToMongo()
		assert.NotNil(t, mongoFilter)
		assert.Contains(t, mongoFilter, "$or")

		// SQL should generate correctly
		sql, args := builder.ToSQL()
		assert.Contains(t, sql, "OR")
		assert.Greater(t, len(args), 0)
	})

	t.Run("fault-quarantine cancellation", func(t *testing.T) {
		builder := New().Build(
			And(
				Eq("healthevent.nodename", "node1"),
				In("healtheventstatus.nodequarantined", []interface{}{"Quarantined", "UnQuarantined"}),
			),
		)

		// MongoDB filter should work
		mongoFilter := builder.ToMongo()
		assert.NotNil(t, mongoFilter)
		assert.Equal(t, "node1", mongoFilter["healthevent.nodename"])

		// SQL should generate correctly
		sql, args := builder.ToSQL()
		assert.Contains(t, sql, "AND")
		assert.Contains(t, sql, "IN")
		assert.Equal(t, 3, len(args)) // node1 + Quarantined + UnQuarantined
	})

	t.Run("health-events-analyzer filter", func(t *testing.T) {
		builder := New().Build(
			Ne("healthevent.agent", "health-events-analyzer"),
		)

		// MongoDB filter should work
		mongoFilter := builder.ToMongo()
		assert.NotNil(t, mongoFilter)
		assert.Contains(t, mongoFilter, "healthevent.agent")

		// SQL should generate correctly
		sql, args := builder.ToSQL()
		assert.Contains(t, sql, "!=")
		assert.Equal(t, []interface{}{"health-events-analyzer"}, args)
	})
}
