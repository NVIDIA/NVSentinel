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
	"fmt"
	"time"

	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/config"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/datastore/providers/mongodb/watcher"
)

// Common operations that consolidate patterns found across all modules

// UpdateHealthEventStatus updates a health event status field
// This consolidates the common pattern used by fault-quarantine-module,
// node-drainer-module, and fault-remediation-module
func UpdateHealthEventStatus(ctx context.Context, client DatabaseClient, eventID string,
	statusField string, status interface{}) error {
	statusPath := fmt.Sprintf("healtheventstatus.%s", statusField)
	return client.UpdateDocumentStatus(ctx, eventID, statusPath, status)
}

// UpdateHealthEventNodeQuarantineStatus updates the node quarantine status
// Used by fault-quarantine-module
func UpdateHealthEventNodeQuarantineStatus(ctx context.Context, client DatabaseClient,
	eventID string, status string) error {
	return UpdateHealthEventStatus(ctx, client, eventID, "nodequarantined", status)
}

// UpdateHealthEventPodEvictionStatus updates the pod eviction status
// Used by node-drainer-module
func UpdateHealthEventPodEvictionStatus(ctx context.Context, client DatabaseClient,
	eventID string, status interface{}) error {
	return UpdateHealthEventStatus(ctx, client, eventID, "userpodsevictionstatus", status)
}

// UpdateHealthEventRemediationStatus updates the remediation status
// Used by fault-remediation-module
func UpdateHealthEventRemediationStatus(ctx context.Context, client DatabaseClient,
	eventID string, status interface{}) error {
	return UpdateHealthEventStatus(ctx, client, eventID, "remediation", status)
}

// Backward compatibility helpers

// ConvertToMongoDBConfig converts the new DatabaseConfig to the existing MongoDBConfig
// This allows existing code to gradually migrate while maintaining compatibility
func ConvertToMongoDBConfig(dbConfig config.DatabaseConfig) MongoDBConfig {
	return MongoDBConfig{
		URI:        dbConfig.GetConnectionURI(),
		Database:   dbConfig.GetDatabaseName(),
		Collection: dbConfig.GetCollectionName(),
		ClientTLSCertConfig: MongoDBClientTLSCertConfig{
			TlsCertPath: dbConfig.GetCertConfig().GetCertPath(),
			TlsKeyPath:  dbConfig.GetCertConfig().GetKeyPath(),
			CaCertPath:  dbConfig.GetCertConfig().GetCACertPath(),
		},
		TotalPingTimeoutSeconds:          dbConfig.GetTimeoutConfig().GetPingTimeoutSeconds(),
		TotalPingIntervalSeconds:         dbConfig.GetTimeoutConfig().GetPingIntervalSeconds(),
		TotalCACertTimeoutSeconds:        dbConfig.GetTimeoutConfig().GetCACertTimeoutSeconds(),
		TotalCACertIntervalSeconds:       dbConfig.GetTimeoutConfig().GetCACertIntervalSeconds(),
		ChangeStreamRetryDeadlineSeconds: dbConfig.GetTimeoutConfig().GetChangeStreamRetryDeadlineSeconds(),
		ChangeStreamRetryIntervalSeconds: dbConfig.GetTimeoutConfig().GetChangeStreamRetryIntervalSeconds(),
	}
}

// Legacy type aliases for backward compatibility
type MongoDBConfig = watcher.MongoDBConfig
type MongoDBClientTLSCertConfig = watcher.MongoDBClientTLSCertConfig

// Database-agnostic constants and helpers
const (
	// Default retry configuration for database operations
	DefaultMaxRetries = 3
	DefaultRetryDelay = 2 * time.Second

	// Common error messages across database implementations
	NoDocumentsFoundError = "no documents in result"
)

// IsNoDocumentsError checks if an error indicates no documents were found
// This abstracts the database-specific error messages
func IsNoDocumentsError(err error) bool {
	if err == nil {
		return false
	}

	errMsg := err.Error()
	return errMsg == "mongo: no documents in result" || errMsg == NoDocumentsFoundError
}

// RetryableUpsert performs an upsert with retry logic for better reliability
// This consolidates the retry pattern used across multiple modules
func RetryableUpsert(
	ctx context.Context,
	client DatabaseClient,
	filter interface{},
	document interface{},
	maxRetries int,
	retryDelay time.Duration,
) (*UpdateResult, error) {
	var lastErr error

	for i := 1; i <= maxRetries; i++ {
		result, err := client.UpsertDocument(ctx, filter, document)
		if err == nil {
			return result, nil
		}

		lastErr = err

		if i < maxRetries {
			time.Sleep(retryDelay)
		}
	}

	return nil, fmt.Errorf("upsert failed after %d retries: %w", maxRetries, lastErr)
}

