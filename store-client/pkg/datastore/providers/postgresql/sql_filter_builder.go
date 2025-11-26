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
	"fmt"
	"log/slog"
	"strings"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// MongoDB operator constants for SQL filter building.
const (
	opOr     = "$or"
	opIn     = "$in"
	opExists = "$exists"
	opEq     = "$eq"
	opGte    = "$gte"
	opGt     = "$gt"
	opLte    = "$lte"
	opLt     = "$lt"
	opMatch  = "$match"

	// operationColumn is the PostgreSQL column name for operation type.
	// The column stores uppercase values (INSERT, UPDATE, DELETE).
	operationColumn = "operation"
)

// Note: opNe is defined in database_client.go

// SQLFilterBuilder converts MongoDB-style pipeline filters to PostgreSQL WHERE clauses.
// This enables server-side filtering to reduce the volume of events fetched from the database.
// Application-side filtering (PipelineFilter) remains as a fallback for edge cases.
type SQLFilterBuilder struct {
	conditions []string
	args       []interface{}
	argIndex   int
}

// NewSQLFilterBuilder creates a new SQL filter builder.
// startArgIndex should be the next available parameter index (e.g., 4 if $1, $2, $3 are already used).
func NewSQLFilterBuilder(startArgIndex int) *SQLFilterBuilder {
	return &SQLFilterBuilder{
		conditions: make([]string, 0),
		args:       make([]interface{}, 0),
		argIndex:   startArgIndex,
	}
}

// BuildFromPipeline converts a MongoDB-style pipeline to SQL WHERE conditions.
// It extracts $match stages and converts them to PostgreSQL JSONB conditions.
func (b *SQLFilterBuilder) BuildFromPipeline(pipeline interface{}) error {
	if pipeline == nil {
		return nil
	}

	stageList := b.convertToStageList(pipeline)
	if stageList == nil {
		return fmt.Errorf("unsupported pipeline type: %T", pipeline)
	}

	for _, stage := range stageList {
		if err := b.parseStage(stage); err != nil {
			slog.Debug("Skipping pipeline stage for SQL conversion",
				"error", err,
				"stageType", fmt.Sprintf("%T", stage))
		}
	}

	return nil
}

// convertToStageList converts various pipeline types to []interface{}.
func (b *SQLFilterBuilder) convertToStageList(pipeline interface{}) []interface{} {
	switch p := pipeline.(type) {
	case []interface{}:
		return p
	case datastore.Pipeline:
		stageList := make([]interface{}, len(p))
		for i, doc := range p {
			stageList[i] = doc
		}

		return stageList
	default:
		return nil
	}
}

// parseStage parses a single pipeline stage.
func (b *SQLFilterBuilder) parseStage(stage interface{}) error {
	stageMap := b.convertToMap(stage)
	if stageMap == nil {
		return fmt.Errorf("stage is not a map or datastore.Document: %T", stage)
	}

	// Currently only support $match stages
	if matchValue, ok := stageMap[opMatch]; ok {
		return b.parseMatchConditions(matchValue)
	}

	return nil
}

// convertToMap converts various types to map[string]interface{}.
func (b *SQLFilterBuilder) convertToMap(value interface{}) map[string]interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		return v
	case datastore.Document:
		result := make(map[string]interface{})
		for _, elem := range v {
			result[elem.Key] = elem.Value
		}

		return result
	default:
		return nil
	}
}

// parseMatchConditions parses $match conditions into SQL.
func (b *SQLFilterBuilder) parseMatchConditions(match interface{}) error {
	conditions := b.convertToMap(match)
	if conditions == nil {
		return fmt.Errorf("match conditions must be a map, got %T", match)
	}

	for field, value := range conditions {
		if field == opOr {
			if err := b.handleOrOperator(value); err != nil {
				slog.Debug("Skipping $or operator for SQL conversion", "error", err)
			}

			continue
		}

		sqlCond, err := b.fieldToSQL(field, value)
		if err != nil {
			slog.Debug("Skipping field for SQL conversion", "field", field, "error", err)

			continue
		}

		if sqlCond != "" {
			b.conditions = append(b.conditions, sqlCond)
		}
	}

	return nil
}

