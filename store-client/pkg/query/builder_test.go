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
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestBuilder_Eq(t *testing.T) {
	tests := []struct {
		name          string
		field         string
		value         any
		expectedMongo map[string]any
		expectedSQL   string
		expectedArgs  []any
	}{
		{
			name:  "simple field equality",
			field: "myField",
			value: "active",
			expectedMongo: map[string]any{
				"myField": "active",
			},
			expectedSQL:  "document->>'myField' = $1",
			expectedArgs: []any{"active"},
		},
		{
			name:  "nested field equality",
			field: "healtheventstatus.nodequarantined",
			value: "Quarantined",
			expectedMongo: map[string]any{
				"healtheventstatus.nodequarantined": "Quarantined",
			},
			expectedSQL:  "COALESCE(document->'healtheventstatus'->>'nodequarantined', document->'healtheventstatus'->>'nodeQuarantined') = $1",
			expectedArgs: []any{"Quarantined"},
		},
		{
			name:  "column field (id)",
			field: "id",
			value: "123",
			expectedMongo: map[string]any{
				"id": "123",
			},
			expectedSQL:  "id = $1",
			expectedArgs: []any{"123"},
		},
		{
			name:  "numeric JSON field equality",
			field: "healthevent.processingstrategy",
			value: int32(1),
			expectedMongo: map[string]any{
				"healthevent.processingstrategy": int32(1),
			},
			expectedSQL:  "document->'healthevent'->>'processingstrategy' = $1",
			expectedArgs: []any{"1"},
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
	expectedMongo := map[string]any{
		"agent": map[string]any{
			"$ne": "health-events-analyzer",
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "document->>'agent' != $1", sql)
	assert.Equal(t, []any{"health-events-analyzer"}, args)
}

func TestBuilder_In(t *testing.T) {
	builder := New().Build(In("myField", []any{"active", "pending"}))

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]any{
		"myField": map[string]any{
			"$in": []any{"active", "pending"},
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "document->>'myField' IN ($1, $2)", sql)
	assert.Equal(t, []any{"active", "pending"}, args)
}

func TestBuilder_In_NativeMongoIDPreserved(t *testing.T) {
	objectID := bson.NewObjectID()
	builder := New().Build(In("_id", []any{objectID}))

	assert.Equal(t, map[string]any{
		"_id": map[string]any{"$in": []any{objectID}},
	}, builder.ToMongo())
}

func TestBuilder_Gt(t *testing.T) {
	builder := New().Build(Gt("count", 10))

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]any{
		"count": map[string]any{
			"$gt": 10,
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "document->>'count' > $1", sql)
	assert.Equal(t, []any{10}, args)
}

func TestBuilder_Gte(t *testing.T) {
	builder := New().Build(Gte("count", 10))

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]any{
		"count": map[string]any{
			"$gte": 10,
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "document->>'count' >= $1", sql)
	assert.Equal(t, []any{10}, args)
}

func TestBuilder_Lt(t *testing.T) {
	builder := New().Build(Lt("count", 100))

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]any{
		"count": map[string]any{
			"$lt": 100,
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "document->>'count' < $1", sql)
	assert.Equal(t, []any{100}, args)
}

func TestBuilder_Lte(t *testing.T) {
	builder := New().Build(Lte("count", 100))

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]any{
		"count": map[string]any{
			"$lte": 100,
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "document->>'count' <= $1", sql)
	assert.Equal(t, []any{100}, args)
}

func TestBuilder_And(t *testing.T) {
	builder := New().Build(
		And(
			Eq("field1", "active"),
			Eq("field2", "critical"),
		),
	)

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]any{
		"field1": "active",
		"field2": "critical",
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "(document->>'field1' = $1) AND (document->>'field2' = $2)", sql)
	assert.Equal(t, []any{"active", "critical"}, args)
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
	expectedMongo := map[string]any{
		"$and": []any{
			map[string]any{
				"count": map[string]any{"$gt": 10},
			},
			map[string]any{
				"count": map[string]any{"$lt": 100},
			},
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "(document->>'count' > $1) AND (document->>'count' < $2)", sql)
	assert.Equal(t, []any{10, 100}, args)
}

func TestBuilder_Or(t *testing.T) {
	builder := New().Build(
		Or(
			Eq("myField", "active"),
			Eq("myField", "pending"),
		),
	)

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	expectedMongo := map[string]any{
		"$or": []any{
			map[string]any{"myField": "active"},
			map[string]any{"myField": "pending"},
		},
	}
	assert.Equal(t, expectedMongo, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "(document->>'myField' = $1) OR (document->>'myField' = $2)", sql)
	assert.Equal(t, []any{"active", "pending"}, args)
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
				In("healtheventstatus.userpodsevictionstatus.status", []any{"", "NotStarted"}),
			),
			// Case 3: AlreadyQuarantined not started
			And(
				Eq("healtheventstatus.nodequarantined", "AlreadyQuarantined"),
				In("healtheventstatus.userpodsevictionstatus.status", []any{"", "NotStarted"}),
			),
		),
	)

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	require.NotNil(t, mongoFilter)
	assert.Contains(t, mongoFilter, "$or")
	orArray := mongoFilter["$or"].([]any)
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
			field:        "myField",
			expectedPath: "document->>'myField'",
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
	assert.Equal(t, map[string]any{}, mongoFilter)

	// Test SQL output
	sql, args := builder.ToSQL()
	assert.Equal(t, "", sql)
	assert.Nil(t, args)
}

func TestBuilder_NilBuilder(t *testing.T) {
	var builder *Builder

	// Test MongoDB output
	mongoFilter := builder.ToMongo()
	assert.Equal(t, map[string]any{}, mongoFilter)

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
			mongoField:   "myField",
			expectedPath: "document->>'myField'",
		},
		{
			name:         "column field (status)",
			mongoField:   "status",
			expectedPath: "status",
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
			name:         "protobuf field casing",
			mongoField:   "healthevent.componentclass",
			expectedPath: "COALESCE(document->'healthevent'->>'componentclass', document->'healthevent'->>'componentClass')",
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
					In("healtheventstatus.userpodsevictionstatus.status", []any{"", "NotStarted"}),
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
				In("healtheventstatus.nodequarantined", []any{"Quarantined", "UnQuarantined"}),
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
		assert.Equal(t, []any{"health-events-analyzer"}, args)
	})
}
