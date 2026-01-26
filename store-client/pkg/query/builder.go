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
	"fmt"
	"strings"
	"unicode"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	mongoIDFieldName = "_id"
)

// Builder provides a database-agnostic query builder
// It generates MongoDB filters or PostgreSQL WHERE clauses from the same API
type Builder struct {
	root Condition
}

// Condition represents a query condition that can be converted to MongoDB or SQL
type Condition interface {
	// ToMongo converts the condition to MongoDB filter format
	ToMongo() map[string]interface{}

	// ToSQL converts the condition to PostgreSQL WHERE clause
	// Returns the SQL string and parameter values
	ToSQL(paramNum int) (string, []interface{}, int)
}

// New creates a new query builder
func New() *Builder {
	return &Builder{}
}

// Build sets the root condition
func (b *Builder) Build(cond Condition) *Builder {
	b.root = cond
	return b
}

// ToMongo generates a MongoDB filter
func (b *Builder) ToMongo() map[string]interface{} {
	if b == nil || b.root == nil {
		return map[string]interface{}{}
	}

	return b.root.ToMongo()
}

// ToSQL generates a PostgreSQL WHERE clause
func (b *Builder) ToSQL() (string, []interface{}) {
	if b == nil || b.root == nil {
		return "", nil
	}

	sql, args, _ := b.root.ToSQL(1)

	return sql, args
}

// ToSQLWithOffset generates SQL with parameter numbering starting from the given offset
func (b *Builder) ToSQLWithOffset(startParam int) (string, []interface{}) {
	if b == nil || b.root == nil {
		return "", nil
	}

	sql, args, _ := b.root.ToSQL(startParam)

	return sql, args
}

// --- Comparison Operators ---

// Eq creates an equality condition (field = value)
func Eq(field string, value interface{}) Condition {
	return &eqCondition{field: field, value: value}
}

// Ne creates a not-equal condition (field != value)
func Ne(field string, value interface{}) Condition {
	return &neCondition{field: field, value: value}
}

// Gt creates a greater-than condition (field > value)
func Gt(field string, value interface{}) Condition {
	return &gtCondition{field: field, value: value}
}

// Gte creates a greater-than-or-equal condition (field >= value)
func Gte(field string, value interface{}) Condition {
	return &gteCondition{field: field, value: value}
}

// Lt creates a less-than condition (field < value)
func Lt(field string, value interface{}) Condition {
	return &ltCondition{field: field, value: value}
}

// Lte creates a less-than-or-equal condition (field <= value)
func Lte(field string, value interface{}) Condition {
	return &lteCondition{field: field, value: value}
}

// In creates an IN condition (field IN (values...))
func In(field string, values []interface{}) Condition {
	return &inCondition{field: field, values: values}
}

// --- Logical Operators ---

// And creates an AND condition
func And(conditions ...Condition) Condition {
	return &andCondition{conditions: conditions}
}

// Or creates an OR condition
func Or(conditions ...Condition) Condition {
	return &orCondition{conditions: conditions}
}

// --- Equality Condition ---

type eqCondition struct {
	field string
	value interface{}
}

func (c *eqCondition) ToMongo() map[string]interface{} {
	if c.field == mongoIDFieldName {
		c.value = convertToMongoObject(c.value)
	}

	return map[string]interface{}{
		c.field: c.value,
	}
}

func (c *eqCondition) ToSQL(paramNum int) (string, []interface{}, int) {
	// Convert field path to JSONB syntax if needed
	sqlField := mongoFieldToJSONB(c.field)

	// Handle nil value specially - PostgreSQL needs IS NULL, not = NULL
	if c.value == nil {
		sql := fmt.Sprintf("%s IS NULL", sqlField)
		return sql, nil, paramNum
	}

	sql := fmt.Sprintf("%s = $%d", sqlField, paramNum)

	return sql, []interface{}{c.value}, paramNum + 1
}

// --- Not-Equal Condition ---

type neCondition struct {
	field string
	value interface{}
}

func (c *neCondition) ToMongo() map[string]interface{} {
	if c.field == mongoIDFieldName {
		c.value = convertToMongoObject(c.value)
	}

	return map[string]interface{}{
		c.field: map[string]interface{}{"$ne": c.value},
	}
}