// handleOrOperator handles $or conditions.
func (b *SQLFilterBuilder) handleOrOperator(value interface{}) error {
	orConditions := b.convertToSlice(value)
	if orConditions == nil {
		return fmt.Errorf("$or value must be an array, got %T", value)
	}

	orParts := b.processOrConditions(orConditions)

	if len(orParts) > 0 {
		b.conditions = append(b.conditions, fmt.Sprintf("(%s)", strings.Join(orParts, " OR ")))
	}

	return nil
}

// convertToSlice converts various array types to []interface{}.
func (b *SQLFilterBuilder) convertToSlice(value interface{}) []interface{} {
	switch v := value.(type) {
	case []interface{}:
		return v
	case datastore.Array:
		result := make([]interface{}, len(v))
		copy(result, v)

		return result
	default:
		return nil
	}
}

// processOrConditions processes each condition in an $or array.
func (b *SQLFilterBuilder) processOrConditions(orConditions []interface{}) []string {
	orParts := make([]string, 0)

	for _, orCond := range orConditions {
		condMap := b.convertToMap(orCond)
		if condMap == nil {
			continue
		}

		branchConditions := b.processBranchConditions(condMap)

		if len(branchConditions) > 0 {
			orParts = append(orParts, b.combineBranchConditions(branchConditions))
		}
	}

	return orParts
}

// processBranchConditions processes conditions within a single OR branch.
func (b *SQLFilterBuilder) processBranchConditions(condMap map[string]interface{}) []string {
	branchConditions := make([]string, 0)

	for field, val := range condMap {
		if field == opOr {
			b.handleNestedOr(val, &branchConditions)

			continue
		}

		sqlCond, err := b.fieldToSQL(field, val)
		if err != nil {
			continue
		}

		if sqlCond != "" {
			branchConditions = append(branchConditions, sqlCond)
		}
	}

	return branchConditions
}

// handleNestedOr handles nested $or within an OR branch.
func (b *SQLFilterBuilder) handleNestedOr(val interface{}, branchConditions *[]string) {
	subBuilder := NewSQLFilterBuilder(b.argIndex)

	if err := subBuilder.handleOrOperator(val); err == nil && subBuilder.HasConditions() {
		*branchConditions = append(*branchConditions, subBuilder.GetWhereClause())
		b.args = append(b.args, subBuilder.GetArgs()...)
		b.argIndex = subBuilder.argIndex
	}
}

// combineBranchConditions combines conditions in an OR branch with AND.
func (b *SQLFilterBuilder) combineBranchConditions(branchConditions []string) string {
	if len(branchConditions) == 1 {
		return branchConditions[0]
	}

	return fmt.Sprintf("(%s)", strings.Join(branchConditions, " AND "))
}

// fieldToSQL converts a field condition to SQL.
// Handles MongoDB-style dot notation: "fullDocument.healthevent.isFatal" -> new_values->'healthevent'->>'isFatal'
func (b *SQLFilterBuilder) fieldToSQL(field string, value interface{}) (string, error) {
	jsonPath := b.fieldToJSONBPath(field)
	if jsonPath == "" {
		return "", fmt.Errorf("could not convert field to JSONB path: %s", field)
	}

	return b.valueToSQL(jsonPath, value)
}

// valueToSQL converts a value to SQL condition.
//
//nolint:cyclop // Switch statement with multiple cases is clearer than splitting
func (b *SQLFilterBuilder) valueToSQL(jsonPath string, value interface{}) (string, error) {
	switch v := value.(type) {
	case bool:
		return b.handleBoolValue(jsonPath, v)
	case string:
		return b.handleStringValue(jsonPath, v)
	case float64:
		return b.handleFloat64Value(jsonPath, v)
	case int:
		return b.handleIntValue(jsonPath, v)
	case map[string]interface{}:
		return b.handleOperators(jsonPath, v)
	case datastore.Document:
		return b.handleOperators(jsonPath, b.convertToMap(v))
	case nil:
		return fmt.Sprintf("%s IS NULL", jsonPath), nil
	case datastore.Array:
		return "", fmt.Errorf("unexpected datastore.Array as field value")
	default:
		return b.handleDefaultValue(jsonPath, v)
	}
}

