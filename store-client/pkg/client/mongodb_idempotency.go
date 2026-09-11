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

package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// healthEventIdempotencyKeyDocumentPath is the dotted document path of the
// idempotency key inside a stored health event document.
const healthEventIdempotencyKeyDocumentPath = "healthevent.metadata." +
	datastore.HealthEventIdempotencyKeyMetadataField

// mongoDuplicateKeyErrorCode is the MongoDB server error code for a unique
// index violation (E11000).
const mongoDuplicateKeyErrorCode = 11000

// mongoDuplicateIndexNameRegex extracts the violated index name from a
// duplicate-key error message, e.g.
// "E11000 duplicate key error collection: db.coll index: <name> dup key: {...}".
// The server does not expose the index name as a structured field on write
// errors (only keyPattern/keyValue), so message parsing is the primary source.
var mongoDuplicateIndexNameRegex = regexp.MustCompile(`index: (\S+)`)

// InsertManyIdempotent inserts the documents in order, so a monitor's events
// land in the order it sent them, and continues past a document that already
// exists under the idempotency index. MongoDB keeps array order only for
// ordered inserts, and an ordered insert stops at its first failure; when that
// failure is such a duplicate (a resent batch) the insert resumes with the
// documents after it. Any other per-document failure ends the insert and is
// reported as a *datastore.BulkWriteFailure with the server's answer; the
// resends skipped are counted in the result, so the caller can tell a resend
// from a failure.
func (c *MongoDBClient) InsertManyIdempotent(ctx context.Context, documents []any) (*InsertManyResult, error) {
	return insertOrderedResumingDuplicates(ctx, documents,
		func(ctx context.Context, documents []any) (*mongo.InsertManyResult, error) {
			return c.mongoCol.InsertMany(ctx, documents)
		})
}

// orderedInsert is one ordered InsertMany call; the driver stops at the first
// failing document and reports it in a mongo.BulkWriteException.
type orderedInsert func(ctx context.Context, documents []any) (*mongo.InsertManyResult, error)

func insertOrderedResumingDuplicates(
	ctx context.Context, documents []any, insert orderedInsert,
) (*InsertManyResult, error) {
	result := &InsertManyResult{InsertedIDs: make([]any, 0, len(documents))}

	for offset := 0; offset < len(documents); {
		res, err := insert(ctx, documents[offset:])
		if err == nil {
			result.InsertedIDs = append(result.InsertedIDs, res.InsertedIDs...)

			return result, nil
		}

		failed, ok := classifyMongoBulkWriteError(err)
		if !ok {
			return nil, datastore.NewInsertError(
				datastore.ProviderMongoDB,
				"failed to insert documents",
				err,
			).WithMetadata("count", len(documents))
		}

		// Ordered: the driver stops at the first failing document, and the
		// documents before it were inserted; their ids lead the driver's list.
		if res != nil && len(res.InsertedIDs) >= failed.DocumentIndex {
			result.InsertedIDs = append(result.InsertedIDs, res.InsertedIDs[:failed.DocumentIndex]...)
		}

		failed.DocumentIndex += offset

		if !failed.DuplicateOn(datastore.HealthEventIdempotencyIndexName) {
			// A real failure: the documents after it were not attempted, and
			// the caller's retry will find the inserted ones as duplicates.
			return nil, &datastore.BulkWriteFailure{
				InsertedCount:  len(result.InsertedIDs),
				DuplicateCount: result.DuplicateCount,
				Failed:         failed,
			}
		}

		result.DuplicateCount++
		offset = failed.DocumentIndex + 1
	}

	return result, nil
}

// classifyMongoBulkWriteError converts a mongo.BulkWriteException into the
// datastore.BulkDocumentError of the document the ordered insert stopped at.
// It returns false when the error is not a per-document bulk write failure
// (for example a write concern error, which affects the whole batch).
func classifyMongoBulkWriteError(err error) (datastore.BulkDocumentError, bool) {
	bwe, ok := errors.AsType[mongo.BulkWriteException](err)
	if !ok {
		return datastore.BulkDocumentError{}, false
	}

	if bwe.WriteConcernError != nil || len(bwe.WriteErrors) == 0 {
		return datastore.BulkDocumentError{}, false
	}

	// Ordered insert: the driver reports the one document it stopped at.
	writeErr := bwe.WriteErrors[0]
	failed := datastore.BulkDocumentError{
		DocumentIndex: writeErr.Index,
		Message:       writeErr.Message,
	}

	if writeErr.Code == mongoDuplicateKeyErrorCode {
		failed.Duplicate = true
		failed.IndexName = extractMongoDuplicateIndexName(writeErr.WriteError)
	}

	return failed, true
}

