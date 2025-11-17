// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package parser

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
)

// ParseSequenceStage parses a JSON stage string and replaces "this." references with actual event values
func ParseSequenceStage(stage string, event datamodels.HealthEventWithStatus) (map[string]interface{}, error) {
	var stageMap map[string]interface{}
	if err := json.Unmarshal([]byte(stage), &stageMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal stage '%s': %w", stage, err)
	}

	for key, value := range stageMap {
		processedValue, err := processValue(value, event)
		if err != nil {
			return nil, err
		}

		stageMap[key] = processedValue
	}

	return stageMap, nil
}

// processValue recursively processes any value type and replaces "this." references with actual event values
func processValue(value interface{}, event datamodels.HealthEventWithStatus) (interface{}, error) {
	switch v := value.(type) {
	case string:
		if strings.HasPrefix(v, "this.") {
			fieldPath := strings.TrimPrefix(v, "this.")

			resolvedValue, err := getValueFromPath(fieldPath, event)
			if err != nil {
				return nil, fmt.Errorf("error in getting value from path '%s': %w", fieldPath, err)
			}

			// CRITICAL: Normalize field names for protobuf values embedded in MongoDB aggregation pipelines
			// This converts camelCase JSON field names (entityType, entityValue) to lowercase BSON field names
			// (entitytype, entityvalue) so they match MongoDB's storage and filter expectations
			normalized := utils.NormalizeFieldNamesForMongoDB(resolvedValue)

			return normalized, nil
		}

		return v, nil
	case map[string]interface{}:
		return processMapValue(v, event)
	case []interface{}:
		return processArrayValue(v, event)
	default:
		return v, nil
	}
}

func processMapValue(v map[string]interface{}, event datamodels.HealthEventWithStatus) (map[string]interface{}, error) {
	result := make(map[string]interface{})

	for key, val := range v {
		processedVal, err := processValue(val, event)
		if err != nil {
			return nil, err
		}

		result[key] = processedVal
	}

	return result, nil
}

func processArrayValue(v []interface{}, event datamodels.HealthEventWithStatus) ([]interface{}, error) {
	result := make([]interface{}, len(v))

	for i, val := range v {
		processedVal, err := processValue(val, event)
		if err != nil {
			return nil, err
		}

		result[i] = processedVal
	}

	return result, nil
}

// ParseSequenceString converts a criteria map into a database filter map
// This uses the same map[string]interface{} type as store-client filters
func ParseSequenceString(
	criteria map[string]any,
	event datamodels.HealthEventWithStatus,
) (map[string]interface{}, error) {
	doc := make(map[string]interface{})
	allowedStringPattern := regexp.MustCompile(`^[a-zA-Z0-9.-]+$`)

	for key, value := range criteria {
		strVal, isString := value.(string)

		if !isString {
			doc[key] = value
			continue
		}

		// "this." reference → resolve from current event
		if strings.HasPrefix(strVal, "this.") {
			fieldPath := strings.TrimPrefix(strVal, "this.")

			resolvedValue, err := getValueFromPath(fieldPath, event)
			if err != nil {
				return nil, fmt.Errorf("error in getting value from path: %w", err)
			}

			doc[key] = resolvedValue

			continue
		}

		// JSON object string representing database operator (e.g. '{"$ne":"x"}')
		var operatorMap map[string]any
		if err := json.Unmarshal([]byte(strVal), &operatorMap); err == nil {
			doc[key] = operatorMap
			continue
		}

		// String with only allowed characters (alphanumeric, dot, and hyphen)
		if allowedStringPattern.MatchString(strVal) {
			doc[key] = strVal
			continue
		}

		return nil, fmt.Errorf("failed to parse criteria '%s'", strVal)
	}

	return doc, nil
}

func getValueFromPath(path string, event datamodels.HealthEventWithStatus) (any, error) {
	parts := strings.Split(path, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid path: %s", path)
	}

	eventValue := reflect.ValueOf(event)
	rootField := findField(eventValue, parts[0])

	if !rootField.IsValid() {
		return nil, fmt.Errorf("invalid root field: %s", parts[0])
	}

	return getValueByReflection(rootField, parts[1:])
}

func getValueByReflection(value reflect.Value, parts []string) (any, error) {
	if len(parts) == 0 {
		return getInterfaceOrNil(value)
	}

	value = dereferencePointer(value)
	if !value.IsValid() {
		return nil, fmt.Errorf("invalid value")
	}

	part := parts[0]

	switch value.Kind() {
	case reflect.Struct:
		return handleStructField(value, part, parts[1:])
	case reflect.Slice, reflect.Array:
		return handleSliceOrArray(value, part, parts[1:])
	case reflect.Map:
		return handleMapKey(value, part, parts[1:])
	case reflect.Invalid,
		reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128,
		reflect.Chan, reflect.Func, reflect.Interface, reflect.Ptr,
		reflect.String, reflect.UnsafePointer:
		return nil, fmt.Errorf("invalid value: %s", value.Kind())
	}

	return nil, fmt.Errorf("invalid value: %s", value.Kind())
}

func getInterfaceOrNil(value reflect.Value) (any, error) {
	if !value.IsValid() {
		return nil, fmt.Errorf("invalid value")
	}

	return value.Interface(), nil
}

func dereferencePointer(value reflect.Value) reflect.Value {
	if value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return reflect.Value{}
		}

		return value.Elem()
	}

	return value
}

func handleStructField(value reflect.Value, fieldName string, remainingParts []string) (any, error) {
	field := findField(value, fieldName)
	if !field.IsValid() {
		return nil, fmt.Errorf("field not found: %s", fieldName)
	}

	return getValueByReflection(field, remainingParts)
}

func handleSliceOrArray(value reflect.Value, indexStr string, remainingParts []string) (any, error) {
	idx, err := strconv.Atoi(indexStr)
	if err != nil || idx < 0 || idx >= value.Len() {
		return nil, fmt.Errorf("invalid index: %s", indexStr)
	}

	return getValueByReflection(value.Index(idx), remainingParts)
}

func handleMapKey(value reflect.Value, key string, remainingParts []string) (any, error) {
	mapValue := value.MapIndex(reflect.ValueOf(key))
	if !mapValue.IsValid() {
		return nil, fmt.Errorf("map key not found: %s", key)
	}

	return getValueByReflection(mapValue, remainingParts)
}

// findField finds a struct field by name using case-insensitive matching.
// Note: case-insensitive matching is necessary because MongoDB uses lowercase field names (e.g., "nodename"),
// and config criteria are written to match MongoDB conventions (e.g., "healthevent.nodename").
// However, Go structs follow PascalCase naming conventions (e.g., NodeName).
// Case-insensitive matching allows seamless resolution of config-defined paths to Go struct fields.
func findField(structValue reflect.Value, fieldName string) reflect.Value {
	structType := structValue.Type()
	for i := 0; i < structValue.NumField(); i++ {
		field := structType.Field(i)
		if strings.EqualFold(field.Name, fieldName) {
			return structValue.Field(i)
		}
	}

	return reflect.Value{}
}
