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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/commons/pkg/ruleset"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

const (
	nodeMatches  = `node.metadata.name == "node-a"`
	nodeMisses   = `node.metadata.name == "other"`
	nodeFails    = `node.metadata.labels["missing"] == "x"`
	eventMatches = `event.agent == "gpu-health-monitor"`
	eventMisses  = `event.agent == "other"`
	eventFails   = `event.metadata["missing"] == "x"`
)

func TestEvaluate_Winner(t *testing.T) {
	t.Parallel()

	disabled := false

	tests := []struct {
		name     string
		ruleSets []testRuleSet
		want     string
	}{
		{
			name: "HigherPriority_WinsOverEarlierInConfig",
			ruleSets: []testRuleSet{
				ruleSetAll("low", 1, nodeMatches),
				ruleSetAll("high", 10, nodeMatches),
			},
			want: "high",
		},
		{
			name: "EqualPriority_FirstInConfigWins",
			ruleSets: []testRuleSet{
				ruleSetAll("first", 5, nodeMatches),
				ruleSetAll("second", 5, nodeMatches),
			},
			want: "first",
		},
		{
			name: "MissedRuleSet_NextOneWins",
			ruleSets: []testRuleSet{
				ruleSetAll("misses", 10, nodeMisses),
				ruleSetAll("matches", 1, nodeMatches),
			},
			want: "matches",
		},
		{
			name: "DisabledRuleSet_IsSkipped",
			ruleSets: []testRuleSet{
				withEnabled(ruleSetAll("off", 10, nodeMatches), &disabled),
				ruleSetAll("on", 1, nodeMatches),
			},
			want: "on",
		},
		{
			name: "NegativePriority_ComesAfterDefault",
			ruleSets: []testRuleSet{
				ruleSetAll("negative", -1, nodeMatches),
				ruleSetAll("default", 0, nodeMatches),
			},
			want: "default",
		},
		{
			name: "HealthEventAndNodeRules_BothRead",
			ruleSets: []testRuleSet{
				ruleSetAll("both", 0, eventMatches, nodeMatches),
			},
			want: "both",
		},
		{
			name: "ErrorAfterWinner_IsNotEvaluated",
			ruleSets: []testRuleSet{
				ruleSetAll("winner", 10, nodeMatches),
				ruleSetAll("fails", 1, nodeFails),
			},
			want: "winner",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			winner, matched, err := compile(t, tt.ruleSets).Evaluate(fullInput())
			require.NoError(t, err)
			require.True(t, matched)
			require.Equal(t, tt.want, winner.Name)
		})
	}
}

func TestEvaluate_NoMatch(t *testing.T) {
	t.Parallel()

	disabled := false

	tests := []struct {
		name     string
		ruleSets []testRuleSet
	}{
		{name: "NoRuleSets"},
		{
			name:     "NoneMatch",
			ruleSets: []testRuleSet{ruleSetAll("misses", 0, nodeMisses), ruleSetAny("also-misses", 0, eventMisses)},
		},
		{
			name:     "OnlyDisabledMatch",
			ruleSets: []testRuleSet{withEnabled(ruleSetAll("off", 0, nodeMatches), &disabled)},
		},
		{
			name:     "DisabledRuleSetThatFails_IsNotEvaluated",
			ruleSets: []testRuleSet{withEnabled(ruleSetAll("off", 0, nodeFails), &disabled)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, matched, err := compile(t, tt.ruleSets).Evaluate(fullInput())
			require.NoError(t, err)
			require.False(t, matched)
		})
	}
}

// A rule set that fails before the winner could itself have won, so its error
// stops the decision. The default does not apply either.
func TestEvaluate_ErrorBeforeWinner_StopsTheDecision(t *testing.T) {
	t.Parallel()

	evaluator := compile(t, []testRuleSet{
		ruleSetAll("fails", 10, nodeFails),
		ruleSetAll("would-win", 1, nodeMatches),
	})

	_, matched, err := evaluator.Evaluate(fullInput())
	require.False(t, matched)
	requireEvaluationError(t, err, "fails")
	require.ErrorContains(t, err, `rule set "fails"`)
}