// extractMongoDuplicateIndexName determines which index a duplicate-key write
// error violated. The message is parsed first (the server does not return the
// index name as a structured field); the structured keyPattern is used as a
// fallback for the idempotency index specifically.
func extractMongoDuplicateIndexName(writeErr mongo.WriteError) string {
	if match := mongoDuplicateIndexNameRegex.FindStringSubmatch(writeErr.Message); len(match) == 2 {
		return match[1]
	}

	// Fallback: a single-field keyPattern on the idempotency key path can only
	// come from the idempotency index.
	if len(writeErr.Raw) == 0 {
		return ""
	}

	value, err := writeErr.Raw.LookupErr("keyPattern")
	if err != nil {
		return ""
	}

	doc, ok := value.DocumentOK()
	if !ok {
		return ""
	}

	elements, err := doc.Elements()
	if err != nil || len(elements) != 1 || elements[0].Key() != healthEventIdempotencyKeyDocumentPath {
		return ""
	}

	return datastore.HealthEventIdempotencyIndexName
}

// EnsureHealthEventIdempotencyIndex makes sure the unique partial index that
// enforces per-event idempotency keys exists with the expected definition. The
// partial filter covers only documents that carry the key, so existing records
// need no backfill. An index of the same name with another definition, which
// CreateOne would refuse with an options conflict, is dropped and recreated:
// the name is ours, and the index Job is how an operator repairs the index.
func (c *MongoDBClient) EnsureHealthEventIdempotencyIndex(ctx context.Context) error {
	switch err := c.VerifyHealthEventIdempotencyIndex(ctx); {
	case err == nil:
		return nil
	case errors.Is(err, datastore.ErrIndexMissing):
	case errors.Is(err, datastore.ErrIndexBuilding):
		// Dropping it would abort another session's build; wait for it.
		return fmt.Errorf("idempotency index %s on collection %s is still being built by another session; retry later: %w",
			datastore.HealthEventIdempotencyIndexName, c.collection, err)
	case errors.Is(err, datastore.ErrIndexMismatch):
		if err := c.mongoCol.Indexes().DropOne(ctx, datastore.HealthEventIdempotencyIndexName); err != nil {
			return fmt.Errorf("failed to drop mismatched idempotency index %s on collection %s: %w",
				datastore.HealthEventIdempotencyIndexName, c.collection, err)
		}
	default:
		return err
	}

	indexModel := mongo.IndexModel{
		Keys: bson.D{{Key: healthEventIdempotencyKeyDocumentPath, Value: 1}},
		Options: options.Index().
			SetName(datastore.HealthEventIdempotencyIndexName).
			SetUnique(true).
			SetPartialFilterExpression(bson.D{{
				Key:   healthEventIdempotencyKeyDocumentPath,
				Value: bson.D{{Key: "$exists", Value: true}},
			}}),
	}

	if _, err := c.mongoCol.Indexes().CreateOne(ctx, indexModel); err != nil {
		return fmt.Errorf("failed to ensure idempotency index %s on collection %s: %w",
			datastore.HealthEventIdempotencyIndexName, c.collection, err)
	}

	return nil
}

// VerifyHealthEventIdempotencyIndex returns nil only when the idempotency index
// exists with the expected name, key path, uniqueness, partial predicate, and a
// completed build. A missing index yields datastore.ErrIndexMissing; any other
// deviation yields datastore.ErrIndexMismatch, both wrapped with detail.
func (c *MongoDBClient) VerifyHealthEventIdempotencyIndex(ctx context.Context) error {
	cursor, err := c.mongoCol.Indexes().List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list indexes on collection %s: %w", c.collection, err)
	}

	var specs []bson.M
	if err := cursor.All(ctx, &specs); err != nil {
		return fmt.Errorf("failed to decode index specifications: %w", err)
	}

	var indexSpec bson.M

	for _, spec := range specs {
		if name, _ := spec["name"].(string); name == datastore.HealthEventIdempotencyIndexName {
			indexSpec = spec
			break
		}
	}

	if indexSpec == nil {
		return fmt.Errorf("%w: index %s not found on collection %s",
			datastore.ErrIndexMissing, datastore.HealthEventIdempotencyIndexName, c.collection)
	}

	// The build check comes first: an index another session is still building
	// is reported as such whatever its definition, so Ensure waits for that
	// build instead of aborting it as a mismatch.
	if err := c.verifyMongoIdempotencyIndexBuildComplete(ctx); err != nil {
		return err
	}

	return verifyMongoIdempotencyIndexSpec(indexSpec)
}

// asPlainMap normalizes a decoded BSON sub-document (bson.M or bson.D) into a
// plain map for structural comparison.
func asPlainMap(value any) (map[string]any, bool) {
	switch v := normalizeValue(value).(type) {
	case bson.M:
		return v, true
	case map[string]any:
		return v, true
	default:
		return nil, false
	}
}

// verifyMongoIdempotencyIndexSpec checks the key path, uniqueness, and partial
// filter expression of a listIndexes specification document.
func verifyMongoIdempotencyIndexSpec(indexSpec bson.M) error {
	key, ok := asPlainMap(indexSpec["key"])
	if !ok || len(key) != 1 || !isNumericOne(key[healthEventIdempotencyKeyDocumentPath]) {
		return fmt.Errorf("%w: index %s does not index exactly {%s: 1}",
			datastore.ErrIndexMismatch, datastore.HealthEventIdempotencyIndexName,
			healthEventIdempotencyKeyDocumentPath)
	}

	if unique, _ := indexSpec["unique"].(bool); !unique {
		return fmt.Errorf("%w: index %s is not unique",
			datastore.ErrIndexMismatch, datastore.HealthEventIdempotencyIndexName)
	}

	if err := verifyMongoIdempotencyPartialFilter(indexSpec["partialFilterExpression"]); err != nil {
		return err
	}

	// A collation (a collection default, for example) would make keys that
	// differ only in case equal, and the client key alphabet is mixed case: a
	// distinct batch would then be dropped as a duplicate.
	if _, has := indexSpec["collation"]; has {
		return fmt.Errorf("%w: index %s has a collation",
			datastore.ErrIndexMismatch, datastore.HealthEventIdempotencyIndexName)
	}

	return nil
}

