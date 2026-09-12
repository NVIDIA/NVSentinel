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
)

const (
	// HealthEventIdempotencyKeyMetadataField is the health event metadata key that
	// carries the server-derived per-event idempotency key. Documents that contain
	// this field are covered by the partial unique idempotency index.
	HealthEventIdempotencyKeyMetadataField = "idempotencyKey"

	// HealthEventIdempotencyIndexName is the name of the unique partial index that
	// enforces per-event idempotency keys on the health events collection/table.
	HealthEventIdempotencyIndexName = "healthevent_idempotency_key_unique"
)

// Sentinel errors returned by VerifyHealthEventIdempotencyIndex. They are wrapped
// with provider-specific detail, so callers must match them with errors.Is.
var (
	// ErrIndexMissing indicates the idempotency index does not exist.
	ErrIndexMissing = errors.New("idempotency index missing")

	// ErrIndexBuilding indicates the index exists but another session is still
	// building it (reported by the MongoDB verification; PostgreSQL's Ensure
	// checks the build progress itself); it wraps ErrIndexMismatch, because
	// the index does not enforce anything yet, and lets callers wait instead
	// of replacing it.
	ErrIndexBuilding = fmt.Errorf("%w: build in progress", ErrIndexMismatch)

	// ErrIndexMismatch indicates an index with the expected name exists but its
	// definition or build state does not match the expected one.
	ErrIndexMismatch = errors.New("idempotency index definition mismatch")
)

// BulkDocumentError describes the failure of a single document within an
// idempotent insert.
type BulkDocumentError struct {
	// DocumentIndex is the position of the failed document in the slice passed
	// to InsertManyIdempotent.
	DocumentIndex int

	// IndexName is the name of the violated index for duplicate-key errors,
	// empty when it could not be determined or the error is not a duplicate.
	IndexName string

	// Duplicate is true when the failure is a duplicate-key violation.
	Duplicate bool

	// Message is the provider error message for this document.
	Message string
}

// DuplicateOn reports whether the failure is a duplicate-key violation of the
// named index, that is, a resend of a document already stored under it.
func (e BulkDocumentError) DuplicateOn(indexName string) bool {
	return e.Duplicate && e.IndexName == indexName
}

// maxFailureMessageLength bounds the provider message quoted by
// BulkWriteFailure.Error, so a verbose server answer cannot flood a log line.
const maxFailureMessageLength = 256

// BulkWriteFailure is the error type returned by InsertManyIdempotent when a
// document fails for a reason other than being a resend already stored under
// the idempotency index (those are skipped and counted in the result). The
// insert is ordered and stops at that document: InsertedCount documents before
// it were stored, DuplicateCount were resends skipped, and the documents after
// it were not attempted; the caller's retry of the whole batch stores them in
// order and meets the stored ones as duplicates. Failed carries the server's
// answer about the document, so the reason survives into logs.
//
//nolint:errname // cross-module API name, consumed outside this module; mirrors mongo.BulkWriteException
type BulkWriteFailure struct {
	InsertedCount  int
	DuplicateCount int
	Failed         BulkDocumentError
}

// Error implements the error interface with the failed document's position,
// the violated index when it is a duplicate, and the provider's message.
func (f *BulkWriteFailure) Error() string {
	var b strings.Builder

	fmt.Fprintf(&b, "bulk write failure at document %d", f.Failed.DocumentIndex)

	if f.Failed.Duplicate {
		fmt.Fprintf(&b, " (duplicate on index %q)", f.Failed.IndexName)
	}

	if msg := f.Failed.Message; msg != "" {
		if len(msg) > maxFailureMessageLength {
			msg = msg[:maxFailureMessageLength] + "..."
		}

		b.WriteString(": " + msg)
	}

	fmt.Fprintf(&b, "; %d documents inserted, %d already stored", f.InsertedCount, f.DuplicateCount)

	return b.String()
}

// AsBulkWriteFailure extracts a *BulkWriteFailure from an error chain.
func AsBulkWriteFailure(err error) (*BulkWriteFailure, bool) {
	return errors.AsType[*BulkWriteFailure](err)
}
