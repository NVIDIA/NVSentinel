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
	"context"
)

// DatabaseClient provides database-agnostic operations
// This interface abstracts common database operations to allow for different database implementations
type DatabaseClient interface {
	// Document operations
	InsertMany(ctx context.Context, documents []interface{}) (*InsertManyResult, error)
	UpdateDocumentStatus(ctx context.Context, documentID string, statusPath string, status interface{}) error
	UpdateDocument(ctx context.Context, filter interface{}, update interface{}) (*UpdateResult, error)
	UpdateManyDocuments(ctx context.Context, filter interface{}, update interface{}) (*UpdateResult, error)
	UpsertDocument(ctx context.Context, filter interface{}, document interface{}) (*UpdateResult, error)

	// Query operations
	FindOne(ctx context.Context, filter interface{}, options *FindOneOptions) (SingleResult, error)
	Find(ctx context.Context, filter interface{}, options *FindOptions) (Cursor, error)
	CountDocuments(ctx context.Context, filter interface{}, options *CountOptions) (int64, error)

	// Aggregation
	Aggregate(ctx context.Context, pipeline interface{}) (Cursor, error)

	// Health checks
	Ping(ctx context.Context) error

	// Change streams (delegated to existing ChangeStreamWatcher)
	NewChangeStreamWatcher(ctx context.Context, tokenConfig TokenConfig, pipeline interface{}) (ChangeStreamWatcher, error)

	// Close the client
	Close(ctx context.Context) error
}

// CollectionClient provides collection-specific operations
// This interface allows modules to work with specific collections while using the common abstractions
type CollectionClient interface {
	DatabaseClient

	// Collection-specific operations
	GetCollectionName() string
	GetDatabaseName() string
}

// Event represents a database change event in a generic way
type Event interface {
	GetDocumentID() (string, error)
	GetNodeName() (string, error)
	GetResumeToken() []byte
	UnmarshalDocument(v interface{}) error
}

// ChangeStreamWatcher abstracts change stream operations
type ChangeStreamWatcher interface {
	Start(ctx context.Context)
	Events() <-chan Event
	MarkProcessed(ctx context.Context, token []byte) error
	Close(ctx context.Context) error
}

// ChangeStreamMetrics provides optional metrics operations for change streams
// Not all implementations are required to support this interface
type ChangeStreamMetrics interface {
	GetUnprocessedEventCount(ctx context.Context, lastProcessedID string) (int64, error)
}

// TokenConfig holds resume token configuration for change streams
type TokenConfig struct {
	ClientName      string
	TokenDatabase   string
	TokenCollection string
}

// Result types - these wrap the underlying database results to provide consistent interfaces

// InsertManyResult represents the result of an insert many operation
type InsertManyResult struct {
	InsertedIDs []interface{}
}

// UpdateResult represents the result of an update operation
type UpdateResult struct {
	MatchedCount  int64
	ModifiedCount int64
	UpsertedCount int64
	UpsertedID    interface{}
}

// SingleResult represents a single document result
type SingleResult interface {
	Decode(v interface{}) error
	Err() error
}

// Cursor represents a query result cursor
type Cursor interface {
	Next(ctx context.Context) bool
	Decode(v interface{}) error
	Close(ctx context.Context) error
	All(ctx context.Context, results interface{}) error
	Err() error
}

// Options types - these provide consistent options across database implementations

// FindOneOptions holds options for FindOne operations
type FindOneOptions struct {
	Sort interface{}
	Skip *int64
}

// FindOptions holds options for Find operations
type FindOptions struct {
	Sort  interface{}
	Limit *int64
	Skip  *int64
}

// CountOptions holds options for Count operations
type CountOptions struct {
	Limit *int64
	Skip  *int64
}

// UpdateOptions holds options for Update operations
type UpdateOptions struct {
	Upsert *bool
}

// InsertResult represents the result of an insert operation
type InsertResult struct {
	InsertedIDs []interface{}
}

// Filter and Update builders - these help construct database-agnostic queries

// FilterBuilder helps build database-agnostic filters
type FilterBuilder interface {
	// Field operations
	Eq(field string, value interface{}) FilterBuilder
	Ne(field string, value interface{}) FilterBuilder
	Gt(field string, value interface{}) FilterBuilder
	Gte(field string, value interface{}) FilterBuilder
	Lt(field string, value interface{}) FilterBuilder
	Lte(field string, value interface{}) FilterBuilder
	In(field string, values interface{}) FilterBuilder

	// Logical operations
	And(filters ...interface{}) FilterBuilder
	Or(filters ...interface{}) FilterBuilder

	// Build the final filter
	Build() interface{}
}

// UpdateBuilder helps build database-agnostic updates
type UpdateBuilder interface {
	// Field operations
	Set(field string, value interface{}) UpdateBuilder
	Unset(field string) UpdateBuilder
	Inc(field string, value interface{}) UpdateBuilder

	// Build the final update
	Build() interface{}
}

// Builder factory functions - these delegate to database-specific implementations

// BuilderFactory defines how to create filter and update builders for specific database providers
type BuilderFactory interface {
	NewFilterBuilder() FilterBuilder
	NewUpdateBuilder() UpdateBuilder
}

// Default builder factory (currently MongoDB)
var defaultBuilderFactory BuilderFactory

// SetBuilderFactory sets the default builder factory for the client package
func SetBuilderFactory(factory BuilderFactory) {
	defaultBuilderFactory = factory
}

// NewFilterBuilder creates a new filter builder using the default factory
func NewFilterBuilder() FilterBuilder {
	if defaultBuilderFactory == nil {
		// Fallback to direct MongoDB builder if no factory is set
		// This import is delayed to avoid circular dependencies
		return newMongoFilterBuilder()
	}

	return defaultBuilderFactory.NewFilterBuilder()
}

// NewUpdateBuilder creates a new update builder using the default factory
func NewUpdateBuilder() UpdateBuilder {
	if defaultBuilderFactory == nil {
		// Fallback to direct MongoDB builder if no factory is set
		return newMongoUpdateBuilder()
	}

	return defaultBuilderFactory.NewUpdateBuilder()
}

// newMongoFilterBuilder is a fallback function that will be implemented via init()
var newMongoFilterBuilder func() FilterBuilder

// newMongoUpdateBuilder is a fallback function that will be implemented via init()
var newMongoUpdateBuilder func() UpdateBuilder
