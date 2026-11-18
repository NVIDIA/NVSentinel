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
	"encoding/json"
	"errors"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/commons/pkg/ruleset"
)

// testRuleSet is the shape a component type will have. Remediation stands in
// for the outcome key that component accepts.
type testRuleSet struct {
	ruleset.RuleSet

	Remediation *testOutcome `toml:"remediation" json:"remediation,omitempty"`
}

type testOutcome struct {
	ruleset.Stop

	Action string `toml:"action" json:"action,omitempty"`
}

var (
	_ ruleset.Entry = testRuleSet{}

	_ ruleset.Outcome = testOutcome{}

	errExactlyOne = errors.New("outcome must set exactly one of stop and action")
)

func (r testRuleSet) Outcome() (ruleset.Outcome, bool) {
	if r.Remediation == nil {
		return nil, false
	}

	return r.Remediation, true
}

func (o testOutcome) Validate() error {
	hasAction := o.Action != ""
	if o.Stopped() == hasAction {
		return errExactlyOne
	}

	return nil
}

func TestRuleSet_IsEnabled(t *testing.T) {
	t.Parallel()

	sets, _ := decode(t, `
[[ruleSets]]
name = "omitted"

[[ruleSets]]
name = "on"
enabled = true

[[ruleSets]]
name = "off"
enabled = false
`)

	require.Equal(t, "omitted", sets[0].Common().Name)
	require.Nil(t, sets[0].Enabled)
	require.True(t, sets[0].IsEnabled())

	require.NotNil(t, sets[1].Enabled)
	require.True(t, *sets[1].Enabled)
	require.True(t, sets[1].IsEnabled())

	require.NotNil(t, sets[2].Enabled)
	require.False(t, *sets[2].Enabled)
	require.False(t, sets[2].IsEnabled())
}

func TestRuleSet_JSONKeepsExplicitFalse(t *testing.T) {
	t.Parallel()

	disabled, _ := decode(t, `
[[ruleSets]]
name = "off"
enabled = false

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
stop = true
`)

	raw, err := json.Marshal(disabled[0])
	require.NoError(t, err)
	require.Contains(t, string(raw), `"enabled":false`)
	require.Contains(t, string(raw), `"stop":true`)
	require.NotContains(t, string(raw), `"priority"`)

	omitted, _ := decode(t, `
[[ruleSets]]
name = "on"

[[ruleSets.match.all]]
kind = "Node"
expression = "true"

[ruleSets.remediation]
action = "REBOOT"
`)

	raw, err = json.Marshal(omitted[0])
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"enabled"`)
	require.Contains(t, string(raw), `"action":"REBOOT"`)
}

func decode(t *testing.T, doc string) ([]testRuleSet, toml.MetaData) {
	t.Helper()

	var cfg struct {
		RuleSets []testRuleSet `toml:"ruleSets"`
	}

	md, err := toml.Decode(doc, &cfg)
	require.NoError(t, err)

	return cfg.RuleSets, md
}