func (c *neCondition) ToSQL(paramNum int) (string, []interface{}, int) {
	sqlField := mongoFieldToJSONB(c.field)

	// Handle nil value specially - PostgreSQL needs IS NOT NULL, not != NULL
	if c.value == nil {
		sql := fmt.Sprintf("%s IS NOT NULL", sqlField)
		return sql, nil, paramNum
	}

	sql := fmt.Sprintf("%s != $%d", sqlField, paramNum)

	return sql, []interface{}{c.value}, paramNum + 1
}

// --- Greater-Than Condition ---

type gtCondition struct {
	field string
	value interface{}
}

func (c *gtCondition) ToMongo() map[string]interface{} {
	if c.field == mongoIDFieldName {
		c.value = convertToMongoObject(c.value)
	}

	return map[string]interface{}{
		c.field: map[string]interface{}{"$gt": c.value},
	}
}

func (c *gtCondition) ToSQL(paramNum int) (string, []interface{}, int) {
	sqlField := mongoFieldToJSONB(c.field)

	sql := fmt.Sprintf("%s > $%d", sqlField, paramNum)

	return sql, []interface{}{c.value}, paramNum + 1
}

// --- Greater-Than-Or-Equal Condition ---

type gteCondition struct {
	field string
	value interface{}
}

func (c *gteCondition) ToMongo() map[string]interface{} {
	if c.field == mongoIDFieldName {
		c.value = convertToMongoObject(c.value)
	}

	return map[string]interface{}{
		c.field: map[string]interface{}{"$gte": c.value},
	}
}

func (c *gteCondition) ToSQL(paramNum int) (string, []interface{}, int) {
	sqlField := mongoFieldToJSONB(c.field)

	sql := fmt.Sprintf("%s >= $%d", sqlField, paramNum)

	return sql, []interface{}{c.value}, paramNum + 1
}

// --- Less-Than Condition ---

type ltCondition struct {
	field string
	value interface{}
}

func (c *ltCondition) ToMongo() map[string]interface{} {
	if c.field == mongoIDFieldName {
		c.value = convertToMongoObject(c.value)
	}

	return map[string]interface{}{
		c.field: map[string]interface{}{"$lt": c.value},
	}
}

func (c *ltCondition) ToSQL(paramNum int) (string, []interface{}, int) {
	sqlField := mongoFieldToJSONB(c.field)

	sql := fmt.Sprintf("%s < $%d", sqlField, paramNum)

	return sql, []interface{}{c.value}, paramNum + 1
}

// --- Less-Than-Or-Equal Condition ---

type lteCondition struct {
	field string
	value interface{}
}

func (c *lteCondition) ToMongo() map[string]interface{} {
	if c.field == mongoIDFieldName {
		c.value = convertToMongoObject(c.value)
	}

	return map[string]interface{}{
		c.field: map[string]interface{}{"$lte": c.value},
	}
}

func (c *lteCondition) ToSQL(paramNum int) (string, []interface{}, int) {
	sqlField := mongoFieldToJSONB(c.field)

	sql := fmt.Sprintf("%s <= $%d", sqlField, paramNum)

	return sql, []interface{}{c.value}, paramNum + 1
}

// --- IN Condition ---

type inCondition struct {
	field  string
	values []interface{}
}

func (c *inCondition) ToMongo() map[string]interface{} {
	values := c.values
	if c.field == mongoIDFieldName {
		values = nil
		for _, value := range c.values {
			values = append(values, convertToMongoObject(value))
		}
	}

	return map[string]interface{}{
		c.field: map[string]interface{}{"$in": values},
	}
}

func (c *inCondition) ToSQL(paramNum int) (string, []interface{}, int) {
	sqlField := mongoFieldToJSONB(c.field)

	// Build placeholders ($1, $2, $3, ...)
	placeholders := make([]string, len(c.values))
	args := make([]interface{}, len(c.values))

	for i, val := range c.values {
		placeholders[i] = fmt.Sprintf("$%d", paramNum+i)
		args[i] = val
	}

	sql := fmt.Sprintf("%s IN (%s)", sqlField, strings.Join(placeholders, ", "))

	return sql, args, paramNum + len(c.values)
}

