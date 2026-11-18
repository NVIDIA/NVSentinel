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

	interval, err := time.ParseDuration(cleanupConfigValue(options,
		"changelogCleanupInterval", "DATASTORE_CHANGELOG_CLEANUP_INTERVAL", "1h"))
	if err != nil || interval <= 0 {
		slog.Warn("Invalid changelog cleanup interval; cleanup disabled")
		return cleanupConfig{}
	}

	size := cleanupPositiveInt(cleanupConfigValue(options,
		"changelogCleanupBatchSize", "DATASTORE_CHANGELOG_CLEANUP_BATCH_SIZE", "10000"))

	batches := cleanupPositiveInt(cleanupConfigValue(options,
		"changelogCleanupMaxBatches", "DATASTORE_CHANGELOG_CLEANUP_MAX_BATCHES", "100"))
	if size == 0 || batches == 0 {
		slog.Warn("Invalid changelog cleanup batch limits; cleanup disabled")
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