// RetryableDocumentUpsert performs an upsert of a complete document with database-agnostic structure
// This function abstracts database-specific upsert requirements (like MongoDB's $set wrapper)
func RetryableDocumentUpsert(
	ctx context.Context,
	client DatabaseClient,
	filter interface{},
	document interface{},
	maxRetries int,
	retryDelay time.Duration,
) (*UpdateResult, error) {
	// Wrap the document in database-specific upsert structure
	// For MongoDB, this means wrapping in $set operator
	// For other databases in the future, this could be different
	upsertDocument := map[string]interface{}{"$set": document}

	return RetryableUpsert(ctx, client, filter, upsertDocument, maxRetries, retryDelay)
}

// RetryableUpdate performs an update with retry logic for better reliability
// This consolidates the retry pattern used across multiple modules
func RetryableUpdate(
	ctx context.Context,
	client DatabaseClient,
	filter interface{},
	update interface{},
	maxRetries int,
	retryDelay time.Duration,
) (*UpdateResult, error) {
	var lastErr error

	for i := 1; i <= maxRetries; i++ {
		result, err := client.UpdateDocument(ctx, filter, update)
		if err == nil {
			return result, nil
		}

		lastErr = err

		if i < maxRetries {
			time.Sleep(retryDelay)
		}
	}

	return nil, fmt.Errorf("update failed after %d retries: %w", maxRetries, lastErr)
}

// BuildTimeRangeFilter creates a time range filter using database-agnostic builders
// This replaces the MongoDB-specific filter construction
func BuildTimeRangeFilter(field string, after *time.Time, before *time.Time) interface{} {
	builder := NewFilterBuilder()

	switch {
	case after != nil && before != nil:
		// Range query: field > after AND field <= before
		timeRange := map[string]interface{}{}
		timeRange["$gt"] = *after
		timeRange["$lte"] = *before
		builder.Eq(field, timeRange)
	case after != nil:
		builder.Gt(field, *after)
	case before != nil:
		builder.Lte(field, *before)
	}

	return builder.Build()
}

// BuildStatusFilter creates a status filter using database-agnostic builders
// This replaces direct MongoDB filter construction
func BuildStatusFilter(field string, status interface{}) interface{} {
	return NewFilterBuilder().Eq(field, status).Build()
}

// BuildInFilter creates an "in" filter using database-agnostic builders
// This replaces direct MongoDB $in operator usage
func BuildInFilter(field string, values interface{}) interface{} {
	return NewFilterBuilder().In(field, values).Build()
}

// BuildNotNullFilter creates a "not null" filter using database-agnostic builders
// This replaces direct MongoDB $ne: null usage
func BuildNotNullFilter(field string) interface{} {
	return NewFilterBuilder().Ne(field, nil).Build()
}

// BuildSetUpdate creates a set update using database-agnostic builders
// This replaces direct MongoDB $set operator usage
func BuildSetUpdate(updates map[string]interface{}) interface{} {
	builder := NewUpdateBuilder()
	for field, value := range updates {
		builder.Set(field, value)
	}
	return builder.Build()
}

// Database-agnostic pipeline builders

// PipelineBuilder helps build database-agnostic aggregation pipelines
type PipelineBuilder interface {
	// Stage operations
	Match(filter interface{}) PipelineBuilder
	Sort(sort interface{}) PipelineBuilder
	Limit(limit int64) PipelineBuilder
	Skip(skip int64) PipelineBuilder
	Project(projection interface{}) PipelineBuilder

	// Build the final pipeline
	Build() interface{}
}

// NewPipelineBuilder creates a new pipeline builder
func NewPipelineBuilder() PipelineBuilder {
	return &mongoPipelineBuilder{stages: make([]interface{}, 0)}
}

// MongoDB-specific pipeline builder implementation
type mongoPipelineBuilder struct {
	stages []interface{}
}

func (pb *mongoPipelineBuilder) Match(filter interface{}) PipelineBuilder {
	stage := map[string]interface{}{
		"$match": filter,
	}
	pb.stages = append(pb.stages, stage)
	return pb
}

func (pb *mongoPipelineBuilder) Sort(sort interface{}) PipelineBuilder {
	stage := map[string]interface{}{
		"$sort": sort,
	}
	pb.stages = append(pb.stages, stage)
	return pb
}

func (pb *mongoPipelineBuilder) Limit(limit int64) PipelineBuilder {
	stage := map[string]interface{}{
		"$limit": limit,
	}
	pb.stages = append(pb.stages, stage)
	return pb
}

func (pb *mongoPipelineBuilder) Skip(skip int64) PipelineBuilder {
	stage := map[string]interface{}{
		"$skip": skip,
	}
	pb.stages = append(pb.stages, stage)
	return pb
}

func (pb *mongoPipelineBuilder) Project(projection interface{}) PipelineBuilder {
	stage := map[string]interface{}{
		"$project": projection,
	}
	pb.stages = append(pb.stages, stage)
	return pb
}

