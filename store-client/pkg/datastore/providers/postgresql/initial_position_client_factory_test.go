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

package postgresql_test

import (
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers/postgresql"
	"github.com/nvidia/nvsentinel/store-client/pkg/factory"
)

func TestClientFactory_PostgreSQLInitialPosition_PersistsOrReturnsError(t *testing.T) {
	t.Setenv("DATASTORE_PROVIDER", "postgresql")
	t.Setenv("MONGODB_DATABASE_NAME", "HealthEventsDatabase")
	t.Setenv("MONGODB_TOKEN_COLLECTION_NAME", "ResumeTokens")
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "write failure"
		}
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectQuery("SELECT resume_token FROM resume_tokens").WithArgs("event-exporter").WillReturnError(sql.ErrNoRows)
			insert := mock.ExpectExec("INSERT INTO resume_tokens .*clock_timestamp.*ON CONFLICT .* DO NOTHING").WithArgs("event-exporter")
			if fail {
				insert.WillReturnError(sql.ErrConnDone)
			} else {
				insert.WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery("SELECT resume_token FROM resume_tokens").WithArgs("event-exporter").
					WillReturnRows(sqlmock.NewRows([]string{"resume_token"}).AddRow(`{"timestamp":"2001-02-03T04:05:06.123456Z","eventID":0}`))
			}

			// Exercise the platform-connectors/event-exporter route, including reset's exporter skip.
			w, err := factory.NewClientFactory(nil).CreateChangeStreamWatcher(t.Context(),
				postgresql.NewPostgreSQLDatabaseClient(db, "HealthEvents"), "event-exporter", nil)
			if fail {
				require.ErrorIs(t, err, sql.ErrConnDone)
				assert.Nil(t, w)
			} else {
				require.NoError(t, err)
				require.NotNil(t, w)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
