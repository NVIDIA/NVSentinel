// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func checkpointJSON(t *testing.T, ts time.Time, id int64) []byte {
	t.Helper()

	token, err := json.Marshal(map[string]any{"timestamp": ts.Format(time.RFC3339Nano), "eventID": id})
	require.NoError(t, err)

	return token
}

func checkpointEvent(id int64, ts time.Time) datastore.EventWithToken {
	return datastore.EventWithToken{
		Event: map[string]any{"clusterTime": ts}, ResumeToken: []byte(strconv.FormatInt(id, 10)),
	}
}

func expectCheckpoint(t *testing.T, mock sqlmock.Sqlmock, processedID int64, ts time.Time, savedID int64) {
	t.Helper()
	mock.ExpectExec("UPDATE datastore_changelog SET processed = TRUE").
		WithArgs(processedID, healthEventsTable).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO resume_tokens").
		WithArgs("test-client", checkpointJSON(t, ts, savedID)).WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestMarkProcessed_BufferedRestart_ReplaysUnacknowledgedDeliveries(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)
	rows := changelogRows()

	for id := int64(1); id <= 5; id++ {
		healthEventRow(rows, id, base.Add(time.Duration(id)*time.Second))
	}

	mock.ExpectQuery(changelogQuery).WithArgs(healthEventsTable, time.Time{}, int64(0)).WillReturnRows(rows)
	require.NoError(t, watcher.fetchNewChanges(ctx))
	require.Equal(t, "1", string((<-watcher.Events()).ResumeToken))
	require.Len(t, watcher.events, 4)
	expectCheckpoint(t, mock, 1, base.Add(2*time.Second), 1)
	require.NoError(t, watcher.MarkProcessed(ctx, []byte("1")))
	require.NoError(t, watcher.Close(ctx))

	restarted := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)
	mock.ExpectQuery("SELECT resume_token FROM resume_tokens").WithArgs("test-client").
		WillReturnRows(sqlmock.NewRows([]string{"resume_token"}).
			AddRow(string(checkpointJSON(t, base.Add(2*time.Second), 1))))
	require.NoError(t, restarted.loadResumePosition(ctx))
	require.Empty(t, restarted.pending)

	rows = changelogRows()

	for id := int64(2); id <= 5; id++ {
		healthEventRow(rows, id, base.Add(time.Duration(id)*time.Second))
	}

	mock.ExpectQuery(changelogQuery).WithArgs(healthEventsTable, base.Add(2*time.Second), int64(1)).WillReturnRows(rows)
	require.NoError(t, restarted.fetchNewChanges(ctx))
	assert.Equal(t, []string{"2", "3", "4", "5"}, drainTokens(restarted))
	expectCheckpoint(t, mock, 5, base.Add(5*time.Second), 5)
	require.NoError(t, restarted.MarkProcessed(ctx, []byte("5")))
	assert.Empty(t, restarted.pending)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestMarkProcessed_CumulativeDeliveryOrder_PreservesOutstandingPositions(t *testing.T) {
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		events      []datastore.EventWithToken
		token       string
		processedID int64
		wantTS      time.Time
		wantID      int64
		wantPending int
	}{
		{
			name: "later delivery covers earlier deliveries", token: "2", processedID: 2,
			events: []datastore.EventWithToken{checkpointEvent(1, base), checkpointEvent(2, base), checkpointEvent(3, base)},
			wantTS: base, wantID: 2, wantPending: 1,
		},
		{
			name: "lower id with later changed_at remains pending", token: "151", processedID: 151,
			events: []datastore.EventWithToken{checkpointEvent(151, base), checkpointEvent(1, base.Add(time.Second))},
			wantTS: base.Add(time.Second), wantID: 0, wantPending: 1,
		},
		{
			name: "lower id later delivery acknowledges high id prefix", token: "1", processedID: 1,
			events: []datastore.EventWithToken{checkpointEvent(151, base), checkpointEvent(1, base.Add(time.Second))},
			wantTS: base.Add(time.Second), wantID: 1,
		},
		{
			name: "late NOTIFY below cursor holds checkpoint back", token: "10", processedID: 10,
			events: []datastore.EventWithToken{checkpointEvent(10, base.Add(time.Second)), checkpointEvent(2, base)},
			wantTS: base, wantID: 1, wantPending: 1,
		},
		{
			name: "earliest matching duplicate delivery only", token: "1", processedID: 1,
			events: []datastore.EventWithToken{checkpointEvent(1, base), checkpointEvent(2, base), checkpointEvent(1, base)},
			wantTS: base, wantID: 0, wantPending: 2,
		},
		{
			name: "unknown token", token: "2", processedID: 2,
			events: []datastore.EventWithToken{checkpointEvent(1, base), checkpointEvent(3, base)},
			wantTS: base, wantID: 0, wantPending: 2,
		},
		{
			name: "unknown token ahead of cursor", token: "999", processedID: 999,
			events: []datastore.EventWithToken{checkpointEvent(1, base), checkpointEvent(3, base)},
			wantTS: base, wantID: 0, wantPending: 2,
		},
		{
			name: "empty token", processedID: 3,
			events: []datastore.EventWithToken{checkpointEvent(1, base), checkpointEvent(3, base)},
			wantTS: base, wantID: 0, wantPending: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)
			require.NoError(t, watcher.sendEventsToChannel(context.Background(), tt.events))
			if tt.token == "999" {
				mock.ExpectQuery("SELECT changed_at FROM datastore_changelog").WithArgs(int64(999)).
					WillReturnRows(sqlmock.NewRows([]string{"changed_at"}).AddRow(base.Add(time.Second)))
			}

			expectCheckpoint(t, mock, tt.processedID, tt.wantTS, tt.wantID)
			require.NoError(t, watcher.MarkProcessed(context.Background(), []byte(tt.token)))
			assert.Len(t, watcher.pending, tt.wantPending)
			assert.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestMarkProcessed_WatermarkAndDuplicate_AcknowledgesOnlyCompletedPrefix(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)
	tracker := client.NewLowWaterMarkTracker()
	seqs := make([]uint64, 5)

	for i := range seqs {
		event := checkpointEvent(int64(i+1), base)
		require.NoError(t, watcher.sendEventsToChannel(ctx, []datastore.EventWithToken{event}))
		seqs[i] = tracker.Register((<-watcher.Events()).ResumeToken)
	}

	require.Nil(t, tracker.MarkDone(seqs[2]))
	require.Nil(t, tracker.MarkDone(seqs[1]))
	token := tracker.MarkDone(seqs[0])
	require.Equal(t, "3", string(token))
	expectCheckpoint(t, mock, 3, base, 3)
	require.NoError(t, watcher.MarkProcessed(ctx, token))

	// A retry or a token from before restart cannot certify deliveries 4 and 5.
	for _, duplicate := range []string{"3", "1"} {
		expectCheckpoint(t, mock, mustParseCheckpointID(t, duplicate), base, 3)
		require.NoError(t, watcher.MarkProcessed(ctx, []byte(duplicate)))
		require.Len(t, watcher.pending, 2)
	}

	require.Nil(t, tracker.MarkDone(seqs[4]))
	token = tracker.MarkDone(seqs[3])
	expectCheckpoint(t, mock, 5, base, 5)
	require.NoError(t, watcher.MarkProcessed(ctx, token))
	assert.Empty(t, watcher.pending)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func mustParseCheckpointID(t *testing.T, token string) int64 {
	t.Helper()

	id, err := strconv.ParseInt(token, 10, 64)
	require.NoError(t, err)

	return id
}

func TestMarkProcessed_LateNotify_RewindsPreviouslySavedCheckpoint(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)
	require.NoError(t, watcher.sendEventsToChannel(ctx, []datastore.EventWithToken{checkpointEvent(10, base)}))
	expectCheckpoint(t, mock, 10, base, 10)
	require.NoError(t, watcher.MarkProcessed(ctx, []byte("10")))

	late := base.Add(-time.Second)
	mock.ExpectQuery(changelogQuery).WithArgs(healthEventsTable, int64(2)).
		WillReturnRows(healthEventRow(changelogRows(), 2, late))
	require.NoError(t, watcher.fetchChangeByID(ctx, 2))
	expectCheckpoint(t, mock, 10, late, 1)
	require.NoError(t, watcher.MarkProcessed(ctx, []byte("10")))
	assert.Equal(t, base, watcher.lastTimestamp)
	assert.Equal(t, int64(10), watcher.lastEventID)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestMarkProcessed_CheckpointWriteFails_ReturnsErrorAndRetryStaysSafe(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)
	require.NoError(t, watcher.sendEventsToChannel(ctx,
		[]datastore.EventWithToken{checkpointEvent(1, base), checkpointEvent(2, base)}))
	mock.ExpectExec("UPDATE datastore_changelog").WithArgs(int64(1), healthEventsTable).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO resume_tokens").WithArgs("test-client", checkpointJSON(t, base, 1)).
		WillReturnError(sql.ErrConnDone)
	require.ErrorIs(t, watcher.MarkProcessed(ctx, []byte("1")), sql.ErrConnDone)
	expectCheckpoint(t, mock, 1, base, 1)
	require.NoError(t, watcher.MarkProcessed(ctx, []byte("1")))
	assert.Len(t, watcher.pending, 1)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestSendEventsToChannel_PendingLimit_BackpressuresAndResumesAfterAck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		defer db.Close()

		ctx := context.Background()
		base := time.Now()
		watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)

		// Model a consumer draining and dropping its deliveries without acknowledging.
		for id := int64(1); id <= pendingDeliveryLimit; id++ {
			require.NoError(t, watcher.sendEventsToChannel(ctx, []datastore.EventWithToken{checkpointEvent(id, base)}))
			<-watcher.Events()
		}

		done := make(chan error, 1)
		go func() {
			done <- watcher.sendEventsToChannel(ctx, []datastore.EventWithToken{checkpointEvent(pendingDeliveryLimit+1, base)})
		}()

		synctest.Wait()
		require.Len(t, watcher.pending, pendingDeliveryLimit)
		require.True(t, watcher.pendingLimitWarned)
		require.Empty(t, watcher.events)
		require.Equal(t, int64(pendingDeliveryLimit), watcher.lastEventID)
		// Empty and unknown acknowledgments must neither certify nor unblock this window.
		for _, token := range []string{"", "0"} {
			processedID := int64(pendingDeliveryLimit)
			if token == "0" {
				processedID = 0
			}

			expectCheckpoint(t, mock, processedID, base, 0)
			require.NoError(t, watcher.MarkProcessed(ctx, []byte(token)))
			synctest.Wait()
			require.Len(t, watcher.pending, pendingDeliveryLimit)
			require.Empty(t, watcher.events)
		}

		expectCheckpoint(t, mock, pendingDeliveryLimit, base, pendingDeliveryLimit)
		require.NoError(t, watcher.MarkProcessed(ctx, []byte(strconv.Itoa(pendingDeliveryLimit))))
		require.NoError(t, <-done)
		assert.Len(t, watcher.pending, 1)
		assert.Equal(t, int64(pendingDeliveryLimit+1), watcher.pending[0].id)
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestSendEventsToChannel_AbandonedSend_RemovesOnlyUnsentDelivery(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(strconv.FormatBool(stop), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				watcher := NewPostgreSQLChangeStreamWatcher(nil, "test-client", healthEventsTable, "", ModePolling)
				watcher.events = make(chan datastore.EventWithToken)
				done := make(chan error, 1)
				go func() {
					done <- watcher.sendEventsToChannel(ctx, []datastore.EventWithToken{checkpointEvent(1, time.Now())})
				}()

				synctest.Wait()
				require.Len(t, watcher.pending, 1)

				if stop {
					require.NoError(t, watcher.Close(ctx))
					require.NoError(t, watcher.Close(ctx))
					require.NoError(t, <-done)
				} else {
					cancel()
					require.ErrorIs(t, <-done, context.Canceled)
				}

				assert.Empty(t, watcher.pending)
			})
		})
	}
}