// --- AND Condition ---

type andCondition struct {
	conditions []Condition
}

func (c *andCondition) ToMongo() map[string]interface{} {
	if len(c.conditions) == 0 {
		return map[string]interface{}{}
	}

	// For AND, we can merge conditions into a single map if they're all field conditions
	// Otherwise, use $and
	result := make(map[string]interface{})
	needsAndOperator := false

	// Try to merge conditions, detecting conflicts
	for _, cond := range c.conditions {
		condMap := cond.ToMongo()

		// Check if this condition has multiple keys or uses operators
		if len(condMap) > 1 {
			needsAndOperator = true
			break
		}

		// Check for field conflicts (same field appears in multiple conditions)
		for key := range condMap {
			if _, exists := result[key]; exists {
				needsAndOperator = true
				break
			}
		}

		if needsAndOperator {
			break
		}

		// No conflict yet, add to result
		for k, v := range condMap {
			result[k] = v
		}
	}

	if needsAndOperator {
		andArray := make([]interface{}, len(c.conditions))
		for i, cond := range c.conditions {
			andArray[i] = cond.ToMongo()
		}

		return map[string]interface{}{"$and": andArray}
	}

	return result
}

func (c *andCondition) ToSQL(paramNum int) (string, []interface{}, int) {
	if len(c.conditions) == 0 {
		return "TRUE", nil, paramNum
	}

	var sqlParts []string

	var allArgs []interface{}

	currentParam := paramNum

	for _, cond := range c.conditions {
		sql, args, nextParam := cond.ToSQL(currentParam)
		sqlParts = append(sqlParts, fmt.Sprintf("(%s)", sql))
		allArgs = append(allArgs, args...)
		currentParam = nextParam
	}

	return strings.Join(sqlParts, " AND "), allArgs, currentParam
}

// --- OR Condition ---

type orCondition struct {
	conditions []Condition
}

func (c *orCondition) ToMongo() map[string]interface{} {
	if len(c.conditions) == 0 {
		return map[string]interface{}{}
	}

	orArray := make([]interface{}, len(c.conditions))
	for i, cond := range c.conditions {
		orArray[i] = cond.ToMongo()
	}

	return map[string]interface{}{"$or": orArray}
}

func (c *orCondition) ToSQL(paramNum int) (string, []interface{}, int) {
	if len(c.conditions) == 0 {
		return "FALSE", nil, paramNum
	}

	var sqlParts []string

	var allArgs []interface{}

	currentParam := paramNum

	for _, cond := range c.conditions {
		sql, args, nextParam := cond.ToSQL(currentParam)
		sqlParts = append(sqlParts, fmt.Sprintf("(%s)", sql))
		allArgs = append(allArgs, args...)
		currentParam = nextParam
	}

	return strings.Join(sqlParts, " OR "), allArgs, currentParam
}

// --- Helper Functions ---

// mongoFieldToJSONB converts a MongoDB dot-notation field path to PostgreSQL JSONB syntax
// Example: "healtheventstatus.nodequarantined" -> "data->>'healtheventstatus'->>'nodequarantined'"
//
// For nested fields in JSONB:
// - First level: data->'field1'
// - Nested: data->'field1'->'field2'
// - Final text extraction: data->'field1'->>'field2' (note ->> for final field)
func mongoFieldToJSONB(fieldPath string) string {
	// Handle simple top-level fields that aren't nested
	if !strings.Contains(fieldPath, ".") {
		snakeCaseField := toSnakeCase(fieldPath)

		// If it's a column name (like "createdAt"), use it directly
		// Map _id to id for PostgreSQL compatibility
		if isColumnField(snakeCaseField) {
			// Convert MongoDB field names to PostgreSQL column names (snake_case)
			switch snakeCaseField {
			case mongoIDFieldName:
				return "id"
			default:
				return snakeCaseField
			}
		}

		return fmt.Sprintf("document->>'%s'", fieldPath)
	}

	// Split the path into parts
	parts := strings.Split(fieldPath, ".")

	// Check if final field needs dual-case support
	if needsDualCaseLookup(parts[len(parts)-1]) {
		return buildDualCaseJSONBPath(parts)
	}

	// Build JSONB path
	// All intermediate parts use -> operator
	// Final part uses ->> operator to extract as text
	var jsonbPath strings.Builder
	jsonbPath.WriteString("document")

	for i, part := range parts {
		if i < len(parts)-1 {
			// Intermediate path: use -> to keep as JSONB
			jsonbPath.WriteString(fmt.Sprintf("->'%s'", part))
		} else {
			// Final path: use ->> to extract as text
			jsonbPath.WriteString(fmt.Sprintf("->>'%s'", part))
		}
	}

	return jsonbPath.String()
}

