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
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestLoadCleanupConfig_Retention_ValidateOrDisable(t *testing.T) {
	for _, raw := range []string{"", "0", "7d", "-24h", "23h59m", "broken", "999999999999999999999h", "24h", "168h"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("DATASTORE_CHANGELOG_RETENTION_EXPERIMENTAL", "true")
			t.Setenv("DATASTORE_CHANGELOG_RETENTION", raw)
			c := loadCleanupConfig(nil)
			if raw != "24h" && raw != "168h" {
				require.Zero(t, c.retention)
				return
			}
			expected, err := time.ParseDuration(raw)
			require.NoError(t, err)
			require.Equal(t, expected, c.retention)
			require.Equal(t, time.Hour, c.interval)
			require.Equal(t, 10000, c.batchSize)
			require.Equal(t, 100, c.maxBatches)
		})
	}
}

func TestLoadCleanupConfig_InvalidLimits_Disable(t *testing.T) {
	t.Setenv("DATASTORE_CHANGELOG_RETENTION_EXPERIMENTAL", "true")
	t.Setenv("DATASTORE_CHANGELOG_RETENTION", "24h")
	for _, env := range []string{"DATASTORE_CHANGELOG_CLEANUP_INTERVAL", "DATASTORE_CHANGELOG_CLEANUP_BATCH_SIZE", "DATASTORE_CHANGELOG_CLEANUP_MAX_BATCHES"} {
		for _, raw := range []string{"-1", "0", "garbage", "9999999999999999999999999"} {
			t.Run(env+raw, func(t *testing.T) { t.Setenv(env, raw); require.Zero(t, loadCleanupConfig(nil).retention) })
		}
	}
	c := loadCleanupConfig(map[string]string{"changelogRetention": "168h", "changelogCleanupInterval": "2h", "changelogCleanupBatchSize": "7", "changelogCleanupMaxBatches": "3"})
	require.Equal(t, cleanupConfig{168 * time.Hour, 2 * time.Hour, 7, 3}, c)
	require.Zero(t, loadCleanupConfig(map[string]string{"changelogRetention": "0"}).retention)
}

func TestLoadCleanupConfig_InvalidConfiguration_WarnsOnceAndKeepsClientUsable(t *testing.T) {
	for _, raw := range []string{"7d", "-24h", "1h", "overflow999999999999999999999999h"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("DATASTORE_CHANGELOG_RETENTION_EXPERIMENTAL", "true")
			t.Setenv("DATASTORE_CHANGELOG_RETENTION", raw)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
			require.NoError(t, err)
			defer db.Close()
			c := NewOwnedPostgreSQLDatabaseClient(t.Context(), db, "health_events").(*PostgreSQLDatabaseClient)
			require.Nil(t, c.cleaner)
			require.Equal(t, 1, strings.Count(logs.String(), `"level":"WARN"`))
			mock.ExpectPing()
			require.NoError(t, c.Ping(t.Context()))
			mock.ExpectClose()
			require.NoError(t, c.Close(t.Context()))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestLoadCleanupConfig_ExperimentalAcknowledgment_RequiresExactTrue(t *testing.T) {
	for _, acknowledgment := range []string{"", "false", "TRUE", "1", "true"} {
		t.Run(acknowledgment, func(t *testing.T) {
			t.Setenv("DATASTORE_CHANGELOG_RETENTION", "24h")
			t.Setenv("DATASTORE_CHANGELOG_RETENTION_EXPERIMENTAL", acknowledgment)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			if acknowledgment == "true" {
				mock.ExpectQuery("SELECT pg_try_advisory_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))
			}
			owner := NewOwnedPostgreSQLDatabaseClient(t.Context(), db, healthEventsTable).(*PostgreSQLDatabaseClient)
			if acknowledgment == "true" {
				require.NotNil(t, owner.cleaner)
			} else {
				require.Nil(t, owner.cleaner)
				require.Equal(t, 1, strings.Count(logs.String(), `"level":"WARN"`))
				require.Contains(t, logs.String(), "inert until remaining ADR-068 prerequisites ship")
			}
			if acknowledgment == "true" {
				require.Eventually(t, func() bool { return mock.ExpectationsWereMet() == nil }, time.Second, time.Millisecond)
			}
			// Close cancels and waits before closing the pool.
			require.NoError(t, owner.cleaner.stop(t.Context()))
			mock.ExpectClose()
			require.NoError(t, owner.Close(t.Context()))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestLoadCleanupConfig_IntervalMinimum_WarnsAndDisables(t *testing.T) {
	t.Setenv("DATASTORE_CHANGELOG_RETENTION", "24h")
	t.Setenv("DATASTORE_CHANGELOG_RETENTION_EXPERIMENTAL", "true")
	for _, raw := range []string{"1ns", "1ms", "1m", "59m59s", "1h", "2h"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("DATASTORE_CHANGELOG_CLEANUP_INTERVAL", raw)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			c := loadCleanupConfig(nil)
			interval, err := time.ParseDuration(raw)
			require.NoError(t, err)
			if interval < time.Hour {
				require.Zero(t, c.retention)
				require.Equal(t, 1, strings.Count(logs.String(), `"level":"WARN"`))
				require.Contains(t, logs.String(), raw)
			} else {
				require.Equal(t, interval, c.interval)
				require.Empty(t, logs.String())
			}
		})
	}
}
