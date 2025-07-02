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

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/common"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/config"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/evaluator"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/store"
	platformconnectorprotos "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"

	"k8s.io/client-go/kubernetes"
)

var (
	quarantineHealthEventAnnotationKey              = common.QuarantineHealthEventAnnotationKey
	quarantineHealthEventAppliedTaintsAnnotationKey = common.QuarantineHealthEventAppliedTaintsAnnotationKey
	quarantineHealthEventIsCordonedAnnotationKey    = common.QuarantineHealthEventIsCordonedAnnotationKey
)

type mockK8sClient struct {
	getNodeAnnotationsFn     func(ctx context.Context, nodeName string) (map[string]string, error)
	getNodesWithAnnotationFn func(ctx context.Context, annotationKey string) ([]string, error)
	taintAndCordonNodeFn     func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error
	unTaintAndUnCordonNodeFn func(ctx context.Context, nodeName string, taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error
	getK8sClientFn           func() kubernetes.Interface
}

func (m *mockK8sClient) GetNodeAnnotations(ctx context.Context, nodeName string) (map[string]string, error) {
	return m.getNodeAnnotationsFn(ctx, nodeName)
}
func (m *mockK8sClient) GetNodesWithAnnotation(ctx context.Context, annotationKey string) ([]string, error) {
	return m.getNodesWithAnnotationFn(ctx, annotationKey)
}
func (m *mockK8sClient) TaintAndCordonNodeAndSetAnnotations(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelsMap map[string]string) error {
	return m.taintAndCordonNodeFn(ctx, nodeName, taints, isCordon, annotations, labelsMap)
}
func (m *mockK8sClient) UnTaintAndUnCordonNodeAndRemoveAnnotations(ctx context.Context, nodeName string, taints []config.Taint, isUnCordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
	return m.unTaintAndUnCordonNodeFn(ctx, nodeName, taints, isUnCordon, annotationKeys, labelsToRemove, labelMap)
}

func (m *mockK8sClient) GetK8sClient() kubernetes.Interface {
	return m.getK8sClientFn()
}

type mockEvaluator struct {
	name           string
	ok             bool
	evalErr        error
	priority       int
	version        string
	ruleEvalResult common.RuleEvaluationResult
}

func (m *mockEvaluator) GetName() string {
	return m.name
}

func (m *mockEvaluator) Evaluate(event *platformconnectorprotos.HealthEvent) (common.RuleEvaluationResult, error) {
	return m.ruleEvalResult, m.evalErr
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
				if _, ok := annotations[common.QuarantineHealthEventAnnotationKey]; !ok {
					t.Errorf("Expected quarantineHealthEvent annotation to be set")
				}
				if len(labelsMap) != 3 {
					t.Errorf("Expected cordon labels to be applied on node %s", nodeName)
				}
				return nil
			},
			getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
				return map[string]string{}, nil
			},
		},
	}

	r := NewReconciler(ctx, cfg, nil)
	r.SetLabelKeys(cfg.TomlConfig.LabelPrefix)

	ruleSetEvals := []evaluator.RuleSetEvaluatorIface{
		&mockEvaluator{name: "ruleset1", ok: true}, // applies taint key1=val1
		&mockEvaluator{name: "ruleset2", ok: true}, // applies taint key2=val2 and cordon
	}

	event := &platformconnectorprotos.HealthEvent{
		NodeName: "node1",
	}

	// Create a wrapper around the health event
	healthEventWithStatus := &store.HealthEventWithStatus{
		HealthEvent: event,
	}

	isQuarantined, ruleEvalResult := r.handleEvent(ctx, healthEventWithStatus, ruleSetEvals,
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
	)

	if isQuarantined == nil {
		t.Errorf("Expected isQuarantined to be non-nil")
	}

	if isQuarantined != nil && *isQuarantined == store.UnQuarantined {
		t.Errorf("Node should be quarantined due to rules")
	}

	// Check the rule evaluation results
	if ruleEvalResult == common.RuleEvaluationRetryAgainInFuture {
		t.Errorf("Unexpected rule kind result: %v", ruleEvalResult)
	}

	if !(*r.nodeInfo.GetQuarantinedNodesMap())["node1"] {
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
			getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
				return map[string]string{}, nil
			},
		},
	}

	r := NewReconciler(ctx, cfg, nil)

	// Initialize label keys
	r.SetLabelKeys(cfg.TomlConfig.LabelPrefix)

	event := &platformconnectorprotos.HealthEvent{
		NodeName: "node1",
	}

	// Create a wrapper around the health event
	healthEventWithStatus := &store.HealthEventWithStatus{
		HealthEvent: event,
	}

	isQuarantined, ruleEvalResult := r.handleEvent(ctx, healthEventWithStatus, []evaluator.RuleSetEvaluatorIface{}, rulesetsConfig{
		TaintConfigMap:     map[string]*config.Taint{},
		CordonConfigMap:    map[string]bool{},
		RuleSetPriorityMap: map[string]int{},
	})

	if isQuarantined == nil {
		t.Errorf("Expected isQuarantined to be non-nil")
	}

	if isQuarantined != nil && *isQuarantined == store.Quarantined {
		t.Errorf("Expected node not to be quarantined when no rules triggered")
	}

	if ruleEvalResult != common.RuleEvaluationNotApplicable {
		t.Errorf("Expected HealthEventRuleNotApplicable rule kind, got %v", ruleEvalResult)
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

	r := NewReconciler(ctx, ReconcilerConfig{
		K8sClient: k8sMock,
	}, nil)

	// Initialize label keys
	r.SetLabelKeys("k88s.nvidia.com/")

	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true)

	event := &platformconnectorprotos.HealthEvent{
		NodeName:         "node1",
		Agent:            "agent1",
		CheckName:        "test",
		ComponentClass:   "class1",
		Version:          1,
		IsHealthy:        true, // triggers unquarantine comparison
		EntitiesImpacted: []*platformconnectorprotos.Entity{{EntityType: "GPU", EntityValue: "gpu0"}},
	}

	isQuarantined := r.handleQuarantinedNode(ctx, event)
	if isQuarantined {
		t.Errorf("Expected node to be unquarantined")
	}
	if (*r.nodeInfo.GetQuarantinedNodesMap())["node1"] {
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

	r := NewReconciler(ctx, ReconcilerConfig{
		K8sClient: k8sMock,
	}, nil)

	// Initialize label keys
	r.SetLabelKeys("k88s.nvidia.com/")

	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true)

	event := &platformconnectorprotos.HealthEvent{
		NodeName:  "node1",
		Agent:     "differentAgent",
		CheckName: "test",
		Version:   1,
		IsHealthy: true,
	}

	isQuarantined := r.handleQuarantinedNode(ctx, event)
	if !isQuarantined {
		t.Errorf("Expected node to remain quarantined")
	}
	if !(*r.nodeInfo.GetQuarantinedNodesMap())["node1"] {
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

	modEventStr, _ := json.Marshal(modEvent)

	if compareHealthEventWithAnnotationEventToCheckUnQuarantine(event, string(modEventStr)) {
		t.Errorf("Expected unquarantine check to fail when agent differs")
	}
}

