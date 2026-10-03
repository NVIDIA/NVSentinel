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
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateSchemaVersion(t *testing.T) {
	tests := []struct {
		name           string
		currentVersion int64
		queryErr       error
		wantErr        string
	}{
		{
			name:           "required version is applied",
			currentVersion: RequiredSchemaVersion,
		},
		{
			name:           "newer compatible version is applied",
			currentVersion: RequiredSchemaVersion + 1,
		},
		{
			name:           "database requires migration",
			currentVersion: RequiredSchemaVersion - 1,
			wantErr:        "apply the pending SQL migrations",
		},
		{
			name:     "version table is unavailable",
			queryErr: errors.New("relation does not exist"),
			wantErr:  "apply the SQL files",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			query := mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionQuery))
			if tt.queryErr != nil {
				query.WillReturnError(tt.queryErr)
			} else {
				query.WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(tt.currentVersion))
			}

			err = ValidateSchemaVersion(context.Background(), db)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}

			assert.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func setSchemaWaitForTest(t *testing.T, timeout, interval time.Duration) {
	t.Helper()

	origTimeout, origInterval := schemaVersionWaitTimeout, schemaVersionPollInterval
	schemaVersionWaitTimeout, schemaVersionPollInterval = timeout, interval

	t.Cleanup(func() {
		schemaVersionWaitTimeout, schemaVersionPollInterval = origTimeout, origInterval
	})
}

func TestWaitForSchemaVersion_MigrationsLand_ReturnsNil(t *testing.T) {
	setSchemaWaitForTest(t, time.Minute, time.Millisecond)

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionQuery)).
		WillReturnError(errors.New("relation does not exist"))
	mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionQuery)).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(RequiredSchemaVersion - 1))
	mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionQuery)).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(RequiredSchemaVersion))

	require.NoError(t, WaitForSchemaVersion(context.Background(), db))
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestWaitForSchemaVersion_NeverMigrated_ReturnsErrorAfterTimeout(t *testing.T) {
	setSchemaWaitForTest(t, 20*time.Millisecond, 5*time.Millisecond)

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.MatchExpectationsInOrder(false)

	for range 10 {
		mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionQuery)).
			WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(RequiredSchemaVersion - 1))
	}

	err = WaitForSchemaVersion(context.Background(), db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ready after waiting")
	assert.Contains(t, err.Error(), "apply the pending SQL migrations")
}

func TestWaitForSchemaVersion_QueryBlocks_ReturnsErrorAfterTimeout(t *testing.T) {
	setSchemaWaitForTest(t, 50*time.Millisecond, time.Minute)

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionQuery)).
		WillDelayFor(time.Minute).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(RequiredSchemaVersion))

	start := time.Now()
	err = WaitForSchemaVersion(context.Background(), db)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ready after waiting")
	assert.Less(t, time.Since(start), 10*time.Second)
}

func TestWaitForSchemaVersion_ContextCancelled_ReturnsContextError(t *testing.T) {
	setSchemaWaitForTest(t, time.Minute, time.Minute)

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionQuery)).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(RequiredSchemaVersion - 1))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = WaitForSchemaVersion(ctx, db)
	require.ErrorIs(t, err, context.Canceled)
}
