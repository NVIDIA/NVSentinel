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

package overrides

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestNewProcessor(t *testing.T) {
	tests := []struct {
		name      string
		config    *Config
		expectErr bool
		errMsg    string
	}{
		{
			name: "empty-rules",
			config: &Config{
				Enabled: true,
				Rules:   []Rule{},
			},
			expectErr: true,
			errMsg:    "no rules defined",
		},
		{
			name: "missing-rule-name",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						When: "event.agent == 'test'",
						Override: Override{
							IsFatal: new(false),
						},
					},
				},
			},
			expectErr: true,
			errMsg:    "name is required",
		},
		{
			name: "missing-when-expression",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "test-rule",
						Override: Override{
							IsFatal: new(false),
						},
					},
				},
			},
			expectErr: true,
			errMsg:    "when expression is required",
		},
		{
			name: "empty-override",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name:     "test-rule",
						When:     "event.agent == 'test'",
						Override: Override{},
					},
				},
			},
			expectErr: true,
			errMsg:    "at least one override field required",
		},
		{
			name: "invalid-cel-syntax",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "invalid-cel",
						When: "event.agent ==",
						Override: Override{
							IsFatal: new(false),
						},
					},
				},
			},
			expectErr: true,
			errMsg:    "CEL compilation failed",
		},
		{
			name: "non-boolean-expression",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "non-boolean",
						When: "event.agent",
						Override: Override{
							IsFatal: new(false),
						},
					},
				},
			},
			expectErr: true,
			errMsg:    "must return boolean",
		},
		{
			name: "custom-action-without-name",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name:     "test-rule",
						When:     `event.agent == "test"`,
						Override: Override{RecommendedAction: new("CUSTOM")},
					},
				},
			},
			expectErr: true,
			errMsg:    "recommendedAction CUSTOM requires a non-empty customRecommendedAction",
		},
		{
			name: "custom-action-with-empty-name",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "test-rule",
						When: `event.agent == "test"`,
						Override: Override{
							RecommendedAction:       new("CUSTOM"),
							CustomRecommendedAction: new(""),
						},
					},
				},
			},
			expectErr: true,
			errMsg:    "recommendedAction CUSTOM requires a non-empty customRecommendedAction",
		},
		{
			name: "custom-name-with-built-in-action",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "test-rule",
						When: `event.agent == "test"`,
						Override: Override{
							RecommendedAction:       new("RESTART_BM"),
							CustomRecommendedAction: new("hardware-repair"),
						},
					},
				},
			},
			expectErr: true,
			errMsg:    "customRecommendedAction requires recommendedAction CUSTOM",
		},
		{
			name: "custom-name-without-action",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name:     "test-rule",
						When:     `event.agent == "test"`,
						Override: Override{CustomRecommendedAction: new("hardware-repair")},
					},
				},
			},
			expectErr: true,
			errMsg:    "customRecommendedAction requires recommendedAction CUSTOM",
		},
		{
			name: "valid-config",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "test-rule",
						When: `event.agent == "test"`,
						Override: Override{
							IsFatal: new(false),
						},
					},
				},
			},
			expectErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc, err := NewProcessor(tt.config)
			if tt.expectErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				require.NoError(t, err)
				require.NotNil(t, proc)
			}
		})
	}
}