// handleBoolValue handles boolean field values.
func (b *SQLFilterBuilder) handleBoolValue(jsonPath string, v bool) (string, error) {
	b.argIndex++
	b.args = append(b.args, v)

	if v {
		return fmt.Sprintf("(%s)::boolean = $%d", jsonPath, b.argIndex), nil
	}

	// For false: field is either false OR missing (NULL in JSONB)
	return fmt.Sprintf("((%s)::boolean = $%d OR %s IS NULL)", jsonPath, b.argIndex, jsonPath), nil
}

// handleStringValue handles string field values.
func (b *SQLFilterBuilder) handleStringValue(jsonPath string, v string) (string, error) {
	b.argIndex++

	// The operation column stores uppercase values (INSERT, UPDATE, DELETE)
	// but MongoDB pipelines use lowercase (insert, update)
	if jsonPath == operationColumn {
		b.args = append(b.args, strings.ToUpper(v))
	} else {
		b.args = append(b.args, v)
	}

	// Handle case-insensitive status/message fields within userpodsevictionstatus
	// Due to historical data having both 'status'/'Status' and 'message'/'Message' keys,
	// we need to check for both casings
	if altPath := b.getAlternateCasePath(jsonPath); altPath != "" {
		return fmt.Sprintf("(%s = $%d OR %s = $%d)", jsonPath, b.argIndex, altPath, b.argIndex), nil
	}

	return fmt.Sprintf("%s = $%d", jsonPath, b.argIndex), nil
}

// getAlternateCasePath returns an alternate JSON path with different casing for status/message fields.
// This handles historical data that may have either 'status'/'Status' or 'message'/'Message'.
// Returns empty string if no alternate path is needed.
func (b *SQLFilterBuilder) getAlternateCasePath(jsonPath string) string {
	// Only handle userpodsevictionstatus.status and userpodsevictionstatus.message
	if strings.HasSuffix(jsonPath, "->>'status'") {
		return strings.TrimSuffix(jsonPath, "->>'status'") + "->>'Status'"
	}

	if strings.HasSuffix(jsonPath, "->>'Status'") {
		return strings.TrimSuffix(jsonPath, "->>'Status'") + "->>'status'"
	}

	if strings.HasSuffix(jsonPath, "->>'message'") {
		return strings.TrimSuffix(jsonPath, "->>'message'") + "->>'Message'"
	}

	if strings.HasSuffix(jsonPath, "->>'Message'") {
		return strings.TrimSuffix(jsonPath, "->>'Message'") + "->>'message'"
	}

	return ""
}

// handleFloat64Value handles float64 field values.
func (b *SQLFilterBuilder) handleFloat64Value(jsonPath string, v float64) (string, error) {
	b.argIndex++
	b.args = append(b.args, v)

	return fmt.Sprintf("(%s)::numeric = $%d", jsonPath, b.argIndex), nil
}

// handleIntValue handles int field values.
func (b *SQLFilterBuilder) handleIntValue(jsonPath string, v int) (string, error) {
	b.argIndex++
	b.args = append(b.args, v)

	return fmt.Sprintf("(%s)::integer = $%d", jsonPath, b.argIndex), nil
}

// handleDefaultValue handles default field values.
func (b *SQLFilterBuilder) handleDefaultValue(jsonPath string, v interface{}) (string, error) {
	b.argIndex++
	b.args = append(b.args, fmt.Sprintf("%v", v))

	return fmt.Sprintf("%s = $%d", jsonPath, b.argIndex), nil
}

// fieldNameMapping maps MongoDB bson field names (lowercase) to PostgreSQL JSON field names (camelCase).
// MongoDB uses lowercase bson tags, PostgreSQL uses camelCase json tags.
var fieldNameMapping = map[string]string{
	"ishealthy":              "isHealthy",
	"isfatal":                "isFatal",
	"nodename":               "nodeName",
	"checkname":              "checkName",
	"errorcode":              "errorCode",
	"componentclass":         "componentClass",
	"entitiesimpacted":       "entitiesImpacted",
	"recommendedaction":      "recommendedAction",
	"generatedtimestamp":     "generatedTimestamp",
	"createdat":              "createdAt",
	"updatedat":              "updatedAt",
	"healthevent":            "healthevent",
	"healtheventstatus":      "healtheventstatus",
	"nodequarantined":        "nodequarantined",
	"userpodsevictionstatus": "userpodsevictionstatus",
	"faultremediated":        "faultremediated",
	"status":                 "status",
	"message":                "message",
}