// TestHandleEventRuleEvaluationRetry tests handleEvent when an evaluator returns RuleEvaluationRetryAgainInFuture
func TestHandleEventRuleEvaluationRetry(t *testing.T) {
	ctx := context.Background()

	// Create base configuration
	cfg := ReconcilerConfig{
		TomlConfig: config.TomlConfig{
			LabelPrefix: "k88s.nvidia.com/",
			RuleSets: []config.RuleSet{
				{
					Name: "maxPercentageRule",
					Taint: config.Taint{
						Key:    "key1",
						Value:  "val1",
						Effect: "NoSchedule",
					},
					Cordon:   config.Cordon{ShouldCordon: true},
					Priority: 10,
				},
			},
		},
		K8sClient: &mockK8sClient{
			getNodesWithAnnotationFn: func(ctx context.Context, annotationKey string) ([]string, error) {
				return []string{}, nil
			},
			taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelsMap map[string]string) error {
				return nil
			},
			getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
				return map[string]string{}, nil
			},
		},
	}

	// Test Case 1: Evaluator returns RetryAgainInFuture (no error)
	t.Run("Evaluator returns RetryAgainInFuture (no error)", func(t *testing.T) {
		r := NewReconciler(ctx, cfg, nil)
		r.SetLabelKeys(cfg.TomlConfig.LabelPrefix)

		// Create evaluator that returns RuleEvaluationRetryAgainInFuture without error
		ruleSetEval := &mockEvaluator{
			name:           "RuleEvaluationRetryAgainInFuture",
			ok:             true, // ok=true likely means no error returned by mock
			ruleEvalResult: common.RuleEvaluationRetryAgainInFuture,
		}

		event := &platformconnectorprotos.HealthEvent{
			NodeName: "node1",
		}

		// Create a wrapper around the health event
		healthEventWithStatus := &store.HealthEventWithStatus{
			HealthEvent: event,
		}

		// Call handleEvent with the MaxPercentageRule evaluator
		status, ruleEvalResult := r.handleEvent(ctx, healthEventWithStatus, []evaluator.RuleSetEvaluatorIface{ruleSetEval},
			rulesetsConfig{
				TaintConfigMap: map[string]*config.Taint{
					"RuleEvaluationRetryAgainInFuture": &cfg.TomlConfig.RuleSets[0].Taint,
				},
				CordonConfigMap: map[string]bool{
					"RuleEvaluationRetryAgainInFuture": true,
				},
				RuleSetPriorityMap: map[string]int{
					"RuleEvaluationRetryAgainInFuture": 10,
				},
			},
		)

		// When RuleEvaluationRetryAgainInFuture is returned, the node should NOT be quarantined immediately
		if status != nil {
			t.Errorf("Expected status to be nil when rule evaluation is RetryAgainInFuture, got %v", *status)
		}

		// The ruleEvalResult should be RuleEvaluationRetryAgainInFuture
		if ruleEvalResult != common.RuleEvaluationRetryAgainInFuture {
			t.Errorf("Expected ruleEvalResult to be RuleEvaluationRetryAgainInFuture, got %v", ruleEvalResult)
		}

		// Node should NOT be in quarantined map
		if (*r.nodeInfo.GetQuarantinedNodesMap())["node1"] {
			t.Errorf("Expected node NOT to be in quarantined map when rule evaluation is RetryAgainInFuture")
		}
	})

	// Test Case 2: Evaluator returns RetryAgainInFuture (with error)
	t.Run("Evaluator returns RetryAgainInFuture (with error)", func(t *testing.T) {
		r := NewReconciler(ctx, cfg, nil)
		r.SetLabelKeys(cfg.TomlConfig.LabelPrefix)

		// Create evaluator that returns RuleEvaluationRetryAgainInFuture with an error
		ruleSetEval := &mockEvaluator{
			name:           "RuleEvaluationRetryAgainInFuture",
			ok:             false, // ok=false likely means an error is returned by mock
			ruleEvalResult: common.RuleEvaluationRetryAgainInFuture,
		}

		event := &platformconnectorprotos.HealthEvent{
			NodeName: "node1",
		}

		// Create a wrapper around the health event
		healthEventWithStatus := &store.HealthEventWithStatus{
			HealthEvent: event,
		}

		// Call handleEvent with the MaxPercentageRule evaluator
		status, ruleEvalResult := r.handleEvent(ctx, healthEventWithStatus, []evaluator.RuleSetEvaluatorIface{ruleSetEval},
			rulesetsConfig{
				TaintConfigMap: map[string]*config.Taint{
					"RuleEvaluationRetryAgainInFuture": &cfg.TomlConfig.RuleSets[0].Taint,
				},
				CordonConfigMap: map[string]bool{
					"RuleEvaluationRetryAgainInFuture": true,
				},
				RuleSetPriorityMap: map[string]int{
					"RuleEvaluationRetryAgainInFuture": 10,
				},
			},
		)

		// When RuleEvaluationRetryAgainInFuture is returned (even with error), the node should NOT be quarantined immediately
		if status != nil {
			t.Errorf("Expected status to be nil when rule evaluation is RetryAgainInFuture (with error), got %v", *status)
		}

		// The ruleEvalResult should still be RuleEvaluationRetryAgainInFuture
		if ruleEvalResult != common.RuleEvaluationRetryAgainInFuture {
			t.Errorf("Expected ruleEvalResult to be RuleEvaluationRetryAgainInFuture (with error), got %v", ruleEvalResult)
		}

		// Node should NOT be in quarantined map
		if (*r.nodeInfo.GetQuarantinedNodesMap())["node1"] {
			t.Errorf("Expected node NOT to be in quarantined map when rule evaluation is RetryAgainInFuture (with error)")
		}
	})
}

