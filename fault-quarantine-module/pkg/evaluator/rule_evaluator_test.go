// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/common"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/informer"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/nodeinfo"
	platformconnectorprotos "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
)

func TestEvaluate(t *testing.T) {
	expression := "event.agent == 'GPU' && event.checkName == 'XidError' && ('31' in event.errorCode || '42' in event.errorCode)"
	evaluator, err := NewHealthEventRuleEvaluator(expression)
	if err != nil {
		t.Fatalf("Failed to create HealthEventRuleEvaluator: %v", err)
	}

	eventTrue := &platformconnectorprotos.HealthEvent{
		Agent:     "GPU",
		CheckName: "XidError",
		ErrorCode: []string{"31"},
	}

	result, err := evaluator.Evaluate(eventTrue)
	if err != nil {
		t.Fatalf("Failed to evaluate expression: %v", err)
	}

	if result != common.RuleEvaluationSuccess {
		t.Errorf("Expected evaluation result to be true, got false")
	}

	eventFalse := &platformconnectorprotos.HealthEvent{
		Agent:     "GPU",
		CheckName: "XidError",
		ErrorCode: []string{"50"},
	}

	result, err = evaluator.Evaluate(eventFalse)
	if err != nil {
		t.Fatalf("Failed to evaluate expression: %v", err)
	}

	if result != common.RuleEvaluationFailed {
		t.Errorf("Expected evaluation result to be false, got true")
	}
}

