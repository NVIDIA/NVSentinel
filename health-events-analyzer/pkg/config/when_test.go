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

package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
)

// writeTOML writes content to a temporary TOML file and returns its path.
func writeTOML(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

// TestLoadTomlConfig_When_AppliesPerEvent checks that a loaded when expression decides, per
// event, whether the rule applies, and that a rule without one always applies.
func TestLoadTomlConfig_When_AppliesPerEvent(t *testing.T) {
	cfg, err := config.LoadTomlConfig(writeTOML(t, `
[[rules]]
name = "gated"
evaluate_rule = true
when = "size(event.errorCode) > 0 && event.errorCode[0] == '74'"
stage = ['{"$match": {}}']

[[rules]]
name = "ungated"
evaluate_rule = true
stage = ['{"$match": {}}']
`))
	require.NoError(t, err)
	require.Len(t, cfg.Rules, 2)

	tests := []struct {
		name        string
		errorCode   []string
		wantGated   bool
		wantUngated bool
	}{
		{name: "matching first code", errorCode: []string{"74"}, wantGated: true, wantUngated: true},
		{name: "other code", errorCode: []string{"45"}, wantGated: false, wantUngated: true},
		{name: "matching code not first", errorCode: []string{"45", "74"}, wantGated: false, wantUngated: true},
		{name: "no error code", errorCode: nil, wantGated: false, wantUngated: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			event := &protos.HealthEvent{ErrorCode: tc.errorCode}

			applies, err := cfg.Rules[0].Applies(event)
			require.NoError(t, err)
			assert.Equal(t, tc.wantGated, applies)

			applies, err = cfg.Rules[1].Applies(event)
			require.NoError(t, err)
			assert.Equal(t, tc.wantUngated, applies)
		})
	}
}

// TestLoadTomlConfig_When_InvalidExpressionFailsLoad checks that a when expression that does
// not compile to a boolean stops the load and names the rule.
func TestLoadTomlConfig_When_InvalidExpressionFailsLoad(t *testing.T) {
	tests := []struct {
		name string
		when string
	}{
		{name: "syntax error", when: "event.checkName ==="},
		{name: "not a boolean", when: "event.checkName"},
		{name: "unknown variable", when: "node.name == 'x'"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.LoadTomlConfig(writeTOML(t, `
[[rules]]
name = "broken"
evaluate_rule = true
when = "`+tc.when+`"
stage = ['{"$match": {}}']
`))
			require.Error(t, err)
			assert.Contains(t, err.Error(), `rule "broken"`)
		})
	}
}

// TestApplies_When_EvaluationErrorFallsBackToApplying checks that a when expression that fails
// on an event reports the error and still applies the rule, so the stages decide the match.
func TestApplies_When_EvaluationErrorFallsBackToApplying(t *testing.T) {
	cfg := &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{{
		Name: "reads-missing-key",
		When: "event.metadata['REG0'] == '1'",
	}}}
	require.NoError(t, cfg.Compile())

	applies, err := cfg.Rules[0].Applies(&protos.HealthEvent{})
	require.Error(t, err)
	assert.True(t, applies, "a failed when expression must not skip the rule")
}

// TestApplies_When_NotCompiledFallsBackToApplying checks that a rule built without Compile
// still applies, and reports why its when expression was ignored.
func TestApplies_When_NotCompiledFallsBackToApplying(t *testing.T) {
	rule := config.HealthEventsAnalyzerRule{Name: "not-compiled", When: "event.isFatal == true"}

	applies, err := rule.Applies(&protos.HealthEvent{})
	require.Error(t, err)
	assert.True(t, applies)
}
