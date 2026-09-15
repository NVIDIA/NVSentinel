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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

const testDuplicateKeyMessage = "E11000 duplicate key error collection: nvsentinel.HealthEvents " +
	"index: healthevent_idempotency_key_unique dup key: " +
	`{ healthevent.metadata.idempotencyKey: "pod-uid#key#0" }`

func TestClassifyMongoBulkWriteError(t *testing.T) {
	t.Run("duplicate key errors are classified with the index name", func(t *testing.T) {
		bwe := mongo.BulkWriteException{
			WriteErrors: []mongo.BulkWriteError{
				{WriteError: mongo.WriteError{
					Index:   1,
					Code:    mongoDuplicateKeyErrorCode,
					Message: testDuplicateKeyMessage,
				}},
			},
		}

		failed, ok := classifyMongoBulkWriteError(bwe)
		require.True(t, ok)
		assert.Equal(t, 1, failed.DocumentIndex)
		assert.True(t, failed.Duplicate)
		assert.Equal(t, datastore.HealthEventIdempotencyIndexName, failed.IndexName)
		assert.True(t, failed.DuplicateOn(datastore.HealthEventIdempotencyIndexName))
	})

	t.Run("wrapped exception is classified", func(t *testing.T) {
		bwe := mongo.BulkWriteException{
			WriteErrors: []mongo.BulkWriteError{
				{WriteError: mongo.WriteError{Index: 0, Code: mongoDuplicateKeyErrorCode, Message: testDuplicateKeyMessage}},
			},
		}
		wrapped := fmt.Errorf("insert failed: %w", error(bwe))

		failed, ok := classifyMongoBulkWriteError(wrapped)
		require.True(t, ok)
		assert.Equal(t, 0, failed.DocumentIndex)
	})

	t.Run("non duplicate write errors keep the server's message", func(t *testing.T) {
		bwe := mongo.BulkWriteException{
			WriteErrors: []mongo.BulkWriteError{
				{WriteError: mongo.WriteError{Index: 0, Code: 121, Message: "Document failed validation"}},
			},
		}

		failed, ok := classifyMongoBulkWriteError(bwe)
		require.True(t, ok)
		assert.False(t, failed.Duplicate)
		assert.Empty(t, failed.IndexName)
		assert.Equal(t, "Document failed validation", failed.Message)
	})

	t.Run("write concern errors are not per-document failures", func(t *testing.T) {
		bwe := mongo.BulkWriteException{
			WriteConcernError: &mongo.WriteConcernError{Code: 64, Message: "waiting for replication timed out"},
			WriteErrors: []mongo.BulkWriteError{
				{WriteError: mongo.WriteError{Index: 0, Code: mongoDuplicateKeyErrorCode, Message: testDuplicateKeyMessage}},
			},
		}

		_, ok := classifyMongoBulkWriteError(bwe)
		assert.False(t, ok)
	})

	t.Run("non bulk write errors are not classified", func(t *testing.T) {
		_, ok := classifyMongoBulkWriteError(errors.New("connection reset"))
		assert.False(t, ok)
	})
}

func TestExtractMongoDuplicateIndexName(t *testing.T) {
	t.Run("index name parsed from message", func(t *testing.T) {
		writeErr := mongo.WriteError{Code: mongoDuplicateKeyErrorCode, Message: testDuplicateKeyMessage}
		assert.Equal(t, datastore.HealthEventIdempotencyIndexName, extractMongoDuplicateIndexName(writeErr))
	})

	t.Run("falls back to keyPattern for the idempotency path", func(t *testing.T) {
		raw, err := bson.Marshal(bson.D{{
			Key:   "keyPattern",
			Value: bson.D{{Key: healthEventIdempotencyKeyDocumentPath, Value: 1}},
		}})
		require.NoError(t, err)

		writeErr := mongo.WriteError{
			Code:    mongoDuplicateKeyErrorCode,
			Message: "duplicate key error without the usual format",
			Raw:     raw,
		}
		assert.Equal(t, datastore.HealthEventIdempotencyIndexName, extractMongoDuplicateIndexName(writeErr))
	})

	t.Run("unknown format yields empty name", func(t *testing.T) {
		writeErr := mongo.WriteError{Code: mongoDuplicateKeyErrorCode, Message: "duplicate key"}
		assert.Empty(t, extractMongoDuplicateIndexName(writeErr))
	})
}

