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
	"log"
	"os"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
	"github.com/nvidia/nvsentinel/store-client/pkg/testutils"
)

var (
	testClient *kubernetes.Clientset
	testEnv    *envtest.Environment
)

func TestMain(m *testing.M) {
	var err error

	testEnv = &envtest.Environment{}

	testRestConfig, err := testEnv.Start()
	if err != nil {
		log.Fatalf("Failed to start test environment: %v", err)
	}

	testClient, err = kubernetes.NewForConfig(testRestConfig)
	if err != nil {
		log.Fatalf("Failed to create kubernetes client: %v", err)
	}

	exitCode := m.Run()

	if err := testEnv.Stop(); err != nil {
		log.Fatalf("Failed to stop test environment: %v", err)
	}
	os.Exit(exitCode)
}

func createTestNode(ctx context.Context, t *testing.T, name string, labels map[string]string) {
	t.Helper()

	if labels == nil {
		labels = make(map[string]string)
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: corev1.NodeSpec{},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}

	_, err := testClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test node %s: %v", name, err)
	}
}

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
			ctx := context.Background()
			nodeName := testutils.GenerateTestNodeName("test-node")

			createTestNode(ctx, t, nodeName, tt.nodeLabels)
			defer func() {
				_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
			}()

			nodeInformer, err := informer.NewNodeInformer(testClient, 0)
			if err != nil {
				t.Fatalf("Failed to create NodeInformer: %v", err)
			}

			stopCh := make(chan struct{})
			defer close(stopCh)

			go func() {
				_ = nodeInformer.Run(stopCh)
			}()

			if ok := cache.WaitForCacheSync(stopCh, nodeInformer.GetInformer().HasSynced); !ok {
				t.Fatalf("NodeInformer failed to sync")
			}

			evaluator, err := NewNodeRuleEvaluator(tt.expression, nodeInformer.Lister())
			if err != nil && !tt.expectError {
				t.Fatalf("Failed to create NodeToSkipLabelRuleEvaluator: %v", err)
			}
			if evaluator != nil {
				isEvaluated, err := evaluator.Evaluate(&protos.HealthEvent{
					NodeName: nodeName,
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
		Id:                 "123",
		Version:            1,
		Agent:              "test-agent",
		ComponentClass:     "test-component",
		CheckName:          "test-check",
		IsFatal:            true,
		IsHealthy:          false,
		Message:            "test-message",
		RecommendedAction:  protos.RecommendedAction_RESTART_VM,
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
		"id":                "123",
		"version":           float64(1),
		"agent":             "test-agent",
		"componentClass":    "test-component",
		"checkName":         "test-check",
		"isFatal":           true,
		"isHealthy":         false,
		"message":           "test-message",
		"recommendedAction": float64(protos.RecommendedAction_RESTART_VM),
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
		"processingStrategy":  float64(0),
		"quarantineOverrides": nil,
		"drainOverrides":      nil,
	}

	if !reflect.DeepEqual(result, expectedMap) {
		t.Errorf("Expected map %v, got %v", expectedMap, result)
	}
}
