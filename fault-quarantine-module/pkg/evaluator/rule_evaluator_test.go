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
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/nodeinfo"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestEvaluate(t *testing.T) {
	expression := "event.agent == 'GPU' && event.checkName == 'XidError' && ('31' in event.errorCode || '42' in event.errorCode)"
	evaluator, err := NewHealthEventRuleEvaluator(expression)
	if err != nil {
		t.Fatalf("Failed to create HealthEventRuleEvaluator: %v", err)
	}

	eventTrue := &protos.HealthEvent{
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

	eventFalse := &protos.HealthEvent{
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
				isEvaluated, err := evaluator.Evaluate(&protos.HealthEvent{
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
	event := &protos.HealthEvent{
		Version:            1,
		Agent:              "test-agent",
		ComponentClass:     "test-component",
		CheckName:          "test-check",
		IsFatal:            true,
		IsHealthy:          false,
		Message:            "test-message",
		RecommendedAction:  protos.RecommenedAction_RESTART_VM,
		ErrorCode:          []string{"E001", "E002"},
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "GPU-0"}},
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
		"recommendedAction": float64(protos.RecommenedAction_RESTART_VM),
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
		"nodeName":            "test-node",
		"quarantineOverrides": nil,
		"drainOverrides":      nil,
	}

	if !reflect.DeepEqual(result, expectedMap) {
		t.Errorf("Expected map %v, got %v", expectedMap, result)
	}
}
