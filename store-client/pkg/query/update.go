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
	"strings"
)

// UpdateBuilder provides a database-agnostic update builder
// It generates MongoDB update documents or PostgreSQL UPDATE SET clauses from the same API
type UpdateBuilder struct {
	operations []UpdateOperation
}

// UpdateOperation represents a single update operation
type UpdateOperation interface {
	// ToMongo converts the operation to MongoDB update format
	ToMongo() map[string]interface{}

	// ToSQL converts the operation to PostgreSQL UPDATE SET clause
	// Returns the SQL string and parameter values
	ToSQL(paramNum int) (string, []interface{}, int)
}

// NewUpdate creates a new update builder
func NewUpdate() *UpdateBuilder {
	return &UpdateBuilder{
		operations: make([]UpdateOperation, 0),
	}
}

// Set adds a $set operation (field = value)
func (u *UpdateBuilder) Set(field string, value interface{}) *UpdateBuilder {
	u.operations = append(u.operations, &setOperation{field: field, value: value})
	return u
}

// SetMultiple adds multiple $set operations at once
func (u *UpdateBuilder) SetMultiple(updates map[string]interface{}) *UpdateBuilder {
	for field, value := range updates {
		u.operations = append(u.operations, &setOperation{field: field, value: value})
	}

	return u
}

// ToMongo generates a MongoDB update document
func (u *UpdateBuilder) ToMongo() map[string]interface{} {
	if u == nil || len(u.operations) == 0 {
		return map[string]interface{}{}
	}

	// Combine all $set operations into a single $set document
	setDoc := make(map[string]interface{})

	for _, op := range u.operations {
		opMap := op.ToMongo()
		if setMap, ok := opMap["$set"].(map[string]interface{}); ok {
			for k, v := range setMap {
				setDoc[k] = v
			}
		}
	}

	if len(setDoc) == 0 {
		return map[string]interface{}{}
	}

	return map[string]interface{}{
		"$set": setDoc,
	}
}

// ToSQL generates a PostgreSQL UPDATE SET clause
func (u *UpdateBuilder) ToSQL() (string, []interface{}) {
	if u == nil || len(u.operations) == 0 {
		return "", nil
	}

	var setParts []string

	var allArgs []interface{}

	currentParam := 1

	for _, op := range u.operations {
		sql, args, nextParam := op.ToSQL(currentParam)
		setParts = append(setParts, sql)
		allArgs = append(allArgs, args...)
		currentParam = nextParam
	}

	return strings.Join(setParts, ", "), allArgs
}

// --- Set Operation ---

type setOperation struct {
	field string
	value interface{}
}

func (s *setOperation) ToMongo() map[string]interface{} {
	return map[string]interface{}{
		"$set": map[string]interface{}{
			s.field: s.value,
		},
	}
}

func (s *setOperation) ToSQL(paramNum int) (string, []interface{}, int) {
	// For JSONB updates, we need to use jsonb_set for nested paths
	if strings.Contains(s.field, ".") && !isColumnField(s.field) {
		// Nested JSONB field update
		// Convert "healtheventstatus.nodequarantined" to jsonb_set path
		path := mongoFieldToJSONBPath(s.field)
		// Cast the value to jsonb to ensure PostgreSQL treats it as JSONB
		sql := fmt.Sprintf("document = jsonb_set(document, '{%s}', $%d::jsonb)", path, paramNum)

		return sql, []interface{}{toJSONBValue(s.value)}, paramNum + 1
	}

	// Simple column or top-level JSONB field update
	if isColumnField(s.field) {
		sql := fmt.Sprintf("%s = $%d", s.field, paramNum)
		return sql, []interface{}{s.value}, paramNum + 1
	}

	// Top-level JSONB field
	// Cast the value to jsonb to ensure PostgreSQL treats it as JSONB
	sql := fmt.Sprintf("document = jsonb_set(document, '{%s}', $%d::jsonb)", s.field, paramNum)

	return sql, []interface{}{toJSONBValue(s.value)}, paramNum + 1
}

// mongoFieldToJSONBPath converts MongoDB dot notation to JSONB path array
// Example: "healtheventstatus.nodequarantined" -> "healtheventstatus,nodequarantined"
func mongoFieldToJSONBPath(fieldPath string) string {
	// Split by dots and join with commas for JSONB path
	parts := strings.Split(fieldPath, ".")
	return strings.Join(parts, ",")
}

// toJSONBValue converts a Go value to JSONB-compatible format
func toJSONBValue(value interface{}) string {
	switch v := value.(type) {
	case string:
		return fmt.Sprintf("\"%s\"", v)
	case bool:
		return fmt.Sprintf("%t", v)
	case int, int32, int64, uint, uint32, uint64:
		return fmt.Sprintf("%d", v)
	case float32, float64:
		return fmt.Sprintf("%f", v)
	case nil:
		return "null"
	default:
		// For complex types (structs, maps, slices), marshal to JSON
		// This ensures structs like OperationStatus are properly serialized
		// as JSON objects, not as string representations like "{Succeeded }"
		jsonBytes, err := json.Marshal(v)
		if err != nil {
			// Fallback to string representation if marshal fails
			// This maintains backward compatibility for edge cases
			return fmt.Sprintf("\"%v\"", v)
		}

		return string(jsonBytes)
	}
}
