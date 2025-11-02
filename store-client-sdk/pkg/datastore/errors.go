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

package datastore

import (
	"errors"
	"fmt"
)

// ErrorType represents different types of datastore errors
type ErrorType string

const (
	// Connection errors
	ErrorTypeConnection     ErrorType = "connection"
	ErrorTypeAuthentication ErrorType = "authentication"
	ErrorTypeTimeout        ErrorType = "timeout"
	ErrorTypeCertificate    ErrorType = "certificate"

	// Operation errors
	ErrorTypeQuery       ErrorType = "query"
	ErrorTypeInsert      ErrorType = "insert"
	ErrorTypeUpdate      ErrorType = "update"
	ErrorTypeDelete      ErrorType = "delete"
	ErrorTypeTransaction ErrorType = "transaction"

	// Data errors
	ErrorTypeDocumentNotFound ErrorType = "document_not_found"
	ErrorTypeValidation       ErrorType = "validation"
	ErrorTypeSerialization    ErrorType = "serialization"
	ErrorTypeConversion       ErrorType = "conversion"

	// Configuration errors
	ErrorTypeConfiguration    ErrorType = "configuration"
	ErrorTypeProviderNotFound ErrorType = "provider_not_found"
	ErrorTypeInvalidProvider  ErrorType = "invalid_provider"

	// Change stream errors
	ErrorTypeChangeStream ErrorType = "change_stream"
	ErrorTypeResumeToken  ErrorType = "resume_token"

	// Unknown errors
	ErrorTypeUnknown ErrorType = "unknown"
)

// DatastoreError represents a structured error from datastore operations
type DatastoreError struct {
	Type     ErrorType              `json:"type"`
	Provider DataStoreProvider      `json:"provider"`
	Message  string                 `json:"message"`
	Cause    error                  `json:"-"` // Original error, not serialized
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// Error implements the error interface
func (e *DatastoreError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%s:%s] %s: %v", e.Provider, e.Type, e.Message, e.Cause)
	}

	return fmt.Sprintf("[%s:%s] %s", e.Provider, e.Type, e.Message)
}

// Unwrap returns the underlying error for error wrapping
func (e *DatastoreError) Unwrap() error {
	return e.Cause
}

// Is implements error comparison for errors.Is()
func (e *DatastoreError) Is(target error) bool {
	var datastoreErr *DatastoreError
	if errors.As(target, &datastoreErr) {
		return e.Type == datastoreErr.Type && e.Provider == datastoreErr.Provider
	}

	return false
}

// NewDatastoreError creates a new structured datastore error
func NewDatastoreError(errorType ErrorType, provider DataStoreProvider, message string, cause error) *DatastoreError {
	return &DatastoreError{
		Type:     errorType,
		Provider: provider,
		Message:  message,
		Cause:    cause,
		Metadata: make(map[string]interface{}),
	}
}

// WithMetadata adds metadata to the error
func (e *DatastoreError) WithMetadata(key string, value interface{}) *DatastoreError {
	if e.Metadata == nil {
		e.Metadata = make(map[string]interface{})
	}

	e.Metadata[key] = value

	return e
}

// IsConnectionError checks if the error is a connection-related error
func IsConnectionError(err error) bool {
	var datastoreErr *DatastoreError
	if errors.As(err, &datastoreErr) {
		return datastoreErr.Type == ErrorTypeConnection ||
			datastoreErr.Type == ErrorTypeAuthentication ||
			datastoreErr.Type == ErrorTypeTimeout ||
			datastoreErr.Type == ErrorTypeCertificate
	}

	return false
}

