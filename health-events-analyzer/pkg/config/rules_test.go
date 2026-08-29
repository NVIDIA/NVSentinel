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

	"github.com/stretchr/testify/require"
)

func TestLoadTomlConfigRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.toml")
	contents := `
[[rules]]
name = "RepeatedXID94OnSameGPU"
evaluate_rule = true
stage = []
recommended_action = "CONTACT_SUPPORT"

[rules.recovery]
source_agent = "syslog-health-monitor"
source_check_name = "SysLogsXIDError"
source_error_codes = ["94"]
scope = "entity"
entity_types = ["GPU_UUID"]
`
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	cfg, err := LoadTomlConfig(path)
	require.NoError(t, err)
	require.Len(t, cfg.Rules, 1)
	require.Equal(t, &RecoveryMapping{
		SourceAgent:      "syslog-health-monitor",
		SourceCheckName:  "SysLogsXIDError",
		SourceErrorCodes: []string{"94"},
		Scope:            RecoveryScopeEntity,
		EntityTypes:      []string{"GPU_UUID"},
	}, cfg.Rules[0].Recovery)
}

func TestLoadTomlConfigRejectsInvalidRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.toml")
	contents := `
[[rules]]
name = "invalid-recovery"

[rules.recovery]
scope = "node"
`
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	config, err := LoadTomlConfig(path)
	require.Nil(t, config)
	require.ErrorContains(t, err, "invalid health-events-analyzer config")
	require.ErrorContains(t, err, "source_check_name is required")
}

func TestRecoveryValidationAllowsRulesWithoutMapping(t *testing.T) {
	config := &TomlConfig{Rules: []HealthEventsAnalyzerRule{{Name: "manual-recovery"}}}
	require.NoError(t, config.Validate())
}

func TestRecoveryMappingValidation(t *testing.T) {
	tests := []struct {
		name    string
		mapping *RecoveryMapping
		wantErr string
	}{
		{
			name: "node scope",
			mapping: &RecoveryMapping{
				SourceCheckName: "NodeRecovered",
				Scope:           RecoveryScopeNode,
			},
		},
		{
			name: "entity scope",
			mapping: &RecoveryMapping{
				SourceCheckName: "GpuRecovered",
				Scope:           RecoveryScopeEntity,
				EntityTypes:     []string{"GPU_UUID"},
			},
		},
		{
			name: "missing source check",
			mapping: &RecoveryMapping{
				Scope: RecoveryScopeNode,
			},
			wantErr: "source_check_name is required",
		},
		{
			name: "invalid scope",
			mapping: &RecoveryMapping{
				SourceCheckName: "Recovered",
				Scope:           "cluster",
			},
			wantErr: "scope must be",
		},
		{
			name: "entity scope without entity types",
			mapping: &RecoveryMapping{
				SourceCheckName: "Recovered",
				Scope:           RecoveryScopeEntity,
			},
			wantErr: "entity_types is required",
		},
		{
			name: "node scope with entity types",
			mapping: &RecoveryMapping{
				SourceCheckName: "Recovered",
				Scope:           RecoveryScopeNode,
				EntityTypes:     []string{"GPU_UUID"},
			},
			wantErr: "entity_types must be empty",
		},
		{
			name: "duplicate entity type",
			mapping: &RecoveryMapping{
				SourceCheckName: "Recovered",
				Scope:           RecoveryScopeEntity,
				EntityTypes:     []string{"GPU", "GPU"},
			},
			wantErr: "duplicate value",
		},
		{
			name: "empty error code",
			mapping: &RecoveryMapping{
				SourceCheckName:  "Recovered",
				Scope:            RecoveryScopeNode,
				SourceErrorCodes: []string{""},
			},
			wantErr: "must not contain empty values",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &TomlConfig{Rules: []HealthEventsAnalyzerRule{{
				Name:     "derived-condition",
				Recovery: test.mapping,
			}}}

			err := cfg.Validate()
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, test.wantErr)
		})
	}
}