func TestHandleEventNodeAlreadyCordonedManually(t *testing.T) {
	ctx := context.Background()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k88s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name: "ruleset-1",
				Taint: config.Taint{
					Key:    "key1",
					Value:  "val1",
					Effect: "NoSchedule",
				},
				Cordon:   config.Cordon{ShouldCordon: true},
				Priority: 1,
			},
		},
	}

	// Track if the taint and annotation call was invoked
	taintsSeen := []config.Taint{}
	annotationsSeen := map[string]string{}
	taintCalled := false

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			// node is cordoned manually, no FQM annotation yet
			return map[string]string{}, nil
		},
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string,
			taints []config.Taint, isCordon bool,
			annotations map[string]string, labelsMap map[string]string) error {

			taintCalled = true
			taintsSeen = append(taintsSeen, taints...)

			for k, v := range annotations {
				annotationsSeen[k] = v
			}
			return nil
		},
	}

	cfg := ReconcilerConfig{
		TomlConfig: tomlConfig,
		K8sClient:  k8sMock,
	}

	r := NewReconciler(ctx, cfg, nil)
	r.SetLabelKeys(cfg.TomlConfig.LabelPrefix)

	// Simulate that the node has been cordoned manually (unschedulable) but NOT by FQM
	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true)

	// Prepare the evaluator which will return success so taint should be applied
	ruleSetEvals := []evaluator.RuleSetEvaluatorIface{
		&mockEvaluator{name: "ruleset-1", ruleEvalResult: common.RuleEvaluationSuccess},
	}

	event := &platformconnectorprotos.HealthEvent{NodeName: "node1"}
	healthEventWithStatus := &store.HealthEventWithStatus{HealthEvent: event}

	status, _ := r.handleEvent(ctx, healthEventWithStatus, ruleSetEvals,
		rulesetsConfig{
			TaintConfigMap: map[string]*config.Taint{
				"ruleset-1": &tomlConfig.RuleSets[0].Taint,
			},
			CordonConfigMap: map[string]bool{
				"ruleset-1": true,
			},
			RuleSetPriorityMap: map[string]int{
				"ruleset-1": 1,
			},
		},
	)

	// The reconciler should attempt to taint & annotate the node even though it was already cordoned manually
	if !taintCalled {
		t.Errorf("Expected TaintAndCordonNodeAndSetAnnotations to be called for already cordoned node")
	}

	if status == nil {
		t.Fatalf("Expected non-nil status returned from handleEvent")
	}

	if *status != store.AlreadyQuarantined {
		t.Errorf("Expected status to be AlreadyQuarantined, got %v", *status)
	}

	if len(taintsSeen) == 0 {
		t.Fatalf("expected at least one taint, got none")
	}
	if taintsSeen[0] != tomlConfig.RuleSets[0].Taint {
		t.Errorf("Unexpected taint values: %+v", taintsSeen[0])
	}

	if _, ok := annotationsSeen[common.QuarantineHealthEventAnnotationKey]; !ok {
		t.Errorf("expected %s annotation, but it wasn't passed to the client",
			common.QuarantineHealthEventAnnotationKey)
	}
}

