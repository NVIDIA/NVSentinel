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

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestLoadTomlConfig_ExistingUnknownKeysAndInvalidStages_StillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`unknown_option = true
[[rules]]
name = "legacy"
evaluate_rule = true
processing_strategy = "custom-old-value"
stage = ["not valid JSON"]
`), 0600))
	cfg, err := LoadTomlConfig(path)
	require.NoError(t, err)
	require.Len(t, cfg.Rules, 1)
	require.False(t, cfg.HasAnnotationRecovery())
}

func TestRecoveryMapping_Validation_RejectsUnsafeContracts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mapping RecoveryMapping
		valid   bool
	}{
		{"node", RecoveryMapping{AnnotationKey: "nvsentinel.nvidia.com/recover-xid", Scope: RecoveryScopeNode}, true},
		{"entity", RecoveryMapping{AnnotationKey: "nvsentinel.nvidia.com/recover-xid", Scope: RecoveryScopeEntity, EntityTypes: []string{"GPU_UUID"}}, true},
		{"unqualified key", RecoveryMapping{AnnotationKey: "recover-xid", Scope: RecoveryScopeNode}, false},
		{"missing annotation", RecoveryMapping{Scope: RecoveryScopeNode}, false},
		{"missing scope", RecoveryMapping{AnnotationKey: "nvsentinel.nvidia.com/recover-xid"}, false},
		{"missing identity", RecoveryMapping{AnnotationKey: "nvsentinel.nvidia.com/recover-xid", Scope: RecoveryScopeEntity}, false},
		{"node with entities", RecoveryMapping{AnnotationKey: "nvsentinel.nvidia.com/recover-xid", Scope: RecoveryScopeNode, EntityTypes: []string{"GPU_UUID"}}, false},
		{"duplicate identity", RecoveryMapping{AnnotationKey: "nvsentinel.nvidia.com/recover-xid", Scope: RecoveryScopeEntity, EntityTypes: []string{"GPU_UUID", "GPU_UUID"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mapping.validate()
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	mapping := &RecoveryMapping{AnnotationKey: "nvsentinel.nvidia.com/recover-xid", Scope: RecoveryScopeNode}
	cfg := &TomlConfig{Rules: []HealthEventsAnalyzerRule{{Name: "one", Recovery: mapping}, {Name: "two", Recovery: mapping}}}
	require.ErrorContains(t, cfg.Validate(), "share")
}

// Loading recovery mappings must preserve the CEL gate introduced on main.
func TestLoadTomlConfig_RecoveryPreservesWhenGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[[rules]]
name = "recovery-with-gate"
evaluate_rule = true
when = "event.isFatal == true"
[rules.recovery]
annotation_key = "nvsentinel.nvidia.com/recover-xid"
scope = "node"
`), 0600))
	cfg, err := LoadTomlConfig(path)
	require.NoError(t, err)
	require.True(t, cfg.HasAnnotationRecovery())
	for _, fatal := range []bool{false, true} {
		applies, err := cfg.Rules[0].Applies(&protos.HealthEvent{IsFatal: fatal})
		require.NoError(t, err)
		require.Equal(t, fatal, applies)
	}
}
