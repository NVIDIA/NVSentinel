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
