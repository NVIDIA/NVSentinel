// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
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

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type healthEventIDQueryBuilder struct{}

func (healthEventIDQueryBuilder) ToMongo() map[string]interface{} { return nil }
func (healthEventIDQueryBuilder) ToSQL() (string, []interface{}) {
	return "created_at > $1", []interface{}{"2026-01-01"}
}

func TestPostgreSQLHealthEventStoreFindHealthEventIDsByQueryBatched(t *testing.T) {
	ctx := context.Background()

	t.Run("selects IDs only and delivers batches", func(t *testing.T) {
		db, mockDB, err := sqlmock.New()
		require.NoError(t, err)
		defer db.Close()

		store := &PostgreSQLHealthEventStore{db: db}
		firstQuery := "SELECT id FROM health_events WHERE created_at > $1 ORDER BY id LIMIT 2"
		secondQuery := "SELECT id FROM health_events WHERE created_at > $1 AND id > $2 ORDER BY id LIMIT 2"
		secondID := "00000000-0000-0000-0000-000000000002"

		mockDB.ExpectQuery(regexp.QuoteMeta(firstQuery)).
			WithArgs("2026-01-01").
			WillReturnRows(sqlmock.NewRows([]string{"id"}).
				AddRow("00000000-0000-0000-0000-000000000001").
				AddRow(secondID))
		mockDB.ExpectQuery(regexp.QuoteMeta(secondQuery)).
			WithArgs("2026-01-01", secondID).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).
				AddRow("00000000-0000-0000-0000-000000000003"))

		var batches [][]interface{}
		err = store.FindHealthEventIDsByQueryBatched(ctx, healthEventIDQueryBuilder{}, 2,
			func(batch []interface{}) error {
				batches = append(batches, append([]interface{}(nil), batch...))
				return nil
			})

		require.NoError(t, err)
		require.Len(t, batches, 2)
		assert.Equal(t, []interface{}{
			"00000000-0000-0000-0000-000000000001",
			"00000000-0000-0000-0000-000000000002",
		}, batches[0])
		assert.Equal(t, []interface{}{
			"00000000-0000-0000-0000-000000000003",
		}, batches[1])
		assert.NoError(t, mockDB.ExpectationsWereMet())
	})

	t.Run("callback error stops pagination", func(t *testing.T) {
		db, mockDB, err := sqlmock.New()
		require.NoError(t, err)
		defer db.Close()

		store := &PostgreSQLHealthEventStore{db: db}
		query := "SELECT id FROM health_events WHERE created_at > $1 ORDER BY id LIMIT 1"
		mockDB.ExpectQuery(regexp.QuoteMeta(query)).
			WithArgs("2026-01-01").
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("event-id"))

		callbackErr := errors.New("stop processing")
		err = store.FindHealthEventIDsByQueryBatched(ctx, healthEventIDQueryBuilder{}, 1,
			func([]interface{}) error { return callbackErr })

		assert.ErrorIs(t, err, callbackErr)
		assert.NoError(t, mockDB.ExpectationsWereMet())
	})

	t.Run("rejects invalid batch size", func(t *testing.T) {
		store := &PostgreSQLHealthEventStore{}
		err := store.FindHealthEventIDsByQueryBatched(ctx, healthEventIDQueryBuilder{}, 0,
			func([]interface{}) error { return nil })
		assert.ErrorContains(t, err, "batch size")
	})
}

var _ datastore.QueryBuilder = healthEventIDQueryBuilder{}