// isOperationTypeField checks if the field is the operationType field.
func (b *SQLFilterBuilder) isOperationTypeField(field string) bool {
	return field == "operationType"
}

// fieldToJSONBPath converts MongoDB dot notation to PostgreSQL JSONB path.
// Handles special cases:
// - "operationType" -> "operation" (column, not JSONB)
// - "fullDocument.*" -> "new_values->'document'->..." (adds document level)
// - Field name case conversion (ishealthy -> isHealthy)
//
// Example: "fullDocument.healthevent.isFatal" -> "new_values->'document'->'healthevent'->>'isFatal'"
func (b *SQLFilterBuilder) fieldToJSONBPath(field string) string {
	// Special case: operationType maps to the operation column, not JSONB
	if b.isOperationTypeField(field) {
		return operationColumn
	}

	parts := strings.Split(field, ".")

	startIdx := 0
	column := "new_values"

	// If the path starts with fullDocument, we need to add 'document' level
	// because PostgreSQL stores the document inside new_values->'document'
	needsDocumentLevel := false

	if len(parts) > 0 && parts[0] == "fullDocument" {
		startIdx = 1
		needsDocumentLevel = true
	}

	if len(parts) <= startIdx {
		if needsDocumentLevel {
			return column + "->'document'"
		}

		return column
	}

	path := column

	// Add the 'document' level if needed
	if needsDocumentLevel {
		path += "->'document'"
	}

	for i := startIdx; i < len(parts); i++ {
		// Convert field name case if needed
		fieldName := b.convertFieldName(parts[i])

		if i == len(parts)-1 {
			path += fmt.Sprintf("->>'%s'", fieldName)
		} else {
			path += fmt.Sprintf("->'%s'", fieldName)
		}
	}

	return path
}

// convertFieldName converts MongoDB bson field names to PostgreSQL JSON field names.
func (b *SQLFilterBuilder) convertFieldName(name string) string {
	lowerName := strings.ToLower(name)
	if mapped, ok := fieldNameMapping[lowerName]; ok {
		return mapped
	}

	// Return original name if no mapping found
	return name
}

// handleOperators handles MongoDB operators like $ne, $in, $exists.
//
//nolint:cyclop // Switch statement with multiple operator cases is clearer than splitting
func (b *SQLFilterBuilder) handleOperators(jsonPath string, ops map[string]interface{}) (string, error) {
	var conditions []string

	for op, val := range ops {
		cond, err := b.handleSingleOperator(jsonPath, op, val)
		if err != nil {
			return "", err
		}

		if cond != "" {
			conditions = append(conditions, cond)
		}
	}

	if len(conditions) == 0 {
		return "", nil
	}

	return strings.Join(conditions, " AND "), nil
}

// handleSingleOperator handles a single MongoDB operator.
func (b *SQLFilterBuilder) handleSingleOperator(jsonPath, op string, val interface{}) (string, error) {
	switch op {
	case opNe:
		return b.handleNeOperator(jsonPath, val)
	case opIn:
		return b.handleInOperator(jsonPath, val)
	case opExists:
		return b.handleExistsOperator(jsonPath, val)
	case opGte, opGt, opLte, opLt:
		slog.Debug("Skipping comparison operator for SQL conversion", "operator", op)

		return "", nil
	case opEq:
		return b.handleEqOperator(jsonPath, val)
	default:
		slog.Debug("Skipping unknown operator for SQL conversion", "operator", op)

		return "", nil
	}
}

// handleExistsOperator handles $exists operator.
func (b *SQLFilterBuilder) handleExistsOperator(jsonPath string, val interface{}) (string, error) {
	exists, ok := val.(bool)
	if !ok {
		return "", fmt.Errorf("$exists value must be boolean")
	}

	if exists {
		return fmt.Sprintf("%s IS NOT NULL", jsonPath), nil
	}

	return fmt.Sprintf("%s IS NULL", jsonPath), nil
}