func (pb *mongoPipelineBuilder) Build() interface{} {
	return pb.stages
}

// Convenience functions for common pipeline patterns

// BuildChangeStreamPipeline creates a pipeline for change stream operations
// This abstracts the MongoDB-specific change stream pipeline structure
func BuildChangeStreamPipeline(matchCondition interface{}) interface{} {
	return NewPipelineBuilder().
		Match(matchCondition).
		Build()
}

// BuildMatchOnlyPipeline creates a simple match-only pipeline
// This is commonly used for filtering change streams or aggregations
func BuildMatchOnlyPipeline(filter interface{}) interface{} {
	return NewPipelineBuilder().
		Match(filter).
		Build()
}

// Semantic database methods that eliminate the need for error type checking

// FindOneWithExists performs a FindOne operation and returns whether a document was found
// This eliminates the need for modules to check for "no documents" errors
func FindOneWithExists(
	ctx context.Context,
	client DatabaseClient,
	filter interface{},
	options *FindOneOptions,
	result interface{},
) (found bool, err error) {
	dbResult, err := client.FindOne(ctx, filter, options)
	if err != nil {
		return false, fmt.Errorf("database query failed: %w", err)
	}

	if err := dbResult.Decode(result); err != nil {
		// Check if this is a "no documents found" case
		if IsNoDocumentsError(err) {
			return false, nil // Not found, but not an error
		}
		return false, fmt.Errorf("failed to decode result: %w", err)
	}

	return true, nil
}

// UpdateWithResult performs an update and returns semantic results
// This eliminates the need to inspect UpdateResult fields
func UpdateWithResult(
	ctx context.Context,
	client DatabaseClient,
	filter interface{},
	update interface{},
) (matched int64, modified int64, err error) {
	result, err := client.UpdateDocument(ctx, filter, update)
	if err != nil {
		return 0, 0, fmt.Errorf("update operation failed: %w", err)
	}

	return result.MatchedCount, result.ModifiedCount, nil
}

// UpsertWithResult performs an upsert and returns semantic results
// This eliminates the need to inspect UpdateResult fields for upsert outcomes
func UpsertWithResult(
	ctx context.Context,
	client DatabaseClient,
	filter interface{},
	document interface{},
) (inserted int64, updated int64, err error) {
	result, err := client.UpsertDocument(ctx, filter, document)
	if err != nil {
		return 0, 0, fmt.Errorf("upsert operation failed: %w", err)
	}

	return result.UpsertedCount, result.ModifiedCount, nil
}

// RetryableFindOneWithExists combines FindOneWithExists with retry logic
func RetryableFindOneWithExists(
	ctx context.Context,
	client DatabaseClient,
	filter interface{},
	options *FindOneOptions,
	result interface{},
	maxRetries int,
	retryDelay time.Duration,
) (found bool, err error) {
	var lastErr error

	for i := 1; i <= maxRetries; i++ {
		found, err := FindOneWithExists(ctx, client, filter, options, result)
		if err == nil {
			return found, nil
		}

		lastErr = err

		if i < maxRetries {
			time.Sleep(retryDelay)
		}
	}

	return false, fmt.Errorf("find operation failed after %d retries: %w", maxRetries, lastErr)
}

// RetryableUpdateWithResult combines UpdateWithResult with retry logic
func RetryableUpdateWithResult(
	ctx context.Context,
	client DatabaseClient,
	filter interface{},
	update interface{},
	maxRetries int,
	retryDelay time.Duration,
) (matched int64, modified int64, err error) {
	var lastErr error

	for i := 1; i <= maxRetries; i++ {
		matched, modified, err := UpdateWithResult(ctx, client, filter, update)
		if err == nil {
			return matched, modified, nil
		}

		lastErr = err

		if i < maxRetries {
			time.Sleep(retryDelay)
		}
	}

	return 0, 0, fmt.Errorf("update failed after %d retries: %w", maxRetries, lastErr)
}

// RetryableDocumentUpsertWithResult combines document upsert with retry logic and semantic results
func RetryableDocumentUpsertWithResult(
	ctx context.Context,
	client DatabaseClient,
	filter interface{},
	document interface{},
	maxRetries int,
	retryDelay time.Duration,
) (inserted int64, updated int64, err error) {
	var lastErr error

	for i := 1; i <= maxRetries; i++ {
		// Wrap the document in database-specific upsert structure
		upsertDocument := map[string]interface{}{"$set": document}

		inserted, updated, err := UpsertWithResult(ctx, client, filter, upsertDocument)
		if err == nil {
			return inserted, updated, nil
		}

		lastErr = err

		if i < maxRetries {
			time.Sleep(retryDelay)
		}
	}

	return 0, 0, fmt.Errorf("document upsert failed after %d retries: %w", maxRetries, lastErr)
}
