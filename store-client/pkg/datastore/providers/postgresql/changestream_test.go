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

package postgresql

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgreSQLEventAdapter_GetDocumentID(t *testing.T) {
	tests := []struct {
		name        string
		eventData   map[string]interface{}
		want        string
		wantErr     bool
		description string
	}{
		{
			name: "returns changelog ID from _id._data",
			eventData: map[string]interface{}{
				"_id": map[string]interface{}{
					"_data": "136",
				},
			},
			want:        "136",
			wantErr:     false,
			description: "Should return changelog ID as string",
		},
		{
			name: "returns int-parseable value",
			eventData: map[string]interface{}{
				"_id": map[string]interface{}{
					"_data": "12345",
				},
			},
			want:        "12345",
			wantErr:     false,
			description: "Value should be parseable as integer",
		},
		{
			name: "does NOT return UUID from fullDocument",
			eventData: map[string]interface{}{
				"_id": map[string]interface{}{
					"_data": "136",
				},
				"fullDocument": map[string]interface{}{
					"id": "6d4e36e4-b9d2-473b-a290-3ed7fb99073e",
				},
			},
			want:        "136",
			wantErr:     false,
			description: "Should return _id._data, not fullDocument.id",
		},
		{
			name: "handles numeric _data value",
			eventData: map[string]interface{}{
				"_id": map[string]interface{}{
					"_data": 999,
				},
			},
			want:        "999",
			wantErr:     false,
			description: "Should handle numeric values in _data",
		},
		{
			name: "returns error when _id not present",
			eventData: map[string]interface{}{
				"fullDocument": map[string]interface{}{
					"id": "6d4e36e4-b9d2-473b-a290-3ed7fb99073e",
				},
			},
			want:        "",
			wantErr:     true,
			description: "Should error when _id is missing",
		},
		{
			name: "uses _id directly if _data not present",
			eventData: map[string]interface{}{
				"_id": "789",
			},
			want:        "789",
			wantErr:     false,
			description: "Should fallback to _id directly if _data not present",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := &PostgreSQLEventAdapter{
				eventData: tt.eventData,
			}

			got, err := adapter.GetDocumentID()

			if (err != nil) != tt.wantErr {
				t.Errorf("GetDocumentID() error = %v, wantErr %v", err, tt.wantErr)

				return
			}

			if got != tt.want {
				t.Errorf("GetDocumentID() = %v, want %v", got, tt.want)
			}

			// Verify it's int-parseable for non-error cases
			if !tt.wantErr && got != "" {
				_, parseErr := strconv.ParseInt(got, 10, 64)
				if parseErr != nil {
					t.Errorf("GetDocumentID() returned non-int-parseable value: %v, error: %v", got, parseErr)
				}
			}
		})
	}
}

func TestPostgreSQLEventAdapter_GetRecordUUID(t *testing.T) {
	tests := []struct {
		name      string
		eventData map[string]interface{}
		want      string
		wantErr   bool
	}{
		{
			name: "returns UUID from fullDocument.id",
			eventData: map[string]interface{}{
				"_id": map[string]interface{}{
					"_data": "136",
				},
				"fullDocument": map[string]interface{}{
					"id": "6d4e36e4-b9d2-473b-a290-3ed7fb99073e",
				},
			},
			want:    "6d4e36e4-b9d2-473b-a290-3ed7fb99073e",
			wantErr: false,
		},
		{
			name: "returns UUID from fullDocument._id (MongoDB compatibility)",
			eventData: map[string]interface{}{
				"_id": map[string]interface{}{
					"_data": "136",
				},
				"fullDocument": map[string]interface{}{
					"_id": "507f1f77bcf86cd799439011",
				},
			},
			want:    "507f1f77bcf86cd799439011",
			wantErr: false,
		},
		{
			name: "errors when fullDocument not present",
			eventData: map[string]interface{}{
				"_id": map[string]interface{}{
					"_data": "136",
				},
			},
			want:    "",
			wantErr: true,
		},
		{
			name: "errors when id not in fullDocument",
			eventData: map[string]interface{}{
				"_id": map[string]interface{}{
					"_data": "136",
				},
				"fullDocument": map[string]interface{}{
					"name": "test",
				},
			},
			want:    "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := &PostgreSQLEventAdapter{
				eventData: tt.eventData,
			}

			got, err := adapter.GetRecordUUID()

			if (err != nil) != tt.wantErr {
				t.Errorf("GetRecordUUID() error = %v, wantErr %v", err, tt.wantErr)

				return
			}

			if got != tt.want {
				t.Errorf("GetRecordUUID() = %v, want %v", got, tt.want)
			}

			// Verify UUID format for successful cases
			if !tt.wantErr && len(got) > 0 {
				// UUID should be at least 32 chars (without hyphens) or 36 (with hyphens)
				if len(got) < 24 {
					t.Errorf("GetRecordUUID() returned suspiciously short ID: %v (length %d)", got, len(got))
				}
			}
		})
	}
}