// Inside one rule set, all and any give the same result whatever the order of
// the rules, as && and || do in CEL. A false rule decides all, and a true rule
// decides any, even when another rule of the list fails.
func TestEvaluate_ErrorInsideMatchList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		ruleSet     testRuleSet
		wantMatched bool
		// wantErrRule is the rule the error names. Empty means no error.
		wantErrRule string
	}{
		{name: "All_FalseThenError_DoesNotMatch", ruleSet: ruleSetAll("rs", 0, nodeMisses, nodeFails)},
		{name: "All_ErrorThenFalse_DoesNotMatch", ruleSet: ruleSetAll("rs", 0, nodeFails, nodeMisses)},
		{name: "All_TrueAndError_Fails", ruleSet: ruleSetAll("rs", 0, nodeMatches, nodeFails), wantErrRule: "match.all[1]"},
		{name: "All_ErrorAndTrue_Fails", ruleSet: ruleSetAll("rs", 0, nodeFails, nodeMatches), wantErrRule: "match.all[0]"},
		{name: "Any_TrueThenError_Matches", ruleSet: ruleSetAny("rs", 0, nodeMatches, nodeFails), wantMatched: true},
		{name: "Any_ErrorThenTrue_Matches", ruleSet: ruleSetAny("rs", 0, nodeFails, nodeMatches), wantMatched: true},
		{name: "Any_FalseAndError_Fails", ruleSet: ruleSetAny("rs", 0, nodeMisses, nodeFails), wantErrRule: "match.any[1]"},
		{name: "Any_EventErrorAndFalse_Fails", ruleSet: ruleSetAny("rs", 0, eventFails, eventMisses), wantErrRule: "match.any[0]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, matched, err := compile(t, []testRuleSet{tt.ruleSet}).Evaluate(fullInput())
			require.Equal(t, tt.wantMatched, matched)

			if tt.wantErrRule != "" {
				requireEvaluationError(t, err, "rs")
				require.ErrorContains(t, err, `rule set "rs": `+tt.wantErrRule+": ")
				return
			}

			require.NoError(t, err)
		})
	}
}

func TestEvaluate_MissingInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ruleSet testRuleSet
		input   ruleset.Input
	}{
		{name: "NodeRuleWithoutNode", ruleSet: ruleSetAll("rs", 0, nodeMatches), input: ruleset.Input{Event: testEvent()}},
		{name: "EventRuleWithoutEvent", ruleSet: ruleSetAll("rs", 0, eventMatches), input: ruleset.Input{Node: testNode()}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, matched, err := compile(t, []testRuleSet{tt.ruleSet}).Evaluate(tt.input)
			require.False(t, matched)
			requireEvaluationError(t, err, "rs")
			require.ErrorIs(t, err, ruleset.ErrMissingInput)
			require.ErrorContains(t, err, `rule set "rs": match.all[0]: `)
		})
	}
}

// A component without Nodes, or one that evaluates before it reads the Node,
// can still evaluate rule sets that use only the input it has.
func TestEvaluate_OnlyTheInputTheRulesRead_IsRequired(t *testing.T) {
	t.Parallel()

	winner, matched, err := compile(t, []testRuleSet{ruleSetAll("event-only", 0, eventMatches)}).
		Evaluate(ruleset.Input{Event: testEvent()})
	require.NoError(t, err)
	require.True(t, matched)
	require.Equal(t, "event-only", winner.Name)
}

// The winner is the component's own entry, so the caller reads its outcome.
func TestEvaluate_WinnerKeepsOutcome(t *testing.T) {
	t.Parallel()

	ruleSet := ruleSetAll("reboot", 0, nodeMatches)
	ruleSet.Remediation = &testOutcome{Action: "REBOOT"}

	winner, matched, err := compile(t, []testRuleSet{ruleSet}).Evaluate(fullInput())
	require.NoError(t, err)
	require.True(t, matched)
	require.Equal(t, "REBOOT", winner.Remediation.Action)
}