// verifyMongoIdempotencyPartialFilter checks that the partial filter covers
// exactly the documents carrying a key: {path: {$exists: true}}.
func verifyMongoIdempotencyPartialFilter(expression any) error {
	partialFilter, ok := asPlainMap(expression)
	if !ok || len(partialFilter) != 1 {
		return fmt.Errorf("%w: index %s is missing the expected partial filter expression",
			datastore.ErrIndexMismatch, datastore.HealthEventIdempotencyIndexName)
	}

	keyPredicate, ok := asPlainMap(partialFilter[healthEventIdempotencyKeyDocumentPath])
	if !ok || len(keyPredicate) != 1 {
		return fmt.Errorf("%w: index %s partial filter does not cover %s",
			datastore.ErrIndexMismatch, datastore.HealthEventIdempotencyIndexName,
			healthEventIdempotencyKeyDocumentPath)
	}

	if exists, _ := keyPredicate["$exists"].(bool); !exists {
		return fmt.Errorf("%w: index %s partial filter is not {%s: {$exists: true}}",
			datastore.ErrIndexMismatch, datastore.HealthEventIdempotencyIndexName,
			healthEventIdempotencyKeyDocumentPath)
	}

	return nil
}

// verifyMongoIdempotencyIndexBuildComplete checks that the index is not still
// being built. listIndexes reports an index while its build is in progress,
// and a resend stored during a unique index build could be inserted twice and
// then fail the build at its end. $collStats with storageStats lists the
// builds in progress and works with the read role the chart grants (unlike
// $indexStats, which needs clusterMonitor). A service without the stage keeps
// the specification check alone, and says so.
func (c *MongoDBClient) verifyMongoIdempotencyIndexBuildComplete(ctx context.Context) error {
	pipeline := mongo.Pipeline{bson.D{{Key: "$collStats", Value: bson.D{{Key: "storageStats", Value: bson.D{}}}}}}

	cursor, err := c.mongoCol.Aggregate(ctx, pipeline)
	if err != nil {
		if isUnsupportedStageError(err) {
			slog.Warn("Cannot confirm the idempotency index build is complete: $collStats is not supported "+
				"by this datastore; relying on the index specification alone", "error", err)

			return nil
		}

		return fmt.Errorf("failed to read collection statistics for %s: %w", c.collection, err)
	}

	var stats []bson.M
	if err := cursor.All(ctx, &stats); err != nil {
		return fmt.Errorf("failed to decode collection statistics: %w", err)
	}

	if indexBuildInProgress(stats, datastore.HealthEventIdempotencyIndexName) {
		return fmt.Errorf("%w: index %s", datastore.ErrIndexBuilding, datastore.HealthEventIdempotencyIndexName)
	}

	return nil
}

// indexBuildInProgress reports whether the $collStats documents (one per
// shard) list the named index under storageStats.indexBuilds, the builds in
// progress. Sub-documents are read whether the driver decoded them as bson.D
// or bson.M.
func indexBuildInProgress(stats []bson.M, name string) bool {
	for _, stat := range stats {
		storage, ok := asPlainMap(stat["storageStats"])
		if !ok {
			continue
		}

		builds, _ := normalizeValue(storage["indexBuilds"]).([]any)
		for _, build := range builds {
			if building, _ := build.(string); building == name {
				return true
			}
		}
	}

	return false
}

// mongoUnrecognizedStageCode is the server error for a pipeline stage it does
// not know.
const mongoUnrecognizedStageCode = 40324

// isUnsupportedStageError reports whether an aggregation failed because the
// datastore does not support the stage (MongoDB-compatible services): the
// server's own code, or a message saying so. The stage name alone is not
// evidence: an authorization failure quotes the whole command, stage
// included, and must not pass as "unsupported".
func isUnsupportedStageError(err error) bool {
	if cmdErr, ok := errors.AsType[mongo.CommandError](err); ok && cmdErr.Code == mongoUnrecognizedStageCode {
		return true
	}

	message := strings.ToLower(err.Error())

	return strings.Contains(message, "unrecognized pipeline stage") ||
		(strings.Contains(message, "not supported") && strings.Contains(message, "$collstats"))
}

// isNumericOne reports whether an index key direction value equals 1,
// tolerating the numeric types BSON decoding can produce.
func isNumericOne(value any) bool {
	switch v := value.(type) {
	case int:
		return v == 1
	case int32:
		return v == 1
	case int64:
		return v == 1
	case float64:
		return v == 1
	default:
		return false
	}
}