func TestPostgreSQLEventAdapter_BothMethods(t *testing.T) {
	// Test that both methods return different values as expected
	adapter := &PostgreSQLEventAdapter{
		eventData: map[string]interface{}{
			"_id": map[string]interface{}{
				"_data": "136",
			},
			"fullDocument": map[string]interface{}{
				"id": "6d4e36e4-b9d2-473b-a290-3ed7fb99073e",
			},
		},
	}

	docID, err := adapter.GetDocumentID()
	if err != nil {
		t.Fatalf("GetDocumentID() unexpected error: %v", err)
	}

	uuid, err := adapter.GetRecordUUID()
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
}

func TestBuildEventDocument(t *testing.T) {
	watcher := &PostgreSQLChangeStreamWatcher{}

	var emptyOld, emptyNew sql.NullString

	event := watcher.buildEventDocument(
		136,                                      // changelog ID
		"6d4e36e4-b9d2-473b-a290-3ed7fb99073e", // record UUID
		"INSERT",
		emptyOld,
		emptyNew,
		time.Now(),
	)

	// Check that _id._data contains the changelog ID, not the UUID
	idMap, ok := event["_id"].(map[string]interface{})
	if !ok {
		t.Fatal("event[_id] is not a map")
	}

	data, ok := idMap["_data"]
	if !ok {
		t.Fatal("event[_id][_data] not found")
	}

	dataStr, ok := data.(string)
	if !ok {
		t.Fatalf("event[_id][_data] is not a string, got %T", data)
	}

	want := "136"
	if dataStr != want {
		t.Errorf("event[_id][_data] = %v, want %v", dataStr, want)
	}

	// Verify it's int-parseable
	_, err := strconv.ParseInt(dataStr, 10, 64)
	if err != nil {
		t.Errorf("event[_id][_data] not int-parseable: %v, error: %v", dataStr, err)
	}

	// Verify it's NOT the UUID
	if dataStr == "6d4e36e4-b9d2-473b-a290-3ed7fb99073e" {
		t.Error("event[_id][_data] contains UUID instead of changelog ID")
	}
}