// TestHandleEventNodeAlreadyQuarantinedByFQMStillQuarantined verifies that when a node is already
// quarantined by FQM (i.e. has the quarantine annotation) and receives another *unhealthy* event,
// the reconciler skips further processing and keeps the node quarantined.
func TestHandleEventNodeAlreadyQuarantinedByFQMStillQuarantined(t *testing.T) {
	ctx := context.Background()

	// Build an annotation payload representing the original quarantining event
	originalEvent := &platformconnectorprotos.HealthEvent{
		NodeName:  "node1",
		Agent:     "agent1",
		CheckName: "checkA",
		Version:   1,
		// The original event that quarantined the node was unhealthy
		IsHealthy: false,
	}
	annotationPayload, _ := json.Marshal(originalEvent)

	annotationMap := map[string]string{
		quarantineHealthEventAnnotationKey: string(annotationPayload),
	}

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			return annotationMap, nil
		},
		// These functions should NOT be invoked because reconciler should early-return.
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error {
			t.Fatalf("TaintAndCordonNodeAndSetAnnotations should not be called for already FQM-quarantined node (still unhealthy)")
			return nil
		},
		unTaintAndUnCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
			t.Fatalf("UnTaintAndUnCordonNodeAndRemoveAnnotations should not be called when node remains quarantined")
			return nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)
	// Mark node as cordoned/quarantined in the cache to satisfy nodeAlreadyCordoned check
	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true)

	// Initialize label keys so that handleQuarantinedNode may construct labels correctly if needed.
	r.SetLabelKeys("k88s.nvidia.com/")

	// Incoming event is still unhealthy, hence node should stay quarantined
	incomingEvent := &platformconnectorprotos.HealthEvent{
		NodeName:  "node1",
		Agent:     "agent1",
		CheckName: "checkA",
		Version:   1,
		IsHealthy: false,
	}

	healthEventWithStatus := &store.HealthEventWithStatus{HealthEvent: incomingEvent}

	status, _ := r.handleEvent(ctx, healthEventWithStatus, nil, rulesetsConfig{})

	if status == nil {
		t.Fatalf("Expected non-nil status when node already quarantined by FQM")
	}
	if *status != store.AlreadyQuarantined {
		t.Errorf("Expected status AlreadyQuarantined, got %v", *status)
	}

	// The cache should still indicate the node is quarantined
	if !(*r.nodeInfo.GetQuarantinedNodesMap())["node1"] {
		t.Errorf("Expected node to remain quarantined in cache")
	}
}

