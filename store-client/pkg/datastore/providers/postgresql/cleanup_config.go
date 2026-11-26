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
	"log/slog"
	"os"
	"strconv"
	"time"
)

// Keep the default one-hour NOTIFY catch-up window even across mixed owner intervals.
// Statement and idle-in-transaction timeouts do not bound total writer lifetime.
const minimumCleanupInterval = time.Hour

type cleanupConfig struct {
	retention  time.Duration
	interval   time.Duration
	batchSize  int
	maxBatches int
}

// Resolve once per pool owner. Invalid cleanup settings never prevent ingestion.
func loadCleanupConfig(options map[string]string) cleanupConfig {
	retention, err := time.ParseDuration(cleanupConfigValue(options,
		"changelogRetention", "DATASTORE_CHANGELOG_RETENTION", "0"))
	if err == nil && retention == 0 {
		return cleanupConfig{}
	}

	if err != nil || retention < 24*time.Hour {
		slog.Warn("Invalid changelog retention; cleanup disabled", "minimum", "24h", "error", err)
		return cleanupConfig{}
	}

	rawInterval := cleanupConfigValue(options,
		"changelogCleanupInterval", "DATASTORE_CHANGELOG_CLEANUP_INTERVAL", "1h")
	interval, err := time.ParseDuration(rawInterval)

	if err != nil || interval < minimumCleanupInterval {
		slog.Warn("Invalid changelog cleanup interval; cleanup disabled",
			"value", rawInterval, "minimum", minimumCleanupInterval)

		return cleanupConfig{}
	}

	rawSize := cleanupConfigValue(options,
		"changelogCleanupBatchSize", "DATASTORE_CHANGELOG_CLEANUP_BATCH_SIZE", "10000")
	size := cleanupPositiveInt(rawSize)

	rawBatches := cleanupConfigValue(options,
		"changelogCleanupMaxBatches", "DATASTORE_CHANGELOG_CLEANUP_MAX_BATCHES", "100")
	batches := cleanupPositiveInt(rawBatches)

	if size == 0 || batches == 0 {
		slog.Warn("Invalid changelog cleanup batch limits; cleanup disabled",
			"batch_size", rawSize, "max_batches", rawBatches)

		return cleanupConfig{}
	}

	// This temporary gate stays until the idle fence, rollout docs and acceptance tests ship (#2170).
	// Require the environment acknowledgment explicitly; options cannot bypass the release gate.
	if os.Getenv("DATASTORE_CHANGELOG_RETENTION_EXPERIMENTAL") != "true" {
		slog.Warn("Changelog retention configured but inert until remaining ADR-068 prerequisites ship; " +
			"experimental testing requires DATASTORE_CHANGELOG_RETENTION_EXPERIMENTAL=true")

		return cleanupConfig{}
	}

	return cleanupConfig{retention: retention, interval: interval, batchSize: size, maxBatches: batches}
}

func cleanupConfigValue(options map[string]string, key, env, fallback string) string {
	if raw, ok := options[key]; ok {
		return raw
	}

	if raw := os.Getenv(env); raw != "" {
		return raw
	}

	return fallback
}

func cleanupPositiveInt(raw string) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0
	}

	return value
}
