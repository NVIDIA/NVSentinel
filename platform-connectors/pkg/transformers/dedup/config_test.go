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

package dedup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigParsesDurationsAndIncludeChecks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dedup.toml")
	err := os.WriteFile(path, []byte(`
suppressionWindow = "3m"
cleanupInterval = "60s"
includeChecks = ["SysLogsXIDError", "SysLogsSXIDError"]
`), 0o600)
	require.NoError(t, err)

	cfg, err := LoadConfig(path)

	require.NoError(t, err)
	assert.Equal(t, 3*time.Minute, cfg.SuppressionWindow)
	assert.Equal(t, time.Minute, cfg.CleanupInterval)
	assert.Equal(t, []string{"SysLogsXIDError", "SysLogsSXIDError"}, cfg.IncludeChecks)
}

func TestLoadConfigDefaultsWhenPathMissing(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "missing.toml"))

	require.NoError(t, err)
	assert.Equal(t, DefaultConfig(), cfg)
}
