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
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/nvidia/nvsentinel/commons/pkg/ruleset"
)

var (
	errStopWithEventRule = errors.New("stop rule set must use only Node rules")
	errUnknownAction     = errors.New("action is not in maintenance.actions")
)

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		doc      string
		want     error
		contains string
	}{
		{
			name: "AnyAndStop_Accepted",
			doc: `
[[ruleSets]]
name = "quarantine-only"
enabled = true

[[ruleSets.match.any]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
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

[ruleSets.remediation]
stop = true
`,
		},
		{
			// A bare field read is typed dyn, so startup cannot prove it is a
			// boolean. It must still be accepted: this rule works in
			// fault-quarantine, and ADR-064 keeps those rules working unchanged.
			name: "DynBooleanFieldRead_Accepted",
			doc: `
[[ruleSets]]
name = "unschedulable"

[[ruleSets.match.all]]
kind = "Node"
expression = "node.spec.unschedulable"

[ruleSets.remediation]
stop = true
`,
		},
		{
			name:     "EmptyName_ReturnsErrInvalidName",
			want:     ruleset.ErrInvalidName,
			contains: "ruleSets[0].name",
			doc:      ruleSetNamed(""),
		},
		{
			name:     "UnderscoreName_ReturnsErrInvalidName",
			want:     ruleset.ErrInvalidName,
			contains: "vm_nodes",
			doc:      ruleSetNamed("vm_nodes"),
		},
		{
			name:     "DuplicateName_ReturnsErrDuplicateName",
			want:     ruleset.ErrDuplicateName,
			contains: `ruleSets[1] "same"`,
			doc:      ruleSetNamed("same") + ruleSetNamed("same"),
		},
		{
			name:     "BothAllAndAny_ReturnsErrMatch",
			want:     ruleset.ErrMatch,
			contains: "exactly one of all or any",
			doc: `
[[ruleSets]]
name = "both"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[[ruleSets.match.any]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
stop = true
`,
		},
		{
			name:     "NeitherAllNorAny_ReturnsErrMatch",
			want:     ruleset.ErrMatch,
			contains: "exactly one of all or any",
			doc: `
[[ruleSets]]
name = "no-match"

[ruleSets.remediation]
stop = true
`,
		},
		{
			name:     "EmptyAll_ReturnsErrMatch",
			want:     ruleset.ErrMatch,
			contains: "match.all",
			doc: `
[[ruleSets]]
name = "empty-all"

[ruleSets.match]
all = []

[ruleSets.remediation]
stop = true
`,
		},
		{
			name:     "EmptyAny_ReturnsErrMatch",
			want:     ruleset.ErrMatch,
			contains: "match.any",
			doc: `
[[ruleSets]]
name = "empty-any"

[ruleSets.match]
any = []

[ruleSets.remediation]
stop = true
`,
		},
		{
			name:     "BadKind_ReturnsErrRuleKind",
			want:     ruleset.ErrRuleKind,
			contains: `"Pod"`,
			doc: `
[[ruleSets]]
name = "bad-kind"

[[ruleSets.match.all]]
kind = "Pod"
expression = "true"

[ruleSets.remediation]
stop = true
`,
		},
		{
			name:     "EmptyExpression_ReturnsErrRuleExpression",
			want:     ruleset.ErrRuleExpression,
			contains: "must not be empty",
			doc: `
[[ruleSets]]
name = "empty-expression"

[[ruleSets.match.all]]
kind = "Node"
expression = ""

[ruleSets.remediation]
stop = true
`,
		},
		{
			name:     "UnparseableExpression_ReturnsErrRuleExpression",
			want:     ruleset.ErrRuleExpression,
			contains: "failed to parse",
			doc: `
[[ruleSets]]
name = "bad-parse"

[[ruleSets.match.all]]
kind = "HealthEvent"
expression = "event.agent =="

[ruleSets.remediation]
stop = true
`,
		},
		{
			name:     "NonBooleanExpression_ReturnsErrRuleExpression",
			want:     ruleset.ErrRuleExpression,
			contains: "boolean",
			doc: `
[[ruleSets]]
name = "not-bool"

[[ruleSets.match.all]]
kind = "Node"
expression = "1 + 1"

[ruleSets.remediation]
stop = true
`,
		},
		{
			name:     "NodeRuleReadsEvent_ReturnsErrRuleExpression",
			want:     ruleset.ErrRuleExpression,
			contains: "event",
			doc: `
[[ruleSets]]
name = "wrong-variable"

[[ruleSets.match.all]]
kind = "Node"
expression = 'event.componentClass == "GPU"'

[ruleSets.remediation]
stop = true
`,
		},
		{
			name:     "MissingOutcome_ReturnsErrMissingOutcome",
			want:     ruleset.ErrMissingOutcome,
			contains: `"remediation"`,
			doc: `
[[ruleSets]]
name = "no-outcome"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"
`,
		},
		{
			name:     "BothStopAndAction_ReturnsOutcomeError",
			want:     errExactlyOne,
			contains: "exactly one of stop and action",
			doc: `
[[ruleSets]]
name = "both-fields"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
stop = true
action = "REBOOT"
`,
		},
		{
			name:     "NeitherStopNorAction_ReturnsOutcomeError",
			want:     errExactlyOne,
			contains: "exactly one of stop and action",
			doc: `
[[ruleSets]]
name = "neither-field"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
`,
		},
		{
			name:     "SecondRuleSetInvalid_NamesThatRuleSet",
			want:     ruleset.ErrInvalidName,
			contains: "ruleSets[1]",
			doc:      ruleSetNamed("ok") + ruleSetNamed("Bad"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sets, _ := decode(t, tt.doc)
			err := ruleset.Validate(sets, "remediation", nil)

			if tt.want == nil {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, tt.want)
			require.ErrorContains(t, err, tt.contains)
		})
	}
}

