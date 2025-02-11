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

package reconciler

import (
	"context"
	"encoding/json"
	"testing"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/config"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/evaluator"
	platformconnectorprotos "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
)

type mockK8sClient struct {
	getNodeAnnotationsFn     func(ctx context.Context, nodeName string) (map[string]string, error)
	getNodesWithAnnotationFn func(ctx context.Context, annotationKey string) ([]string, error)
	taintAndCordonNodeFn     func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error
	unTaintAndUnCordonNodeFn func(ctx context.Context, nodeName string, taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error
}

func (m *mockK8sClient) GetNodeAnnotations(ctx context.Context, nodeName string) (map[string]string, error) {
	return m.getNodeAnnotationsFn(ctx, nodeName)
}
func (m *mockK8sClient) GetNodesWithAnnotation(ctx context.Context, annotationKey string) ([]string, error) {
	return m.getNodesWithAnnotationFn(ctx, annotationKey)
}
func (m *mockK8sClient) TaintAndCordonNodeAndSetAnnotations(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error {
	return m.taintAndCordonNodeFn(ctx, nodeName, taints, isCordon, annotations, labelMap)
}
func (m *mockK8sClient) UnTaintAndUnCordonNodeAndRemoveAnnotations(ctx context.Context, nodeName string, taints []config.Taint, isUnCordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
	return m.unTaintAndUnCordonNodeFn(ctx, nodeName, taints, isUnCordon, annotationKeys, labelsToRemove, labelMap)
}

type mockEvaluator struct {
	name     string
	ok       bool
	evalErr  error
	priority int
	version  string
}

func (m *mockEvaluator) GetName() string {
	return m.name
}

func (m *mockEvaluator) Evaluate(event *platformconnectorprotos.HealthEvent) (bool, error) {
	return m.ok, m.evalErr
}

func (m *mockEvaluator) GetPriority() int {
	return m.priority
}

func (m *mockEvaluator) GetVersion() string {
	return m.version
}

func TestHandleEvent(t *testing.T) {
	ctx := context.Background()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k88s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name: "ruleset1",
				Taint: config.Taint{
					Key:    "key1",
					Value:  "val1",
					Effect: "NoSchedule",
				},
				Cordon:   config.Cordon{ShouldCordon: false},
				Priority: 10,
			},
			{
				Name: "ruleset2",
				Taint: config.Taint{
					Key:    "key2",
					Value:  "val2",
					Effect: "NoExecute",
				},
				Cordon:   config.Cordon{ShouldCordon: true},
				Priority: 5,
			},
		},
	}

	cfg := ReconcilerConfig{
		TomlConfig: tomlConfig,

		K8sClient: &mockK8sClient{
			getNodesWithAnnotationFn: func(ctx context.Context, annotationKey string) ([]string, error) {
				// Initially no quarantined nodes
				return []string{}, nil
			},
			taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelsMap map[string]string) error {
				// ensure it is called with correct parameters
				if nodeName != "node1" {
					t.Errorf("Expected node name node1, got %s", nodeName)
				}
				// We know from these rules one taint and cordon should happen
				if len(taints) != 2 {
					t.Errorf("Expected 2 taints to be applied, got %d", len(taints))
				}
				if !isCordon {
					t.Errorf("Expected node to be cordoned")
				}
				if _, ok := annotations[quarantineHealthEventAnnotationKey]; !ok {
					t.Errorf("Expected quarantineHealthEvent annotation to be set")
				}
				if len(labelsMap) != 3 {
					t.Errorf("Expected cordon labels to be applied on node %s", nodeName)
				}
				return nil
			},
		},
	}

	r := NewReconciler(cfg)

	ruleSetEvals := []evaluator.RuleSetEvaluatorIface{
		&mockEvaluator{name: "ruleset1", ok: true}, // applies taint key1=val1
		&mockEvaluator{name: "ruleset2", ok: true}, // applies taint key2=val2 and cordon
	}

	quarantinedNodesMap := make(map[string]bool)

	event := &platformconnectorprotos.HealthEvent{
		NodeName: "node1",
	}

	isQuarantined := r.handleEvent(ctx, event, ruleSetEvals,
		rulesetsConfig{
			TaintConfigMap: map[string]*config.Taint{
				"ruleset1": &tomlConfig.RuleSets[0].Taint,
				"ruleset2": &tomlConfig.RuleSets[1].Taint,
			},
			CordonConfigMap: map[string]bool{
				"ruleset1": false,
				"ruleset2": true,
			},
			RuleSetPriorityMap: map[string]int{
				"ruleset1": 10,
				"ruleset2": 5,
			},
		},
		quarantinedNodesMap,
	)

	if !isQuarantined {
		t.Errorf("Node should be quarantined due to rules")
	}

	if !quarantinedNodesMap["node1"] {
		t.Errorf("Expected quarantinedNodesMap[node1] to be true")
	}
}

// Test handleEvent with no rules triggered
func TestHandleEventNoRulesTriggered(t *testing.T) {
	ctx := context.Background()
	cfg := ReconcilerConfig{
		TomlConfig: config.TomlConfig{
			RuleSets: []config.RuleSet{},
		},
		K8sClient: &mockK8sClient{
			getNodesWithAnnotationFn: func(ctx context.Context, annotationKey string) ([]string, error) {
				return []string{}, nil
			},
			// Should not be called in this scenario
			taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelsMap map[string]string) error {
				t.Errorf("TaintAndCordonNodeAndSetAnnotations should not be called when no rules triggered.")
				return nil
			},
		},
	}

	r := NewReconciler(cfg)

	event := &platformconnectorprotos.HealthEvent{
		NodeName: "node1",
	}

	isQuarantined := r.handleEvent(ctx, event, []evaluator.RuleSetEvaluatorIface{}, rulesetsConfig{
		TaintConfigMap:     map[string]*config.Taint{},
		CordonConfigMap:    map[string]bool{},
		RuleSetPriorityMap: map[string]int{},
	}, map[string]bool{})

	if isQuarantined {
		t.Errorf("Expected node not to be quarantined when no rules triggered")
	}
}