// The evaluator sorts its own copy, so the caller's configuration keeps its order.
func TestCompile_DoesNotReorderTheInput(t *testing.T) {
	t.Parallel()

	ruleSets := []testRuleSet{ruleSetAll("low", 1, nodeMatches), ruleSetAll("high", 10, nodeMatches)}

	compile(t, ruleSets)
	require.Equal(t, "low", ruleSets[0].Name)
	require.Equal(t, "high", ruleSets[1].Name)
}

func TestCompile_Errors(t *testing.T) {
	t.Parallel()

	disabled := false

	tests := []struct {
		name     string
		ruleSet  testRuleSet
		want     error
		contains string
	}{
		{
			name:     "BadExpression",
			ruleSet:  ruleSetAll("bad", 0, "node.metadata.name =="),
			want:     ruleset.ErrRuleExpression,
			contains: `rule set "bad": match.all[0].expression: `,
		},
		{
			name:     "BadKind",
			ruleSet:  testRuleSet{RuleSet: ruleset.RuleSet{Name: "pod", Match: ruleset.Match{All: []ruleset.Rule{{Kind: "Pod", Expression: "true"}}}}},
			want:     ruleset.ErrRuleKind,
			contains: `rule set "pod": match.all[0].kind: `,
		},
		{
			// A disabled rule set is not evaluated, so it is not compiled either.
			// Validate still rejects it at startup.
			name:    "DisabledBadExpression_IsNotCompiled",
			ruleSet: withEnabled(ruleSetAll("off", 0, "node.metadata.name =="), &disabled),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ruleset.Compile([]testRuleSet{tt.ruleSet})
			if tt.want == nil {
				require.NoError(t, err)
				return
			}

			require.ErrorIs(t, err, tt.want)
			require.ErrorContains(t, err, tt.contains)
		})
	}
}

func compile(t *testing.T, ruleSets []testRuleSet) *ruleset.Evaluator[testRuleSet] {
	t.Helper()

	evaluator, err := ruleset.Compile(ruleSets)
	require.NoError(t, err)

	return evaluator
}

func requireEvaluationError(t *testing.T, err error, ruleSet string) {
	t.Helper()

	var evalErr *ruleset.EvaluationError

	require.True(t, errors.As(err, &evalErr), "want *ruleset.EvaluationError, got %v", err)
	require.Equal(t, ruleSet, evalErr.RuleSet)
}

func ruleSetAll(name string, priority int, expressions ...string) testRuleSet {
	return testRuleSet{RuleSet: ruleset.RuleSet{Name: name, Priority: priority, Match: ruleset.Match{All: rules(expressions)}}}
}

func ruleSetAny(name string, priority int, expressions ...string) testRuleSet {
	return testRuleSet{RuleSet: ruleset.RuleSet{Name: name, Priority: priority, Match: ruleset.Match{Any: rules(expressions)}}}
}

func withEnabled(ruleSet testRuleSet, enabled *bool) testRuleSet {
	ruleSet.Enabled = enabled

	return ruleSet
}

// rules gives each expression the kind of the variable it reads.
func rules(expressions []string) []ruleset.Rule {
	out := make([]ruleset.Rule, 0, len(expressions))

	for _, expression := range expressions {
		kind := ruleset.KindNode
		if strings.HasPrefix(expression, "event") {
			kind = ruleset.KindHealthEvent
		}

		out = append(out, ruleset.Rule{Kind: kind, Expression: expression})
	}

	return out
}

func fullInput() ruleset.Input {
	return ruleset.Input{Event: testEvent(), Node: testNode()}
}

func testEvent() *protos.HealthEvent {
	return &protos.HealthEvent{Agent: "gpu-health-monitor", NodeName: "node-a"}
}

func testNode() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"pool": "gpu"}}}
}