func TestValidate_AcceptedRuleSet_KeepsDecodedFields(t *testing.T) {
	t.Parallel()

	sets, _ := decode(t, `
[[ruleSets]]
name = "vm-gpu"
priority = 10

[[ruleSets.match.all]]
kind = "Node"
expression = '"example.com/platform" in node.metadata.labels && node.metadata.labels["example.com/platform"] == "vm"'

[[ruleSets.match.all]]
kind = "HealthEvent"
expression = 'event.componentClass == "GPU"'

[ruleSets.remediation]
action = "REPLACE_VM"
`)

	require.NoError(t, ruleset.Validate(sets, "remediation", nil))
	require.Len(t, sets, 1)
	require.True(t, sets[0].IsEnabled())
	require.Nil(t, sets[0].Enabled)
	require.Equal(t, 10, sets[0].Priority)
	require.Len(t, sets[0].Match.All, 2)
	require.Nil(t, sets[0].Match.Any)
	require.Equal(t, "REPLACE_VM", sets[0].Remediation.Action)
	require.False(t, sets[0].Remediation.Stopped())
}

func TestValidate_NameLength(t *testing.T) {
	t.Parallel()

	maxName := strings.Repeat("a", validation.DNS1123LabelMaxLength)
	tooLong := strings.Repeat("a", validation.DNS1123LabelMaxLength+1)

	accepted, _ := decode(t, ruleSetNamed(maxName))
	require.NoError(t, ruleset.Validate(accepted, "remediation", nil))

	rejected, _ := decode(t, ruleSetNamed(tooLong))
	err := ruleset.Validate(rejected, "remediation", nil)
	require.ErrorIs(t, err, ruleset.ErrInvalidName)
	require.ErrorContains(t, err, tooLong)
}

func TestValidate_DisabledBrokenRuleSet_StillFails(t *testing.T) {
	t.Parallel()

	sets, _ := decode(t, `
[[ruleSets]]
name = "off"
enabled = false

[[ruleSets.match.all]]
kind = "Node"
expression = "1 + 1"

[ruleSets.remediation]
stop = true
`)

	require.False(t, sets[0].IsEnabled())

	err := ruleset.Validate(sets, "remediation", nil)
	require.ErrorIs(t, err, ruleset.ErrRuleExpression)
}

func TestValidate_TwoBadRules_ReturnsEach(t *testing.T) {
	t.Parallel()

	sets, _ := decode(t, `
[[ruleSets]]
name = "two"

[[ruleSets.match.all]]
kind = "Pod"
expression = "true"

[[ruleSets.match.all]]
kind = "Node"
expression = "1 + 1"

[ruleSets.remediation]
stop = true
`)

	err := ruleset.Validate(sets, "remediation", nil)
	require.ErrorIs(t, err, ruleset.ErrRuleKind)
	require.ErrorIs(t, err, ruleset.ErrRuleExpression)
	require.ErrorContains(t, err, "match.all[0]")
	require.ErrorContains(t, err, "match.all[1]")
}

