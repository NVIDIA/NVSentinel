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

package factory

import (
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestClientFactory_CleanupOwnsPool_NoSetupDDL(t *testing.T) {
	t.Setenv("DATASTORE_PROVIDER", "postgresql")
	t.Setenv("DATASTORE_HOST", "localhost")
	t.Setenv("DATASTORE_DATABASE", "test")
	t.Setenv("DATASTORE_USERNAME", "test")
	t.Setenv("MONGODB_COLLECTION_NAME", "HealthEvents")
	t.Setenv("DATASTORE_CHANGELOG_RETENTION_EXPERIMENTAL", "true")
	t.Setenv("DATASTORE_CHANGELOG_RETENTION", "24h")
	factory, err := NewClientFactoryFromEnv()
	require.NoError(t, err)
	ran := make(chan struct{})
	matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		err := sqlmock.QueryMatcherRegexp.Match(expected, actual)
		if err == nil && actual == "SELECT pg_advisory_unlock($1)" {
			close(ran)
		}
		return err
	})
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true), sqlmock.QueryMatcherOption(matcher))
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectPing()
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery("SELECT.*to_regclass").WillReturnRows(sqlmock.NewRows([]string{"ready"}).AddRow(false))
	mock.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
	mock.ExpectClose()
	c, err := factory.createPostgreSQLDatabaseClient(t.Context(), func(string) (*sql.DB, error) { return db, nil })
	require.NoError(t, err)
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("factory cleaner did not run")
	}
	require.NoError(t, c.Close(t.Context()))
	require.NoError(t, mock.ExpectationsWereMet())
}