func TestVerifyMongoIdempotencyIndexSpec(t *testing.T) {
	validSpec := func() bson.M {
		return bson.M{
			"name":   datastore.HealthEventIdempotencyIndexName,
			"key":    bson.M{healthEventIdempotencyKeyDocumentPath: int32(1)},
			"unique": true,
			"partialFilterExpression": bson.M{
				healthEventIdempotencyKeyDocumentPath: bson.M{"$exists": true},
			},
		}
	}

	t.Run("valid spec passes", func(t *testing.T) {
		assert.NoError(t, verifyMongoIdempotencyIndexSpec(validSpec()))
	})

	t.Run("valid spec with bson.D sub-documents passes", func(t *testing.T) {
		spec := bson.M{
			"name":   datastore.HealthEventIdempotencyIndexName,
			"key":    bson.D{{Key: healthEventIdempotencyKeyDocumentPath, Value: int32(1)}},
			"unique": true,
			"partialFilterExpression": bson.D{{
				Key:   healthEventIdempotencyKeyDocumentPath,
				Value: bson.D{{Key: "$exists", Value: true}},
			}},
		}
		assert.NoError(t, verifyMongoIdempotencyIndexSpec(spec))
	})

	t.Run("wrong key path is a mismatch", func(t *testing.T) {
		spec := validSpec()
		spec["key"] = bson.M{"healthevent.nodename": int32(1)}

		err := verifyMongoIdempotencyIndexSpec(spec)
		assert.ErrorIs(t, err, datastore.ErrIndexMismatch)
	})

	t.Run("compound key is a mismatch", func(t *testing.T) {
		spec := validSpec()
		spec["key"] = bson.M{
			healthEventIdempotencyKeyDocumentPath: int32(1),
			"healthevent.nodename":                int32(1),
		}

		err := verifyMongoIdempotencyIndexSpec(spec)
		assert.ErrorIs(t, err, datastore.ErrIndexMismatch)
	})

	t.Run("non unique index is a mismatch", func(t *testing.T) {
		spec := validSpec()
		spec["unique"] = false

		err := verifyMongoIdempotencyIndexSpec(spec)
		assert.ErrorIs(t, err, datastore.ErrIndexMismatch)
	})

	t.Run("missing partial filter is a mismatch", func(t *testing.T) {
		spec := validSpec()
		delete(spec, "partialFilterExpression")

		err := verifyMongoIdempotencyIndexSpec(spec)
		assert.ErrorIs(t, err, datastore.ErrIndexMismatch)
	})

	t.Run("wrong partial filter predicate is a mismatch", func(t *testing.T) {
		spec := validSpec()
		spec["partialFilterExpression"] = bson.M{
			healthEventIdempotencyKeyDocumentPath: bson.M{"$exists": false},
		}

		err := verifyMongoIdempotencyIndexSpec(spec)
		assert.ErrorIs(t, err, datastore.ErrIndexMismatch)
	})
}

func TestIsNumericOne(t *testing.T) {
	assert.True(t, isNumericOne(1))
	assert.True(t, isNumericOne(int32(1)))
	assert.True(t, isNumericOne(int64(1)))
	assert.True(t, isNumericOne(float64(1)))
	assert.False(t, isNumericOne(int32(-1)))
	assert.False(t, isNumericOne("1"))
	assert.False(t, isNumericOne(nil))
}

// scriptedOrderedInsert plays the driver for insertOrderedResumingDuplicates:
// each call inserts documents in order until the scripted failure for one of
// them, reported the way an ordered InsertMany reports it.
type scriptedOrderedInsert struct {
	failures map[string]mongo.WriteError // document -> write error
	calls    [][]any
}

func (s *scriptedOrderedInsert) insert(_ context.Context, documents []any) (*mongo.InsertManyResult, error) {
	s.calls = append(s.calls, documents)

	res := &mongo.InsertManyResult{}

	for i, doc := range documents {
		if writeErr, failed := s.failures[doc.(string)]; failed {
			writeErr.Index = i

			return res, mongo.BulkWriteException{WriteErrors: []mongo.BulkWriteError{{WriteError: writeErr}}}
		}

		res.InsertedIDs = append(res.InsertedIDs, doc)
	}

	return res, nil
}

