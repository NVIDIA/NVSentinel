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
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/nodecache"
)

type MockRuleEvaluator struct {
	result bool
	err    error
}

func (m *MockRuleEvaluator) Evaluate(
	_ context.Context,
	healthEvent *protos.HealthEvent,
) (bool, error) {
	return m.result, m.err
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
			expected:  common.RuleEvaluationFailed,
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
			expected:  common.RuleEvaluationFailed,
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluator := &AnyRuleSetEvaluator{
				evaluators: tt.evaluators,
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
	evaluator, err := newHealthEventRule(`event.metadata["missing"].startsWith("value")`)
	require.NoError(t, err)

	ruleSet := config.RuleSetMeta{Name: "RuleSet"}

	for _, ruleSetEvaluator := range []RuleSetEvaluatorIface{
		NewAllRuleSetEvaluator([]RuleEvaluator{evaluator}, ruleSet),
		NewAnyRuleSetEvaluator([]RuleEvaluator{evaluator}, ruleSet),
	} {
		result, err := ruleSetEvaluator.Evaluate(context.Background(), &protos.HealthEvent{})
		assert.Equal(t, common.RuleEvaluationFailed, result)
		require.Error(t, err)
		assert.True(t, coldstart.IsPermanentError(err), "%T: %v", ruleSetEvaluator, err)
	}
}

// A Node rule reads the cached node normally, and the live node during cold
// start, when the cache may not yet hold what earlier events wrote.
func TestInitializeRuleSetEvaluators_NodeRule_RecoveryReadsLiveNode(t *testing.T) {
	clientset := fake.NewSimpleClientset(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

	nodeInformer, err := informer.NewNodeInformer(clientset, 0,
		informer.GPUNodeLabel, informer.GPUNodeLabelValue, nodecache.Keys{})
	require.NoError(t, err)

	stopCh := make(chan struct{})
	defer close(stopCh)

	require.NoError(t, nodeInformer.Run(stopCh))

	ruleSetEvals, err := InitializeRuleSetEvaluators([]config.RuleSetMeta{{
		Name:    "node-rule",
		Enabled: true,
		Match: config.Match{
			All: []config.Rule{{Kind: "Node", Expression: `node.metadata.name == "node-a"`}},
		},
	}}, nodeInformer)
	require.NoError(t, err)
	require.Len(t, ruleSetEvals, 1)

	countNodeGets := func() int {
		count := 0

		for _, action := range clientset.Actions() {
			if action.Matches("get", "nodes") {
				count++
			}
		}

		return count
	}

	event := &protos.HealthEvent{NodeName: "node-a"}

	result, err := ruleSetEvals[0].Evaluate(context.Background(), event)
	require.NoError(t, err)
	assert.Equal(t, common.RuleEvaluationSuccess, result)
	assert.Zero(t, countNodeGets(), "normal evaluation must read the cache")

	result, err = ruleSetEvals[0].Evaluate(coldstart.WithRecoveryContext(context.Background()), event)
	require.NoError(t, err)
	assert.Equal(t, common.RuleEvaluationSuccess, result)
	assert.Equal(t, 1, countNodeGets(), "cold start must read the live node")
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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluators, err := createEvaluators(tt.rules, nil)
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

}

func TestNewAnyRuleSetEvaluator(t *testing.T) {
	evaluators := []RuleEvaluator{
		&MockRuleEvaluator{result: true, err: nil},
	}
	ruleset := config.RuleSetMeta{
		Name:     "AnyRuleSet",
		Version:  "1",
		Priority: 1,
	}

	anyEvaluator := NewAnyRuleSetEvaluator(evaluators, ruleset)

	if anyEvaluator.GetName() != "AnyRuleSet" {
		t.Errorf("Expected Name to be 'AnyRuleSet', got %s", anyEvaluator.GetName())
	}

	if !reflect.DeepEqual(anyEvaluator.evaluators, evaluators) {
		t.Errorf("Evaluators do not match")
	}
}

func TestNewAllRuleSetEvaluator(t *testing.T) {
	evaluators := []RuleEvaluator{
		&MockRuleEvaluator{result: true, err: nil},
	}
	ruleset := config.RuleSetMeta{
		Name:     "AllRuleSet",
		Version:  "1",
		Priority: 1,
	}

	allEvaluator := NewAllRuleSetEvaluator(evaluators, ruleset)

	if allEvaluator.GetName() != "AllRuleSet" {
		t.Errorf("Expected Name to be 'AllRuleSet', got %s", allEvaluator.GetName())
	}

	if !reflect.DeepEqual(allEvaluator.evaluators, evaluators) {
		t.Errorf("Evaluators do not match")
	}
}
