// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package celrule

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

var primitiveKinds = map[reflect.Kind]bool{
	reflect.Bool:       true,
	reflect.Int:        true,
	reflect.Int8:       true,
	reflect.Int16:      true,
	reflect.Int32:      true,
	reflect.Int64:      true,
	reflect.Uint:       true,
	reflect.Uint8:      true,
	reflect.Uint16:     true,
	reflect.Uint32:     true,
	reflect.Uint64:     true,
	reflect.Uintptr:    true,
	reflect.Float32:    true,
	reflect.Float64:    true,
	reflect.Complex64:  true,
	reflect.Complex128: true,
	reflect.String:     true,
}

// recursively converts any Go value into a JSON-compatible structure
// with all fields present. Structs become map[string]interface{}, slices become []interface{},
// maps become map[string]interface{}. Zero-values or nil pointers appear as null in the final map
func structToInterface(v reflect.Value) any {
	if !v.IsValid() {
		return nil
	}

	kind := v.Kind()

	if primitiveKinds[kind] {
		return v.Interface()
	}

	return handleComplexType(v, kind)
}

func handleComplexType(v reflect.Value, kind reflect.Kind) any {
	switch kind {
	case reflect.Pointer:
		return handlePointer(v)
	case reflect.Struct:
		return handleStruct(v)
	case reflect.Slice, reflect.Array:
		return handleSliceOrArray(v)
	case reflect.Map:
		return handleMap(v)
	case reflect.Interface:
		return handleInterface(v)
	case reflect.Invalid, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return nil
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128,
		reflect.String:
		return v.Interface()
	default:
		return v.Interface()
	}
}

func handlePointer(v reflect.Value) any {
	if v.IsNil() {
		return nil
	}

	return structToInterface(v.Elem())
}

func handleStruct(v reflect.Value) any {
	result := make(map[string]any)
	typ := v.Type()

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			continue
		}

		jsonTag := field.Tag.Get("json")
		if jsonTag == "" {
			continue
		}

		name := extractJSONFieldName(jsonTag, field.Name)

		fieldVal := structToInterface(v.Field(i))
		result[name] = fieldVal
	}

	return result
}

func extractJSONFieldName(jsonTag, fieldName string) string {
	name := jsonTag
	if idx := strings.Index(name, ","); idx != -1 {
		name = name[:idx]
	}

	if name == "" {
		name = fieldName
	}

	return name
}

func handleSliceOrArray(v reflect.Value) any {
	if v.Kind() == reflect.Slice && v.IsNil() {
		return nil
	}

	sliceResult := make([]any, v.Len())

	for i := 0; i < v.Len(); i++ {
		sliceResult[i] = structToInterface(v.Index(i))
	}

	return sliceResult
}

func handleMap(v reflect.Value) any {
	if v.IsNil() {
		return nil
	}

	mapResult := make(map[string]any)

	for _, key := range v.MapKeys() {
		mapResult[key.String()] = structToInterface(v.MapIndex(key))
	}

	return mapResult
}

func handleInterface(v reflect.Value) any {
	if v.IsNil() {
		return nil
	}

	return structToInterface(v.Elem())
}

// RoundTrip converts v to the map a HealthEvent rule reads as "event". Every
// field is present, even zero values, and is keyed by its JSON tag name. Enums
// are numbers, and all numbers are float64.
func RoundTrip(v any) (map[string]any, error) {
	val := reflect.ValueOf(v)
	obj := structToInterface(val)

	b, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal intermediate object: %w", err)
	}

	var j any
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON back to map: %w", err)
	}

	m, ok := j.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected JSON object after roundtrip")
	}

	return m, nil
}
