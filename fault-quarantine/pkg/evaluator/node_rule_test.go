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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/nodecache"
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

			nodeInformer, err := informer.NewNodeInformer(testClient, 0, informer.GPUNodeLabel, informer.GPUNodeLabelValue, nodecache.Keys{})
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

			rules, err := createRules([]config.Rule{{Kind: "Node", Expression: tt.expression}}, nodeInformer)
			if err != nil && !tt.expectError {
				t.Fatalf("Failed to create Node rule: %v", err)
			}
			if len(rules) > 0 {
				evaluator := NewAllRuleSetEvaluator(rules, config.RuleSetMeta{Name: tt.name})
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
