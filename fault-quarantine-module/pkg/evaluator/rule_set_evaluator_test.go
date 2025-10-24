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
	"errors"
	"reflect"
	"testing"

	multierror "github.com/hashicorp/go-multierror"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/config"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"k8s.io/client-go/kubernetes/fake"
)

type MockRuleEvaluator struct {
	result bool
	err    error
}

func (m *MockRuleEvaluator) Evaluate(healthEvent *protos.HealthEvent) (common.RuleEvaluationResult, error) {
	if m.result {
		return common.RuleEvaluationSuccess, m.err
	}
	return common.RuleEvaluationFailed, m.err
}

func TestAnyRuleSetEvaluator_Evaluate(t *testing.T) {
	tests := []struct {
		name       string
		evaluators []RuleEvaluator
		event      *protos.HealthEvent
		expected   common.RuleEvaluationResult
		expectErr  bool
	}{
		{
			name: "One evaluator returns true",
			evaluators: []RuleEvaluator{
				&MockRuleEvaluator{result: false, err: nil},
				&MockRuleEvaluator{result: true, err: nil},
				&MockRuleEvaluator{result: false, err: nil},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationSuccess,
			expectErr: false,
		},
		{
			name: "All evaluators return false",
			evaluators: []RuleEvaluator{
				&MockRuleEvaluator{result: false, err: nil},
				&MockRuleEvaluator{result: false, err: nil},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationFailed,
			expectErr: false,
		},
		{
			name: "Evaluator returns error",
			evaluators: []RuleEvaluator{
				&MockRuleEvaluator{result: false, err: errors.New("evaluation error")},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationErroredOut,
			expectErr: true,
		},
		{
			name: "Evaluator returns true despite error in others",
			evaluators: []RuleEvaluator{
				&MockRuleEvaluator{result: false, err: errors.New("evaluation error")},
				&MockRuleEvaluator{result: true, err: nil},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationSuccess,
			expectErr: false,
		},
		{
			name: "All evaluators return error",
			evaluators: []RuleEvaluator{
				&MockRuleEvaluator{result: false, err: errors.New("error 1")},
				&MockRuleEvaluator{result: false, err: errors.New("error 2")},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationErroredOut,
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluator := &AnyRuleSetEvaluator{
				evaluators: tt.evaluators,
				baseRuleSetEvaluator: baseRuleSetEvaluator{
					Name:     "TestAnyRuleSet",
					Version:  "1",
					Priority: 1,
				},
			}

			result, err := evaluator.Evaluate(tt.event)
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
		evaluators []RuleEvaluator
		event      *protos.HealthEvent
		expected   common.RuleEvaluationResult
		expectErr  bool
	}{
		{
			name: "All evaluators return true",
			evaluators: []RuleEvaluator{
				&MockRuleEvaluator{result: true, err: nil},
				&MockRuleEvaluator{result: true, err: nil},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationSuccess,
			expectErr: false,
		},
		{
			name: "One evaluator returns false",
			evaluators: []RuleEvaluator{
				&MockRuleEvaluator{result: true, err: nil},
				&MockRuleEvaluator{result: false, err: nil},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationFailed,
			expectErr: false,
		},
		{
			name: "Evaluator returns error",
			evaluators: []RuleEvaluator{
				&MockRuleEvaluator{result: true, err: nil},
				&MockRuleEvaluator{result: false, err: errors.New("evaluation error")},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationFailed,
			expectErr: true,
		},
		{
			name: "All evaluators return error",
			evaluators: []RuleEvaluator{
				&MockRuleEvaluator{result: false, err: errors.New("error 1")},
				&MockRuleEvaluator{result: false, err: errors.New("error 2")},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationFailed,
			expectErr: true,
		},
		{
			name: "All evaluators return true with one error",
			evaluators: []RuleEvaluator{
				&MockRuleEvaluator{result: true, err: errors.New("error 1")},
				&MockRuleEvaluator{result: true, err: nil},
			},
			event:     &protos.HealthEvent{},
			expected:  common.RuleEvaluationSuccess,
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluator := &AllRuleSetEvaluator{
				evaluators: tt.evaluators,
				baseRuleSetEvaluator: baseRuleSetEvaluator{
					Name:     "TestAllRuleSet",
					Version:  "1",
					Priority: 1,
				},
			}

			result, err := evaluator.Evaluate(tt.event)
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

	ruleSet1 := config.RuleSet{
		Name:     "RuleSet1",
		Version:  "1",
		Priority: 1,
		Match: config.Match{
			Any: []config.Rule{mockRule1, mockRule2},
		},
	}

	ruleSet2 := config.RuleSet{
		Name:     "RuleSet2",
		Version:  "1",
		Priority: 2,
		Match: config.Match{
			All: []config.Rule{mockRule1},
		},
	}

	ruleSetInvalid := config.RuleSet{
		Name:     "RuleSetInvalid",
		Version:  "1",
		Priority: 3,
		Match: config.Match{
			Any: []config.Rule{invalidRule},
		},
	}

	tests := []struct {
		name          string
		ruleSets      []config.RuleSet
		expectedCount int
		expectErr     bool
	}{
		{
			name:          "Valid rule sets",
			ruleSets:      []config.RuleSet{ruleSet1, ruleSet2},
			expectedCount: 2,
			expectErr:     false,
		},
		{
			name:          "Invalid rule set",
			ruleSets:      []config.RuleSet{ruleSetInvalid},
			expectedCount: 0,
			expectErr:     true,
		},
		{
			name:          "Mixed valid and invalid rule sets",
			ruleSets:      []config.RuleSet{ruleSet1, ruleSetInvalid},
			expectedCount: 1,
			expectErr:     true,
		},
		{
			name:          "No rule sets",
			ruleSets:      []config.RuleSet{},
			expectedCount: 0,
			expectErr:     false,
		},
	}

	clientset := fake.NewSimpleClientset()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluators, err := InitializeRuleSetEvaluators(tt.ruleSets, clientset, nil)
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

func TestCreateEvaluators(t *testing.T) {
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

	clientset := fake.NewSimpleClientset()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluators, err := createEvaluators(tt.rules, clientset, nil)
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

	if baseEvaluator.GetVersion() != "1" {
		t.Errorf("Expected Version to be '1', got %s", baseEvaluator.GetVersion())
	}

	if baseEvaluator.GetPriority() != 5 {
		t.Errorf("Expected Priority to be 5, got %d", baseEvaluator.GetPriority())
	}
}

func TestNewAnyRuleSetEvaluator(t *testing.T) {
	evaluators := []RuleEvaluator{
		&MockRuleEvaluator{result: true, err: nil},
	}
	ruleset := config.RuleSet{
		Name:     "AnyRuleSet",
		Version:  "1",
		Priority: 1,
	}

	anyEvaluator := NewAnyRuleSetEvaluator(evaluators, ruleset)

	if anyEvaluator.GetName() != "AnyRuleSet" {
		t.Errorf("Expected Name to be 'AnyRuleSet', got %s", anyEvaluator.GetName())
	}

	if anyEvaluator.GetVersion() != "1" {
		t.Errorf("Expected Version to be '1', got %s", anyEvaluator.GetVersion())
	}

	if anyEvaluator.GetPriority() != 1 {
		t.Errorf("Expected Priority to be 1, got %d", anyEvaluator.GetPriority())
	}

	if !reflect.DeepEqual(anyEvaluator.evaluators, evaluators) {
		t.Errorf("Evaluators do not match")
	}
}

func TestNewAllRuleSetEvaluator(t *testing.T) {
	evaluators := []RuleEvaluator{
		&MockRuleEvaluator{result: true, err: nil},
	}
	ruleset := config.RuleSet{
		Name:     "AllRuleSet",
		Version:  "1",
		Priority: 1,
	}

	allEvaluator := NewAllRuleSetEvaluator(evaluators, ruleset)

	if allEvaluator.GetName() != "AllRuleSet" {
		t.Errorf("Expected Name to be 'AllRuleSet', got %s", allEvaluator.GetName())
	}

	if allEvaluator.GetVersion() != "1" {
		t.Errorf("Expected Version to be '1', got %s", allEvaluator.GetVersion())
	}

	if allEvaluator.GetPriority() != 1 {
		t.Errorf("Expected Priority to be 1, got %d", allEvaluator.GetPriority())
	}

	if !reflect.DeepEqual(allEvaluator.evaluators, evaluators) {
		t.Errorf("Evaluators do not match")
	}
}