func TestApplyOverrides(t *testing.T) {
	tests := []struct {
		name              string
		config            *Config
		event             *pb.HealthEvent
		expectedIsFatal   bool
		expectedIsHealthy bool
		expectedAction    pb.RecommendedAction
		expectedCustom    string
	}{
		{
			name: "disabled-processor-no-change",
			config: &Config{
				Enabled: false,
			},
			event: &pb.HealthEvent{
				Agent:             "test-agent",
				IsFatal:           true,
				IsHealthy:         false,
				RecommendedAction: pb.RecommendedAction_CONTACT_SUPPORT,
			},
			expectedIsFatal:   true,
			expectedIsHealthy: false,
			expectedAction:    pb.RecommendedAction_CONTACT_SUPPORT,
		},
		{
			name: "rule-matches-applies-override",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "test-rule",
						When: `event.agent == "test-agent"`,
						Override: Override{
							IsFatal:           new(false),
							RecommendedAction: new("NONE"),
						},
					},
				},
			},
			event: &pb.HealthEvent{
				Agent:             "test-agent",
				IsFatal:           true,
				RecommendedAction: pb.RecommendedAction_CONTACT_SUPPORT,
			},
			expectedIsFatal: false,
			expectedAction:  pb.RecommendedAction_NONE,
		},
		{
			name: "rule-no-match-unchanged",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "test-rule",
						When: `event.agent == "other-agent"`,
						Override: Override{
							IsFatal: new(false),
						},
					},
				},
			},
			event: &pb.HealthEvent{
				Agent:             "test-agent",
				IsFatal:           true,
				RecommendedAction: pb.RecommendedAction_CONTACT_SUPPORT,
			},
			expectedIsFatal: true,
			expectedAction:  pb.RecommendedAction_CONTACT_SUPPORT,
		},
		{
			name: "first-match-wins",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "first-rule",
						When: `event.agent == "test-agent"`,
						Override: Override{
							RecommendedAction: new("NONE"),
						},
					},
					{
						Name: "second-rule",
						When: `event.agent == "test-agent"`,
						Override: Override{
							RecommendedAction: new("CONTACT_SUPPORT"),
						},
					},
				},
			},
			event: &pb.HealthEvent{
				Agent:             "test-agent",
				RecommendedAction: pb.RecommendedAction_REPLACE_VM,
			},
			expectedAction: pb.RecommendedAction_NONE,
		},
		{
			name: "partial-override-works",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name: "test-rule",
						When: `event.agent == "test-agent"`,
						Override: Override{
							IsFatal: new(false),
						},
					},
				},
			},
			event: &pb.HealthEvent{
				Agent:             "test-agent",
				IsFatal:           true,
				IsHealthy:         false,
				RecommendedAction: pb.RecommendedAction_CONTACT_SUPPORT,
			},
			expectedIsFatal:   false,
			expectedIsHealthy: false,
			expectedAction:    pb.RecommendedAction_CONTACT_SUPPORT,
		},
		{
			name:   "built-in-action-to-custom-action",
			config: customActionConfig(`event.agent == "test-agent"`, "CUSTOM", new("hardware-repair")),
			event: &pb.HealthEvent{
				Agent:             "test-agent",
				IsFatal:           true,
				RecommendedAction: pb.RecommendedAction_CONTACT_SUPPORT,
			},
			expectedIsFatal: true,
			expectedAction:  pb.RecommendedAction_CUSTOM,
			expectedCustom:  "hardware-repair",
		},
		{
			name:   "custom-action-to-other-custom-action",
			config: customActionConfig(`event.agent == "test-agent"`, "CUSTOM", new("hardware-repair")),
			event: &pb.HealthEvent{
				Agent:                   "test-agent",
				IsFatal:                 true,
				RecommendedAction:       pb.RecommendedAction_CUSTOM,
				CustomRecommendedAction: "replace-disk",
			},
			expectedIsFatal: true,
			expectedAction:  pb.RecommendedAction_CUSTOM,
			expectedCustom:  "hardware-repair",
		},
		{
			name:   "custom-action-to-built-in-action-clears-name",
			config: customActionConfig(`event.agent == "test-agent"`, "RESTART_BM", nil),
			event: &pb.HealthEvent{
				Agent:                   "test-agent",
				IsFatal:                 true,
				RecommendedAction:       pb.RecommendedAction_CUSTOM,
				CustomRecommendedAction: "hardware-repair",
			},
			expectedIsFatal: true,
			expectedAction:  pb.RecommendedAction_RESTART_BM,
		},
		{
			name: "override-without-action-keeps-custom-action",
			config: &Config{
				Enabled: true,
				Rules: []Rule{
					{
						Name:     "test-rule",
						When:     `event.agent == "test-agent"`,
						Override: Override{IsFatal: new(false)},
					},
				},
			},
			event: &pb.HealthEvent{
				Agent:                   "test-agent",
				IsFatal:                 true,
				RecommendedAction:       pb.RecommendedAction_CUSTOM,
				CustomRecommendedAction: "replace-disk",
			},
			expectedIsFatal: false,
			expectedAction:  pb.RecommendedAction_CUSTOM,
			expectedCustom:  "replace-disk",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc, err := NewProcessor(tt.config)
			require.NoError(t, err)

			err = proc.Transform(context.Background(), tt.event)
			require.NoError(t, err)

			assert.Equal(t, tt.expectedIsFatal, tt.event.IsFatal, "isFatal mismatch")
			assert.Equal(t, tt.expectedIsHealthy, tt.event.IsHealthy, "isHealthy mismatch")
			assert.Equal(t, tt.expectedAction, tt.event.RecommendedAction, "recommendedAction mismatch")
			assert.Equal(t, tt.expectedCustom, tt.event.CustomRecommendedAction, "customRecommendedAction mismatch")
		})
	}
}

