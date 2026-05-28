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
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadHealthEventBehaviourOverrides(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	tomlConfig := `
[[policies]]
name = "operator-pod-unhealthy"
enabled = true

[policies.resource]
group = ""
version = "v1"
kind = "Pod"

[policies.predicate]
expression = "true"

[policies.healthEvent]
componentClass = "Software"
isFatal = true
message = "operator pod is unhealthy"
recommendedAction = "CONTACT_SUPPORT"
errorCode = ["OPERATOR_POD_UNHEALTHY"]

[policies.healthEvent.quarantineOverrides]
force = true

[policies.healthEvent.drainOverrides]
skip = true
`
	require.NoError(t, os.WriteFile(configPath, []byte(tomlConfig), 0o600))

	cfg, err := Load(configPath)
	require.NoError(t, err)
	require.Len(t, cfg.Policies, 1)

	healthEvent := cfg.Policies[0].HealthEvent
	require.NotNil(t, healthEvent.QuarantineOverrides)
	require.True(t, healthEvent.QuarantineOverrides.Force)
	require.False(t, healthEvent.QuarantineOverrides.Skip)
	require.NotNil(t, healthEvent.DrainOverrides)
	require.False(t, healthEvent.DrainOverrides.Force)
	require.True(t, healthEvent.DrainOverrides.Skip)
}

func TestLoadRejectsConflictingBehaviourOverrides(t *testing.T) {
	tests := []struct {
		name         string
		overrideTOML string
		wantError    string
	}{
		{
			name: "quarantine overrides force and skip",
			overrideTOML: `
[policies.healthEvent.quarantineOverrides]
force = true
skip = true
`,
			wantError: `healthEvent.quarantineOverrides cannot set both force and skip`,
		},
		{
			name: "drain overrides force and skip",
			overrideTOML: `
[policies.healthEvent.drainOverrides]
force = true
skip = true
`,
			wantError: `healthEvent.drainOverrides cannot set both force and skip`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.toml")
			tomlConfig := `
[[policies]]
name = "operator-pod-unhealthy"
enabled = true

[policies.resource]
group = ""
version = "v1"
kind = "Pod"

[policies.predicate]
expression = "true"

[policies.healthEvent]
componentClass = "Software"
isFatal = true
message = "operator pod is unhealthy"
recommendedAction = "CONTACT_SUPPORT"
errorCode = ["OPERATOR_POD_UNHEALTHY"]
` + tt.overrideTOML
			require.NoError(t, os.WriteFile(configPath, []byte(tomlConfig), 0o600))

			_, err := Load(configPath)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantError)
		})
	}
}
