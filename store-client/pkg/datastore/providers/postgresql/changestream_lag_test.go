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

package postgresql

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const changelogQuery = "SELECT id, record_id, operation, old_values, new_values, changed_at"

func changelogRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "record_id", "operation", "old_values", "new_values", "changed_at"})
}

// A watcher that has not polled yet has no evidence either way, and must report neither
// timestamp rather than claiming to be caught up.
func TestLagStateIsUnknownBeforeFirstPoll(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)

	defer db.Close()

	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)

	lastEmptyBatch, lastEventRead := watcher.LagState()

	assert.True(t, lastEmptyBatch.IsZero())
	assert.True(t, lastEventRead.IsZero())
}

// A poll that returns no rows is the empty batch: affirmative evidence that this consumer is
// caught up with its own filtered view of the changelog.
func TestEmptyPollRecordsCaughtUp(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	defer db.Close()

	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)

	mock.ExpectQuery(changelogQuery).WillReturnRows(changelogRows())

	before := time.Now()
	require.NoError(t, watcher.fetchNewChanges(context.Background()))

	lastEmptyBatch, lastEventRead := watcher.LagState()

	assert.False(t, lastEmptyBatch.Before(before), "empty batch should be recorded at poll time")
	assert.True(t, lastEventRead.IsZero(), "no event was read")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// A poll that returns rows records the newest changed_at, so a consumer working through a
// backlog reports the age of what it is reading rather than zero.
func TestPollWithRowsRecordsNewestChangedAt(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	defer db.Close()

	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)

	oldest := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	newest := oldest.Add(time.Hour)

	mock.ExpectQuery(changelogQuery).WillReturnRows(changelogRows().
		AddRow(1, "record-1", "INSERT", nil, `{"a":1}`, oldest).
		AddRow(2, "record-2", "INSERT", nil, `{"a":2}`, newest))

	require.NoError(t, watcher.fetchNewChanges(context.Background()))

	lastEmptyBatch, lastEventRead := watcher.LagState()

	assert.Equal(t, newest, lastEventRead.UTC())
	assert.True(t, lastEmptyBatch.IsZero(), "a poll that returned rows is not an empty batch")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// Rows arriving out of order must not pull the recorded position backwards, which would make a
// lagging consumer look closer to caught up than it is.
func TestOutOfOrderRowsDoNotRewindLag(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	defer db.Close()

	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)

	newest := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	mock.ExpectQuery(changelogQuery).WillReturnRows(changelogRows().
		AddRow(1, "record-1", "INSERT", nil, `{"a":1}`, newest).
		AddRow(2, "record-2", "INSERT", nil, `{"a":2}`, newest.Add(-30*time.Minute)))

	require.NoError(t, watcher.fetchNewChanges(context.Background()))

	_, lastEventRead := watcher.LagState()
	assert.Equal(t, newest, lastEventRead.UTC())
	assert.NoError(t, mock.ExpectationsWereMet())
}

// Draining a backlog and then finding nothing left is the sequence a recovering consumer goes
// through: the empty batch that follows is what brings reported lag back down.
func TestDrainThenEmptyPollRecordsCaughtUp(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	defer db.Close()

	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)

	backlogged := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)

	mock.ExpectQuery(changelogQuery).WillReturnRows(changelogRows().
		AddRow(1, "record-1", "INSERT", nil, `{"a":1}`, backlogged))
	mock.ExpectQuery(changelogQuery).WillReturnRows(changelogRows())

	ctx := context.Background()
	require.NoError(t, watcher.fetchNewChanges(ctx))

	_, lastEventRead := watcher.LagState()
	require.Equal(t, backlogged, lastEventRead.UTC())

	require.NoError(t, watcher.fetchNewChanges(ctx))

	lastEmptyBatch, lastEventRead := watcher.LagState()

	assert.True(t, lastEmptyBatch.After(lastEventRead),
		"the empty batch is newer than the backlogged event, so lag is measured from it")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// A failed poll is not evidence of anything. Recording it either way would report a healthy
// consumer while the query is broken.
func TestFailedPollRecordsNothing(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	defer db.Close()

	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)

	mock.ExpectQuery(changelogQuery).WillReturnError(assert.AnError)

	require.Error(t, watcher.fetchNewChanges(context.Background()))

	lastEmptyBatch, lastEventRead := watcher.LagState()

	assert.True(t, lastEmptyBatch.IsZero())
	assert.True(t, lastEventRead.IsZero())
	assert.NoError(t, mock.ExpectationsWereMet())
}