// TestHandleEventNodeAlreadyQuarantinedByFQMUnquarantine verifies that when a node is already
// quarantined by FQM but receives the corresponding *healthy* event, the reconciler un-quarantines
// it and updates the status appropriately.
func TestHandleEventNodeAlreadyQuarantinedByFQMUnquarantine(t *testing.T) {
	ctx := context.Background()

	// The annotation reflects the original unhealthy event that caused quarantine
	originalEvent := &platformconnectorprotos.HealthEvent{
		NodeName:       "node1",
		Agent:          "agent1",
		CheckName:      "checkA",
		ComponentClass: "class1",
		Version:        1,
		IsHealthy:      false,
	}
	annotationPayload, _ := json.Marshal(originalEvent)

	annotationMap := map[string]string{
		quarantineHealthEventAnnotationKey:              string(annotationPayload),
		quarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"key1","Value":"val1","Effect":"NoSchedule"}]`,
		quarantineHealthEventIsCordonedAnnotationKey:    "True",
	}

	unquarantineCalled := false

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			return annotationMap, nil
		},
		unTaintAndUnCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
			unquarantineCalled = true
			if !isUncordon {
				t.Errorf("Expected isUncordon to be true when un-quarantining the node")
			}
			return nil
		},
		// No new tainting expected in this path
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error {
			t.Fatalf("TaintAndCordonNodeAndSetAnnotations should not be called when node is being unquarantined")
			return nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)
	// Mark node as currently quarantined
	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true)
	r.SetLabelKeys("k88s.nvidia.com/")

	// Incoming *healthy* event that matches annotation ‑- should trigger un-quarantine
	incomingEvent := &platformconnectorprotos.HealthEvent{
		NodeName:       "node1",
		Agent:          "agent1",
		CheckName:      "checkA",
		ComponentClass: "class1",
		Version:        1,
		IsHealthy:      true,
		EntitiesImpacted: []*platformconnectorprotos.Entity{{
			EntityType:  "GPU",
			EntityValue: "gpu0",
		}},
	}

	healthEventWithStatus := &store.HealthEventWithStatus{HealthEvent: incomingEvent}

	status, _ := r.handleEvent(ctx, healthEventWithStatus, nil, rulesetsConfig{})

	if status == nil {
		t.Fatalf("Expected non-nil status when node already quarantined by FQM")
	}
	if *status != store.UnQuarantined {
		t.Errorf("Expected status UnQuarantined after healthy event, got %v", *status)
	}

	if !unquarantineCalled {
		t.Errorf("Expected UnTaintAndUnCordonNodeAndRemoveAnnotations to be invoked for healthy event")
	}

	// The cache must reflect that the node is no longer quarantined
	if (*r.nodeInfo.GetQuarantinedNodesMap())["node1"] {
		t.Errorf("Expected node to be removed from quarantined cache after unquarantine")
	}
}
