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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
	"github.com/nvidia/nvsentinel/store-client/pkg/testutils"
)

type directNodeReaderStub struct {
	node  *corev1.Node
	err   error
	calls int
}

func (s *directNodeReaderStub) GetNodeDirect(context.Context, string) (*corev1.Node, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}

	return s.node.DeepCopy(), nil
}

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
	labels[informer.GPUNodeLabel] = "true"

	node := &corev1.Node{
		Name:   name,
		Labels: labels,
		Spec:   corev1.NodeSpec{},
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

func TestNodeRuleEvaluatorWithMetadataAndSpecOnly(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	node := &corev1.Node{
		Name:        "slim-node",
		Labels:      map[string]string{"environment": "production"},
		Annotations: map[string]string{"maintenance": "false"},
		Spec: corev1.NodeSpec{
			Unschedulable: true,
			Taints: []corev1.Taint{{
				Key:    "dedicated",
				Value:  "gpu",
				Effect: corev1.TaintEffectNoSchedule,
			}},
		},
	}
	if err := indexer.Add(node); err != nil {
		t.Fatalf("indexer.Add() error = %v", err)
	}

	evaluator, err := NewNodeRuleEvaluator(
		`node.metadata.name == "slim-node" &&
		 node.metadata.labels["environment"] == "production" &&
		 node.metadata.annotations["maintenance"] == "false" &&
		 node.spec.unschedulable &&
		 node.spec.taints.exists(t, t.key == "dedicated")`,
		corelisters.NewNodeLister(indexer),
	)
	if err != nil {
		t.Fatalf("NewNodeRuleEvaluator() error = %v", err)
	}

	result, err := evaluator.Evaluate(context.Background(), &protos.HealthEvent{NodeName: "slim-node"})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if result != common.RuleEvaluationSuccess {
		t.Fatalf("Evaluate() = %v, want success", result)
	}
}

func TestNodeRuleEvaluator_RecoveryRead_UsesCurrentNode(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-a",
			Labels: map[string]string{"state": "stale"},
		},
	}); err != nil {
		t.Fatalf("indexer.Add() error = %v", err)
	}

	reader := &directNodeReaderStub{node: &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-a",
			Labels: map[string]string{"state": "current"},
		},
	}}
	evaluator, err := newNodeRuleEvaluator(
		`node.metadata.labels["state"] == "current"`,
		corelisters.NewNodeLister(indexer),
		reader,
	)
	if err != nil {
		t.Fatalf("newNodeRuleEvaluator() error = %v", err)
	}

	result, err := evaluator.Evaluate(context.Background(), &protos.HealthEvent{NodeName: "node-a"})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if result != common.RuleEvaluationFailed || reader.calls != 0 {
		t.Fatalf("normal evaluation result/calls = %v/%d, want failed/0", result, reader.calls)
	}

	recoveryCtx := coldstart.WithRecoveryContext(context.Background())
	result, err = evaluator.Evaluate(
		recoveryCtx,
		&protos.HealthEvent{NodeName: "node-a"},
	)
	if err != nil {
		t.Fatalf("recovery Evaluate() error = %v", err)
	}
	if result != common.RuleEvaluationSuccess || reader.calls != 1 {
		t.Fatalf("recovery evaluation result/calls = %v/%d, want success/1", result, reader.calls)
	}

	result, err = evaluator.Evaluate(recoveryCtx, &protos.HealthEvent{NodeName: "node-a"})
	if err != nil {
		t.Fatalf("second recovery Evaluate() error = %v", err)
	}
	if result != common.RuleEvaluationSuccess || reader.calls != 1 {
		t.Fatalf("second recovery evaluation result/calls = %v/%d, want success/1", result, reader.calls)
	}
}

func TestNodeRuleEvaluator_DeletedNodeDuringRecovery_ReturnsPermanentError(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	reader := &directNodeReaderStub{
		err: apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, "deleted-node"),
	}
	evaluator, err := newNodeRuleEvaluator(
		`node.metadata.name == "deleted-node"`,
		corelisters.NewNodeLister(indexer),
		reader,
	)
	require.NoError(t, err)

	result, err := evaluator.Evaluate(
		coldstart.WithRecoveryContext(context.Background()),
		&protos.HealthEvent{NodeName: "deleted-node"},
	)

	assert.Equal(t, common.RuleEvaluationFailed, result)
	require.Error(t, err)
	assert.True(t, coldstart.IsPermanentError(err))
	assert.Equal(t, 1, reader.calls)
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

	result, err := evaluator.Evaluate(context.Background(), eventTrue)
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

	result, err = evaluator.Evaluate(context.Background(), eventFalse)
	if err != nil {
		t.Fatalf("Failed to evaluate expression: %v", err)
	}

	if result != common.RuleEvaluationFailed {
		t.Errorf("Expected evaluation result to be false, got true")
	}
}

