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

package client

import (
	"errors"
	"fmt"
)

// Database-agnostic error types that abstract provider-specific errors

// ErrNoDocuments indicates that no documents were found for the given query
var ErrNoDocuments = errors.New("no documents found")

// ErrConnectionFailed indicates a database connection failure
var ErrConnectionFailed = errors.New("database connection failed")

// ErrOperationTimeout indicates a database operation timed out
var ErrOperationTimeout = errors.New("database operation timed out")

// ErrInvalidFilter indicates an invalid filter was provided
var ErrInvalidFilter = errors.New("invalid filter")

// ErrDuplicateKey indicates a duplicate key constraint violation
var ErrDuplicateKey = errors.New("duplicate key error")

// DatabaseError wraps provider-specific errors with context
type DatabaseError struct {
	Op       string // Operation that failed (e.g., "find", "update", "upsert")
	Provider string // Database provider (e.g., "mongodb", "postgresql")
	Err      error  // Underlying provider-specific error
}

func (e *DatabaseError) Error() string {
	return fmt.Sprintf("%s operation failed (%s): %v", e.Op, e.Provider, e.Err)
}

func (e *DatabaseError) Unwrap() error {
	return e.Err
}

// WrapProviderError wraps a provider-specific error into a standard database error
func WrapProviderError(op, provider string, err error) error {
	if err == nil {
		return nil
	}

	// Check for common patterns and map to standard errors
	errMsg := err.Error()

	// MongoDB-specific error mapping
	if provider == "mongodb" {
		switch {
		case errMsg == "mongo: no documents in result":
			return ErrNoDocuments
		case contains(errMsg, "connection"):
			return &DatabaseError{Op: op, Provider: provider, Err: ErrConnectionFailed}
		case contains(errMsg, "timeout"):
			return &DatabaseError{Op: op, Provider: provider, Err: ErrOperationTimeout}
		case contains(errMsg, "duplicate key"):
			return &DatabaseError{Op: op, Provider: provider, Err: ErrDuplicateKey}
		}
	}

	// For unmapped errors, wrap the original error
	return &DatabaseError{Op: op, Provider: provider, Err: err}
}

// Helper function to check if string contains substring (case-insensitive)
func contains(s, substr string) bool {
	return len(s) >= len(substr) &&
		(s == substr ||
			(len(s) > len(substr) &&
				substrMatch(s, substr)))
}

func substrMatch(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
