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
	"fmt"
)

// RequiredSchemaVersion is the minimum PostgreSQL schema version required by
// this store-client release. DDL is applied separately from the application by
// running the SQL files in migrations/ in filename order.
const RequiredSchemaVersion int64 = 2

const currentSchemaVersionQuery = `
	SELECT COALESCE(MAX(version), 0)
	FROM nvsentinel_schema_migrations
`

// ValidateSchemaVersion verifies that the database has been migrated before an
// application starts using it. This check is read-only and requires no DDL
// privileges.
func ValidateSchemaVersion(ctx context.Context, db *sql.DB) error {
	var currentVersion int64

	if err := db.QueryRowContext(ctx, currentSchemaVersionQuery).Scan(&currentVersion); err != nil {
		return fmt.Errorf(
			"failed to read PostgreSQL schema version; apply the SQL files in "+
				"store-client/pkg/datastore/providers/postgresql/migrations in filename order: %w",
			err,
		)
	}

	if currentVersion < RequiredSchemaVersion {
		return fmt.Errorf(
			"PostgreSQL schema version %d is older than required version %d; "+
				"apply the pending SQL migrations before starting NVSentinel",
			currentVersion,
			RequiredSchemaVersion,
		)
	}

	return nil
}
