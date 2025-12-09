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

// Package envutil provides utilities for reading and parsing environment variables with
// type-safe defaults and consistent error handling across NVSentinel components.
package envutil

import (
	"os"
	"strconv"
)

// GetEnvInt retrieves an integer environment variable.
// If the variable is not set or cannot be parsed as an integer, returns defaultVal.
func GetEnvInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}

	return defaultVal
}

// GetEnvBool retrieves a boolean environment variable.
// If the variable is not set or cannot be parsed as a boolean, returns defaultVal.
// Accepts: true, false, 1, 0, t, f, T, F, TRUE, FALSE (case-insensitive).
func GetEnvBool(key string, defaultVal bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}

	return defaultVal
}

// GetEnvString retrieves a string environment variable.
// If the variable is not set, returns defaultVal.
func GetEnvString(key string, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return defaultVal
}