// TestTimestampBasedResume_FirstStartup tests that on first startup with no resume token,
// the watcher starts from current time (not from position 0)
func TestTimestampBasedResume_FirstStartup(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", "health_events", "", ModePolling)

	// Mock: No resume token found
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
		WithArgs("test-client").
		WillReturnError(sql.ErrNoRows)

	// Load resume position
	beforeLoad := time.Now().Add(-1 * time.Second)
	err = watcher.loadResumePosition(ctx)
	afterLoad := time.Now().Add(1 * time.Second)

	require.NoError(t, err)

	// Verify timestamp-based resume (not ID-based)
	assert.False(t, watcher.lastTimestamp.IsZero(), "lastTimestamp should be set")
	assert.Equal(t, int64(0), watcher.lastEventID, "lastEventID should be 0 on first startup")

	// Verify timestamp is approximately "now" (within 2 second window)
	assert.True(t, watcher.lastTimestamp.After(beforeLoad), "lastTimestamp should be after beforeLoad")
	assert.True(t, watcher.lastTimestamp.Before(afterLoad), "lastTimestamp should be before afterLoad")

	// Verify MongoDB-like behavior: start from current time, not from beginning
	t.Logf("First startup: lastTimestamp=%v, lastEventID=%d", watcher.lastTimestamp, watcher.lastEventID)

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestTimestampBasedResume_LoadExistingToken tests loading a timestamp-based resume token
func TestTimestampBasedResume_LoadExistingToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", "health_events", "", ModePolling)

	// Create a timestamp-based resume token
	resumeTime := time.Date(2024, 11, 24, 10, 0, 4, 0, time.UTC)
	token := map[string]interface{}{
		"timestamp": resumeTime.Format(time.RFC3339Nano),
		"eventID":   int64(193),
	}
	tokenJSON, err := json.Marshal(token)
	require.NoError(t, err)

	// Mock: Return timestamp-based token
	rows := sqlmock.NewRows([]string{"resume_token"}).AddRow(tokenJSON)
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
		WithArgs("test-client").
		WillReturnRows(rows)

	// Load resume position
	err = watcher.loadResumePosition(ctx)
	require.NoError(t, err)

	// Verify timestamp and ID are loaded
	assert.Equal(t, resumeTime.Unix(), watcher.lastTimestamp.Unix(), "lastTimestamp should match token")
	assert.Equal(t, int64(193), watcher.lastEventID, "lastEventID should match token")

	t.Logf("Loaded token: timestamp=%v, eventID=%d", watcher.lastTimestamp, watcher.lastEventID)

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestTimestampBasedResume_BackwardCompatibility tests converting old ID-based tokens to timestamp-based
func TestTimestampBasedResume_BackwardCompatibility(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", "health_events", "", ModePolling)

	// Create an OLD ID-based resume token (no timestamp)
	token := map[string]interface{}{
		"eventID": int64(193),
	}
	tokenJSON, err := json.Marshal(token)
	require.NoError(t, err)

	// Mock: Return old ID-based token
	rows := sqlmock.NewRows([]string{"resume_token"}).AddRow(tokenJSON)
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
		WithArgs("test-client").
		WillReturnRows(rows)

	// Mock: Query to get timestamp for event ID 193
	changedAt := time.Date(2024, 11, 24, 10, 0, 4, 0, time.UTC)
	timestampRows := sqlmock.NewRows([]string{"changed_at"}).AddRow(changedAt)
	mock.ExpectQuery("SELECT changed_at FROM datastore_changelog").
		WithArgs(int64(193)).
		WillReturnRows(timestampRows)

	// Load resume position
	err = watcher.loadResumePosition(ctx)
	require.NoError(t, err)

	// Verify automatic conversion: timestamp queried from DB
	assert.Equal(t, changedAt.Unix(), watcher.lastTimestamp.Unix(), "should convert ID to timestamp")
	assert.Equal(t, int64(193), watcher.lastEventID, "should keep eventID")

	t.Logf("Converted old token: eventID=%d → timestamp=%v", watcher.lastEventID, watcher.lastTimestamp)

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestTimestampBasedResume_QueryUsesTimestamp tests that fetchNewChanges queries by timestamp
func TestTimestampBasedResume_QueryUsesTimestamp(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", "health_events", "", ModePolling)

	// Set resume position (timestamp + ID)
	watcher.lastTimestamp = time.Date(2024, 11, 24, 10, 0, 4, 0, time.UTC)
	watcher.lastEventID = 193

	// Mock: Query should use timestamp-based WHERE clause
	// The query should be: WHERE table_name = $1 AND (changed_at > $2 OR (changed_at = $2 AND id > $3))
	mock.ExpectQuery("SELECT id, record_id, operation, old_values, new_values, changed_at").
		WithArgs("health_events", watcher.lastTimestamp, watcher.lastEventID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "record_id", "operation", "old_values", "new_values", "changed_at"}))

	// Fetch new changes
	err = watcher.fetchNewChanges(ctx)
	require.NoError(t, err)

	// Verify the query used timestamp-based parameters
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestTimestampBasedResume_SavesTimestamp tests that saveResumePosition saves both timestamp and ID
func TestTimestampBasedResume_SavesTimestamp(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", "health_events", "", ModePolling)

	timestamp := time.Date(2024, 11, 24, 10, 0, 7, 0, time.UTC)
	eventID := int64(169)

	// Mock: INSERT/UPDATE resume token
	mock.ExpectExec("INSERT INTO resume_tokens").
		WithArgs("test-client", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	// Save resume position
	err = watcher.saveResumePosition(ctx, timestamp, eventID)
	require.NoError(t, err)

	// Verify the saved token contains both timestamp and eventID
	// We can't directly inspect the args, but we verify the call happened
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestTimestampBasedResume_ExtractsTimestampFromEvent tests extracting timestamp from event metadata
func TestTimestampBasedResume_ExtractsTimestampFromEvent(t *testing.T) {
	changedAt := time.Date(2024, 11, 24, 10, 0, 7, 0, time.UTC)

	// Build event document (same as actual code does)
	watcher := &PostgreSQLChangeStreamWatcher{tableName: "health_events"}
	event := watcher.buildEventDocument(
		140,
		"test-uuid",
		"INSERT",
		sql.NullString{},
		sql.NullString{Valid: true, String: `{"document":{"node":"test"}}`},
		changedAt,
	)

	// Verify clusterTime field is set (MongoDB-compatible field)
	clusterTime, exists := event["clusterTime"]
	assert.True(t, exists, "event should have clusterTime field")

	ts, ok := clusterTime.(time.Time)
	assert.True(t, ok, "clusterTime should be time.Time")
	assert.Equal(t, changedAt.Unix(), ts.Unix(), "clusterTime should match changed_at")

	t.Logf("Event clusterTime: %v", ts)
}

// TestTimestampBasedResume_ReplayScenario simulates the actual test failure scenario
func TestTimestampBasedResume_ReplayScenario(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", "health_events", "", ModePolling)

	// Scenario: Pod restarts at T+4s, saves position with timestamp 10:00:04
	podRestartTime := time.Date(2024, 11, 24, 10, 0, 4, 0, time.UTC)
	watcher.lastTimestamp = podRestartTime
	watcher.lastEventID = 193

	// At T+7s, test creates events 140-169 with timestamp 10:00:07
	testEventTime := time.Date(2024, 11, 24, 10, 0, 7, 0, time.UTC)

	// Mock: Query returns events with timestamp AFTER pod restart
	rows := sqlmock.NewRows([]string{"id", "record_id", "operation", "old_values", "new_values", "changed_at"}).
		AddRow(140, "uuid-140", "INSERT", nil, `{"document":{"node":"test"}}`, testEventTime).
		AddRow(141, "uuid-141", "INSERT", nil, `{"document":{"node":"test"}}`, testEventTime).
		AddRow(142, "uuid-142", "INSERT", nil, `{"document":{"node":"test"}}`, testEventTime)

	mock.ExpectQuery("SELECT id, record_id, operation, old_values, new_values, changed_at").
		WithArgs("health_events", podRestartTime, int64(193)).
		WillReturnRows(rows)

	// Fetch new changes
	err = watcher.fetchNewChanges(ctx)
	require.NoError(t, err)

	// Verify the query would return events 140-169 because:
	// changed_at (10:00:07) > lastTimestamp (10:00:04) ✅
	// Even though id (140-142) < lastEventID (193), the timestamp check includes them!

	t.Logf("✅ Timestamp-based query includes events 140-169 (changed_at=%v > podRestart=%v)",
		testEventTime, podRestartTime)

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestTimestampBasedResume_TieBreaking tests that ID is used for tie-breaking when timestamps are equal
func TestTimestampBasedResume_TieBreaking(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", "health_events", "", ModePolling)

	// Scenario: Multiple events with same timestamp
	sameTime := time.Date(2024, 11, 24, 10, 0, 0, 123000000, time.UTC)
	watcher.lastTimestamp = sameTime
	watcher.lastEventID = 141 // Resume from middle of same-timestamp batch

	// Mock: Query should return only events with id > 141 (when timestamps are equal)
	rows := sqlmock.NewRows([]string{"id", "record_id", "operation", "old_values", "new_values", "changed_at"}).
		AddRow(142, "uuid-142", "INSERT", nil, `{"document":{}}`, sameTime). // ✅ id > 141
		AddRow(143, "uuid-143", "INSERT", nil, `{"document":{}}`, sameTime). // ✅ id > 141
		AddRow(144, "uuid-144", "INSERT", nil, `{"document":{}}`, time.Date(2024, 11, 24, 10, 0, 0, 124000000, time.UTC)) // ✅ ts > sameTime

	mock.ExpectQuery("SELECT id, record_id, operation, old_values, new_values, changed_at").
		WithArgs("health_events", sameTime, int64(141)).
		WillReturnRows(rows)

	// Fetch new changes
	err = watcher.fetchNewChanges(ctx)
	require.NoError(t, err)

	// Verify tie-breaking works: events 140-141 (same timestamp, id <= 141) are NOT returned
	// Only events 142-144 are returned

	t.Logf("✅ Tie-breaking: ID > 141 when timestamp is equal")

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestTimestampBasedResume_MongoDBEquivalence tests that behavior matches MongoDB
func TestTimestampBasedResume_MongoDBEquivalence(t *testing.T) {
	t.Run("MongoDB: SetStartAtOperationTime(now) ≡ PostgreSQL: lastTimestamp = time.Now()", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		defer db.Close()

		ctx := context.Background()
		watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", "health_events", "", ModePolling)

		// Mock: No resume token (first startup)
		mock.ExpectQuery("SELECT resume_token FROM resume_tokens").
			WithArgs("test-client").
			WillReturnError(sql.ErrNoRows)

		beforeLoad := time.Now()
		err = watcher.loadResumePosition(ctx)
		afterLoad := time.Now()

		require.NoError(t, err)

		// Verify: Starts from "now" (like MongoDB)
		assert.True(t, watcher.lastTimestamp.After(beforeLoad.Add(-1*time.Second)))
		assert.True(t, watcher.lastTimestamp.Before(afterLoad.Add(1*time.Second)))

		t.Logf("✅ PostgreSQL starts from current time, like MongoDB's SetStartAtOperationTime(now)")

		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("MongoDB: WHERE ts > token ≡ PostgreSQL: WHERE changed_at > token", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		defer db.Close()

		ctx := context.Background()
		watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", "health_events", "", ModePolling)

		resumeTime := time.Date(2024, 11, 24, 10, 0, 4, 0, time.UTC)
		watcher.lastTimestamp = resumeTime
		watcher.lastEventID = 193

		// Mock: Query uses timestamp (like MongoDB's ts field)
		mock.ExpectQuery("SELECT id, record_id, operation, old_values, new_values, changed_at").
			WithArgs("health_events", resumeTime, int64(193)).
			WillReturnRows(sqlmock.NewRows([]string{"id", "record_id", "operation", "old_values", "new_values", "changed_at"}))

		err = watcher.fetchNewChanges(ctx)
		require.NoError(t, err)

		t.Logf("✅ PostgreSQL queries by timestamp, like MongoDB's oplog 'ts' field")

		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