func idempotencyDuplicate() mongo.WriteError {
	return mongo.WriteError{
		Code:    mongoDuplicateKeyErrorCode,
		Message: "E11000 duplicate key error collection: db.health_events index: " + datastore.HealthEventIdempotencyIndexName + " dup key: {}",
	}
}

// TestInsertOrderedResumingDuplicates_ResendStoresMissingEventsInOrder: a
// resend whose first and third events already exist inserts the others, in
// order, and counts both duplicates in the result.
func TestInsertOrderedResumingDuplicates_ResendStoresMissingEventsInOrder(t *testing.T) {
	driver := &scriptedOrderedInsert{failures: map[string]mongo.WriteError{"a": idempotencyDuplicate(), "c": idempotencyDuplicate()}}

	result, err := insertOrderedResumingDuplicates(context.Background(), []any{"a", "b", "c", "d"}, driver.insert)
	require.NoError(t, err, "a resend is a success, not a failure")
	require.Equal(t, 2, result.DuplicateCount)
	require.Equal(t, []any{"b", "d"}, result.InsertedIDs)
	require.Equal(t, [][]any{{"a", "b", "c", "d"}, {"b", "c", "d"}, {"d"}}, driver.calls,
		"each duplicate stops the ordered insert, which resumes right after it")
}

// TestInsertOrderedResumingDuplicates_RealFailureStops: a failure that is not
// an idempotency duplicate ends the insert; the documents after it are not
// attempted and the failure is not a clean resend.
func TestInsertOrderedResumingDuplicates_RealFailureStops(t *testing.T) {
	driver := &scriptedOrderedInsert{failures: map[string]mongo.WriteError{
		"b": {Code: 121, Message: "Document failed validation"},
	}}

	_, err := insertOrderedResumingDuplicates(context.Background(), []any{"a", "b", "c"}, driver.insert)

	failure, ok := datastore.AsBulkWriteFailure(err)
	require.True(t, ok)
	require.Equal(t, 1, failure.InsertedCount)
	require.Equal(t, 1, failure.Failed.DocumentIndex)
	require.False(t, failure.Failed.Duplicate)
	require.Contains(t, err.Error(), "Document failed validation", "the server's answer survives into the error")
	require.Len(t, driver.calls, 1, "nothing after a real failure is attempted")
}

// TestInsertOrderedResumingDuplicates_OtherIndexDuplicateAfterResumeStops: a
// duplicate on another unique index, met after an idempotency duplicate was
// resumed past, ends the insert like any real failure.
func TestInsertOrderedResumingDuplicates_OtherIndexDuplicateAfterResumeStops(t *testing.T) {
	driver := &scriptedOrderedInsert{failures: map[string]mongo.WriteError{
		"a": idempotencyDuplicate(),
		"c": {Code: mongoDuplicateKeyErrorCode, Message: "E11000 duplicate key error collection: db.health_events index: _id_ dup key: {}"},
	}}

	_, err := insertOrderedResumingDuplicates(context.Background(), []any{"a", "b", "c", "d"}, driver.insert)

	failure, ok := datastore.AsBulkWriteFailure(err)
	require.True(t, ok)
	require.Equal(t, 1, failure.InsertedCount, "only b was stored")
	require.Equal(t, 1, failure.DuplicateCount, "a was a resend")
	require.Equal(t, 2, failure.Failed.DocumentIndex)
	require.True(t, failure.Failed.Duplicate)
	require.Equal(t, "_id_", failure.Failed.IndexName)
	require.Contains(t, err.Error(), `duplicate on index "_id_"`)
	require.Equal(t, [][]any{{"a", "b", "c", "d"}, {"b", "c", "d"}}, driver.calls, "nothing after the real failure is attempted")
}

