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

package cancellation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTOML(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "cancellations.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

func TestLoadConfig_MissingFile(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Empty(t, cfg.Checks)
}

func TestLoadConfig_Valid(t *testing.T) {
	path := writeTOML(t, `
[[checks]]
  name    = "SysLogsXIDError"
  enabled = true

  [[checks.cancellations]]
    onErrorCode      = "162"
    cancelErrorCodes = ["163"]

  [[checks.cancellations]]
    onErrorCode      = "31"
    cancelErrorCodes = ["13", "43"]
`)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.Len(t, cfg.Checks, 1)

	check := cfg.FindCheck("SysLogsXIDError")
	require.NotNil(t, check)
	assert.True(t, check.Enabled)
	require.Len(t, check.Rules, 2)
	assert.Equal(t, "162", check.Rules[0].OnErrorCode)
	assert.Equal(t, []string{"163"}, check.Rules[0].CancelErrorCodes)
	assert.Equal(t, []string{"13", "43"}, check.Rules[1].CancelErrorCodes)
}

func TestFindCheck_DisabledReturnsNil(t *testing.T) {
	path := writeTOML(t, `
[[checks]]
  name    = "SysLogsXIDError"
  enabled = false

  [[checks.cancellations]]
    onErrorCode      = "162"
    cancelErrorCodes = ["163"]
`)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Nil(t, cfg.FindCheck("SysLogsXIDError"))
}

func TestFindCheck_UnknownReturnsNil(t *testing.T) {
	cfg := &Config{}
	assert.Nil(t, cfg.FindCheck("anything"))

	var nilCfg *Config
	assert.Nil(t, nilCfg.FindCheck("anything"))
}

func TestValidateAgainstEnabledChecks(t *testing.T) {
	cfg := &Config{Checks: []CheckCancellations{{Name: "SysLogsXIDError", Enabled: true}}}

	// Enabled set includes the rule's check → passes.
	require.NoError(t, ValidateAgainstEnabledChecks(cfg,
		[]string{"SysLogsXIDError", "SysLogsSXIDError"}))

	// Enabled set excludes it → rejected with "not enabled" message.
	err := ValidateAgainstEnabledChecks(cfg,
		[]string{"SysLogsSXIDError", "SysLogsGPUFallenOff"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `name "SysLogsXIDError" is not enabled`)

	// nil cfg is a no-op.
	assert.NoError(t, ValidateAgainstEnabledChecks(nil, nil))
}

func TestValidateSupportedChecks(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *Config
		supported []string
		wantErr   string
	}{
		{
			name:      "nil config is a no-op",
			cfg:       nil,
			supported: []string{"SysLogsXIDError"},
		},
		{
			name:      "empty config passes",
			cfg:       &Config{},
			supported: []string{"SysLogsXIDError"},
		},
		{
			name: "supported check passes",
			cfg: &Config{
				Checks: []CheckCancellations{{Name: "SysLogsXIDError", Enabled: true}},
			},
			supported: []string{"SysLogsXIDError"},
		},
		{
			name: "unsupported check is rejected",
			cfg: &Config{
				Checks: []CheckCancellations{{Name: "SysLogsSXIDError", Enabled: true}},
			},
			supported: []string{"SysLogsXIDError"},
			wantErr:   `name "SysLogsSXIDError" does not have cancellation support`,
		},
		{
			name: "unsupported disabled check is still rejected",
			cfg: &Config{
				Checks: []CheckCancellations{{Name: "SysLogsSXIDError", Enabled: false}},
			},
			supported: []string{"SysLogsXIDError"},
			wantErr:   `name "SysLogsSXIDError" does not have cancellation support`,
		},
		{
			name: "empty supported list rejects every check",
			cfg: &Config{
				Checks: []CheckCancellations{{Name: "SysLogsXIDError", Enabled: true}},
			},
			supported: nil,
			wantErr:   `name "SysLogsXIDError" does not have cancellation support`,
		},
		{
			name: "first unsupported check wins (positional error)",
			cfg: &Config{
				Checks: []CheckCancellations{
					{Name: "SysLogsXIDError", Enabled: true},
					{Name: "Bogus", Enabled: true},
				},
			},
			supported: []string{"SysLogsXIDError"},
			wantErr:   `checks[1]: name "Bogus"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSupportedChecks(tc.cfg, tc.supported)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestValidate_RejectsInvalidConfigs(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "empty onErrorCode",
			body: `
[[checks]]
  name    = "SysLogsXIDError"
  enabled = true
  [[checks.cancellations]]
    onErrorCode      = ""
    cancelErrorCodes = ["163"]
`,
			wantErr: "onErrorCode: must be set",
		},
		{
			name: "empty cancelErrorCodes",
			body: `
[[checks]]
  name    = "SysLogsXIDError"
  enabled = true
  [[checks.cancellations]]
    onErrorCode      = "162"
    cancelErrorCodes = []
`,
			wantErr: "cancelErrorCodes must be non-empty",
		},
		{
			name: "duplicate onErrorCode",
			body: `
[[checks]]
  name    = "SysLogsXIDError"
  enabled = true
  [[checks.cancellations]]
    onErrorCode      = "162"
    cancelErrorCodes = ["163"]
  [[checks.cancellations]]
    onErrorCode      = "162"
    cancelErrorCodes = ["164"]
`,
			wantErr: "duplicate onErrorCode",
		},
		{
			name: "self cancel",
			body: `
[[checks]]
  name    = "SysLogsXIDError"
  enabled = true
  [[checks.cancellations]]
    onErrorCode      = "162"
    cancelErrorCodes = ["162"]
`,
			wantErr: "cancels its own source error code",
		},
		{
			name: "duplicate cancelErrorCode within rule",
			body: `
[[checks]]
  name    = "SysLogsXIDError"
  enabled = true
  [[checks.cancellations]]
    onErrorCode      = "162"
    cancelErrorCodes = ["163", "163"]
`,
			wantErr: "duplicate cancelErrorCode",
		},
		{
			name: "empty cancelErrorCode entry",
			body: `
[[checks]]
  name    = "SysLogsXIDError"
  enabled = true
  [[checks.cancellations]]
    onErrorCode      = "162"
    cancelErrorCodes = ["163", ""]
`,
			wantErr: "cancelErrorCodes[1]: must be set",
		},
		{
			name: "duplicate check name",
			body: `
[[checks]]
  name    = "SysLogsXIDError"
  enabled = true
  [[checks.cancellations]]
    onErrorCode      = "162"
    cancelErrorCodes = ["163"]

[[checks]]
  name    = "SysLogsXIDError"
  enabled = true
  [[checks.cancellations]]
    onErrorCode      = "31"
    cancelErrorCodes = ["13"]
`,
			wantErr: "duplicate check name",
		},
		{
			name: "missing check name",
			body: `
[[checks]]
  enabled = true
  [[checks.cancellations]]
    onErrorCode      = "162"
    cancelErrorCodes = ["163"]
`,
			wantErr: "name: must be set",
		},
		{
			name: "padded check name",
			body: `
[[checks]]
  name    = "SysLogsXIDError "
  enabled = true
  [[checks.cancellations]]
    onErrorCode      = "162"
    cancelErrorCodes = ["163"]
`,
			wantErr: "must not have leading or trailing whitespace",
		},
		{
			name: "padded onErrorCode",
			body: `
[[checks]]
  name    = "SysLogsXIDError"
  enabled = true
  [[checks.cancellations]]
    onErrorCode      = " 162"
    cancelErrorCodes = ["163"]
`,
			wantErr: "onErrorCode: must not have leading or trailing whitespace",
		},
		{
			name: "padded cancelErrorCode entry",
			body: `
[[checks]]
  name    = "SysLogsXIDError"
  enabled = true
  [[checks.cancellations]]
    onErrorCode      = "162"
    cancelErrorCodes = ["163 "]
`,
			wantErr: "cancelErrorCodes[0]: must not have leading or trailing whitespace",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTOML(t, tc.body)
			_, err := LoadConfig(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
