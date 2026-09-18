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

package envutil

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestGetEnvInt(t *testing.T) {
	tests := []struct {
		name       string
		envKey     string
		envValue   string
		defaultVal int
		expected   int
	}{
		{
			name:       "valid integer",
			envKey:     "TEST_INT",
			envValue:   "42",
			defaultVal: 10,
			expected:   42,
		},
		{
			name:       "invalid integer returns default",
			envKey:     "TEST_INT",
			envValue:   "not-a-number",
			defaultVal: 10,
			expected:   10,
		},
		{
			name:       "empty string returns default",
			envKey:     "TEST_INT",
			envValue:   "",
			defaultVal: 10,
			expected:   10,
		},
		{
			name:       "negative integer",
			envKey:     "TEST_INT",
			envValue:   "-100",
			defaultVal: 10,
			expected:   -100,
		},
		{
			name:       "zero value",
			envKey:     "TEST_INT",
			envValue:   "0",
			defaultVal: 10,
			expected:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv(tt.envKey, tt.envValue)
			defer os.Unsetenv(tt.envKey)

			result := GetEnvInt(tt.envKey, tt.defaultVal)
			if result != tt.expected {
				t.Errorf("GetEnvInt(%q, %d) = %d, want %d", tt.envKey, tt.defaultVal, result, tt.expected)
			}
		})
	}

	// Test unset variable separately
	t.Run("unset variable returns default", func(t *testing.T) {
		result := GetEnvInt("UNSET_VAR_THAT_SHOULD_NOT_EXIST", 99)
		if result != 99 {
			t.Errorf("GetEnvInt for unset variable = %d, want 99", result)
		}
	})
}

func TestGetEnvBool(t *testing.T) {
	tests := []struct {
		name       string
		envKey     string
		envValue   string
		defaultVal bool
		expected   bool
	}{
		{
			name:       "true string",
			envKey:     "TEST_BOOL",
			envValue:   "true",
			defaultVal: false,
			expected:   true,
		},
		{
			name:       "false string",
			envKey:     "TEST_BOOL",
			envValue:   "false",
			defaultVal: true,
			expected:   false,
		},
		{
			name:       "1 evaluates to true",
			envKey:     "TEST_BOOL",
			envValue:   "1",
			defaultVal: false,
			expected:   true,
		},
		{
			name:       "0 evaluates to false",
			envKey:     "TEST_BOOL",
			envValue:   "0",
			defaultVal: true,
			expected:   false,
		},
		{
			name:       "t evaluates to true",
			envKey:     "TEST_BOOL",
			envValue:   "t",
			defaultVal: false,
			expected:   true,
		},
		{
			name:       "f evaluates to false",
			envKey:     "TEST_BOOL",
			envValue:   "f",
			defaultVal: true,
			expected:   false,
		},
		{
			name:       "TRUE uppercase evaluates to true",
			envKey:     "TEST_BOOL",
			envValue:   "TRUE",
			defaultVal: false,
			expected:   true,
		},
		{
			name:       "FALSE uppercase evaluates to false",
			envKey:     "TEST_BOOL",
			envValue:   "FALSE",
			defaultVal: true,
			expected:   false,
		},
		{
			name:       "invalid bool returns default",
			envKey:     "TEST_BOOL",
			envValue:   "maybe",
			defaultVal: true,
			expected:   true,
		},
		{
			name:       "empty string returns default",
			envKey:     "TEST_BOOL",
			envValue:   "",
			defaultVal: false,
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv(tt.envKey, tt.envValue)
			defer os.Unsetenv(tt.envKey)

			result := GetEnvBool(tt.envKey, tt.defaultVal)
			if result != tt.expected {
				t.Errorf("GetEnvBool(%q, %t) = %t, want %t", tt.envKey, tt.defaultVal, result, tt.expected)
			}
		})
	}

	// Test unset variable separately
	t.Run("unset variable returns default", func(t *testing.T) {
		result := GetEnvBool("UNSET_BOOL_VAR_THAT_SHOULD_NOT_EXIST", true)
		if result != true {
			t.Errorf("GetEnvBool for unset variable = %t, want true", result)
		}
	})
}

