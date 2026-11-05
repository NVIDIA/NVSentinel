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

package evaluator

import (
	"context"
	"errors"
	"reflect"
	"testing"

	multierror "github.com/hashicorp/go-multierror"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/commons/pkg/celrule"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
)

type mockRule struct {
	result bool
	err    error
}

func (m *mockRule) Evaluate(context.Context, *protos.HealthEvent) (bool, error) {
	return m.result, m.err
}

func TestAnyRuleSetEvaluator_Evaluate(t *testing.T) {
	tests := []struct {
		name       string
		rules []Rule
		event      *protos.HealthEvent
		expected   common.RuleEvaluationResult
		expectErr  bool
	}{
		{
			name: "One evaluator returns true",
			rules: []Rule{
				&mockRule{result: false, err: nil},
				&mockRule{result: true, err: nil},
				&mockRule{result: false, err: nil},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationSuccess,
			expectErr: false,
		},
		{
			name: "All evaluators return false",
			rules: []Rule{
				&mockRule{result: false, err: nil},
				&mockRule{result: false, err: nil},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationFailed,
			expectErr: false,
		},
		{
			name: "Evaluator returns error",
			rules: []Rule{
				&mockRule{result: false, err: errors.New("evaluation error")},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationFailed,
			expectErr: true,
		},
		{
			name: "Evaluator returns true despite error in others",
			rules: []Rule{
				&mockRule{result: false, err: errors.New("evaluation error")},
				&mockRule{result: true, err: nil},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationSuccess,
			expectErr: false,
		},
		{
			name: "All evaluators return error",
			rules: []Rule{
				&mockRule{result: false, err: errors.New("error 1")},
				&mockRule{result: false, err: errors.New("error 2")},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationFailed,
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluator := &AnyRuleSetEvaluator{
				rules:      tt.rules,
				Name:       "TestAnyRuleSet",
				Version:    "1",
				Priority:   1,
			}

			result, err := evaluator.Evaluate(context.Background(), tt.event)
			if result != tt.expected {
				t.Errorf("Expected result %v, got %v", tt.expected, result)
			}

			if tt.expectErr {
				if err == nil {
					t.Errorf("Expected error, got nil")
				} else if merr, ok := err.(*multierror.Error); !ok || len(merr.Errors) == 0 {
					t.Errorf("Expected multierror with errors, got %v", err)
				}
			} else if err != nil {
				t.Errorf("Expected no error, got %v", err)
			}
		})
	}
}

func TestAllRuleSetEvaluator_Evaluate(t *testing.T) {
	tests := []struct {
		name       string
		rules []Rule
		event      *protos.HealthEvent
		expected   common.RuleEvaluationResult
		expectErr  bool
	}{
		{
			name: "All evaluators return true",
			rules: []Rule{
				&mockRule{result: true, err: nil},
				&mockRule{result: true, err: nil},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationSuccess,
			expectErr: false,
		},
		{
			name: "One evaluator returns false",
			rules: []Rule{
				&mockRule{result: true, err: nil},
				&mockRule{result: false, err: nil},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationFailed,
			expectErr: false,
		},
		{
			name: "Evaluator returns error",
			rules: []Rule{
				&mockRule{result: true, err: nil},
				&mockRule{result: false, err: errors.New("evaluation error")},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationFailed,
			expectErr: true,
		},
		{
			name: "All evaluators return error",
			rules: []Rule{
				&mockRule{result: false, err: errors.New("error 1")},
				&mockRule{result: false, err: errors.New("error 2")},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationFailed,
			expectErr: true,
		},
		{
			name: "All evaluators return true with one error",
			rules: []Rule{
				&mockRule{result: true, err: errors.New("error 1")},
				&mockRule{result: true, err: nil},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationSuccess,
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluator := &AllRuleSetEvaluator{
				rules:      tt.rules,
				Name:       "TestAllRuleSet",
				Version:    "1",
				Priority:   1,
			}

			result, err := evaluator.Evaluate(context.Background(), tt.event)
			if result != tt.expected {
				t.Errorf("Expected result %v, got %v", tt.expected, result)
			}

			if tt.expectErr {
				if err == nil {
					t.Errorf("Expected error, got nil")
				}
			} else if err != nil {
				t.Errorf("Expected no error, got %v", err)
			}
		})
	}
}

// An expression that fails on the event fails the same way on every replay, so
// cold start must not retry it.
func TestRuleSetEvaluator_EvaluationError_IsPermanent(t *testing.T) {
	rule, err := celrule.NewHealthEventRule(`event.metadata["missing"].startsWith("value")`)
	require.NoError(t, err)

	ruleSet := config.RuleSetMeta{Name: "RuleSet"}

	for _, evaluator := range []RuleSetEvaluatorIface{
		NewAllRuleSetEvaluator([]Rule{rule}, ruleSet),
		NewAnyRuleSetEvaluator([]Rule{rule}, ruleSet),
	} {
		result, err := evaluator.Evaluate(context.Background(), &protos.HealthEvent{})
		assert.Equal(t, common.RuleEvaluationFailed, result)
		require.Error(t, err)
		assert.True(t, coldstart.IsPermanentError(err), "%T: %v", evaluator, err)
	}
}

// The node cache derives what it retains from reads of common.NodeCELVar, so
// it must name the variable celrule binds the Node to. common does not import
// celrule, which would add cel-go to every module that imports common.
func TestNodeCELVar_MatchesCelrule(t *testing.T) {
	require.Equal(t, celrule.NodeVar, common.NodeCELVar)
}

func TestInitializeRuleSetEvaluators(t *testing.T) {
	mockRule1 := config.Rule{
		Kind:       "HealthEvent",
		Expression: "event.isHealthy == false",
	}

	mockRule2 := config.Rule{
		Kind:       "HealthEvent",
		Expression: "event.isFatal == true",
	}

	invalidRule := config.Rule{
		Kind:       "UnknownKind",
		Expression: "",
	}

	ruleSet1 := config.RuleSetMeta{
		Enabled:  true,
		Name:     "RuleSet1",
		Version:  "1",
		Priority: 1,
		Match: config.Match{
			Any: []config.Rule{mockRule1, mockRule2},
		},
	}

	ruleSet2 := config.RuleSetMeta{
		Enabled:  true,
		Name:     "RuleSet2",
		Version:  "1",
		Priority: 2,
		Match: config.Match{
			All: []config.Rule{mockRule1},
		},
	}

	ruleSetInvalid := config.RuleSetMeta{
		Enabled:  true,
		Name:     "RuleSetInvalid",
		Version:  "1",
		Priority: 3,
		Match: config.Match{
			Any: []config.Rule{invalidRule},
		},
	}

	tests := []struct {
		name          string
		ruleSets      []config.RuleSetMeta
		expectedCount int
		expectErr     bool
	}{
		{
			name:          "Valid rule sets",
			ruleSets:      []config.RuleSetMeta{ruleSet1, ruleSet2},
			expectedCount: 2,
			expectErr:     false,
		},
		{
			name:          "Invalid rule set",
			ruleSets:      []config.RuleSetMeta{ruleSetInvalid},
			expectedCount: 0,
			expectErr:     true,
		},
		{
			name:          "Mixed valid and invalid rule sets",
			ruleSets:      []config.RuleSetMeta{ruleSet1, ruleSetInvalid},
			expectedCount: 1,
			expectErr:     true,
		},
		{
			name:          "No rule sets",
			ruleSets:      []config.RuleSetMeta{},
			expectedCount: 0,
			expectErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluators, err := InitializeRuleSetEvaluators(tt.ruleSets, nil)
			if len(evaluators) != tt.expectedCount {
				t.Errorf("Expected %d evaluators, got %d", tt.expectedCount, len(evaluators))
			}

			if tt.expectErr {
				if err == nil {
					t.Errorf("Expected error, got nil")
				} else if merr, ok := err.(*multierror.Error); !ok || len(merr.Errors) == 0 {
					t.Errorf("Expected multierror with errors, got %v", err)
				}
			} else if err != nil {
				t.Errorf("Expected no error, got %v", err)
			}
		})
	}
}

func TestCreateRules(t *testing.T) {
	validRule := config.Rule{
		Kind:       "HealthEvent",
		Expression: "event.isHealthy == false",
	}

	invalidRule := config.Rule{
		Kind:       "UnknownKind",
		Expression: "",
	}

	invalidExpressionRule := config.Rule{
		Kind:       "HealthEvent",
		Expression: "invalid syntax",
	}

	tests := []struct {
		name          string
		rules         []config.Rule
		expectedCount int
		expectErr     bool
	}{
		{
			name:          "Valid rules",
			rules:         []config.Rule{validRule},
			expectedCount: 1,
			expectErr:     false,
		},
		{
			name:          "Invalid rule kind",
			rules:         []config.Rule{invalidRule},
			expectedCount: 0,
			expectErr:     true,
		},
		{
			name:          "Invalid expression",
			rules:         []config.Rule{invalidExpressionRule},
			expectedCount: 0,
			expectErr:     true,
		},
		{
			name:          "Mixed valid and invalid rules",
			rules:         []config.Rule{validRule, invalidRule},
			expectedCount: 1,
			expectErr:     true,
		},
		{
			name:          "No rules",
			rules:         []config.Rule{},
			expectedCount: 0,
			expectErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules, err := createRules(tt.rules, nil)
			if len(rules) != tt.expectedCount {
				t.Errorf("Expected %d rules, got %d", tt.expectedCount, len(rules))
			}

			if tt.expectErr {
				if err == nil {
					t.Errorf("Expected error, got nil")
				} else if merr, ok := err.(*multierror.Error); !ok || len(merr.Errors) == 0 {
					t.Errorf("Expected multierror with errors, got %v", err)
				}
			} else if err != nil {
				if merr, ok := err.(*multierror.Error); ok && len(merr.Errors) > 0 {
					t.Errorf("Expected no errors, but got multierror: %v", err)
				}
			}
		})
	}
}

func TestBaseRuleSetEvaluatorMethods(t *testing.T) {
	baseEvaluator := baseRuleSetEvaluator{
		Name:     "BaseEvaluator",
		Version:  "1",
		Priority: 5,
	}

	if baseEvaluator.GetName() != "BaseEvaluator" {
		t.Errorf("Expected Name to be 'BaseEvaluator', got %s", baseEvaluator.GetName())
	}

}

func TestNewAnyRuleSetEvaluator(t *testing.T) {
	rules := []Rule{
		&mockRule{result: true, err: nil},
	}
	ruleset := config.RuleSetMeta{
		Name:     "AnyRuleSet",
		Version:  "1",
		Priority: 1,
	}

	anyEvaluator := NewAnyRuleSetEvaluator(rules, ruleset)

	if anyEvaluator.GetName() != "AnyRuleSet" {
		t.Errorf("Expected Name to be 'AnyRuleSet', got %s", anyEvaluator.GetName())
	}

	if !reflect.DeepEqual(anyEvaluator.rules, rules) {
		t.Errorf("Rules do not match")
	}
}

func TestNewAllRuleSetEvaluator(t *testing.T) {
	rules := []Rule{
		&mockRule{result: true, err: nil},
	}
	ruleset := config.RuleSetMeta{
		Name:     "AllRuleSet",
		Version:  "1",
		Priority: 1,
	}

	allEvaluator := NewAllRuleSetEvaluator(rules, ruleset)

	if allEvaluator.GetName() != "AllRuleSet" {
		t.Errorf("Expected Name to be 'AllRuleSet', got %s", allEvaluator.GetName())
	}

	if !reflect.DeepEqual(allEvaluator.rules, rules) {
		t.Errorf("Rules do not match")
	}
}
