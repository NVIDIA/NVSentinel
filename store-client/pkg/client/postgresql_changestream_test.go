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
	"strconv"
	"testing"
	"time"
)

func Test_postgresqlEvent_GetDocumentID(t *testing.T) {
	tests := []struct {
		name        string
		changelogID int64
		recordID    string
		want        string
		wantErr     bool
	}{
		{
			name:        "returns changelog ID as string",
			changelogID: 136,
			recordID:    "6d4e36e4-b9d2-473b-a290-3ed7fb99073e",
			want:        "136",
			wantErr:     false,
		},
		{
			name:        "returns large changelog ID",
			changelogID: 999999,
			recordID:    "uuid-here",
			want:        "999999",
			wantErr:     false,
		},
		{
			name:        "returns zero changelog ID",
			changelogID: 0,
			recordID:    "some-uuid",
			want:        "0",
			wantErr:     false,
		},
		{
			name:        "does NOT return UUID",
			changelogID: 42,
			recordID:    "6d4e36e4-b9d2-473b-a290-3ed7fb99073e",
			want:        "42",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &postgresqlEvent{
				changelogID: tt.changelogID,
				tableName:   "health_events",
				recordID:    tt.recordID,
				operation:   "INSERT",
				changedAt:   time.Now(),
			}

			got, err := e.GetDocumentID()

			if (err != nil) != tt.wantErr {
				t.Errorf("GetDocumentID() error = %v, wantErr %v", err, tt.wantErr)

				return
			}

			if got != tt.want {
				t.Errorf("GetDocumentID() = %v, want %v", got, tt.want)
			}

			// Verify it's int-parseable
			if !tt.wantErr {
				parsed, parseErr := strconv.ParseInt(got, 10, 64)
				if parseErr != nil {
					t.Errorf("GetDocumentID() returned non-int-parseable value: %v, error: %v", got, parseErr)
				}

				if parsed != tt.changelogID {
					t.Errorf("Parsed value %d doesn't match changelogID %d", parsed, tt.changelogID)
				}
			}

			// Verify it's NOT the UUID
			if got == tt.recordID {
				t.Errorf("GetDocumentID() returned recordID (UUID) instead of changelogID")
			}
		})
	}
}

func Test_postgresqlEvent_GetRecordUUID(t *testing.T) {
	tests := []struct {
		name        string
		changelogID int64
		recordID    string
		want        string
		wantErr     bool
	}{
		{
			name:        "returns record UUID",
			changelogID: 136,
			recordID:    "6d4e36e4-b9d2-473b-a290-3ed7fb99073e",
			want:        "6d4e36e4-b9d2-473b-a290-3ed7fb99073e",
			wantErr:     false,
		},
		{
			name:        "returns any string as UUID",
			changelogID: 100,
			recordID:    "some-custom-id",
			want:        "some-custom-id",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &postgresqlEvent{
				changelogID: tt.changelogID,
				tableName:   "health_events",
				recordID:    tt.recordID,
				operation:   "INSERT",
				changedAt:   time.Now(),
			}

			got, err := e.GetRecordUUID()

			if (err != nil) != tt.wantErr {
				t.Errorf("GetRecordUUID() error = %v, wantErr %v", err, tt.wantErr)

				return
			}

			if got != tt.want {
				t.Errorf("GetRecordUUID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_postgresqlEvent_BothMethods(t *testing.T) {
	// Test that both methods return different values as expected
	e := &postgresqlEvent{
		changelogID: 136,
		tableName:   "health_events",
		recordID:    "6d4e36e4-b9d2-473b-a290-3ed7fb99073e",
		operation:   "INSERT",
		changedAt:   time.Now(),
	}

	docID, err := e.GetDocumentID()
	if err != nil {
		t.Fatalf("GetDocumentID() unexpected error: %v", err)
	}

	uuid, err := e.GetRecordUUID()
	if err != nil {
		t.Fatalf("GetRecordUUID() unexpected error: %v", err)
	}

	// They should be different
	if docID == uuid {
		t.Errorf("GetDocumentID() and GetRecordUUID() returned same value: %v", docID)
	}

	// Document ID should be int-parseable
	_, err = strconv.ParseInt(docID, 10, 64)
	if err != nil {
		t.Errorf("GetDocumentID() not int-parseable: %v, error: %v", docID, err)
	}

	// UUID should be longer
	if len(uuid) <= len(docID) {
		t.Errorf("GetRecordUUID() should return longer UUID than GetDocumentID(), got UUID=%s (len %d) vs docID=%s (len %d)",
			uuid, len(uuid), docID, len(docID))
	}

	// Document ID should be the changelog ID
	if docID != "136" {
		t.Errorf("GetDocumentID() = %v, want 136", docID)
	}

	// UUID should be the record ID
	if uuid != "6d4e36e4-b9d2-473b-a290-3ed7fb99073e" {
		t.Errorf("GetRecordUUID() = %v, want 6d4e36e4-b9d2-473b-a290-3ed7fb99073e", uuid)
	}
}

func Test_postgresqlEvent_GetResumeToken(t *testing.T) {
	e := &postgresqlEvent{
		changelogID: 136,
		tableName:   "health_events",
		recordID:    "6d4e36e4-b9d2-473b-a290-3ed7fb99073e",
		operation:   "INSERT",
		changedAt:   time.Now(),
	}

	token := e.GetResumeToken()
	tokenStr := string(token)

	// Resume token should match the changelog ID
	want := "136"
	if tokenStr != want {
		t.Errorf("GetResumeToken() = %v, want %v", tokenStr, want)
	}

	// It should match GetDocumentID()
	docID, err := e.GetDocumentID()
	if err != nil {
		t.Fatalf("GetDocumentID() unexpected error: %v", err)
	}

	if tokenStr != docID {
		t.Errorf("GetResumeToken() = %v, but GetDocumentID() = %v, they should match", tokenStr, docID)
	}

	// It should be int-parseable
	_, err = strconv.ParseInt(tokenStr, 10, 64)
	if err != nil {
		t.Errorf("GetResumeToken() not int-parseable: %v, error: %v", tokenStr, err)
	}
}