func TestMarkProcessed_ConcurrentDeliveryAndAck_CheckpointsNeverPassPending(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	const count = 100

	ctx := context.Background()
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)

	for id := int64(1); id <= count; id++ {
		expectCheckpoint(t, mock, id, base, id)
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		for id := int64(1); id <= count; id++ {
			assert.NoError(t, watcher.sendEventsToChannel(ctx, []datastore.EventWithToken{checkpointEvent(id, base)}))
		}
	})

	for range count {
		event := <-watcher.Events()
		require.NoError(t, watcher.MarkProcessed(ctx, event.ResumeToken))
	}

	wg.Wait()
	assert.Empty(t, watcher.pending)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestSendEventsToChannel_PendingLimit_CancellationAndStopReleaseSender(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(strconv.FormatBool(stop), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				watcher := NewPostgreSQLChangeStreamWatcher(nil, "test-client", healthEventsTable, "", ModePolling)
				for id := int64(1); id <= pendingDeliveryLimit; id++ {
					require.NoError(t, watcher.sendEventsToChannel(ctx,
						[]datastore.EventWithToken{checkpointEvent(id, time.Now())}))
					<-watcher.Events()
				}

				done := make(chan error, 1)
				go func() {
					done <- watcher.sendEventsToChannel(ctx,
						[]datastore.EventWithToken{checkpointEvent(pendingDeliveryLimit+1, time.Now())})
				}()

				synctest.Wait()
				require.True(t, watcher.pendingLimitWarned)

				if stop {
					require.NoError(t, watcher.Close(ctx))
					require.NoError(t, <-done)
				} else {
					cancel()
					require.ErrorIs(t, <-done, context.Canceled)
				}

				assert.Len(t, watcher.pending, pendingDeliveryLimit)
				assert.Equal(t, uint64(pendingDeliveryLimit), watcher.deliverySeq)
			})
		})
	}
}

func TestMarkProcessed_FilteredTail_SavesCursorWhenMatchingDeliveriesAreAcknowledged(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	watcher := NewPostgreSQLChangeStreamWatcher(db, "test-client", healthEventsTable, "", ModePolling)
	watcher.pipelineFilter, err = NewPipelineFilter([]any{map[string]any{
		"$match": map[string]any{"operationType": "insert"},
	}})
	require.NoError(t, err)

	matching := checkpointEvent(1, base)
	matching.Event["operationType"] = "insert"
	filtered := checkpointEvent(2, base.Add(time.Second))
	filtered.Event["operationType"] = "update"
	require.NoError(t, watcher.sendEventsToChannel(ctx, []datastore.EventWithToken{matching, filtered}))
	require.Len(t, watcher.pending, 1)
	require.Len(t, watcher.events, 1)
	expectCheckpoint(t, mock, 1, base.Add(time.Second), 2)
	require.NoError(t, watcher.MarkProcessed(ctx, []byte("1")))
	assert.Empty(t, watcher.pending)
	assert.NoError(t, mock.ExpectationsWereMet())
}