// TestInsertOrderedResumingDuplicates_LastDocumentDuplicate: a duplicate at
// the end costs no extra round trip and the batch counts as a clean resend.
func TestInsertOrderedResumingDuplicates_LastDocumentDuplicate(t *testing.T) {
	driver := &scriptedOrderedInsert{failures: map[string]mongo.WriteError{"c": idempotencyDuplicate()}}

	result, err := insertOrderedResumingDuplicates(context.Background(), []any{"a", "b", "c"}, driver.insert)
	require.NoError(t, err)
	require.Equal(t, 1, result.DuplicateCount)
	require.Equal(t, []any{"a", "b"}, result.InsertedIDs)
	require.Len(t, driver.calls, 1)
}

// TestInsertOrderedResumingDuplicates_AllStored: no failure, one round trip.
func TestInsertOrderedResumingDuplicates_AllStored(t *testing.T) {
	driver := &scriptedOrderedInsert{}

	result, err := insertOrderedResumingDuplicates(context.Background(), []any{"a", "b"}, driver.insert)
	require.NoError(t, err)
	require.Equal(t, []any{"a", "b"}, result.InsertedIDs)
	require.Len(t, driver.calls, 1)
}

// TestIsUnsupportedStageError: a datastore without $collStats keeps the
// specification check alone; any other failure of the build check is reported.
func TestIsUnsupportedStageError(t *testing.T) {
	assert.True(t, isUnsupportedStageError(mongo.CommandError{
		Code: mongoUnrecognizedStageCode, Message: "Unrecognized pipeline stage name: '$collStats'",
	}))
	assert.True(t, isUnsupportedStageError(errors.New("Unrecognized pipeline stage name: '$collStats'")))
	assert.True(t, isUnsupportedStageError(errors.New("$collStats is not supported by this service")))
	assert.False(t, isUnsupportedStageError(mongo.CommandError{Code: 13, Message: "not authorized on nvsentinel"}))
	assert.False(t, isUnsupportedStageError(mongo.CommandError{
		Code: 13,
		Message: "not authorized on nvsentinel to execute command { aggregate: \"HealthEvents\", " +
			"pipeline: [ { $collStats: { storageStats: {} } } ], cursor: {} }",
	}), "an authorization failure quoting the command is not an unsupported stage")
	assert.False(t, isUnsupportedStageError(errors.New("connection reset by peer")))
}

// TestIndexBuildInProgress: the build list is read whether the driver decoded
// the sub-documents as bson.D or bson.M, across the documents of a sharded
// collection; a missing list means no build.
func TestIndexBuildInProgress(t *testing.T) {
	name := datastore.HealthEventIdempotencyIndexName

	assert.True(t, indexBuildInProgress([]bson.M{
		{"storageStats": bson.D{{Key: "indexBuilds", Value: bson.A{"other", name}}}},
	}, name), "bson.D sub-document")
	assert.True(t, indexBuildInProgress([]bson.M{
		{"storageStats": bson.M{"indexBuilds": bson.A{}}},
		{"storageStats": bson.M{"indexBuilds": []any{name}}},
	}, name), "second shard")
	assert.False(t, indexBuildInProgress([]bson.M{
		{"storageStats": bson.M{"indexBuilds": bson.A{"other"}}},
	}, name))
	assert.False(t, indexBuildInProgress([]bson.M{{"storageStats": bson.M{}}}, name), "no build list")
	assert.False(t, indexBuildInProgress(nil, name))
}

// TestVerifyMongoIdempotencyIndexSpec_RejectsCollation: an otherwise matching
// index under a collation is a mismatch.
func TestVerifyMongoIdempotencyIndexSpec_RejectsCollation(t *testing.T) {
	spec := bson.M{
		"name":   datastore.HealthEventIdempotencyIndexName,
		"key":    bson.M{healthEventIdempotencyKeyDocumentPath: int32(1)},
		"unique": true,
		"partialFilterExpression": bson.M{
			healthEventIdempotencyKeyDocumentPath: bson.M{"$exists": true},
		},
	}
	require.NoError(t, verifyMongoIdempotencyIndexSpec(spec))

	spec["collation"] = bson.M{"locale": "en", "strength": int32(2)}
	err := verifyMongoIdempotencyIndexSpec(spec)
	require.ErrorIs(t, err, datastore.ErrIndexMismatch)
	assert.Contains(t, err.Error(), "collation")
}