func TestApplyOverrides_CustomAction_CountsEachChangedField(t *testing.T) {
	const ruleName = "custom-action-metrics"

	cfg := customActionConfig(`event.agent == "test-agent"`, "CUSTOM", new("hardware-repair"))
	cfg.Rules[0].Name = ruleName

	proc, err := NewProcessor(cfg)
	require.NoError(t, err)

	actionBefore := testutil.ToFloat64(overridesApplied.WithLabelValues(ruleName, "recommendedAction"))
	customBefore := testutil.ToFloat64(overridesApplied.WithLabelValues(ruleName, "customRecommendedAction"))

	// Built-in to custom: both fields change.
	require.NoError(t, proc.Transform(context.Background(), &pb.HealthEvent{
		Agent:             "test-agent",
		RecommendedAction: pb.RecommendedAction_CONTACT_SUPPORT,
	}))

	// Another custom action to this one: only the name changes.
	require.NoError(t, proc.Transform(context.Background(), &pb.HealthEvent{
		Agent:                   "test-agent",
		RecommendedAction:       pb.RecommendedAction_CUSTOM,
		CustomRecommendedAction: "replace-disk",
	}))

	// Already this custom action: nothing changes.
	require.NoError(t, proc.Transform(context.Background(), &pb.HealthEvent{
		Agent:                   "test-agent",
		RecommendedAction:       pb.RecommendedAction_CUSTOM,
		CustomRecommendedAction: "hardware-repair",
	}))

	assert.InDelta(t, 1, testutil.ToFloat64(overridesApplied.WithLabelValues(ruleName, "recommendedAction"))-actionBefore, 0)
	assert.InDelta(t, 2,
		testutil.ToFloat64(overridesApplied.WithLabelValues(ruleName, "customRecommendedAction"))-customBefore, 0)
}

// customActionConfig returns a config with one rule that sets recommendedAction and, when
// customAction is not nil, customRecommendedAction.
func customActionConfig(when, action string, customAction *string) *Config {
	return &Config{
		Enabled: true,
		Rules: []Rule{
			{
				Name: "test-rule",
				When: when,
				Override: Override{
					RecommendedAction:       &action,
					CustomRecommendedAction: customAction,
				},
			},
		},
	}
}

func TestLoadConfig_CustomAction_ReadsChartOutput(t *testing.T) {
	// The rule as the chart renders it in overrides.toml.
	path := filepath.Join(t.TempDir(), "overrides.toml")
	require.NoError(t, os.WriteFile(path, []byte(`enabled = true

[[rules]]
name = "nvlink-to-hardware-repair"
when = "event.checkName == \"GpuNvlinkWatch\""
override = {recommendedAction = "CUSTOM", customRecommendedAction = "hardware-repair"}
`), 0o600))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	require.Len(t, cfg.Rules, 1)
	require.NotNil(t, cfg.Rules[0].Override.CustomRecommendedAction)
	assert.Equal(t, "hardware-repair", *cfg.Rules[0].Override.CustomRecommendedAction)
}