func TestNodeToSkipLabelRuleEvaluator(t *testing.T) {
	tests := []struct {
		name           string
		expression     string
		nodeLabels     map[string]string
		expectEvaluate common.RuleEvaluationResult
		expectError    bool
	}{
		{
			name:       "Node should not be skipped - label present with value true",
			expression: `!('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false")`,
			nodeLabels: map[string]string{
				"k8saas.nvidia.com/ManagedByNVSentinel": "true",
			},
			expectEvaluate: common.RuleEvaluationSuccess,
			expectError:    false,
		},
		{
			name:           "Node should not be skipped - label not present",
			expression:     `!(has(node.metadata.labels) && 'k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false")`,
			nodeLabels:     map[string]string{},
			expectEvaluate: common.RuleEvaluationSuccess,
			expectError:    false,
		},
		{
			name:       "Node should be skipped - label present with value false",
			expression: `!('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false")`,
			nodeLabels: map[string]string{
				"k8saas.nvidia.com/ManagedByNVSentinel": "false",
			},
			expectEvaluate: common.RuleEvaluationFailed,
			expectError:    false,
		},
		{
			name:           "Invalid expression",
			expression:     "invalid.expression",
			nodeLabels:     map[string]string{},
			expectEvaluate: common.RuleEvaluationFailed,
			expectError:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			// Create mock node object with labels from test case
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: tt.nodeLabels, // Keep original labels from test case
				},
				Spec: corev1.NodeSpec{},
			}

			// Ensure the required label for the informer exists.
			// The NodeInformer specifically looks for GpuNodeLabel ("nvidia.com/gpu.present").
			if node.Labels == nil {
				node.Labels = make(map[string]string)
			}
			// Add the label the informer expects, preserving existing labels
			node.Labels[informer.GpuNodeLabel] = "true"

			clientset := fake.NewSimpleClientset(node)
			workSignal := make(chan struct{}, 1)
			// Use 0 resync period for tests unless specific timing is needed
			nodeInfo := nodeinfo.NewNodeInfo(workSignal)
			nodeInformer, err := informer.NewNodeInformer(clientset, 0, workSignal, nodeInfo)
			if err != nil {
				t.Fatalf("Failed to create NodeInformer: %v", err)
			}
			stopCh := make(chan struct{})
			defer close(stopCh)

			go nodeInformer.Run(stopCh)

			// Wait for the cache to sync
			if ok := cache.WaitForCacheSync(stopCh, nodeInformer.HasSynced); !ok {
				t.Fatalf("failed to wait for caches to sync")
			}
			// Create evaluator with mocked client
			evaluator, err := NewNodeRuleEvaluator(tt.expression, nodeInformer.Lister())
			if err != nil && !tt.expectError {
				t.Fatalf("Failed to create NodeToSkipLabelRuleEvaluator: %v", err)
			}
			if evaluator != nil {
				isEvaluated, err := evaluator.Evaluate(&platformconnectorprotos.HealthEvent{
					NodeName: "test-node",
				})
				if (err != nil) != tt.expectError {
					t.Errorf("Failed to evaluate expression: %s: %+v", tt.name, err)
					return
				}
				if isEvaluated != tt.expectEvaluate {
					t.Errorf("Expected evaluator %s to return %d but got %d", tt.name, tt.expectEvaluate, isEvaluated)
				}
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	eventTime := timestamppb.New(time.Now())
	event := &platformconnectorprotos.HealthEvent{
		Version:            1,
		Agent:              "test-agent",
		ComponentClass:     "test-component",
		CheckName:          "test-check",
		IsFatal:            true,
		IsHealthy:          false,
		Message:            "test-message",
		RecommendedAction:  platformconnectorprotos.RecommenedAction_NODE_REBOOT,
		ErrorCode:          []string{"E001", "E002"},
		EntitiesImpacted:   []*platformconnectorprotos.Entity{{EntityType: "GPU", EntityValue: "GPU-0"}},
		Metadata:           map[string]string{"key1": "value1"},
		GeneratedTimestamp: eventTime,
		NodeName:           "test-node",
	}

	result, err := RoundTrip(event)
	if err != nil {
		t.Fatalf("Failed to roundtrip event: %v", err)
	}

	expectedMap := map[string]interface{}{
		"version":           float64(1),
		"agent":             "test-agent",
		"componentClass":    "test-component",
		"checkName":         "test-check",
		"isFatal":           true,
		"isHealthy":         false,
		"message":           "test-message",
		"recommendedAction": float64(platformconnectorprotos.RecommenedAction_NODE_REBOOT),
		"errorCode":         []interface{}{"E001", "E002"},
		"entitiesImpacted": []interface{}{
			map[string]interface{}{
				"entityType":  "GPU",
				"entityValue": "GPU-0",
			},
		},
		"metadata": map[string]interface{}{"key1": "value1"},
		"generatedTimestamp": map[string]interface{}{
			"seconds": float64(eventTime.GetSeconds()),
			"nanos":   float64(eventTime.GetNanos()),
		},
		"nodeName": "test-node",
	}

	if !reflect.DeepEqual(result, expectedMap) {
		t.Errorf("Expected map %v, got %v", expectedMap, result)
	}
}

// Mock NodeInfoProvider for testing MaxPercentage rule
type mockNodeInfoProvider struct {
	TotalNodes     int
	CordonedNodes  int
	Err            error
	InformerSynced bool
}

func (m *mockNodeInfoProvider) GetGpuNodeCounts() (int, int, error) {
	if !m.InformerSynced {
		return 0, 0, fmt.Errorf("informer not synced") // Simulate not synced error
	}
	return m.TotalNodes, m.CordonedNodes, m.Err
}

func (m *mockNodeInfoProvider) HasSynced() bool {
	return m.InformerSynced
}

func TestMaxPercentageOfNodesToCordonRuleEvaluator_Evaluate(t *testing.T) {
	// Define the expression to be used by the constructor
	expression := "maxPercentageOfNodesToCordon <= 50" // Example: Allow cordon if resulting percentage is <= 50%

	// Dummy health event (content doesn't matter for this rule)
	event := &platformconnectorprotos.HealthEvent{NodeName: "node-1"}

	testCases := []struct {
		name                string
		mockProvider        *mockNodeInfoProvider
		expectedResult      common.RuleEvaluationResult
		expectError         bool
		expectedErrorSubstr string // Optional: check for specific error messages
	}{
		{
			name: "Informer Not Synced",
			mockProvider: &mockNodeInfoProvider{
				InformerSynced: false, // Simulate not synced
			},
			expectedResult:      common.RuleEvaluationErroredOut, // Should error out if informer isn't ready
			expectError:         true,
			expectedErrorSubstr: "informer not synced",
		},
		{
			name: "GetGpuNodeCounts Error",
			mockProvider: &mockNodeInfoProvider{
				InformerSynced: true,
				Err:            fmt.Errorf("internal informer error"), // Simulate error from GetGpuNodeCounts
			},
			expectedResult:      common.RuleEvaluationErroredOut,
			expectError:         true,
			expectedErrorSubstr: "internal informer error",
		},
		{
			name: "Zero Total Nodes",
			mockProvider: &mockNodeInfoProvider{
				InformerSynced: true,
				TotalNodes:     0,
				CordonedNodes:  0,
			},
			expectedResult:      common.RuleEvaluationFailed, // As per current logic
			expectError:         true,                        // Error is returned in this case too
			expectedErrorSubstr: "no GPU nodes found",
		},
		{
			name: "Cordon Allowed (0/10 nodes -> 10% <= 50%)",
			mockProvider: &mockNodeInfoProvider{
				InformerSynced: true,
				TotalNodes:     10,
				CordonedNodes:  0, // (0+1)/10 = 10%
			},
			expectedResult: common.RuleEvaluationSuccess,
			expectError:    false,
		},
		{
			name: "Cordon Allowed (4/10 nodes -> 50% <= 50%)",
			mockProvider: &mockNodeInfoProvider{
				InformerSynced: true,
				TotalNodes:     10,
				CordonedNodes:  4, // (4+1)/10 = 50%
			},
			expectedResult: common.RuleEvaluationSuccess,
			expectError:    false,
		},
		{
			name: "Cordon Disallowed (5/10 nodes -> 60% > 50%)",
			mockProvider: &mockNodeInfoProvider{
				InformerSynced: true,
				TotalNodes:     10,
				CordonedNodes:  5, // (5+1)/10 = 60%
			},
			// Current logic returns RetryAgainInFuture when disallowed
			expectedResult: common.RuleEvaluationRetryAgainInFuture,
			expectError:    false, // No error, just disallowed
		},
		{
			name: "Cordon Disallowed (1/1 node -> 200% > 50%)", // Note: potentialPercentage calculation (1+1)/1 = 200%
			mockProvider: &mockNodeInfoProvider{
				InformerSynced: true,
				TotalNodes:     1,
				CordonedNodes:  1,
			},
			expectedResult: common.RuleEvaluationRetryAgainInFuture,
			expectError:    false,
		},
		{
			name: "Cordon Allowed (0/1 node -> 100% - But rule uses <= 50%, so this case should retry)",
			// This case tests if the rule correctly prevents cordon even if it's the first one, if total nodes is low.
			mockProvider: &mockNodeInfoProvider{
				InformerSynced: true,
				TotalNodes:     1,
				CordonedNodes:  0, // (0+1)/1 = 100%
			},
			expectedResult: common.RuleEvaluationRetryAgainInFuture,
			expectError:    false,
		},
		{
			name: "Cordon Allowed (0/2 nodes -> 50% <= 50%)",
			mockProvider: &mockNodeInfoProvider{
				InformerSynced: true,
				TotalNodes:     2,
				CordonedNodes:  0, // (0+1)/2 = 50%
			},
			expectedResult: common.RuleEvaluationSuccess,
			expectError:    false,
		},
		{
			name: "Cordon Disallowed (1/2 nodes -> 100% > 50%)",
			mockProvider: &mockNodeInfoProvider{
				InformerSynced: true,
				TotalNodes:     2,
				CordonedNodes:  1, // (1+1)/2 = 100%
			},
			expectedResult: common.RuleEvaluationRetryAgainInFuture,
			expectError:    false,
		},
		// Note: CEL evaluation errors and non-boolean results are hard to trigger
		// with a simple expression and mock setup, but could be added if needed.
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Use the constructor to create the evaluator
			evaluator, err := NewMaxPercentageOfNodesToCordonRuleEvaluator(expression, tc.mockProvider)
			// We expect the constructor to succeed for a valid expression
			assert.NoError(t, err, "NewMaxPercentageOfNodesToCordonRuleEvaluator failed for a valid expression")
			assert.NotNil(t, evaluator, "Evaluator should not be nil after successful construction")

			result, err := evaluator.Evaluate(event)

			assert.Equal(t, tc.expectedResult, result, "Unexpected RuleEvaluationResult")

			if tc.expectError {
				assert.Error(t, err, "Expected an error, but got nil")
				if tc.expectedErrorSubstr != "" && err != nil {
					assert.Contains(t, err.Error(), tc.expectedErrorSubstr, "Error message mismatch")
				}
			} else {
				assert.NoError(t, err, "Expected no error, but got one: %v", err)
			}
		})
	}
}
