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

package ruleset_test

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/commons/pkg/ruleset"
)

func TestCheckUndecoded_DifferentOutcomeKey_NamesTheKey(t *testing.T) {
	t.Parallel()

	sets, md := decode(t, `
[[ruleSets]]
name = "copied-from-drainer"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.drain]
stop = true
`)

	err := ruleset.CheckUndecoded(md, "remediation")
	require.ErrorIs(t, err, ruleset.ErrUnknownKey)
	require.ErrorContains(t, err, "ruleSets.drain")
	require.NotContains(t, err.Error(), "ruleSets.drain.stop")
	require.ErrorContains(t, err, "remediation")

	err = ruleset.Validate(sets, "remediation", nil)
	require.ErrorIs(t, err, ruleset.ErrMissingOutcome)
	require.ErrorContains(t, err, "remediation")
}

func TestCheckUndecoded_MisspelledField_NamesTheKey(t *testing.T) {
	t.Parallel()

	sets, md := decode(t, `
[[ruleSets]]
name = "vm-nodes"
priorty = 10

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
stop = true
acton = "REBOOT"
`)

	err := ruleset.CheckUndecoded(md, "remediation")
	require.ErrorIs(t, err, ruleset.ErrUnknownKey)
	require.ErrorContains(t, err, "ruleSets.priorty")
	require.ErrorContains(t, err, "ruleSets.remediation.acton")

	require.NoError(t, ruleset.Validate(sets, "remediation", nil))
	require.Equal(t, 0, sets[0].Priority)
	require.True(t, sets[0].Remediation.Stopped())
	require.Empty(t, sets[0].Remediation.Action)
}

func TestCheckUndecoded_DifferentCaseRoot_NamesTheKey(t *testing.T) {
	t.Parallel()

	sets, md := decode(t, `
[[RuleSets]]
name = "vm-nodes"
priorty = 10

[[RuleSets.match.all]]
kind = "Node"
expression = "true"

[RuleSets.remediation]
stop = true
acton = "REBOOT"
`)

	require.Len(t, sets, 1)

	err := ruleset.CheckUndecoded(md, "remediation")
	require.ErrorIs(t, err, ruleset.ErrUnknownKey)
	require.ErrorContains(t, err, "RuleSets.priorty")
	require.ErrorContains(t, err, "RuleSets.remediation.acton")
}

func TestCheckUndecoded_RootSpelling(t *testing.T) {
	t.Parallel()

	ruleSet := func(root, name string) string {
		return `
[[` + root + `]]
name = "` + name + `"

[[` + root + `.match.all]]
kind = "Node"
expression = "true"

[` + root + `.remediation]
stop = true
`
	}

	tests := []struct {
		name string
		doc  string
		want bool
	}{
		{name: "OneDifferentCaseSpelling_Accepted", doc: ruleSet("RuleSets", "a") + ruleSet("RuleSets", "b")},
		{name: "TwoSpellings_Rejected", doc: ruleSet("ruleSets", "a") + ruleSet("RuleSets", "b"), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, md := decode(t, tt.doc)
			err := ruleset.CheckUndecoded(md, "remediation")

			if !tt.want {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, ruleset.ErrMixedRootKey)
			require.ErrorContains(t, err, `"ruleSets"`)
			require.ErrorContains(t, err, `"RuleSets"`)
		})
	}
}

func TestCheckUndecoded_RepeatedUnknownKey_ReportedOnce(t *testing.T) {
	t.Parallel()

	_, md := decode(t, `
[[ruleSets]]
name = "one"

[ruleSets.drain]
stop = true

[[ruleSets]]
name = "two"

[ruleSets.drain]
stop = true
`)

	err := ruleset.CheckUndecoded(md, "remediation")
	require.ErrorIs(t, err, ruleset.ErrUnknownKey)
	require.Equal(t, 1, strings.Count(err.Error(), "ruleSets.drain:"))
}

func TestCheckUndecoded_KeyOutsideRuleSets_IsIgnored(t *testing.T) {
	t.Parallel()

	_, md := decode(t, `
extra = "ignored"

[[ruleSets]]
name = "ok"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
stop = true
`)

	require.NotEmpty(t, md.Undecoded())
	require.NoError(t, ruleset.CheckUndecoded(md, "remediation"))
}

func TestCheckUndecoded_KnownFields_ReturnsNil(t *testing.T) {
	t.Parallel()

	_, md := decode(t, `
[[ruleSets]]
name = "ok"
enabled = false
priority = 3

[[ruleSets.match.any]]
kind = "HealthEvent"
expression = 'event.componentClass == "GPU"'

[ruleSets.remediation]
action = "REBOOT"
`)

	require.NoError(t, ruleset.CheckUndecoded(md, "remediation"))
}

func TestCheckUndecoded_NoMetadata_ReturnsNil(t *testing.T) {
	t.Parallel()

	require.NoError(t, ruleset.CheckUndecoded(toml.MetaData{}, "remediation"))
}