func TestGetEnvString(t *testing.T) {
	tests := []struct {
		name       string
		envKey     string
		envValue   string
		defaultVal string
		expected   string
	}{
		{
			name:       "non-empty string",
			envKey:     "TEST_STRING",
			envValue:   "hello world",
			defaultVal: "default",
			expected:   "hello world",
		},
		{
			name:       "empty string returns default",
			envKey:     "TEST_STRING",
			envValue:   "",
			defaultVal: "default",
			expected:   "default",
		},
		{
			name:       "string with spaces",
			envKey:     "TEST_STRING",
			envValue:   "  value with spaces  ",
			defaultVal: "default",
			expected:   "  value with spaces  ",
		},
		{
			name:       "special characters",
			envKey:     "TEST_STRING",
			envValue:   "user@example.com:8080/path?query=value",
			defaultVal: "default",
			expected:   "user@example.com:8080/path?query=value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv(tt.envKey, tt.envValue)
			defer os.Unsetenv(tt.envKey)

			result := GetEnvString(tt.envKey, tt.defaultVal)
			if result != tt.expected {
				t.Errorf("GetEnvString(%q, %q) = %q, want %q", tt.envKey, tt.defaultVal, result, tt.expected)
			}
		})
	}

	// Test unset variable separately
	t.Run("unset variable returns default", func(t *testing.T) {
		result := GetEnvString("UNSET_STRING_VAR_THAT_SHOULD_NOT_EXIST", "default-value")
		if result != "default-value" {
			t.Errorf("GetEnvString for unset variable = %q, want 'default-value'", result)
		}
	})
}

func TestParseEnvInt(t *testing.T) {
	t.Setenv("TEST_PARSE_INT", "")

	got, err := ParseEnvInt("TEST_PARSE_INT", 7)
	if err != nil || got != 7 {
		t.Fatalf("unset: got %d, %v; want the default 7", got, err)
	}

	t.Setenv("TEST_PARSE_INT", "42")

	if got, err = ParseEnvInt("TEST_PARSE_INT", 7); err != nil || got != 42 {
		t.Fatalf("valid: got %d, %v; want 42", got, err)
	}

	t.Setenv("TEST_PARSE_INT", "forty-two")

	if _, err = ParseEnvInt("TEST_PARSE_INT", 7); err == nil || !strings.Contains(err.Error(), "TEST_PARSE_INT") {
		t.Fatalf("invalid: want an error naming the variable, got %v", err)
	}
}

func TestParseEnvFloat64(t *testing.T) {
	t.Setenv("TEST_PARSE_FLOAT", "")

	got, err := ParseEnvFloat64("TEST_PARSE_FLOAT", 1.5)
	if err != nil || got != 1.5 {
		t.Fatalf("unset: got %v, %v; want the default 1.5", got, err)
	}

	t.Setenv("TEST_PARSE_FLOAT", "100.5")

	if got, err = ParseEnvFloat64("TEST_PARSE_FLOAT", 1.5); err != nil || got != 100.5 {
		t.Fatalf("valid: got %v, %v; want 100.5", got, err)
	}

	t.Setenv("TEST_PARSE_FLOAT", "fast")

	if _, err = ParseEnvFloat64("TEST_PARSE_FLOAT", 1.5); err == nil || !strings.Contains(err.Error(), "TEST_PARSE_FLOAT") {
		t.Fatalf("invalid: want an error naming the variable, got %v", err)
	}
}

func TestParseEnvDuration(t *testing.T) {
	t.Setenv("TEST_PARSE_DURATION", "")

	got, err := ParseEnvDuration("TEST_PARSE_DURATION", time.Minute)
	if err != nil || got != time.Minute {
		t.Fatalf("unset: got %v, %v; want the default 1m", got, err)
	}

	t.Setenv("TEST_PARSE_DURATION", "30s")

	if got, err = ParseEnvDuration("TEST_PARSE_DURATION", time.Minute); err != nil || got != 30*time.Second {
		t.Fatalf("valid: got %v, %v; want 30s", got, err)
	}

	t.Setenv("TEST_PARSE_DURATION", "soon")

	if _, err = ParseEnvDuration("TEST_PARSE_DURATION", time.Minute); err == nil || !strings.Contains(err.Error(), "TEST_PARSE_DURATION") {
		t.Fatalf("invalid: want an error naming the variable, got %v", err)
	}
}
