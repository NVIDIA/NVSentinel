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

//nolint:goconst // Test file with intentional string repetition for clarity

package postgresql

import (
	"strings"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/store-client/pkg/query"
)

func TestFindOneFilterGeneration(t *testing.T) {
	//nolint:goconst // Test operator strings are clear as literals
	tests := []struct {
		name           string
		filter         map[string]interface{}
		expectedSQL    string
		expectedArgs   []interface{}
		expectError    bool
	}{
		{
			name: "simple equality",
			filter: map[string]interface{}{
				"healthevent.nodename": "test-node-1",
			},
			expectedSQL: "COALESCE(document->'healthevent'->>'nodename', " +
				"document->'healthevent'->>'nodeName') = $1",
			expectedArgs: []interface{}{"test-node-1"},
			expectError:  false,
		},
		{
			name: "single operator - $in",
			filter: map[string]interface{}{
				"healtheventstatus.nodequarantined": map[string]interface{}{
					"$in": []interface{}{"Quarantined", "UnQuarantined"},
				},
			},
			expectedSQL: "COALESCE(document->'healtheventstatus'->>'nodequarantined', " +
				"document->'healtheventstatus'->>'nodeQuarantined') IN ($1, $2)",
			expectedArgs: []interface{}{"Quarantined", "UnQuarantined"},
			expectError:  false,
		},
		{
			name: "single operator - $gte",
			filter: map[string]interface{}{
				"createdAt": map[string]interface{}{
					"$gte": time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			expectedSQL:  "created_at >= $1",
			expectedArgs: []interface{}{time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
			expectError:  false,
		},
		{
			name: "multiple conditions - equality + $in + $gte",
			filter: map[string]interface{}{
				"healthevent.nodename": "test-node-1",
				"healtheventstatus.nodequarantined": map[string]interface{}{
					"$in": []interface{}{"Quarantined", "AlreadyQuarantined"},
				},
				"createdAt": map[string]interface{}{
					"$gte": time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			// Note: The actual SQL will have conditions in map iteration order
			// We'll check that all parts are present
			expectedSQL:  "", // Will check components individually
			expectedArgs: nil,
			expectError:  false,
		},
		{
			name: "real-world case - CancelLatestQuarantiningEvents filter",
			filter: map[string]interface{}{
				"healthevent.nodename": "kwok-kata-test-node-1",
				"healtheventstatus.nodequarantined": map[string]interface{}{
					"$in": []interface{}{"Quarantined", "UnQuarantined"},
				},
			},
			// Check for COALESCE in the output for dual-case support
			expectedSQL:  "", // Will check components individually
			expectedArgs: nil,
			expectError:  false,
		},
		{
			name: "invalid operator",
			filter: map[string]interface{}{
				"field": map[string]interface{}{
					"$invalid": "value",
				},
			},
			expectedSQL:  "",
			expectedArgs: nil,
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build conditions using the same logic as FindOne
			var conditions []query.Condition

			for key, value := range tt.filter {
				if valueMap, isMap := value.(map[string]interface{}); isMap {
					for op, opValue := range valueMap {
						var cond query.Condition

						//nolint:goconst // Test MongoDB operator strings are clear as literals
						switch op {
						case "$ne":
							cond = query.Ne(key, opValue)
						case "$eq":
							cond = query.Eq(key, opValue)
						case "$gt":
							cond = query.Gt(key, opValue)
						case "$gte":
							cond = query.Gte(key, opValue)
						case "$lt":
							cond = query.Lt(key, opValue)
						case "$lte":
							cond = query.Lte(key, opValue)
						case "$in":
							if inValues, ok := opValue.([]interface{}); ok {
								cond = query.In(key, inValues)
							} else {
								if !tt.expectError {
									t.Errorf("Expected $in to have []interface{}, got %T", opValue)
								}
								return
							}
						default:
							if !tt.expectError {
								t.Errorf("Unexpected operator: %s", op)
							}
							return
						}

						if cond != nil {
							conditions = append(conditions, cond)
						}
					}
				} else {
					conditions = append(conditions, query.Eq(key, value))
				}
			}

			if tt.expectError {
				return
			}

			// Combine conditions
			var finalCondition query.Condition
			if len(conditions) == 1 {
				finalCondition = conditions[0]
			} else if len(conditions) > 1 {
				finalCondition = query.And(conditions...)
			}

			builder := query.New().Build(finalCondition)
			whereClause, args := builder.ToSQL()

			t.Logf("Generated SQL: %s", whereClause)
			t.Logf("Generated Args: %v", args)

			if tt.name == "multiple conditions - equality + $in + $gte" {
				// For multiple conditions, check that all parts are present
				// The exact order depends on map iteration
				if len(args) != 4 { // nodename=1 + in(2,3) + gte(4)
					t.Errorf("Expected 4 args, got %d: %v", len(args), args)
				}

				// Check that WHERE clause contains all expected parts
				expectedParts := []string{
					"COALESCE(document->'healthevent'->>'nodename', document->'healthevent'->>'nodeName')",
					"COALESCE(document->'healtheventstatus'->>'nodequarantined', document->'healtheventstatus'->>'nodeQuarantined') IN",
					"created_at >=",
					"AND",
				}

				for _, part := range expectedParts {
					if !strings.Contains(whereClause, part) {
						t.Errorf("Expected WHERE clause to contain '%s', got: %s", part, whereClause)
					}
				}
			} else if tt.name == "real-world case - CancelLatestQuarantiningEvents filter" {
				t.Logf("Real-world filter SQL: %s", whereClause)

				// Check for dual-case COALESCE support
				if !strings.Contains(whereClause, "COALESCE") {
					t.Errorf("Expected WHERE clause to contain COALESCE for dual-case support, got: %s", whereClause)
				}

				// Check both nodename and nodequarantined are present
				if !strings.Contains(whereClause, "nodename") {
					t.Errorf("Expected WHERE clause to contain 'nodename', got: %s", whereClause)
				}

				if !strings.Contains(whereClause, "nodequarantined") {
					t.Errorf("Expected WHERE clause to contain 'nodequarantined', got: %s", whereClause)
				}

				// Should have 3 args: nodename value + 2 IN values
				if len(args) != 3 {
					t.Errorf("Expected 3 args, got %d: %v", len(args), args)
				}
			} else if tt.expectedSQL != "" {
				if whereClause != tt.expectedSQL {
					t.Errorf("Expected SQL: %s\nGot: %s", tt.expectedSQL, whereClause)
				}

				if len(args) != len(tt.expectedArgs) {
					t.Errorf("Expected %d args, got %d: %v", len(tt.expectedArgs), len(args), args)
				}
			}
		})
	}
}