// IsRetryableError checks if the error should be retried
func IsRetryableError(err error) bool {
	var datastoreErr *DatastoreError
	if errors.As(err, &datastoreErr) {
		switch datastoreErr.Type {
		case ErrorTypeConnection, ErrorTypeTimeout, ErrorTypeChangeStream:
			return true
		case ErrorTypeAuthentication, ErrorTypeCertificate, ErrorTypeQuery, ErrorTypeInsert,
			ErrorTypeUpdate, ErrorTypeDelete, ErrorTypeTransaction, ErrorTypeDocumentNotFound,
			ErrorTypeValidation, ErrorTypeSerialization, ErrorTypeConversion, ErrorTypeConfiguration,
			ErrorTypeProviderNotFound, ErrorTypeInvalidProvider, ErrorTypeResumeToken, ErrorTypeUnknown:
			return false
		default:
			return false
		}
	}

	return false
}

// IsNotFoundError checks if the error indicates a document was not found
func IsNotFoundError(err error) bool {
	var datastoreErr *DatastoreError
	if errors.As(err, &datastoreErr) {
		return datastoreErr.Type == ErrorTypeDocumentNotFound
	}

	return false
}

// Error constructors for common error scenarios

// NewConnectionError creates a connection error
func NewConnectionError(provider DataStoreProvider, message string, cause error) *DatastoreError {
	return NewDatastoreError(ErrorTypeConnection, provider, message, cause)
}

// NewAuthenticationError creates an authentication error
func NewAuthenticationError(provider DataStoreProvider, message string, cause error) *DatastoreError {
	return NewDatastoreError(ErrorTypeAuthentication, provider, message, cause)
}

// NewTimeoutError creates a timeout error
func NewTimeoutError(provider DataStoreProvider, message string, cause error) *DatastoreError {
	return NewDatastoreError(ErrorTypeTimeout, provider, message, cause)
}

// NewQueryError creates a query error
func NewQueryError(provider DataStoreProvider, message string, cause error) *DatastoreError {
	return NewDatastoreError(ErrorTypeQuery, provider, message, cause)
}

// NewInsertError creates an insert error
func NewInsertError(provider DataStoreProvider, message string, cause error) *DatastoreError {
	return NewDatastoreError(ErrorTypeInsert, provider, message, cause)
}

// NewUpdateError creates an update error
func NewUpdateError(provider DataStoreProvider, message string, cause error) *DatastoreError {
	return NewDatastoreError(ErrorTypeUpdate, provider, message, cause)
}

// NewDocumentNotFoundError creates a document not found error
func NewDocumentNotFoundError(provider DataStoreProvider, message string, cause error) *DatastoreError {
	return NewDatastoreError(ErrorTypeDocumentNotFound, provider, message, cause)
}

// NewValidationError creates a validation error
func NewValidationError(provider DataStoreProvider, message string, cause error) *DatastoreError {
	return NewDatastoreError(ErrorTypeValidation, provider, message, cause)
}

// NewConfigurationError creates a configuration error
func NewConfigurationError(provider DataStoreProvider, message string, cause error) *DatastoreError {
	return NewDatastoreError(ErrorTypeConfiguration, provider, message, cause)
}

// NewProviderNotFoundError creates a provider not found error
func NewProviderNotFoundError(provider DataStoreProvider, message string, cause error) *DatastoreError {
	return NewDatastoreError(ErrorTypeProviderNotFound, provider, message, cause)
}

// NewChangeStreamError creates a change stream error
func NewChangeStreamError(provider DataStoreProvider, message string, cause error) *DatastoreError {
	return NewDatastoreError(ErrorTypeChangeStream, provider, message, cause)
}

// NewSerializationError creates a serialization error
func NewSerializationError(provider DataStoreProvider, message string, cause error) *DatastoreError {
	return NewDatastoreError(ErrorTypeSerialization, provider, message, cause)
}

// NewTransactionError creates a transaction error
func NewTransactionError(provider DataStoreProvider, message string, cause error) *DatastoreError {
	return NewDatastoreError(ErrorTypeTransaction, provider, message, cause)
}

// NewUnknownError creates an unknown error
func NewUnknownError(provider DataStoreProvider, message string, cause error) *DatastoreError {
	return NewDatastoreError(ErrorTypeUnknown, provider, message, cause)
}
