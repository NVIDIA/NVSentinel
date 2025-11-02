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

package configmanager

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// GetEnvVar retrieves an environment variable and converts it to type T.
// Type must be explicitly specified: GetEnvVar[int]("PORT", nil, nil)
// If defaultValue is nil, the environment variable is required.
// If defaultValue is non-nil, it will be used when the environment variable is not set.
// Optional validator function validates the final value (from env or default).
//
// Supported types:
//   - int
//   - uint
//   - float64
//   - bool (accepts: "true" or "false", case-insensitive)
//   - string
//
// Example usage:
//
//	// Required env var
//	port, err := configmanager.GetEnvVar[int]("PORT", nil, nil)
//
//	// With default value
//	defaultTimeout := 30
//	timeout, err := configmanager.GetEnvVar[int]("TIMEOUT", &defaultTimeout, nil)
//
//	// With default and validation
//	defaultMaxConn := 100
//	maxConn, err := configmanager.GetEnvVar[int]("MAX_CONN", &defaultMaxConn, func(v int) error {
//	    if v <= 0 { return fmt.Errorf("must be positive") }
//	    return nil
//	})
//
//	// Required with validation
//	workers, err := configmanager.GetEnvVar[int]("WORKERS", nil, func(v int) error {
//	    if v <= 0 { return fmt.Errorf("must be positive") }
//	    return nil
//	})
func GetEnvVar[T any](name string, defaultValue *T, validator func(T) error) (T, error) {
	var zero T

	valueStr, exists := os.LookupEnv(name)
	if !exists {
		return handleMissingEnvVarWithDefault(name, defaultValue, validator)
	}

	value, err := parseValue[T](valueStr)
	if err != nil {
		return zero, fmt.Errorf("error converting %s: %w", name, err)
	}

	if validator != nil {
		if err := validator(value); err != nil {
			return zero, fmt.Errorf("validation failed for %s: %w", name, err)
		}
	}

	return value, nil
}

func handleMissingEnvVarWithDefault[T any](name string, defaultValue *T, validator func(T) error) (T, error) {
	var zero T

	if defaultValue == nil {
		return zero, fmt.Errorf("environment variable %s is not set", name)
	}

	if validator != nil {
		if err := validator(*defaultValue); err != nil {
			return zero, fmt.Errorf("validation failed for default value of %s: %w", name, err)
		}
	}

	return *defaultValue, nil
}

func parseValue[T any](valueStr string) (T, error) {
	var zero T

	switch any(zero).(type) {
	case string:
		return any(valueStr).(T), nil
	case int:
		return parseAndConvert[T](parseInt(valueStr))
	case uint:
		return parseAndConvert[T](parseUint(valueStr))
	case float64:
		return parseAndConvert[T](parseFloat64(valueStr))
	case bool:
		return parseAndConvert[T](parseBool(valueStr))
	default:
		return zero, fmt.Errorf("unsupported type %T", zero)
	}
}

func parseAndConvert[T any](value any, err error) (T, error) {
	var zero T
	if err != nil {
		return zero, err
	}

	return any(value).(T), nil
}

func parseInt(valueStr string) (int, error) {
	v, err := strconv.ParseInt(valueStr, 10, 64)
	if err != nil {
		return 0, err
	}

	if v < math.MinInt || v > math.MaxInt {
		return 0, fmt.Errorf("value %d out of range for int type", v)
	}

	return int(v), nil
}

func parseUint(valueStr string) (uint, error) {
	v, err := strconv.ParseUint(valueStr, 10, 64)
	if err != nil {
		return 0, err
	}

	if v > math.MaxUint {
		return 0, fmt.Errorf("value %d out of range for uint type", v)
	}

	return uint(v), nil
}

func parseFloat64(valueStr string) (float64, error) {
	return strconv.ParseFloat(valueStr, 64)
}

// parseBool parses boolean values (accepts "true" or "false")
func parseBool(valueStr string) (bool, error) {
	valueStr = strings.ToLower(strings.TrimSpace(valueStr))

	switch valueStr {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean value: %s (must be 'true' or 'false')", valueStr)
	}
}

// EnvVarSpec defines a specification for reading an environment variable.
// All fields except Name are optional.
//
// Example usage:
//
//	specs := []configmanager.EnvVarSpec{
//	    {Name: "DATABASE_URL"},  // Required by default
//	    {Name: "PORT", Optional: true, DefaultValue: "5432"},  // Optional with default
//	}
//	envVars, errors := configmanager.ReadEnvVars(specs)
//	if len(errors) > 0 {
//	    return fmt.Errorf("missing required vars: %v", errors)
//	}
type EnvVarSpec struct {
	Name     string // Required: The environment variable name to read
	Optional bool   // Optional: If true, env var is optional; if false, it's required (default: false/required)
	// DefaultValue is used when env var is not set.
	// Empty string defaults are treated as "no value" and excluded from results map.
	DefaultValue string
}

// ReadEnvVars reads multiple environment variables based on the provided specifications.
// Returns a map of environment variable names to their values and a slice of errors.
// Environment variables are required by default unless Optional is set to true.
//
// Example usage:
//
//	specs := []configmanager.EnvVarSpec{
//	    {Name: "DATABASE_URI"},                                    // Required
//	    {Name: "DATABASE_NAME"},                                   // Required
//	    {Name: "DATABASE_PORT", Optional: true, DefaultValue: "5432"}, // Included with default
//	    {Name: "DEBUG_MODE", Optional: true, DefaultValue: ""},    // NOT included (empty default)
//	}
//	envVars, errors := configmanager.ReadEnvVars(specs)
//	if len(errors) > 0 {
//	    log.Fatalf("Missing required environment variables: %v", errors)
//	}
//	// Use the values
//	dbURI := envVars["DATABASE_URI"]
//	dbName := envVars["DATABASE_NAME"]
//	dbPort := envVars["DATABASE_PORT"]  // Will be "5432" if not set
func ReadEnvVars(specs []EnvVarSpec) (map[string]string, []error) {
	results := make(map[string]string)

	var errors []error

	for _, spec := range specs {
		value, exists := os.LookupEnv(spec.Name)

		if !exists {
			defaultVal, err := handleMissingEnvVar(spec)
			if err != nil {
				errors = append(errors, err)
			}

			// Only include non-empty defaults in results map to distinguish "not set" from "set to empty"
			if defaultVal != "" {
				results[spec.Name] = defaultVal
			}

			continue
		}

		results[spec.Name] = value
	}

	return results, errors
}

func handleMissingEnvVar(spec EnvVarSpec) (string, error) {
	if spec.Optional {
		return spec.DefaultValue, nil
	}

	return "", fmt.Errorf("required environment variable %s is not set", spec.Name)
}
