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
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
)

func TestFindHealthEventsByQueryBatched_MultipleRows_UsesStableKeysetPagination(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	store := NewPostgreSQLHealthEventStore(db)
	filter := query.New().Build(query.Eq("node_quarantined", nil))
	t1 := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)
	t3 := t2.Add(time.Second)

	firstQuery := "SELECT id, created_at, document FROM health_events WHERE (node_quarantined IS NULL) " +
		"ORDER BY created_at ASC, id ASC LIMIT 2"
	mock.ExpectQuery(regexp.QuoteMeta(firstQuery)).WillReturnRows(
		sqlmock.NewRows([]string{"id", "created_at", "document"}).
			AddRow("00000000-0000-0000-0000-000000000001", t1, []byte(`{}`)).
			AddRow("00000000-0000-0000-0000-000000000002", t2, []byte(`{}`)),
	)

	secondQuery := "SELECT id, created_at, document FROM health_events WHERE (node_quarantined IS NULL) " +
		"AND (created_at, id) > ($1, $2) ORDER BY created_at ASC, id ASC LIMIT 2"
	mock.ExpectQuery(regexp.QuoteMeta(secondQuery)).
		WithArgs(t2, "00000000-0000-0000-0000-000000000002").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "document"}).
			AddRow("00000000-0000-0000-0000-000000000003", t3, []byte(`{}`)))

	var batches [][]datastore.HealthEventWithStatus
	err = store.FindHealthEventsByQueryBatched(
		context.Background(),
		filter,
		2,
		func(batch []datastore.HealthEventWithStatus) error {
			batches = append(batches, batch)

			return nil
		},
	)
	require.NoError(t, err)
	require.Len(t, batches, 2)
	require.Len(t, batches[0], 2)
	require.Len(t, batches[1], 1)
	assert.Equal(t, t1, batches[0][0].CreatedAt)
	assert.Equal(t, "00000000-0000-0000-0000-000000000001", batches[0][0].RawEvent["id"])
	assert.Equal(t, t3, batches[1][0].CreatedAt)
	assert.Equal(t, "00000000-0000-0000-0000-000000000003", batches[1][0].RawEvent["id"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFindHealthEventsByQueryBatched_CallbackError_StopsIteration(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	store := NewPostgreSQLHealthEventStore(db)
	filter := query.New().Build(query.Eq("node_quarantined", nil))
	createdAt := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	queryText := "SELECT id, created_at, document FROM health_events WHERE (node_quarantined IS NULL) " +
		"ORDER BY created_at ASC, id ASC LIMIT 1"
	mock.ExpectQuery(regexp.QuoteMeta(queryText)).WillReturnRows(
		sqlmock.NewRows([]string{"id", "created_at", "document"}).
			AddRow("00000000-0000-0000-0000-000000000001", createdAt, []byte(`{}`)),
	)

	callbackErr := errors.New("stop")
	err = store.FindHealthEventsByQueryBatched(
		context.Background(),
		filter,
		1,
		func([]datastore.HealthEventWithStatus) error { return callbackErr },
	)
	require.ErrorIs(t, err, callbackErr)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFindHealthEventsByQueryBatched_ORFilter_ParenthesizesBeforeCursor(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	store := NewPostgreSQLHealthEventStore(db)
	filter := query.New().Build(query.Or(
		query.Eq("node_quarantined", nil),
		query.Eq("node_quarantined", "NotStarted"),
	))
	t1 := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)

	firstQuery := "SELECT id, created_at, document FROM health_events WHERE " +
		"((node_quarantined IS NULL) OR (node_quarantined = $1)) " +
		"ORDER BY created_at ASC, id ASC LIMIT 1"
	mock.ExpectQuery(regexp.QuoteMeta(firstQuery)).
		WithArgs("NotStarted").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "document"}).
			AddRow("00000000-0000-0000-0000-000000000001", t1, []byte(`{}`)))

	secondQuery := "SELECT id, created_at, document FROM health_events WHERE " +
		"((node_quarantined IS NULL) OR (node_quarantined = $1)) " +
		"AND (created_at, id) > ($2, $3) ORDER BY created_at ASC, id ASC LIMIT 1"
	mock.ExpectQuery(regexp.QuoteMeta(secondQuery)).
		WithArgs("NotStarted", t1, "00000000-0000-0000-0000-000000000001").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "document"}).
			AddRow("00000000-0000-0000-0000-000000000002", t2, []byte(`{}`)))

	thirdQuery := "SELECT id, created_at, document FROM health_events WHERE " +
		"((node_quarantined IS NULL) OR (node_quarantined = $1)) " +
		"AND (created_at, id) > ($2, $3) ORDER BY created_at ASC, id ASC LIMIT 1"
	mock.ExpectQuery(regexp.QuoteMeta(thirdQuery)).
		WithArgs("NotStarted", t2, "00000000-0000-0000-0000-000000000002").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "document"}))

	var ids []any
	err = store.FindHealthEventsByQueryBatched(
		context.Background(),
		filter,
		1,
		func(batch []datastore.HealthEventWithStatus) error {
			for i := range batch {
				ids = append(ids, batch[i].RawEvent["id"])
			}

			return nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, []any{
		"00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-000000000002",
	}, ids)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFindHealthEventsByQueryBatched_InvalidDocument_SkipsAndAdvancesCursor(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	store := NewPostgreSQLHealthEventStore(db)
	filter := query.New().Build(query.Eq("node_quarantined", nil))
	t1 := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)

	firstQuery := "SELECT id, created_at, document FROM health_events WHERE (node_quarantined IS NULL) " +
		"ORDER BY created_at ASC, id ASC LIMIT 1"
	mock.ExpectQuery(regexp.QuoteMeta(firstQuery)).WillReturnRows(
		sqlmock.NewRows([]string{"id", "created_at", "document"}).
			AddRow("00000000-0000-0000-0000-000000000001", t1, []byte(`{invalid`)),
	)

	secondQuery := "SELECT id, created_at, document FROM health_events WHERE (node_quarantined IS NULL) " +
		"AND (created_at, id) > ($1, $2) ORDER BY created_at ASC, id ASC LIMIT 1"
	mock.ExpectQuery(regexp.QuoteMeta(secondQuery)).
		WithArgs(t1, "00000000-0000-0000-0000-000000000001").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "document"}).
			AddRow("00000000-0000-0000-0000-000000000002", t2, []byte(`{}`)))

	thirdQuery := "SELECT id, created_at, document FROM health_events WHERE (node_quarantined IS NULL) " +
		"AND (created_at, id) > ($1, $2) ORDER BY created_at ASC, id ASC LIMIT 1"
	mock.ExpectQuery(regexp.QuoteMeta(thirdQuery)).
		WithArgs(t2, "00000000-0000-0000-0000-000000000002").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "document"}))

	var recovered []datastore.HealthEventWithStatus
	err = store.FindHealthEventsByQueryBatched(
		context.Background(),
		filter,
		1,
		func(batch []datastore.HealthEventWithStatus) error {
			recovered = append(recovered, batch...)

			return nil
		},
	)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	assert.Equal(t, "00000000-0000-0000-0000-000000000002", recovered[0].RawEvent["id"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFindHealthEventsByQueryBatched_MalformedStatusPreservesRawHealthState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	store := NewPostgreSQLHealthEventStore(db)
	filter := query.New().Build(query.Eq("node_quarantined", nil))
	createdAt := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	queryText := "SELECT id, created_at, document FROM health_events WHERE (node_quarantined IS NULL) " +
		"ORDER BY created_at ASC, id ASC LIMIT 2"
	document := []byte(`{
		"healthevent":{"agent":"agent","componentClass":"GPU","checkName":"check","nodeName":"node-a","isHealthy":false},
		"healtheventstatus":{"lastremediationtimestamp":"not-a-timestamp"}
	}`)
	mock.ExpectQuery(regexp.QuoteMeta(queryText)).WillReturnRows(
		sqlmock.NewRows([]string{"id", "created_at", "document"}).
			AddRow("00000000-0000-0000-0000-000000000001", createdAt, document),
	)

	var recovered []datastore.HealthEventWithStatus
	err = store.FindHealthEventsByQueryBatched(
		context.Background(), filter, 2,
		func(batch []datastore.HealthEventWithStatus) error {
			recovered = append(recovered, batch...)

			return nil
		},
	)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	assert.Equal(t, createdAt, recovered[0].CreatedAt)
	assert.Equal(t, "00000000-0000-0000-0000-000000000001", recovered[0].RawEvent["id"])
	rawHealth, ok := recovered[0].RawEvent["healthevent"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, rawHealth["isHealthy"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDecodeHealthEventDocument_LegacyNullRawEventPreservesRawMap(t *testing.T) {
	document := []byte(`{
		"healthevent":{"agent":"agent","componentClass":"GPU","checkName":"check","nodeName":"node-a","isHealthy":false},
		"healtheventstatus":{},
		"RawEvent":null
	}`)

	event, err := decodeHealthEventDocument(document)
	require.NoError(t, err)
	require.NotNil(t, event)
	require.NotNil(t, event.RawEvent)
	assert.Contains(t, event.RawEvent, "RawEvent")
	assert.Nil(t, event.RawEvent["RawEvent"])

	rawHealthEvent, ok := event.RawEvent["healthevent"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "node-a", rawHealthEvent["nodeName"])
}

func TestFindLatestHealthEventByQuery_Result_PreservesCreatedAt(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	store := NewPostgreSQLHealthEventStore(db)
	createdAt := time.Date(2026, time.August, 28, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)SELECT created_at, document FROM health_events.*ORDER BY created_at DESC, id DESC`).
		WithArgs("node-a").
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "document"}).
			AddRow(createdAt, []byte(`{}`)))

	event, err := store.FindLatestHealthEventByQuery(
		context.Background(),
		query.New().Build(query.Eq("node_name", "node-a")),
	)
	require.NoError(t, err)
	require.NotNil(t, event)
	assert.Equal(t, createdAt, event.CreatedAt)
	assert.NotNil(t, event.RawEvent)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFindLatestHealthEventByQuery_EqualTimestamp_UsesDescendingIDTieBreaker(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	store := NewPostgreSQLHealthEventStore(db)
	createdAt := time.Date(2026, time.August, 28, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)SELECT created_at, document FROM health_events.*ORDER BY created_at DESC, id DESC`).
		WithArgs("node-a").
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "document"}).
			AddRow(createdAt, []byte(`{"id":"00000000-0000-0000-0000-000000000002"}`)))

	event, err := store.FindLatestHealthEventByQuery(
		context.Background(),
		query.New().Build(query.Eq("node_name", "node-a")),
	)
	require.NoError(t, err)
	require.NotNil(t, event)
	assert.Equal(t, "00000000-0000-0000-0000-000000000002", event.RawEvent["id"])
	require.NoError(t, mock.ExpectationsWereMet())
}