func TestHealthEventRuleEvaluator_EvaluationError_ReturnsPermanentError(t *testing.T) {
	ruleEvaluator, err := NewHealthEventRuleEvaluator(
		`event.metadata["missing"].startsWith("value")`,
	)
	require.NoError(t, err)

	_, err = ruleEvaluator.Evaluate(context.Background(), &protos.HealthEvent{})
	require.Error(t, err)
	assert.True(t, coldstart.IsPermanentError(err))
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
		// ADR-040: nvsentinel.dgxc.nvidia.com/managed=false skips quarantine.
		{
			name:       "ADR-040 managed=false skips quarantine",
			expression: `!('nvsentinel.dgxc.nvidia.com/managed' in node.metadata.labels && node.metadata.labels['nvsentinel.dgxc.nvidia.com/managed'] == "false")`,
			nodeLabels: map[string]string{
				"nvsentinel.dgxc.nvidia.com/managed": "false",
			},
			expectEvaluate: common.RuleEvaluationFailed,
			expectError:    false,
		},
		{
			name:           "ADR-040 managed label absent — quarantine proceeds",
			expression:     `!('nvsentinel.dgxc.nvidia.com/managed' in node.metadata.labels && node.metadata.labels['nvsentinel.dgxc.nvidia.com/managed'] == "false")`,
			nodeLabels:     map[string]string{},
			expectEvaluate: common.RuleEvaluationSuccess,
			expectError:    false,
		},
		{
			name:       "ADR-040 managed=true — quarantine proceeds (only 'false' opts out)",
			expression: `!('nvsentinel.dgxc.nvidia.com/managed' in node.metadata.labels && node.metadata.labels['nvsentinel.dgxc.nvidia.com/managed'] == "false")`,
			nodeLabels: map[string]string{
				"nvsentinel.dgxc.nvidia.com/managed": "true",
			},
			expectEvaluate: common.RuleEvaluationSuccess,
			expectError:    false,
		},
		// Combined expression matching the default rulesets: both old and ADR-040 labels respected.
		{
			name: "combined expression: ADR-040 managed=false skips even if k8saas label absent",
			expression: `!('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false") &&
            !('nvsentinel.dgxc.nvidia.com/managed' in node.metadata.labels && node.metadata.labels['nvsentinel.dgxc.nvidia.com/managed'] == "false")`,
			nodeLabels: map[string]string{
				"nvsentinel.dgxc.nvidia.com/managed": "false",
			},
			expectEvaluate: common.RuleEvaluationFailed,
			expectError:    false,
		},
		{
			name: "combined expression: no opt-out labels — quarantine proceeds",
			expression: `!('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false") &&
            !('nvsentinel.dgxc.nvidia.com/managed' in node.metadata.labels && node.metadata.labels['nvsentinel.dgxc.nvidia.com/managed'] == "false")`,
			nodeLabels:     map[string]string{},
			expectEvaluate: common.RuleEvaluationSuccess,
			expectError:    false,
		},
		{
			// Verifies the legacy k8saas compatibility clause still skips quarantine
			// independently of the ADR-040 label, so removing it would break this test.
			name: "combined expression: legacy k8saas=false skips quarantine (backwards compat)",
			expression: `!('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false") &&
            !('nvsentinel.dgxc.nvidia.com/managed' in node.metadata.labels && node.metadata.labels['nvsentinel.dgxc.nvidia.com/managed'] == "false")`,
			nodeLabels: map[string]string{
				"k8saas.nvidia.com/ManagedByNVSentinel": "false",
			},
			expectEvaluate: common.RuleEvaluationFailed,
			expectError:    false,
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

			nodeInformer, err := informer.NewNodeInformer(testClient, 0, informer.GPUNodeLabel, informer.GPUNodeLabelValue)
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
				isEvaluated, err := evaluator.Evaluate(context.Background(), &protos.HealthEvent{
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

	expectedMap := map[string]any{
		"id":                "123",
		"version":           float64(1),
		"agent":             "test-agent",
		"componentClass":    "test-component",
		"checkName":         "test-check",
		"isFatal":           true,
		"isHealthy":         false,
		"message":           "test-message",
		"recommendedAction": float64(protos.RecommendedAction_RESTART_VM),
		"errorCode":         []any{"E001", "E002"},
		"entitiesImpacted": []any{
			map[string]any{
				"entityType":  "GPU",
				"entityValue": "GPU-0",
			},
		},
		"metadata": map[string]any{"key1": "value1"},
		"generatedTimestamp": map[string]any{
			"seconds": float64(eventTime.GetSeconds()),
			"nanos":   float64(eventTime.GetNanos()),
		},
		"nodeName":                "test-node",
		"processingStrategy":      float64(0),
		"quarantineOverrides":     nil,
		"drainOverrides":          nil,
		"customRecommendedAction": "",
	}

	if !reflect.DeepEqual(result, expectedMap) {
		t.Errorf("Expected map %v, got %v", expectedMap, result)
	}
}
