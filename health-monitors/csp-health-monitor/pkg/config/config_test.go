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

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const baseConfig = `
clusterName = "cluster-a"
maintenanceEventPollIntervalSeconds = 60
triggerQuarantineWorkflowTimeLimitMinutes = 30
postMaintenanceHealthyDelayMinutes = 15
nodeReadinessTimeoutMinutes = 60
`

func loadConfig(t *testing.T, extra string) (*Config, error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(baseConfig+extra), 0o600))

	return LoadConfig(path)
}

func TestLoadConfigAkamai(t *testing.T) {
	tests := []struct {
		name    string
		extra   string
		wantErr string
	}{
		{
			name:  "defaults to the production endpoint",
			extra: "[akamai]\nenabled = true\npollingIntervalSeconds = 30\n",
		},
		{
			name:  "explicit production endpoint",
			extra: "[akamai]\nenabled = true\npollingIntervalSeconds = 60\napiEndpoint = \"https://api.linode.com/v4\"\n",
		},
		{
			name:    "polling interval below the minimum",
			extra:   "[akamai]\nenabled = true\npollingIntervalSeconds = 10\n",
			wantErr: "akamai.pollingIntervalSeconds must be at least",
		},
		{
			name:    "endpoint on a foreign host",
			extra:   "[akamai]\nenabled = true\npollingIntervalSeconds = 30\napiEndpoint = \"https://example.com/v4\"\n",
			wantErr: "not an approved",
		},
		{
			name:    "endpoint over http",
			extra:   "[akamai]\nenabled = true\npollingIntervalSeconds = 30\napiEndpoint = \"http://api.linode.com/v4\"\n",
			wantErr: "cleartext",
		},
		{
			name:  "disabled Akamai is not validated",
			extra: "[akamai]\nenabled = false\npollingIntervalSeconds = 1\napiEndpoint = \"http://example.com\"\n",
		},
		{
			name: "two CSPs enabled",
			extra: "[akamai]\nenabled = true\npollingIntervalSeconds = 30\n" +
				"[aws]\nenabled = true\npollingIntervalSeconds = 30\n",
			wantErr: "multiple CSPs enabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(t, tt.extra)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)
			assert.NotNil(t, cfg)
		})
	}
}