func convertToMongoObject(id interface{}) interface{} {
	idStr := id.(string)

	objID, err := primitive.ObjectIDFromHex(idStr)
	if err != nil {
		return id
	}

	return objID
}

// needsDualCaseLookup checks if a field name needs dual-case support (lowercase and camelCase)
func needsDualCaseLookup(fieldName string) bool {
	// Fields that might be stored in either lowercase or camelCase
	dualCaseFields := map[string]bool{
		"nodename":        true, // Can be nodename or nodeName
		"nodequarantined": true, // Can be nodequarantined or nodeQuarantined
	}

	return dualCaseFields[strings.ToLower(fieldName)]
}

// buildDualCaseJSONBPath builds a COALESCE expression to handle both lowercase and camelCase
func buildDualCaseJSONBPath(parts []string) string {
	var basePath strings.Builder
	basePath.WriteString("document")

	// Build the base path up to the last element
	for i := 0; i < len(parts)-1; i++ {
		basePath.WriteString(fmt.Sprintf("->'%s'", parts[i]))
	}

	lastField := parts[len(parts)-1]
	basePathStr := basePath.String()

	// Try lowercase first, then camelCase
	lowercaseField := strings.ToLower(lastField)
	camelCaseField := toCamelCase(lowercaseField)

	// Return COALESCE to try both cases
	return fmt.Sprintf("COALESCE(%s->>'%s', %s->>'%s')",
		basePathStr, lowercaseField,
		basePathStr, camelCaseField)
}

// toCamelCase converts a lowercase field name to camelCase
func toCamelCase(s string) string {
	// Handle specific known conversions
	switch s {
	case "nodename":
		return "nodeName"
	case "nodequarantined":
		return "nodeQuarantined"
	default:
		// Generic conversion: capitalize first letter after each word boundary
		if len(s) == 0 {
			return s
		}

		return s
	}
}

// isColumnField checks if a field is a table column (not JSONB)
func isColumnField(field string) bool {
	columnFields := map[string]bool{
		// Common fields
		"id":         true,
		"created_at": true,
		"updated_at": true,
		// Note: MongoDB uses _id, but we map it to id column for PostgreSQL
		mongoIDFieldName: true,

		// Health events denormalized columns
		"node_quarantined":           true,
		"user_pods_eviction_status":  true,
		"node_name":                  true,
		"event_type":                 true,
		"severity":                   true,
		"recommended_action":         true,
		"fault_remediated":           true,
		"last_remediation_timestamp": true,

		// Maintenance events denormalized columns (snake_case)
		"event_id":                 true,
		"csp":                      true,
		"cluster_name":             true,
		"status":                   true,
		"csp_status":               true,
		"scheduled_start_time":     true,
		"actual_end_time":          true,
		"event_received_timestamp": true,
		"last_updated_timestamp":   true,
	}

	return columnFields[field]
}

// toSnakeCase converts PascalCase or camelCase strings to snake_case for PostgreSQL
// Examples:
//   - "HealthEvents" -> "health_events"
//   - "MaintenanceEvents" -> "maintenance_events"
//   - "scheduledStartTime" -> "scheduled_start_time"
func toSnakeCase(s string) string {
	if s == "" {
		return s
	}

	var result strings.Builder

	for i, r := range s {
		if unicode.IsUpper(r) {
			// Add underscore before uppercase letters (except first character)
			if i > 0 {
				result.WriteRune('_')
			}

			result.WriteRune(unicode.ToLower(r))
		} else {
			result.WriteRune(r)
		}
	}

	return result.String()
}

// --- Type Conversion Helpers ---
