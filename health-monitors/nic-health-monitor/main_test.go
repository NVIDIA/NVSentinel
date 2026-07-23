// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	return path
}

func TestLoadConfig_MissingFileFallsBackToDefaults(t *testing.T) {
	cfg, err := loadConfig(filepath.Join(t.TempDir(), "does-not-exist.toml"))

	require.NoError(t, err, "a missing config file is the supported compatibility fallback")
	require.NotNil(t, cfg)
	assert.Equal(t, "/nvsentinel/sys/class/infiniband", cfg.SysClassInfinibandPath)
	assert.Equal(t, "/nvsentinel/sys/class/net", cfg.SysClassNetPath)
	assert.False(t, cfg.CounterDetection.Enabled)
}

func TestLoadConfig_MalformedTOMLFailsStartup(t *testing.T) {
	path := writeConfigFile(t, "this is not toml = = =")

	cfg, err := loadConfig(path)

	require.Error(t, err, "malformed TOML must fail startup, not fall back to defaults")
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), path)
}

func TestLoadConfig_InvalidCounterConfigFailsStartup(t *testing.T) {
	// The exact regression from the field: a thresholdType typo used to
	// silently disable ALL counter monitoring via the defaults fallback.
	path := writeConfigFile(t, `
[counterDetection]
enabled = true

[[counterDetection.counters]]
name = "link_downed"
enabled = true
thresholdType = "velocty"
threshold = 0.0
`)

	cfg, err := loadConfig(path)

	require.Error(t, err, "an invalid counter definition must fail startup")
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "thresholdType")
}

func TestLoadConfig_InvalidRegexFailsStartup(t *testing.T) {
	path := writeConfigFile(t, `nicExclusionRegex = "["`)

	cfg, err := loadConfig(path)

	require.Error(t, err, "an invalid exclusion regex must fail startup")
	assert.Nil(t, cfg)
}

func TestLoadConfig_ValidFileParses(t *testing.T) {
	path := writeConfigFile(t, `
nicExclusionRegex = "^ignored.*"

[counterDetection]
enabled = true

[[counterDetection.counters]]
name = "link_downed"
enabled = true
thresholdType = "delta"
threshold = 0.0
`)

	cfg, err := loadConfig(path)

	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.True(t, cfg.CounterDetection.Enabled)
	require.Len(t, cfg.CounterDetection.Counters, 1)
	assert.Equal(t, "link_downed", cfg.CounterDetection.Counters[0].Name)
	assert.True(t, cfg.CounterDetection.Counters[0].IsFatal,
		"validation must stamp the code-owned severity onto the counter")
}
