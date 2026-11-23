// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/commons/pkg/ruleset"
)

func TestValidateRuleSets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		doc      string
		want     []error
		contains []string
	}{
		{
			name: "AllNodeRules_Accepted",
			doc: `
[[ruleSets]]
name = "quarantine-only"
priority = 100

[[ruleSets.match.all]]
kind = "Node"
expression = '"example.com/pool" in node.metadata.labels && node.metadata.labels["example.com/pool"] == "quarantine-only"'

[ruleSets.drain]
stop = true
`,
		},
		{
			name: "AnyNodeRules_Accepted",
			doc: `
[[ruleSets]]
name = "hold"

[[ruleSets.match.any]]
kind = "Node"
expression = "true"

[ruleSets.drain]
stop = true
`,
		},
		{
			name: "Disabled_Accepted",
			doc: `
[[ruleSets]]
name = "off"
enabled = false

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.drain]
stop = true
`,
		},
		{
			name: "StopFalse_ReturnsErrDrainOutcome",
			doc: `
[[ruleSets]]
name = "no-stop"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.drain]
stop = false
`,
			want:     []error{ErrDrainOutcome},
			contains: []string{"drain.stop must be true"},
		},
		{
			name: "EmptyDrain_ReturnsErrDrainOutcome",
			doc: `
[[ruleSets]]
name = "empty-drain"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.drain]
`,
			want: []error{ErrDrainOutcome},
		},
		{
			name: "HealthEventInAll_ReturnsErrStopWithHealthEventRule",
			doc: `
[[ruleSets]]
name = "by-event"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[[ruleSets.match.all]]
kind = "HealthEvent"
expression = "true"

[ruleSets.drain]
stop = true
`,
			want: []error{ErrStopWithHealthEventRule},
		},
		{
			name: "HealthEventInAny_ReturnsErrStopWithHealthEventRule",
			doc: `
[[ruleSets]]
name = "by-event"

[[ruleSets.match.any]]
kind = "HealthEvent"
expression = "true"

[ruleSets.drain]
stop = true
`,
			want: []error{ErrStopWithHealthEventRule},
		},
		{
			name: "DisabledHealthEvent_ReturnsErrStopWithHealthEventRule",
			doc: `
[[ruleSets]]
name = "off"
enabled = false

[[ruleSets.match.all]]
kind = "HealthEvent"
expression = "true"

[ruleSets.drain]
stop = true
`,
			want: []error{ErrStopWithHealthEventRule},
		},
		{
			name: "StopFalseAndHealthEvent_ReturnsBothErrors",
			doc: `
[[ruleSets]]
name = "both"

[[ruleSets.match.any]]
kind = "HealthEvent"
expression = "true"

[ruleSets.drain]
stop = false
`,
			want:     []error{ErrDrainOutcome, ErrStopWithHealthEventRule},
			contains: []string{`ruleSets[0] "both"`},
		},
		{
			name: "MissingDrain_ReturnsErrMissingOutcome",
			doc: `
[[ruleSets]]
name = "no-outcome"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"
`,
			want:     []error{ruleset.ErrMissingOutcome},
			contains: []string{`"drain"`},
		},
		{
			name: "SecondRuleSetInvalid_NamesThatRuleSet",
			doc: `
[[ruleSets]]
name = "ok"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.drain]
stop = true

[[ruleSets]]
name = "bad"

[[ruleSets.match.all]]
kind = "HealthEvent"
expression = "true"

[ruleSets.drain]
stop = false
`,
			want:     []error{ErrDrainOutcome, ErrStopWithHealthEventRule},
			contains: []string{`ruleSets[1] "bad"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateRuleSets(decodeTomlConfig(t, tt.doc).RuleSets)
			if len(tt.want) == 0 {
				require.NoError(t, err)

				return
			}

			for _, want := range tt.want {
				require.ErrorIs(t, err, want)
			}

			for _, text := range tt.contains {
				require.ErrorContains(t, err, text)
			}
		})
	}
}

func TestLoadTomlConfigFromString_RuleSets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		doc      string
		want     error
		absent   error
		contains string
	}{
		{
			name: "EmptyList_Loads",
			doc:  "ruleSets = []\n",
		},
		{
			name:     "ValidStopWithBadTimeout_ReturnsTimeoutError",
			absent:   ErrRuleSetsNotSupported,
			contains: "deleteAfterTimeout must be a positive integer",
			doc: `
deleteAfterTimeoutMinutes = -1

[[ruleSets]]
name = "quarantine-only"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.drain]
stop = true
`,
		},
		{
			name:     "ValidStop_ReturnsErrRuleSetsNotSupported",
			want:     ErrRuleSetsNotSupported,
			contains: "not supported in this release",
			doc: `
[[ruleSets]]
name = "quarantine-only"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.drain]
stop = true
`,
		},
		{
			name:   "InvalidName_ReturnsValidationError",
			want:   ruleset.ErrInvalidName,
			absent: ErrRuleSetsNotSupported,
			doc: `
[[ruleSets]]
name = "Bad_Name"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.drain]
stop = true
`,
		},
		{
			name:     "RemediationKey_ReturnsErrUnknownKey",
			want:     ruleset.ErrUnknownKey,
			absent:   ErrRuleSetsNotSupported,
			contains: "remediation",
			doc: `
[[ruleSets]]
name = "copied"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
stop = true
`,
		},
		{
			name:     "MethodKey_ReturnsErrUnknownKey",
			want:     ruleset.ErrUnknownKey,
			absent:   ErrRuleSetsNotSupported,
			contains: "method",
			doc: `
[[ruleSets]]
name = "slurm"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.drain]
stop = true
method = "Custom"
`,
		},
		{
			name:     "MisspelledPriority_ReturnsErrUnknownKey",
			want:     ruleset.ErrUnknownKey,
			absent:   ErrRuleSetsNotSupported,
			contains: "priorty",
			doc: `
[[ruleSets]]
name = "quarantine-only"
priorty = 100

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.drain]
stop = true
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := loadTestConfig(tt.doc)
			if tt.want == nil && tt.contains == "" {
				require.NoError(t, err)
				require.Empty(t, cfg.RuleSets)

				return
			}

			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
			}

			if tt.absent != nil {
				require.NotErrorIs(t, err, tt.absent)
			}

			if tt.contains != "" {
				require.ErrorContains(t, err, tt.contains)
			}
		})
	}
}

func TestLoadTomlConfigFromString_UnknownKeyAndInvalidFields_ReportsAll(t *testing.T) {
	t.Parallel()

	_, err := loadTestConfig(`
[[ruleSets]]
name = "Bad_Name"
priorty = 100

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.drain]
stop = false
`)

	require.ErrorIs(t, err, ruleset.ErrUnknownKey)
	require.ErrorContains(t, err, "priorty")
	require.ErrorIs(t, err, ruleset.ErrInvalidName)
	require.ErrorIs(t, err, ErrDrainOutcome)
	require.NotErrorIs(t, err, ErrRuleSetsNotSupported)
}

func TestLoadTomlConfigFromString_NoRuleSets_KeepsDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := loadTestConfig("deleteAfterTimeoutMinutes = 15\n")
	require.NoError(t, err)
	require.Empty(t, cfg.RuleSets)
	require.Equal(t, 15, cfg.DeleteAfterTimeoutMinutes)
	require.Equal(t, 5, cfg.NotReadyTimeoutMinutes)
}

func decodeTomlConfig(t *testing.T, doc string) TomlConfig {
	t.Helper()

	var cfg TomlConfig

	_, err := toml.Decode(doc, &cfg)
	require.NoError(t, err)

	return cfg
}