// handleEqOperator handles $eq operator.
func (b *SQLFilterBuilder) handleEqOperator(jsonPath string, val interface{}) (string, error) {
	cond, err := b.valueToSQL("", val)
	if err != nil {
		return "", err
	}

	return strings.Replace(cond, "new_values", jsonPath, 1), nil
}

// handleNeOperator handles $ne (not equal) operator.
func (b *SQLFilterBuilder) handleNeOperator(jsonPath string, val interface{}) (string, error) {
	switch v := val.(type) {
	case bool:
		return b.handleNeBool(jsonPath, v)
	case string:
		return b.handleNeString(jsonPath, v)
	case nil:
		return fmt.Sprintf("%s IS NOT NULL", jsonPath), nil
	default:
		return b.handleNeDefault(jsonPath, v)
	}
}

// handleNeBool handles $ne with boolean value.
func (b *SQLFilterBuilder) handleNeBool(jsonPath string, v bool) (string, error) {
	b.argIndex++
	b.args = append(b.args, v)

	if v {
		return fmt.Sprintf("((%s)::boolean = false OR %s IS NULL)", jsonPath, jsonPath), nil
	}

	return fmt.Sprintf("(%s)::boolean = true", jsonPath), nil
}

// handleNeString handles $ne with string value.
func (b *SQLFilterBuilder) handleNeString(jsonPath string, v string) (string, error) {
	b.argIndex++
	b.args = append(b.args, v)

	return fmt.Sprintf("(%s IS NULL OR %s != $%d)", jsonPath, jsonPath, b.argIndex), nil
}

// handleNeDefault handles $ne with default value type.
func (b *SQLFilterBuilder) handleNeDefault(jsonPath string, v interface{}) (string, error) {
	b.argIndex++
	b.args = append(b.args, fmt.Sprintf("%v", v))

	return fmt.Sprintf("(%s IS NULL OR %s != $%d)", jsonPath, jsonPath, b.argIndex), nil
}

// handleInOperator handles $in operator.
func (b *SQLFilterBuilder) handleInOperator(jsonPath string, val interface{}) (string, error) {
	arr := b.convertToSlice(val)
	if arr == nil {
		return "", fmt.Errorf("$in value must be an array, got %T", val)
	}

	if len(arr) == 0 {
		return "false", nil
	}

	// Check if this is the operation column (operationType field)
	// The operation column stores uppercase values (INSERT, UPDATE, DELETE)
	// but MongoDB pipelines use lowercase (insert, update)
	isOperationColumn := jsonPath == operationColumn

	placeholders := make([]string, len(arr))

	for i, v := range arr {
		b.argIndex++

		strVal := fmt.Sprintf("%v", v)
		if isOperationColumn {
			strVal = strings.ToUpper(strVal)
		}

		b.args = append(b.args, strVal)
		placeholders[i] = fmt.Sprintf("$%d", b.argIndex)
	}

	inClause := strings.Join(placeholders, ", ")

	// Handle case-insensitive status/message fields within userpodsevictionstatus
	// Due to historical data having both 'status'/'Status' and 'message'/'Message' keys,
	// we need to check for both casings
	if altPath := b.getAlternateCasePath(jsonPath); altPath != "" {
		return fmt.Sprintf("(%s IN (%s) OR %s IN (%s))", jsonPath, inClause, altPath, inClause), nil
	}

	return fmt.Sprintf("%s IN (%s)", jsonPath, inClause), nil
}

// GetWhereClause returns the SQL WHERE clause fragment (without leading AND).
// Returns empty string if no conditions were added.
func (b *SQLFilterBuilder) GetWhereClause() string {
	if len(b.conditions) == 0 {
		return ""
	}

	return strings.Join(b.conditions, " AND ")
}

// GetWhereClauseWithAnd returns the SQL WHERE clause with leading " AND ".
// Returns empty string if no conditions were added.
func (b *SQLFilterBuilder) GetWhereClauseWithAnd() string {
	clause := b.GetWhereClause()
	if clause == "" {
		return ""
	}

	return " AND " + clause
}

// GetArgs returns the SQL arguments for the WHERE clause.
func (b *SQLFilterBuilder) GetArgs() []interface{} {
	return b.args
}

// HasConditions returns true if any conditions were added.
func (b *SQLFilterBuilder) HasConditions() bool {
	return len(b.conditions) > 0
}