// Test handleQuarantinedNode: scenario where unquarantine should occur
func TestHandleQuarantinedNodeUnquarantine(t *testing.T) {
	ctx := context.Background()
	annotationsMap := map[string]string{
		quarantineHealthEventAnnotationKey: `{
			"NodeName":"node1",
			"CheckName":"test",
			"Agent":"agent1",
			"Version":1,
			"ComponentClass":"class1",
			"EntitiesImpacted":[{"EntityType":"GPU","EntityValue":"gpu0"}]
		}`,
		quarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"key1","Value":"val1","Effect":"NoSchedule"}]`,
		quarantineHealthEventIsCordonedAnnotationKey:    "True",
	}

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			return annotationsMap, nil
		},
		unTaintAndUnCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
			// Check that correct taints and annotations are removed
			if nodeName != "node1" {
				t.Errorf("Expected node name node1, got %s", nodeName)
			}
			if len(taints) != 1 {
				t.Errorf("Expected 1 taint to remove")
			}
			if !isUncordon {
				t.Errorf("Expected node to be uncordoned")
			}
			expectedKeys := map[string]bool{
				quarantineHealthEventAnnotationKey:              true,
				quarantineHealthEventAppliedTaintsAnnotationKey: true,
				quarantineHealthEventIsCordonedAnnotationKey:    true,
			}
			for _, k := range annotationKeys {
				if !expectedKeys[k] {
					t.Errorf("Unexpected annotation key removed: %s", k)
				}
			}
			if len(labelMap) != 2 {
				t.Errorf("Expected uncordon labels to be applied on node %s", nodeName)
			}
			return nil
		},
	}

	r := NewReconciler(ReconcilerConfig{
		K8sClient: k8sMock,
	})

	quarantinedNodesMap := map[string]bool{"node1": true}
	event := &platformconnectorprotos.HealthEvent{
		NodeName:         "node1",
		Agent:            "agent1",
		CheckName:        "test",
		ComponentClass:   "class1",
		Version:          1,
		IsHealthy:        true, // triggers unquarantine comparison
		EntitiesImpacted: []*platformconnectorprotos.Entity{{EntityType: "GPU", EntityValue: "gpu0"}},
	}

	isQuarantined := r.handleQuarantinedNode(ctx, event, quarantinedNodesMap)
	if isQuarantined {
		t.Errorf("Expected node to be unquarantined")
	}
	if quarantinedNodesMap["node1"] {
		t.Errorf("quarantinedNodesMap[node1] should be false after unquarantine")
	}
}

// Test handleQuarantinedNode: scenario where node stays quarantined
func TestHandleQuarantinedNodeNoUnquarantine(t *testing.T) {
	ctx := context.Background()
	// The annotation event differs from incoming event - no unquarantine
	annotationsMap := map[string]string{
		quarantineHealthEventAnnotationKey: `{
			"NodeName":"node1",
			"CheckName":"test",
			"Agent":"agent1",
			"Version":1,
			"IsHealthy":true,
			"ComponentClass":"class1",
			"EntitiesImpacted":[{"EntityType":"GPU","EntityValue":"gpu0"}]
		}`,
	}

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			return annotationsMap, nil
		},
		unTaintAndUnCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
			t.Errorf("Should not be called if no unquarantine needed")
			return nil
		},
	}

	r := NewReconciler(ReconcilerConfig{
		K8sClient: k8sMock,
	})

	quarantinedNodesMap := map[string]bool{"node1": true}
	event := &platformconnectorprotos.HealthEvent{
		NodeName:  "node1",
		Agent:     "differentAgent",
		CheckName: "test",
		Version:   1,
		IsHealthy: true,
	}

	isQuarantined := r.handleQuarantinedNode(ctx, event, quarantinedNodesMap)
	if !isQuarantined {
		t.Errorf("Expected node to remain quarantined")
	}
	if !quarantinedNodesMap["node1"] {
		t.Errorf("quarantinedNodesMap[node1] should still be true")
	}
}

func TestCompareHealthEventWithAnnotationEventToCheckUnQuarantine(t *testing.T) {
	event := &platformconnectorprotos.HealthEvent{
		NodeName:       "node1",
		CheckName:      "checkA",
		Agent:          "agent",
		ComponentClass: "class1",
		Version:        1,
		IsHealthy:      true,
		EntitiesImpacted: []*platformconnectorprotos.Entity{
			{EntityType: "GPU", EntityValue: "gpu0"},
		},
	}

	annotationEventStr, _ := json.Marshal(event)

	// Same event should return true since IsHealthy matches and all fields match
	if !compareHealthEventWithAnnotationEventToCheckUnQuarantine(event, string(annotationEventStr)) {
		t.Errorf("Expected unquarantine check to succeed for identical events")
	}

	modEvent := &platformconnectorprotos.HealthEvent{
		NodeName:       "node1",
		CheckName:      "checkA",
		Agent:          "agent",
		ComponentClass: "class1",
		Version:        1,
		IsHealthy:      true,
		EntitiesImpacted: []*platformconnectorprotos.Entity{
			{EntityType: "GPU", EntityValue: "gpu0"},
		},
	}
	// Modify one field
	modEvent.Agent = "anotherAgent"
	modEventStr, _ := json.Marshal(&modEvent)
	if compareHealthEventWithAnnotationEventToCheckUnQuarantine(event, string(modEventStr)) {
		t.Errorf("Expected unquarantine check to fail when agent differs")
	}
}