func TestValidate_SeveralProblems_ReturnsEach(t *testing.T) {
	t.Parallel()

	sets, _ := decode(t, `
[[ruleSets]]
name = "BAD_NAME"

[ruleSets.remediation]
stop = true
action = "REBOOT"
`)

	err := ruleset.Validate(sets, "remediation", nil)
	require.ErrorIs(t, err, ruleset.ErrInvalidName)
	require.ErrorIs(t, err, ruleset.ErrMatch)
	require.ErrorIs(t, err, errExactlyOne)
}

func TestValidate_EmptyList_ReturnsNil(t *testing.T) {
	t.Parallel()

	require.NoError(t, ruleset.Validate([]testRuleSet(nil), "remediation", nil))
	require.NoError(t, ruleset.Validate([]testRuleSet{}, "remediation", nil))
}

func TestValidate_Check_SeesWholeRuleSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind string
		want error
	}{
		{name: "StopWithNodeRule_Accepted", kind: ruleset.KindNode},
		{name: "StopWithHealthEventRule_Rejected", kind: ruleset.KindHealthEvent, want: errStopWithEventRule},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sets, _ := decode(t, `
[[ruleSets]]
name = "hold"

[[ruleSets.match.all]]
kind = "`+tt.kind+`"
expression = "true"

[ruleSets.remediation]
stop = true
`)

			err := ruleset.Validate(sets, "remediation", stopUsesOnlyNodeRules)
			if tt.want == nil {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, tt.want)
			require.ErrorContains(t, err, `ruleSets[0] "hold"`)
		})
	}
}

func TestValidate_Check_ReadsComponentConfig(t *testing.T) {
	t.Parallel()

	sets, _ := decode(t, `
[[ruleSets]]
name = "known"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
action = "REBOOT"

[[ruleSets]]
name = "unknown"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
action = "REPLACE_VM"

[[ruleSets]]
name = "stopped"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
stop = true
`)

	err := ruleset.Validate(sets, "remediation", actionIn(map[string]struct{}{"REBOOT": {}}))
	require.ErrorIs(t, err, errUnknownAction)
	require.ErrorContains(t, err, `ruleSets[1] "unknown"`)
	require.ErrorContains(t, err, `"REPLACE_VM"`)
	require.NotContains(t, err.Error(), `"known"`)
	require.NotContains(t, err.Error(), `"stopped"`)
}

func TestValidate_Check_RunsOnlyForRuleSetsWithOutcome(t *testing.T) {
	t.Parallel()

	sets, _ := decode(t, ruleSetNamed("on")+`
[[ruleSets]]
name = "off"
enabled = false

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
stop = true

[[ruleSets]]
name = "both-fields"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
stop = true
action = "REBOOT"

[[ruleSets]]
name = "no-outcome"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"
`)

	var checked []string

	err := ruleset.Validate(sets, "remediation", func(rs testRuleSet) error {
		checked = append(checked, rs.Name)

		return nil
	})

	require.ErrorIs(t, err, ruleset.ErrMissingOutcome)
	require.ErrorIs(t, err, errExactlyOne)
	require.Equal(t, []string{"on", "off", "both-fields"}, checked)
}

func ruleSetNamed(name string) string {
	return `
[[ruleSets]]
name = "` + name + `"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
stop = true
`
}

func stopUsesOnlyNodeRules(rs testRuleSet) error {
	if !rs.Remediation.Stopped() {
		return nil
	}

	for _, rule := range slices.Concat(rs.Match.All, rs.Match.Any) {
		if rule.Kind != ruleset.KindNode {
			return errStopWithEventRule
		}
	}

	return nil
}

func actionIn(actions map[string]struct{}) func(testRuleSet) error {
	return func(rs testRuleSet) error {
		if rs.Remediation.Stopped() {
			return nil
		}

		if _, ok := actions[rs.Remediation.Action]; !ok {
			return fmt.Errorf("%w: %q", errUnknownAction, rs.Remediation.Action)
		}

		return nil
	}
}
