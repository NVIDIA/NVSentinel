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

package datastore

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdempotencyConstants(t *testing.T) {
	// These values are part of the wire and schema contract with the
	// deployment platform connector and the index migration Job; they must
	// not drift.
	assert.Equal(t, "idempotencyKey", HealthEventIdempotencyKeyMetadataField)
	assert.Equal(t, "healthevent_idempotency_key_unique", HealthEventIdempotencyIndexName)
}

func TestBulkWriteFailureError(t *testing.T) {
	t.Run("a duplicate on another index names the index and keeps the server's message", func(t *testing.T) {
		failure := &BulkWriteFailure{
			InsertedCount:  3,
			DuplicateCount: 1,
			Failed: BulkDocumentError{
				DocumentIndex: 4, Duplicate: true, IndexName: "some_other_unique_index",
				Message: "E11000 duplicate key error collection: db.HealthEvents index: some_other_unique_index",
			},
		}

		assert.Equal(t, `bulk write failure at document 4 (duplicate on index "some_other_unique_index"): `+
			`E11000 duplicate key error collection: db.HealthEvents index: some_other_unique_index; `+
			`3 documents inserted, 1 already stored`, failure.Error())
	})

	t.Run("a document the server refused keeps its message", func(t *testing.T) {
		failure := &BulkWriteFailure{Failed: BulkDocumentError{DocumentIndex: 0, Message: "Document failed validation"}}

		assert.Equal(t, "bulk write failure at document 0: Document failed validation; 0 documents inserted, 0 already stored",
			failure.Error())
	})

	t.Run("a long server message is cut, an empty one is left out", func(t *testing.T) {
		long := &BulkWriteFailure{Failed: BulkDocumentError{Message: strings.Repeat("x", maxFailureMessageLength+50)}}
		assert.Contains(t, long.Error(), strings.Repeat("x", maxFailureMessageLength)+"...")
		assert.NotContains(t, long.Error(), strings.Repeat("x", maxFailureMessageLength+1))

		empty := &BulkWriteFailure{Failed: BulkDocumentError{DocumentIndex: 2}}
		assert.Equal(t, "bulk write failure at document 2; 0 documents inserted, 0 already stored", empty.Error())
	})
}

func TestBulkDocumentErrorDuplicateOn(t *testing.T) {
	tests := []struct {
		name     string
		docErr   BulkDocumentError
		expected bool
	}{
		{"duplicate on the idempotency index", BulkDocumentError{Duplicate: true, IndexName: HealthEventIdempotencyIndexName}, true},
		{"duplicate on a different index", BulkDocumentError{Duplicate: true, IndexName: "some_other_unique_index"}, false},
		{"not a duplicate", BulkDocumentError{Duplicate: false, Message: "document too large"}, false},
		{"duplicate with unknown index name", BulkDocumentError{Duplicate: true, IndexName: ""}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.docErr.DuplicateOn(HealthEventIdempotencyIndexName))
		})
	}
}

func TestAsBulkWriteFailure(t *testing.T) {
	failure := &BulkWriteFailure{
		InsertedCount: 1,
		Failed:        BulkDocumentError{DocumentIndex: 1, Duplicate: true, IndexName: "some_other_unique_index"},
	}

	t.Run("direct failure", func(t *testing.T) {
		extracted, ok := AsBulkWriteFailure(failure)
		require.True(t, ok)
		assert.Same(t, failure, extracted)
	})

	t.Run("wrapped failure", func(t *testing.T) {
		wrapped := fmt.Errorf("store connector failed: %w", failure)

		extracted, ok := AsBulkWriteFailure(wrapped)
		require.True(t, ok)
		assert.Same(t, failure, extracted)
	})

	t.Run("unrelated error", func(t *testing.T) {
		extracted, ok := AsBulkWriteFailure(errors.New("connection refused"))
		assert.False(t, ok)
		assert.Nil(t, extracted)
	})

	t.Run("nil error", func(t *testing.T) {
		extracted, ok := AsBulkWriteFailure(nil)
		assert.False(t, ok)
		assert.Nil(t, extracted)
	})
}
