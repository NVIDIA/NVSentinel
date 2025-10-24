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

package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/evaluator"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/informer"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/statemanager"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
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
	updateNodeAnnotationsFn  func(ctx context.Context, nodeName string, annotations map[string]string) error
	getK8sClientFn           func() kubernetes.Interface
	ensureConfigMapFn        func(ctx context.Context, name, namespace string, initialStatus string) error
	readCBStateFn            func(ctx context.Context, name, namespace string) (string, error)
	writeCBStateFn           func(ctx context.Context, name, namespace, status string) error
	getTotalGpuNodesFn       func(ctx context.Context) (int, error)
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

func (m *mockK8sClient) UpdateNodeAnnotations(ctx context.Context, nodeName string, annotations map[string]string) error {
	return m.updateNodeAnnotationsFn(ctx, nodeName, annotations)
}

func (m *mockK8sClient) GetK8sClient() kubernetes.Interface {
	return m.getK8sClientFn()
}

func (m *mockK8sClient) EnsureCircuitBreakerConfigMap(ctx context.Context, name, namespace string, initialStatus string) error {
	if m.ensureConfigMapFn != nil {
		return m.ensureConfigMapFn(ctx, name, namespace, initialStatus)
	}
	return nil
}

func (m *mockK8sClient) ReadCircuitBreakerState(ctx context.Context, name, namespace string) (string, error) {
	if m.readCBStateFn != nil {
		return m.readCBStateFn(ctx, name, namespace)
	}
	return "", nil
}

func (m *mockK8sClient) WriteCircuitBreakerState(ctx context.Context, name, namespace, status string) error {
	if m.writeCBStateFn != nil {
		return m.writeCBStateFn(ctx, name, namespace, status)
	}
	return nil
}

func (m *mockK8sClient) GetTotalGpuNodes(ctx context.Context) (int, error) {
	if m.getTotalGpuNodesFn != nil {
		return m.getTotalGpuNodesFn(ctx)
	}
	return 10, nil // Default value for tests
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

func (m *mockEvaluator) Evaluate(event *protos.HealthEvent) (common.RuleEvaluationResult, error) {
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

	circuitBreakerConfig := CircuitBreakerConfig{
		Namespace:  "nvsentinel",
		Name:       "fault-quarantine-circuit-breaker",
		Percentage: 50,
		Duration:   5 * time.Minute,
	}

	cfg := ReconcilerConfig{
		TomlConfig:     tomlConfig,
		CircuitBreaker: circuitBreakerConfig,
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
				if len(labelsMap) != 4 {
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

	event := &protos.HealthEvent{
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

	quarantinedNodes := r.nodeInfo.GetQuarantinedNodesCopy()
	if !quarantinedNodes["node1"] {
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

	event := &protos.HealthEvent{
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

	if isQuarantined != nil {
		t.Errorf("Expected isQuarantined to be nil")
	}

	if ruleEvalResult != common.RuleEvaluationNotApplicable {
		t.Errorf("Expected HealthEventRuleNotApplicable rule kind, got %v", ruleEvalResult)
	}
}

// Test handleQuarantinedNode: scenario where unquarantine should occur
func TestHandleQuarantinedNodeUnquarantine(t *testing.T) {
	ctx := context.Background()
	annotationsMap := map[string]string{
		quarantineHealthEventAnnotationKey: `[{
			"NodeName":"node1",
			"CheckName":"GpuNvLinkWatch",
			"Agent":"agent1",
			"Version":1,
			"ComponentClass":"GPU",
			"EntitiesImpacted":[{"EntityType":"GPU","EntityValue":"0"}]
		}]`,
		quarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"key1","Value":"val1","Effect":"NoSchedule"}]`,
		quarantineHealthEventIsCordonedAnnotationKey:    "True",
	}

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			return annotationsMap, nil
		},
		updateNodeAnnotationsFn: func(ctx context.Context, nodeName string, annotations map[string]string) error {
			// Update the annotations map for subsequent reads
			for k, v := range annotations {
				annotationsMap[k] = v
			}
			return nil
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

	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true, true)

	event := &protos.HealthEvent{
		NodeName:         "node1",
		Agent:            "agent1",
		CheckName:        "GpuNvLinkWatch", // Must match the annotation
		ComponentClass:   "GPU",            // Must match the annotation
		Version:          1,
		IsHealthy:        true,                                                                     // triggers unquarantine comparison
		EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}}, // Must match annotation
	}

	isQuarantined := r.handleQuarantinedNode(ctx, event)
	if isQuarantined {
		t.Errorf("Expected node to be unquarantined")
	}
	quarantinedNodes := r.nodeInfo.GetQuarantinedNodesCopy()
	if quarantinedNodes["node1"] {
		t.Errorf("quarantinedNodesMap[node1] should be false after unquarantine")
	}
}

// Test handleQuarantinedNode: scenario where node stays quarantined
func TestHandleQuarantinedNodeNoUnquarantine(t *testing.T) {
	ctx := context.Background()
	// The annotation event differs from incoming event - no unquarantine
	annotationsMap := map[string]string{
		quarantineHealthEventAnnotationKey: `[{
			"NodeName":"node1",
			"CheckName":"GpuNvLinkWatch",
			"Agent":"agent1",
			"Version":1,
			"IsHealthy":true,
			"ComponentClass":"GPU",
			"EntitiesImpacted":[{"EntityType":"GPU","EntityValue":"0"}]
		}]`,
	}

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			return annotationsMap, nil
		},
		updateNodeAnnotationsFn: func(ctx context.Context, nodeName string, annotations map[string]string) error {
			// Update the annotations map for subsequent reads
			for k, v := range annotations {
				annotationsMap[k] = v
			}
			return nil
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

	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true, false)

	event := &protos.HealthEvent{
		NodeName:         "node1",
		Agent:            "gpu-health-monitor", // Different agent should not match
		CheckName:        "GpuNvLinkWatch",
		Version:          1,
		IsHealthy:        true,
		EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
	}

	isQuarantined := r.handleQuarantinedNode(ctx, event)
	if !isQuarantined {
		t.Errorf("Expected node to remain quarantined")
	}
	quarantinedNodes := r.nodeInfo.GetQuarantinedNodesCopy()
	if !quarantinedNodes["node1"] {
		t.Errorf("quarantinedNodesMap[node1] should still be true")
	}
}

// Test: Node is first uncordoned (manually), then a health event is sent, and annotation should be removed.
func TestHandleEvent_ManualUncordonThenHealthEvent(t *testing.T) {
	ctx := context.Background()

	// Step 1: Node is cordoned and annotation is set (simulate FQM quarantine)
	originalEvent := &protos.HealthEvent{
		NodeName:       "node1",
		Agent:          "agent1",
		CheckName:      "checkA",
		ComponentClass: "class1",
		Version:        1,
		IsHealthy:      false,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}
	annotationsMap := map[string]string{
		quarantineHealthEventAnnotationKey:              func() string { b, _ := json.Marshal(originalEvent); return string(b) }(),
		quarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"key1","Value":"val1","Effect":"NoSchedule"}]`,
		quarantineHealthEventIsCordonedAnnotationKey:    "True",
	}

	// Step 2: Node is manually uncordoned (simulate by setting Unschedulable=false and annotation still present)
	annotationRemoved := false
	var removedAnnotationKeys []string

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			// Simulate annotation still present after manual uncordon
			return annotationsMap, nil
		},
		updateNodeAnnotationsFn: func(ctx context.Context, nodeName string, annotations map[string]string) error {
			// Update the annotations map for subsequent reads
			for k, v := range annotations {
				annotationsMap[k] = v
			}
			return nil
		},
		unTaintAndUnCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
			annotationRemoved = true
			removedAnnotationKeys = annotationKeys
			return nil
		},
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error {
			// Should not be called in this scenario
			t.Errorf("TaintAndCordonNodeAndSetAnnotations should not be called in manual uncordon/annotation removal test")
			return nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)
	r.SetLabelKeys("k88s.nvidia.com/")

	// Step 3: Node is manually uncordoned (simulate by removing from cache)
	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", false, false)

	// Step 4: Health event is sent (simulate a healthy event matching the annotation)
	healthEvent := &protos.HealthEvent{
		NodeName:       "node1",
		Agent:          "agent1",
		CheckName:      "checkA",
		ComponentClass: "class1",
		Version:        1,
		IsHealthy:      true,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}
	healthEventWithStatus := &store.HealthEventWithStatus{HealthEvent: healthEvent}

	status, _ := r.handleEvent(ctx, healthEventWithStatus, nil, rulesetsConfig{})

	if status == nil {
		t.Fatalf("Expected non-nil status when node is manually uncordoned and annotation should be removed")
	}
	if *status != store.UnQuarantined {
		t.Errorf("Expected status UnQuarantined after manual uncordon and annotation removal, got %v", *status)
	}
	if !annotationRemoved {
		t.Errorf("Expected UnTaintAndUnCordonNodeAndRemoveAnnotations to be called to remove annotation")
	}
	expectedKeys := map[string]bool{
		quarantineHealthEventAnnotationKey:              true,
		quarantineHealthEventAppliedTaintsAnnotationKey: true,
		quarantineHealthEventIsCordonedAnnotationKey:    true,
	}
	for _, k := range removedAnnotationKeys {
		if !expectedKeys[k] {
			t.Errorf("Unexpected annotation key removed: %s", k)
		}
	}
	// The cache must reflect that the node is no longer quarantined
	quarantinedNodes := r.nodeInfo.GetQuarantinedNodesCopy()
	if quarantinedNodes["node1"] {
		t.Errorf("Expected node to be removed from quarantined cache after annotation removal")
	}
}

// TestMultiGPUPartialHealthyEvent tests that partial healthy events for multi-GPU nodes
// do not trigger uncordoning when some GPUs are still unhealthy
func TestMultiGPUPartialHealthyEvent(t *testing.T) {
	ctx := context.Background()

	// Annotation event shows 8 GPUs with errors (GPU 0-7)
	annotationEvent := &protos.HealthEvent{
		NodeName:       "node1",
		CheckName:      "GpuNvlinkWatch",
		Agent:          "gpu-health-monitor",
		ComponentClass: "GPU",
		Version:        1,
		IsHealthy:      false,
		Message:        "GPU NvLink link is currently down",
		ErrorCode:      []string{"DCGM_FR_NVLINK_DOWN"},
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
			{EntityType: "GPU", EntityValue: "1"},
			{EntityType: "GPU", EntityValue: "2"},
			{EntityType: "GPU", EntityValue: "3"},
			{EntityType: "GPU", EntityValue: "4"},
			{EntityType: "GPU", EntityValue: "5"},
			{EntityType: "GPU", EntityValue: "6"},
			{EntityType: "GPU", EntityValue: "7"},
		},
	}
	annotationEventStr, _ := json.Marshal(annotationEvent)

	// Setup mock K8s client with existing quarantine annotation
	annotationsMap := map[string]string{
		quarantineHealthEventAnnotationKey:              string(annotationEventStr),
		quarantineHealthEventIsCordonedAnnotationKey:    "True",
		quarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"test","Value":"test","Effect":"NoSchedule"}]`,
	}

	updateAnnotationsCalled := false
	uncordonCalled := false

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			return annotationsMap, nil
		},
		updateNodeAnnotationsFn: func(ctx context.Context, nodeName string, annotations map[string]string) error {
			updateAnnotationsCalled = true
			// Update the mock's annotations for subsequent calls
			for k, v := range annotations {
				annotationsMap[k] = v
			}
			return nil
		},
		unTaintAndUnCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
			uncordonCalled = true
			return nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)
	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true, true)

	// Test 1: Partial healthy event (only GPU 4 recovers)
	partialHealthyEvent := &store.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			NodeName:       "node1",
			CheckName:      "GpuNvlinkWatch",
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			Version:        1,
			IsHealthy:      true,
			Message:        "GPU NvLink watch reported no errors",
			ErrorCode:      []string{},
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "4"},
			},
		},
	}

	status, _ := r.handleEvent(ctx, partialHealthyEvent, nil, rulesetsConfig{})

	// Should update annotation but NOT uncordon
	if !updateAnnotationsCalled {
		t.Errorf("Expected annotation to be updated for partial recovery")
	}
	if uncordonCalled {
		t.Errorf("Expected node to remain cordoned for partial GPU recovery")
	}
	if status == nil || *status != store.AlreadyQuarantined {
		t.Errorf("Expected AlreadyQuarantined status for partial recovery, got %v", status)
	}

	// Reset flags
	updateAnnotationsCalled = false
	uncordonCalled = false

	// Test 2: Full healthy event (all GPUs recover)
	fullHealthyEvent := &store.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			NodeName:       "node1",
			CheckName:      "GpuNvlinkWatch",
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			Version:        1,
			IsHealthy:      true,
			Message:        "GPU NvLink watch reported no errors",
			ErrorCode:      []string{},
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "0"},
				{EntityType: "GPU", EntityValue: "1"},
				{EntityType: "GPU", EntityValue: "2"},
				{EntityType: "GPU", EntityValue: "3"},
				{EntityType: "GPU", EntityValue: "4"},
				{EntityType: "GPU", EntityValue: "5"},
				{EntityType: "GPU", EntityValue: "6"},
				{EntityType: "GPU", EntityValue: "7"},
			},
		},
	}

	// Update the mock to simulate GPU 4 already removed from annotation
	updatedAnnotation := &protos.HealthEvent{
		NodeName:       "node1",
		CheckName:      "GpuNvlinkWatch",
		Agent:          "gpu-health-monitor",
		ComponentClass: "GPU",
		Version:        1,
		IsHealthy:      false,
		Message:        "GPU NvLink link is currently down",
		ErrorCode:      []string{"DCGM_FR_NVLINK_DOWN"},
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
			{EntityType: "GPU", EntityValue: "1"},
			{EntityType: "GPU", EntityValue: "2"},
			{EntityType: "GPU", EntityValue: "3"},
			// GPU 4 removed
			{EntityType: "GPU", EntityValue: "5"},
			{EntityType: "GPU", EntityValue: "6"},
			{EntityType: "GPU", EntityValue: "7"},
		},
	}
	updatedAnnotationStr, _ := json.Marshal(updatedAnnotation)
	annotationsMap[quarantineHealthEventAnnotationKey] = string(updatedAnnotationStr)

	status, _ = r.handleEvent(ctx, fullHealthyEvent, nil, rulesetsConfig{})

	// Should trigger uncordon when all GPUs are healthy
	if !uncordonCalled {
		t.Errorf("Expected node to be uncordoned when all GPUs recover")
	}
	if status == nil || *status != store.UnQuarantined {
		t.Errorf("Expected UnQuarantined status for full recovery, got %v", status)
	}
}

// TestSkipRedundantCordoning tests that redundant cordoning is skipped when node is already cordoned
func TestSkipRedundantCordoning(t *testing.T) {
	ctx := context.Background()

	// Existing annotation for GpuNvlinkWatch
	existingEvent := &protos.HealthEvent{
		NodeName:       "node1",
		CheckName:      "GpuNvlinkWatch",
		Agent:          "gpu-health-monitor",
		ComponentClass: "GPU",
		Version:        1,
		IsHealthy:      false,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
		Message: "GPU 7's NvLink link 15 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics",
	}
	existingAnnotationStr, _ := json.Marshal(existingEvent)

	annotationsMap := map[string]string{
		quarantineHealthEventAnnotationKey:           string(existingAnnotationStr),
		quarantineHealthEventIsCordonedAnnotationKey: "True",
	}

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			return annotationsMap, nil
		},
		updateNodeAnnotationsFn: func(ctx context.Context, nodeName string, annotations map[string]string) error {
			// Update the annotations map for subsequent reads
			for k, v := range annotations {
				annotationsMap[k] = v
			}
			return nil
		},
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error {
			// Should not be called for redundant cordoning
			t.Errorf("TaintAndCordonNode should not be called when node is already cordoned for different check")
			return nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)

	// New event with different checkName
	newEvent := &store.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			NodeName:       "node1",
			CheckName:      "GpuMemWatch", // Different check
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			Version:        1,
			IsHealthy:      false,
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "1"},
			},
			Message: "GPU 123456 had uncorrectable memory errors and row remapping failed. Run a field diagnostic on the GPU",
		},
	}

	status, _ := r.handleEvent(ctx, newEvent, nil, rulesetsConfig{})

	if status == nil || *status != store.AlreadyQuarantined {
		t.Errorf("Expected AlreadyQuarantined status for redundant cordoning, got %v", status)
	}
}

// TestSkipDuplicateUnhealthyEntities tests that duplicate unhealthy events for already tracked entities are skipped
func TestSkipDuplicateUnhealthyEntities(t *testing.T) {
	ctx := context.Background()

	// Create new format annotation with GPU 0 and GPU 1 having errors
	existingMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	existingEvent := &protos.HealthEvent{
		NodeName:       "node1",
		CheckName:      "GpuNvlinkWatch",
		Agent:          "gpu-health-monitor",
		ComponentClass: "GPU",
		Version:        1,
		IsHealthy:      false,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
			{EntityType: "GPU", EntityValue: "1"},
		},
		Message: "GPU 0's NvLink link 15 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics",
	}
	existingMap.AddOrUpdateEvent(existingEvent)
	existingAnnotationStr, _ := json.Marshal(existingMap)

	annotationsMap := map[string]string{
		quarantineHealthEventAnnotationKey:           string(existingAnnotationStr),
		quarantineHealthEventIsCordonedAnnotationKey: "True",
	}

	updateCalled := false
	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			return annotationsMap, nil
		},
		updateNodeAnnotationsFn: func(ctx context.Context, nodeName string, annotations map[string]string) error {
			updateCalled = true
			for k, v := range annotations {
				annotationsMap[k] = v
			}
			return nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)
	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true, true)

	// New event with same entities (GPU 0) - should be skipped
	duplicateEvent := &store.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			NodeName:       "node1",
			CheckName:      "GpuNvlinkWatch",
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			Version:        1,
			IsHealthy:      false,
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "0"}, // Already tracked
			},
			Message: "GPU 0's NvLink link 15 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics",
		},
	}

	status, _ := r.handleEvent(ctx, duplicateEvent, nil, rulesetsConfig{})

	if !updateCalled {
		// Good - update should not be called for duplicate entities
	} else {
		t.Errorf("UpdateNodeAnnotations should not be called for duplicate entities")
	}

	if status == nil || *status != store.AlreadyQuarantined {
		t.Errorf("Expected AlreadyQuarantined status for duplicate entities, got %v", status)
	}

	// Now test with a mix of existing and new entities
	updateCalled = false
	mixedEvent := &store.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			NodeName:       "node1",
			CheckName:      "GpuNvlinkWatch",
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			Version:        1,
			IsHealthy:      false,
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "1"}, // Already tracked
				{EntityType: "GPU", EntityValue: "2"}, // New entity
			},
			Message: "GPU 0's NvLink link 15 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics",
		},
	}

	status, _ = r.handleEvent(ctx, mixedEvent, nil, rulesetsConfig{})

	if !updateCalled {
		t.Errorf("UpdateNodeAnnotations should be called when new entities are present")
	}

	if status == nil || *status != store.AlreadyQuarantined {
		t.Errorf("Expected AlreadyQuarantined status after updating with new entities, got %v", status)
	}
}

// TestHandleEventRuleEvaluationRetry tests handleEvent when an evaluator returns RuleEvaluationRetryAgainInFuture
// TestHandleHealthyEventWithoutQuarantineAnnotation tests that healthy events
// without existing quarantine annotations are skipped
func TestHandleHealthyEventWithoutQuarantineAnnotation(t *testing.T) {
	ctx := context.Background()

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			// No quarantine annotations exist
			return map[string]string{}, nil
		},
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error {
			t.Error("TaintAndCordonNode should not be called for healthy events without quarantine annotation")
			return nil
		},
	}

	mockEvaluator := &mockEvaluator{
		name:           "test-eval",
		ok:             true,
		ruleEvalResult: common.RuleEvaluationSuccess,
		priority:       1,
		version:        "v1",
	}

	ruleSetEvals := []evaluator.RuleSetEvaluatorIface{mockEvaluator}

	rulesetsConfig := rulesetsConfig{
		TaintConfigMap:     map[string]*config.Taint{"test-eval": {Key: "test", Value: "test", Effect: "NoSchedule"}},
		CordonConfigMap:    map[string]bool{"test-eval": true},
		RuleSetPriorityMap: map[string]int{"test-eval": 1},
	}

	r := NewReconciler(ctx, ReconcilerConfig{
		K8sClient: k8sMock,
	}, nil)

	// Healthy event
	event := &store.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			NodeName:  "node1",
			IsHealthy: true,
		},
	}

	status, ruleEval := r.handleEvent(ctx, event, ruleSetEvals, rulesetsConfig)

	// Status should be nil for healthy events without quarantine annotation
	if status != nil {
		t.Errorf("Expected nil status for healthy event without quarantine annotation, got %v", status)
	}

	if ruleEval != common.RuleEvaluationNotApplicable {
		t.Errorf("Expected RuleEvaluationNotApplicable, got %v", ruleEval)
	}
}

// TestHandleUnhealthyEventWithoutQuarantineAnnotation tests that unhealthy events
// without existing quarantine annotations are still processed
func TestHandleUnhealthyEventWithoutQuarantineAnnotation(t *testing.T) {
	ctx := context.Background()

	taintAndCordonCalled := false
	var addedLabels map[string]string
	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			// No quarantine annotations exist
			return map[string]string{}, nil
		},
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error {
			taintAndCordonCalled = true
			addedLabels = labelMap
			return nil
		},
	}

	mockEvaluator := &mockEvaluator{
		name:           "test-eval",
		ok:             true,
		ruleEvalResult: common.RuleEvaluationSuccess,
		priority:       1,
		version:        "v1",
	}

	ruleSetEvals := []evaluator.RuleSetEvaluatorIface{mockEvaluator}

	rulesetsConfig := rulesetsConfig{
		TaintConfigMap:     map[string]*config.Taint{"test-eval": {Key: "test", Value: "test", Effect: "NoSchedule"}},
		CordonConfigMap:    map[string]bool{"test-eval": true},
		RuleSetPriorityMap: map[string]int{"test-eval": 1},
	}

	circuitBreakerConfig := CircuitBreakerConfig{
		Namespace:  "nvsentinel",
		Name:       "fault-quarantine-circuit-breaker",
		Percentage: 50,
		Duration:   5 * time.Minute,
	}

	r := NewReconciler(ctx, ReconcilerConfig{
		K8sClient:      k8sMock,
		CircuitBreaker: circuitBreakerConfig,
	}, nil)

	// Initialize label keys
	r.SetLabelKeys("k88s.nvidia.com/")

	// Unhealthy event
	event := &store.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			NodeName:  "node1",
			IsHealthy: false,
		},
	}

	status, _ := r.handleEvent(ctx, event, ruleSetEvals, rulesetsConfig)

	// Status should be Quarantined for unhealthy events that trigger rules
	if status == nil || *status != store.Quarantined {
		t.Errorf("Expected Quarantined status for unhealthy event, got %v", status)
	}

	if !taintAndCordonCalled {
		t.Error("Expected TaintAndCordonNode to be called for unhealthy event")
	}
	normalizedTime := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	expectedLabels := map[string]string{
		cordonedByLabelKey:                   common.ServiceName,
		cordonedReasonLabelKey:               "test-eval",
		cordonedTimestampLabelKey:            normalizedTime,
		statemanager.NVSentinelStateLabelKey: string(statemanager.QuarantinedLabelValue),
	}
	if _, ok := addedLabels[cordonedTimestampLabelKey]; !ok {
		t.Errorf("Missing expected label %s", cordonedTimestampLabelKey)
	}
	addedLabels[cordonedTimestampLabelKey] = normalizedTime
	if !reflect.DeepEqual(addedLabels, expectedLabels) {
		t.Errorf("Unexpected set of labels added in TaintAndCordonNodeAndSetAnnotations: %v compared to %v", addedLabels, expectedLabels)
	}
}

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

		event := &protos.HealthEvent{
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
		quarantinedNodes := r.nodeInfo.GetQuarantinedNodesCopy()
		if quarantinedNodes["node1"] {
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

		event := &protos.HealthEvent{
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
		quarantinedNodes := r.nodeInfo.GetQuarantinedNodesCopy()
		if quarantinedNodes["node1"] {
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

	circuitBreakerConfig := CircuitBreakerConfig{
		Namespace:  "nvsentinel",
		Name:       "fault-quarantine-circuit-breaker",
		Percentage: 50,
		Duration:   5 * time.Minute,
	}

	cfg := ReconcilerConfig{
		TomlConfig:     tomlConfig,
		K8sClient:      k8sMock,
		CircuitBreaker: circuitBreakerConfig,
	}

	r := NewReconciler(ctx, cfg, nil)
	r.SetLabelKeys(cfg.TomlConfig.LabelPrefix)

	// Simulate that the node has been cordoned manually (unschedulable) but NOT by FQM
	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true, false)

	// Prepare the evaluator which will return success so taint should be applied
	ruleSetEvals := []evaluator.RuleSetEvaluatorIface{
		&mockEvaluator{name: "ruleset-1", ruleEvalResult: common.RuleEvaluationSuccess},
	}

	event := &protos.HealthEvent{NodeName: "node1"}
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

	if *status != store.Quarantined {
		t.Errorf("Expected status to be Quarantined, got %v", *status)
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
	originalEvent := &protos.HealthEvent{
		NodeName:  "node1",
		Agent:     "agent1",
		CheckName: "checkA",
		Version:   1,
		// The original event that quarantined the node was unhealthy
		IsHealthy: false,
	}

	annotationMap := map[string]string{
		quarantineHealthEventAnnotationKey: func() string { b, _ := json.Marshal(originalEvent); return string(b) }(),
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
		updateNodeAnnotationsFn: func(ctx context.Context, nodeName string, annotations map[string]string) error {
			for k, v := range annotations {
				annotationMap[k] = v
			}
			return nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)
	// Mark node as cordoned/quarantined in the cache to satisfy nodeAlreadyCordoned check
	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true, false)

	// Initialize label keys so that handleQuarantinedNode may construct labels correctly if needed.
	r.SetLabelKeys("k88s.nvidia.com/")

	// Incoming event is still unhealthy, hence node should stay quarantined
	incomingEvent := &protos.HealthEvent{
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
	quarantinedNodes := r.nodeInfo.GetQuarantinedNodesCopy()
	if !quarantinedNodes["node1"] {
		t.Errorf("Expected node to remain quarantined in cache")
	}
}

// TestHandleEventNodeAlreadyQuarantinedByFQMUnquarantine verifies that when a node is already
// quarantined by FQM but receives the corresponding *healthy* event, the reconciler un-quarantines
// it and updates the status appropriately.
func TestHandleEventNodeAlreadyQuarantinedByFQMUnquarantine(t *testing.T) {
	ctx := context.Background()

	// The annotation reflects the original unhealthy event that caused quarantine in new format
	originalMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	originalEvent := &protos.HealthEvent{
		NodeName:       "node1",
		Agent:          "agent1",
		CheckName:      "checkA",
		ComponentClass: "class1",
		Version:        1,
		IsHealthy:      false,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}
	originalMap.AddOrUpdateEvent(originalEvent)

	annotationMap := map[string]string{
		quarantineHealthEventAnnotationKey:              func() string { b, _ := json.Marshal(originalMap); return string(b) }(),
		quarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"key1","Value":"val1","Effect":"NoSchedule"}]`,
		quarantineHealthEventIsCordonedAnnotationKey:    "True",
	}

	unquarantineCalled := false
	var removedLabels []string
	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			return annotationMap, nil
		},
		unTaintAndUnCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
			unquarantineCalled = true
			if !isUncordon {
				t.Errorf("Expected isUncordon to be true when un-quarantining the node")
			}
			removedLabels = labelsToRemove
			return nil
		},
		// No new tainting expected in this path
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error {
			t.Fatalf("TaintAndCordonNodeAndSetAnnotations should not be called when node is being unquarantined")
			return nil
		},
		updateNodeAnnotationsFn: func(ctx context.Context, nodeName string, annotations map[string]string) error {
			for k, v := range annotations {
				annotationMap[k] = v
			}
			return nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)
	// Mark node as currently quarantined
	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true, false)
	r.SetLabelKeys("k88s.nvidia.com/")

	// Incoming *healthy* event that matches annotation ‑- should trigger un-quarantine
	incomingEvent := &protos.HealthEvent{
		NodeName:       "node1",
		Agent:          "agent1",
		CheckName:      "checkA",
		ComponentClass: "class1",
		Version:        1,
		IsHealthy:      true,
		EntitiesImpacted: []*protos.Entity{{
			EntityType:  "GPU",
			EntityValue: "0",
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

	expectedRemovedLabels := []string{
		cordonedByLabelKey,
		cordonedReasonLabelKey,
		cordonedTimestampLabelKey,
		statemanager.NVSentinelStateLabelKey,
	}
	if !reflect.DeepEqual(removedLabels, expectedRemovedLabels) {
		t.Errorf("Unexpected set of labels removed from UnTaintAndUnCordonNodeAndRemoveAnnotations: %v", removedLabels)
	}
	// The cache must reflect that the node is no longer quarantined
	quarantinedNodes := r.nodeInfo.GetQuarantinedNodesCopy()
	if quarantinedNodes["node1"] {
		t.Errorf("Expected node to be removed from quarantined cache after unquarantine")
	}
}

// Test cache consistency during quarantine and unquarantine operations
func TestCacheConsistencyDuringQuarantineUnquarantine(t *testing.T) {
	ctx := context.Background()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
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

	// Track API calls
	apiCallCount := 0

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			apiCallCount++
			// Return empty annotations initially
			return map[string]string{}, nil
		},
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string,
			taints []config.Taint, isCordon bool,
			annotations map[string]string, labelsMap map[string]string) error {
			return nil
		},
		unTaintAndUnCordonNodeFn: func(ctx context.Context, nodeName string,
			taints []config.Taint, isUncordon bool,
			annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
			return nil
		},
		getK8sClientFn: func() kubernetes.Interface {
			// Return a fake client for buildNodeAnnotationsCache
			return fake.NewSimpleClientset()
		},
	}

	circuitBreakerConfig := CircuitBreakerConfig{
		Namespace:  "nvsentinel",
		Name:       "fault-quarantine-circuit-breaker",
		Percentage: 50,
		Duration:   5 * time.Minute,
	}

	cfg := ReconcilerConfig{
		TomlConfig:     tomlConfig,
		K8sClient:      k8sMock,
		CircuitBreaker: circuitBreakerConfig,
	}

	// Create work signal for proper nodeInfo initialization
	workSignal := make(chan struct{}, 10)
	r := NewReconciler(ctx, cfg, workSignal)
	r.SetLabelKeys(cfg.TomlConfig.LabelPrefix)

	// Build initial cache
	err := r.buildNodeAnnotationsCache(ctx)
	if err != nil {
		t.Fatalf("Failed to build initial cache: %v", err)
	}

	// Pre-populate cache for node1 with empty annotations to avoid API call
	r.nodeAnnotationsCache.Store("node1", map[string]string{})

	// Test 1: First event - should use cache (empty annotations)
	event1 := &protos.HealthEvent{NodeName: "node1"}
	healthEventWithStatus1 := &store.HealthEventWithStatus{HealthEvent: event1}

	// Mock evaluator that returns success
	ruleSetEvals := []evaluator.RuleSetEvaluatorIface{
		&mockEvaluator{name: "ruleset-1", ruleEvalResult: common.RuleEvaluationSuccess},
	}

	apiCallCount = 0 // Reset counter after initial cache build
	status1, _ := r.handleEvent(ctx, healthEventWithStatus1, ruleSetEvals, rulesetsConfig{
		TaintConfigMap:  map[string]*config.Taint{"ruleset-1": &tomlConfig.RuleSets[0].Taint},
		CordonConfigMap: map[string]bool{"ruleset-1": true},
	})

	if apiCallCount != 0 {
		t.Errorf("Expected 0 API calls (should use cache), got %d", apiCallCount)
	}

	if status1 == nil || *status1 != store.Quarantined {
		t.Errorf("Expected Quarantined status, got %v", status1)
	}

	// Verify cache was updated with quarantine annotations
	cached, ok := r.nodeAnnotationsCache.Load("node1")
	if !ok {
		t.Fatal("Expected node1 to be in cache after quarantine")
	}

	cachedAnnotations := cached.(map[string]string)
	if _, exists := cachedAnnotations[common.QuarantineHealthEventAnnotationKey]; !exists {
		t.Error("Expected quarantine annotation in cache after quarantine operation")
	}

	// Test 2: Second event on same node - should use updated cache
	event2 := &protos.HealthEvent{
		NodeName:       "node1",
		IsHealthy:      true,
		Agent:          event1.Agent,
		CheckName:      event1.CheckName,
		ComponentClass: event1.ComponentClass,
		Version:        event1.Version,
	}
	healthEventWithStatus2 := &store.HealthEventWithStatus{HealthEvent: event2}

	// Update mock to return quarantine annotations if API is called (shouldn't be)
	k8sMock.getNodeAnnotationsFn = func(ctx context.Context, nodeName string) (map[string]string, error) {
		apiCallCount++
		t.Error("API should not be called - cache should be used")
		return cachedAnnotations, nil
	}

	status2, _ := r.handleEvent(ctx, healthEventWithStatus2, nil, rulesetsConfig{})

	if apiCallCount != 0 {
		t.Errorf("Expected 0 API calls for second event (should use cache), got %d", apiCallCount)
	}

	if status2 == nil || *status2 != store.UnQuarantined {
		t.Errorf("Expected UnQuarantined status, got %v", status2)
	}

	// Verify cache was updated to remove quarantine annotations
	cached2, ok2 := r.nodeAnnotationsCache.Load("node1")
	if !ok2 {
		t.Fatal("Expected node1 to still be in cache after unquarantine")
	}

	cachedAnnotations2 := cached2.(map[string]string)
	if len(cachedAnnotations2) != 0 {
		t.Errorf("Expected empty annotations in cache after unquarantine, got %v", cachedAnnotations2)
	}
}

// Test cache fallback behavior when node not in cache
func TestCacheFallbackForUncachedNode(t *testing.T) {
	ctx := context.Background()

	apiCallCount := 0
	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			apiCallCount++
			return map[string]string{
				common.QuarantineHealthEventAnnotationKey: "existing-event",
			}, nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)

	// Don't build initial cache - simulate a new node
	annotations, err := r.getNodeQuarantineAnnotations(ctx, "new-node")

	if err != nil {
		t.Fatalf("Expected successful API fallback, got error: %v", err)
	}

	if apiCallCount != 1 {
		t.Errorf("Expected 1 API call for uncached node, got %d", apiCallCount)
	}

	if annotations[common.QuarantineHealthEventAnnotationKey] != "existing-event" {
		t.Errorf("Expected quarantine annotation from API, got %v", annotations)
	}

	// Verify node was added to cache
	cached, ok := r.nodeAnnotationsCache.Load("new-node")
	if !ok {
		t.Fatal("Expected new-node to be cached after API call")
	}

	cachedAnnotations := cached.(map[string]string)
	if cachedAnnotations[common.QuarantineHealthEventAnnotationKey] != "existing-event" {
		t.Error("Expected API result to be cached")
	}

	// Second call should use cache
	apiCallCount = 0
	annotations2, err2 := r.getNodeQuarantineAnnotations(ctx, "new-node")

	if err2 != nil {
		t.Fatalf("Expected successful cache hit, got error: %v", err2)
	}

	if apiCallCount != 0 {
		t.Errorf("Expected 0 API calls (should use cache), got %d", apiCallCount)
	}

	if annotations2[common.QuarantineHealthEventAnnotationKey] != "existing-event" {
		t.Errorf("Expected cached annotation, got %v", annotations2)
	}
}

// Test manual uncordon scenario with cache
func TestManualUncordonWithCache(t *testing.T) {
	ctx := context.Background()

	originalEvent := &protos.HealthEvent{
		NodeName:       "node1",
		Agent:          "agent1",
		CheckName:      "checkA",
		ComponentClass: "class1",
		Version:        1,
		IsHealthy:      false,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}
	// Convert to array format as healthEventsAnnotationMap expects an array
	annotationPayload, _ := json.Marshal([]*protos.HealthEvent{originalEvent})

	apiCallCount := 0
	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			apiCallCount++
			// Should not be called if cache is working
			t.Error("API should not be called when cache has the data")
			return map[string]string{
				quarantineHealthEventAnnotationKey: string(annotationPayload),
			}, nil
		},
		unTaintAndUnCordonNodeFn: func(ctx context.Context, nodeName string,
			taints []config.Taint, isUncordon bool,
			annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
			return nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)
	r.SetLabelKeys("k8s.nvidia.com/")

	// Pre-populate cache with quarantine annotations (simulating node was quarantined)
	r.nodeAnnotationsCache.Store("node1", map[string]string{
		quarantineHealthEventAnnotationKey:              string(annotationPayload),
		quarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"key1","Value":"val1","Effect":"NoSchedule"}]`,
		quarantineHealthEventIsCordonedAnnotationKey:    "True",
	})

	// Simulate manual uncordon by updating nodeInfo
	// (In reality, this would be done by the node informer)
	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", false, true)

	// Send healthy event that matches the quarantine
	healthyEvent := &protos.HealthEvent{
		NodeName:       "node1",
		Agent:          "agent1",
		CheckName:      "checkA",
		ComponentClass: "class1",
		Version:        1,
		IsHealthy:      true,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}
	healthEventWithStatus := &store.HealthEventWithStatus{HealthEvent: healthyEvent}

	apiCallCount = 0 // Reset counter
	status, _ := r.handleEvent(ctx, healthEventWithStatus, nil, rulesetsConfig{})

	if apiCallCount != 0 {
		t.Errorf("Expected 0 API calls (should use cache), got %d", apiCallCount)
	}

	if status == nil || *status != store.UnQuarantined {
		t.Errorf("Expected UnQuarantined status after manual uncordon + healthy event, got %v", status)
	}

	// Verify cache was updated to remove annotations
	cached, ok := r.nodeAnnotationsCache.Load("node1")
	if !ok {
		t.Fatal("Expected node1 to still be in cache")
	}

	cachedAnnotations := cached.(map[string]string)
	if len(cachedAnnotations) != 0 {
		t.Errorf("Expected empty annotations in cache after cleanup, got %v", cachedAnnotations)
	}
}

// Test concurrent cache access
func TestConcurrentCacheAccess(t *testing.T) {
	ctx := context.Background()

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			// Simulate some delay
			time.Sleep(10 * time.Millisecond)
			return map[string]string{
				common.QuarantineHealthEventAnnotationKey: nodeName + "-event",
			}, nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)

	// Pre-populate cache with some nodes
	for i := 0; i < 10; i++ {
		nodeName := fmt.Sprintf("node%d", i)
		r.nodeAnnotationsCache.Store(nodeName, map[string]string{
			common.QuarantineHealthEventAnnotationKey: nodeName + "-cached",
		})
	}

	var wg sync.WaitGroup
	errors := make(chan error, 100)

	// Concurrent readers
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(nodeNum int) {
			defer wg.Done()
			nodeName := fmt.Sprintf("node%d", nodeNum%10)
			annotations, err := r.getNodeQuarantineAnnotations(ctx, nodeName)
			if err != nil {
				errors <- fmt.Errorf("reader error for %s: %v", nodeName, err)
				return
			}
			// Check if we got the expected value (might be updated by concurrent writers)
			actual := annotations[common.QuarantineHealthEventAnnotationKey]
			// Accept either the original cached value or an updated value from concurrent writers
			if actual != nodeName+"-cached" && !strings.HasPrefix(actual, nodeName+"-updated-") {
				errors <- fmt.Errorf("expected %s-cached or %s-updated-*, got %s",
					nodeName, nodeName, actual)
			}
		}(i)
	}

	// Concurrent writers (updating cache)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(nodeNum int) {
			defer wg.Done()
			nodeName := fmt.Sprintf("node%d", nodeNum%10)
			newAnnotations := map[string]string{
				common.QuarantineHealthEventAnnotationKey: nodeName + "-updated-" + fmt.Sprintf("%d", nodeNum),
			}
			r.updateCacheWithQuarantineAnnotations(nodeName, newAnnotations)
		}(i)
	}

	// Concurrent deleters - only delete from specific nodes to avoid conflicts
	for i := 20; i < 25; i++ {
		wg.Add(1)
		go func(nodeNum int) {
			defer wg.Done()
			nodeName := fmt.Sprintf("node%d", nodeNum)
			// Pre-populate these nodes for deletion
			r.nodeAnnotationsCache.Store(nodeName, map[string]string{
				common.QuarantineHealthEventAnnotationKey: nodeName + "-to-delete",
			})
			// Then delete the annotation
			r.updateCacheWithUnquarantineAnnotations(nodeName,
				[]string{common.QuarantineHealthEventAnnotationKey})
		}(i)
	}

	wg.Wait()
	close(errors)

	// Check for any errors
	var errorCount int
	for err := range errors {
		t.Errorf("Concurrent access error: %v", err)
		errorCount++
	}

	if errorCount > 0 {
		t.Fatalf("Had %d errors during concurrent access", errorCount)
	}
}

// Test cache behavior when annotations change externally
func TestCacheUpdateFromNodeInformer(t *testing.T) {
	ctx := context.Background()

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			// Should not be called if cache is properly updated by informer
			t.Error("API should not be called when cache is updated by informer")
			return nil, nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)

	// Simulate node informer callback with new annotations
	newAnnotations := map[string]string{
		common.QuarantineHealthEventAnnotationKey:              "external-event",
		common.QuarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"external","Value":"taint"}]`,
	}
	r.handleNodeAnnotationChange("node1", newAnnotations)

	// Try to get annotations - should come from cache
	annotations, err := r.getNodeQuarantineAnnotations(ctx, "node1")
	if err != nil {
		t.Fatalf("Expected successful cache hit, got error: %v", err)
	}

	if annotations[common.QuarantineHealthEventAnnotationKey] != "external-event" {
		t.Errorf("Expected externally updated annotation, got %v", annotations)
	}

	// Simulate node deletion
	r.handleNodeAnnotationChange("node1", nil)

	// Verify node was removed from cache
	_, ok := r.nodeAnnotationsCache.Load("node1")
	if ok {
		t.Error("Expected node1 to be removed from cache after deletion")
	}
}

// Test buildNodeAnnotationsCache with various node states
func TestBuildNodeAnnotationsCacheWithVariousStates(t *testing.T) {
	ctx := context.Background()

	// Create fake k8s client with various nodes
	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Annotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey: "event1",
				"other-annotation":                        "ignored",
			},
		},
	}

	node2 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node2",
			Annotations: map[string]string{
				common.QuarantineHealthEventIsCordonedAnnotationKey: "True",
			},
		},
	}

	node3 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node3",
			Annotations: map[string]string{}, // No quarantine annotations
		},
	}

	node4 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node4",
			// No annotations at all
		},
	}

	fakeClient := fake.NewSimpleClientset(node1, node2, node3, node4)

	k8sMock := &mockK8sClient{
		getK8sClientFn: func() kubernetes.Interface {
			return fakeClient
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)

	err := r.buildNodeAnnotationsCache(ctx)
	if err != nil {
		t.Fatalf("Failed to build cache: %v", err)
	}

	// Verify all nodes are in cache
	tests := []struct {
		nodeName        string
		expectedAnns    map[string]string
		expectedInCache bool
	}{
		{
			nodeName: "node1",
			expectedAnns: map[string]string{
				common.QuarantineHealthEventAnnotationKey: "event1",
			},
			expectedInCache: true,
		},
		{
			nodeName: "node2",
			expectedAnns: map[string]string{
				common.QuarantineHealthEventIsCordonedAnnotationKey: "True",
			},
			expectedInCache: true,
		},
		{
			nodeName:        "node3",
			expectedAnns:    map[string]string{},
			expectedInCache: true,
		},
		{
			nodeName:        "node4",
			expectedAnns:    map[string]string{},
			expectedInCache: true,
		},
	}

	for _, tt := range tests {
		cached, ok := r.nodeAnnotationsCache.Load(tt.nodeName)
		if ok != tt.expectedInCache {
			t.Errorf("Node %s: expected in cache = %v, got %v", tt.nodeName, tt.expectedInCache, ok)
			continue
		}

		if !ok {
			continue
		}

		cachedAnns := cached.(map[string]string)
		if len(cachedAnns) != len(tt.expectedAnns) {
			t.Errorf("Node %s: expected %d annotations, got %d", tt.nodeName,
				len(tt.expectedAnns), len(cachedAnns))
		}

		for key, expectedVal := range tt.expectedAnns {
			if cachedAnns[key] != expectedVal {
				t.Errorf("Node %s: expected annotation %s=%s, got %s",
					tt.nodeName, key, expectedVal, cachedAnns[key])
			}
		}

		// Verify non-quarantine annotations are not cached
		if _, exists := cachedAnns["other-annotation"]; exists {
			t.Errorf("Node %s: non-quarantine annotation should not be cached", tt.nodeName)
		}
	}
}

// Test cache performance with many events
func TestCachePerformanceWithManyEvents(t *testing.T) {
	ctx := context.Background()

	apiCallCount := 0
	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			apiCallCount++
			// Simulate API latency
			time.Sleep(5 * time.Millisecond)
			return map[string]string{}, nil
		},
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string,
			taints []config.Taint, isCordon bool,
			annotations map[string]string, labelsMap map[string]string) error {
			return nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)

	// Pre-populate cache with 100 nodes
	for i := 0; i < 100; i++ {
		nodeName := fmt.Sprintf("node%d", i)
		r.nodeAnnotationsCache.Store(nodeName, map[string]string{})
	}

	// Process 1000 events across 100 nodes
	startTime := time.Now()
	for i := 0; i < 1000; i++ {
		nodeName := fmt.Sprintf("node%d", i%100)
		event := &protos.HealthEvent{NodeName: nodeName}
		healthEventWithStatus := &store.HealthEventWithStatus{HealthEvent: event}

		// Mock evaluator that returns not applicable
		ruleSetEvals := []evaluator.RuleSetEvaluatorIface{
			&mockEvaluator{name: "ruleset-1", ruleEvalResult: common.RuleEvaluationNotApplicable},
		}

		r.handleEvent(ctx, healthEventWithStatus, ruleSetEvals, rulesetsConfig{})
	}
	duration := time.Since(startTime)

	if apiCallCount > 0 {
		t.Errorf("Expected 0 API calls with cache, got %d", apiCallCount)
	}

	if duration > 200*time.Millisecond {
		t.Errorf("Processing 1000 events took too long: %v (expected < 200ms)", duration)
	}

	t.Logf("Processed 1000 events in %v with cache (0 API calls)", duration)
}

// Test that mutations to returned maps don't affect the cache
func TestCacheReturnsCopyNotReference(t *testing.T) {
	ctx := context.Background()

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			return map[string]string{
				common.QuarantineHealthEventAnnotationKey: "original-value",
				"other-annotation":                        "should-be-ignored",
			}, nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)

	// Test 1: fetchAndCacheQuarantineAnnotations returns a copy
	annotations1, err := r.fetchAndCacheQuarantineAnnotations(ctx, "test-node")
	if err != nil {
		t.Fatalf("Expected successful fetch, got error: %v", err)
	}

	// Mutate the returned map
	annotations1[common.QuarantineHealthEventAnnotationKey] = "mutated-value"
	annotations1["new-key"] = "new-value"

	// Get from cache again (should use cached version)
	annotations2, err := r.getNodeQuarantineAnnotations(ctx, "test-node")
	if err != nil {
		t.Fatalf("Expected successful cache hit, got error: %v", err)
	}

	// Verify the cached value wasn't mutated
	if annotations2[common.QuarantineHealthEventAnnotationKey] != "original-value" {
		t.Errorf("Cache was mutated! Expected 'original-value', got '%s'",
			annotations2[common.QuarantineHealthEventAnnotationKey])
	}

	if _, exists := annotations2["new-key"]; exists {
		t.Error("Cache was mutated! Unexpected key 'new-key' found in cache")
	}

	// Test 2: getNodeQuarantineAnnotations also returns a copy
	annotations3, err := r.getNodeQuarantineAnnotations(ctx, "test-node")
	if err != nil {
		t.Fatalf("Expected successful cache hit, got error: %v", err)
	}

	// Mutate this map too
	annotations3[common.QuarantineHealthEventAnnotationKey] = "another-mutation"

	// Get from cache once more
	annotations4, err := r.getNodeQuarantineAnnotations(ctx, "test-node")
	if err != nil {
		t.Fatalf("Expected successful cache hit, got error: %v", err)
	}

	// Verify the cached value still wasn't mutated
	if annotations4[common.QuarantineHealthEventAnnotationKey] != "original-value" {
		t.Errorf("Cache was mutated! Expected 'original-value', got '%s'",
			annotations4[common.QuarantineHealthEventAnnotationKey])
	}
}

// TestHandleManualUncordon tests the manual uncordon handler
func TestHandleManualUncordon(t *testing.T) {
	ctx := context.Background()

	originalEvent := &protos.HealthEvent{
		NodeName:       "node1",
		Agent:          "agent1",
		CheckName:      "checkA",
		ComponentClass: "class1",
		Version:        1,
		IsHealthy:      false,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}

	// Simulate FQ annotations on the node
	existingAnnotations := map[string]string{
		common.QuarantineHealthEventAnnotationKey:              func() string { b, _ := json.Marshal(originalEvent); return string(b) }(),
		common.QuarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"key1","Value":"val1","Effect":"NoSchedule"}]`,
		common.QuarantineHealthEventIsCordonedAnnotationKey:    common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
	}

	removedAnnotationKeys := []string{}
	addedAnnotations := map[string]string{}
	var removedTaints []config.Taint

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			if nodeName != "node1" {
				t.Errorf("Expected node1, got %s", nodeName)
			}
			return existingAnnotations, nil
		},
		unTaintAndUnCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
			if nodeName != "node1" {
				t.Errorf("Expected node1, got %s", nodeName)
			}
			if isUncordon {
				t.Errorf("Should not try to uncordon again - node is already manually uncordoned")
			}
			removedAnnotationKeys = annotationKeys
			removedTaints = taints

			expectedLabelsToRemove := []string{statemanager.NVSentinelStateLabelKey}
			if !slices.Equal(expectedLabelsToRemove, labelsToRemove) {
				t.Errorf("Should remove labels %v, got %v", expectedLabelsToRemove, labelsToRemove)
			}
			if len(labelMap) > 0 {
				t.Errorf("Should not add any labels, got %v", labelMap)
			}

			return nil
		},
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error {
			if nodeName != "node1" {
				t.Errorf("Expected node1, got %s", nodeName)
			}
			if isCordon {
				t.Errorf("Should not cordon the node")
			}
			if len(taints) > 0 {
				t.Errorf("Should not add any taints")
			}
			addedAnnotations = annotations
			return nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)
	r.SetLabelKeys("k8s.nvidia.com/")

	// Initialize nodeInfo and mark node as quarantined
	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true, true)

	// Call handleManualUncordon
	err := r.handleManualUncordon("node1")
	if err != nil {
		t.Fatalf("handleManualUncordon failed: %v", err)
	}

	// Verify annotations were removed
	expectedRemovedAnnotations := []string{
		common.QuarantineHealthEventAnnotationKey,
		common.QuarantineHealthEventAppliedTaintsAnnotationKey,
		common.QuarantineHealthEventIsCordonedAnnotationKey,
	}
	if len(removedAnnotationKeys) != len(expectedRemovedAnnotations) {
		t.Errorf("Expected %d annotations to be removed, got %d", len(expectedRemovedAnnotations), len(removedAnnotationKeys))
	}
	for _, key := range expectedRemovedAnnotations {
		found := false
		for _, removedKey := range removedAnnotationKeys {
			if removedKey == key {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected annotation %s to be removed", key)
		}
	}

	// Verify taints were removed
	if len(removedTaints) != 1 {
		t.Errorf("Expected 1 taint to be removed, got %d", len(removedTaints))
	} else {
		if removedTaints[0].Key != "key1" || removedTaints[0].Value != "val1" || removedTaints[0].Effect != "NoSchedule" {
			t.Errorf("Unexpected taint removed: %+v", removedTaints[0])
		}
	}

	// Verify manual uncordon annotation was added
	if addedAnnotations[common.QuarantinedNodeUncordonedManuallyAnnotationKey] != common.QuarantinedNodeUncordonedManuallyAnnotationValue {
		t.Errorf("Expected manual uncordon annotation to be added, got %v", addedAnnotations)
	}

	// Verify node is no longer marked as quarantined in nodeInfo cache
	// We update this immediately for consistency with the metric
	quarantinedNodes := r.nodeInfo.GetQuarantinedNodesCopy()
	if quarantinedNodes["node1"] {
		t.Errorf("Expected node1 to be removed from quarantined nodes cache")
	}

	// Note: We don't verify the annotation cache here because it will be updated
	// by onNodeAnnotationsChanged when the subsequent update event is processed
}

// TestManualUncordonEndToEnd tests the complete flow of manual uncordon from detection to state updates
func TestManualUncordonEndToEnd(t *testing.T) {
	ctx := context.Background()

	// Create test nodes
	quarantinedNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node-1",
			Labels: map[string]string{
				informer.GpuNodeLabel:                "true",
				statemanager.NVSentinelStateLabelKey: string(statemanager.RemediationFailedLabelValue),
			},
			Annotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey:              `{"nodeName":"test-node-1","agent":"test","checkName":"test","isHealthy":false}`,
				common.QuarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"fault","Value":"gpu","Effect":"NoSchedule"}]`,
				common.QuarantineHealthEventIsCordonedAnnotationKey:    common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
			},
		},
		Spec: corev1.NodeSpec{
			Unschedulable: true, // Node is cordoned
		},
	}

	normalNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node-2",
			Labels: map[string]string{
				informer.GpuNodeLabel: "true",
			},
		},
		Spec: corev1.NodeSpec{
			Unschedulable: false,
		},
	}

	// Create fake k8s client with initial nodes
	fakeClient := fake.NewSimpleClientset(quarantinedNode, normalNode)

	// Track API calls with mutex protection for concurrent access
	var mu sync.Mutex
	updateCount := 0

	// Wrap the client to intercept updates
	fakeClient.PrependReactor("update", "nodes", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		mu.Lock()
		updateCount++
		mu.Unlock()
		return false, nil, nil // Let it proceed
	})

	// Create reconciler with mock K8s client
	mockK8sClient := &mockK8sClient{
		getK8sClientFn: func() kubernetes.Interface {
			return fakeClient
		},
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			node, err := fakeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return node.Annotations, nil
		},
		unTaintAndUnCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
			// Simulate removing annotations and taints
			node, err := fakeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				return err
			}

			// Remove specified annotations
			for _, key := range annotationKeys {
				delete(node.Annotations, key)
			}

			// Remove specified taints
			var newTaints []corev1.Taint
			for _, existingTaint := range node.Spec.Taints {
				shouldRemove := false
				for _, taintToRemove := range taints {
					if existingTaint.Key == taintToRemove.Key &&
						existingTaint.Value == taintToRemove.Value &&
						string(existingTaint.Effect) == taintToRemove.Effect {
						shouldRemove = true
						break
					}
				}
				if !shouldRemove {
					newTaints = append(newTaints, existingTaint)
				}
			}
			node.Spec.Taints = newTaints

			delete(node.Labels, statemanager.NVSentinelStateLabelKey)

			_, err = fakeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			return err
		},
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error {
			// Simulate adding annotations
			node, err := fakeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				return err
			}

			if node.Annotations == nil {
				node.Annotations = make(map[string]string)
			}
			for k, v := range annotations {
				node.Annotations[k] = v
			}

			_, err = fakeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			return err
		},
	}

	// Create reconciler
	workSignal := make(chan struct{}, 10)
	r := NewReconciler(ctx, ReconcilerConfig{
		K8sClient: mockK8sClient,
		DryRun:    false,
	}, workSignal)
	r.SetLabelKeys("k8s.nvidia.com/")

	// Create NodeInformer
	nodeInformer, err := informer.NewNodeInformer(fakeClient, 0, workSignal, r.nodeInfo)
	if err != nil {
		t.Fatalf("Failed to create NodeInformer: %v", err)
	}

	// Track callback invocations with thread-safe access
	manualUncordonCalled := false
	manualUncordonNodeName := ""
	annotationChanges := make(map[string]int) // Track how many times each node's annotations changed

	// Set up callbacks
	nodeInformer.SetOnManualUncordonCallback(func(nodeName string) error {
		mu.Lock()
		manualUncordonCalled = true
		manualUncordonNodeName = nodeName
		mu.Unlock()
		return r.handleManualUncordon(nodeName)
	})

	nodeInformer.SetOnNodeAnnotationsChangedCallback(func(nodeName string, annotations map[string]string) {
		mu.Lock()
		annotationChanges[nodeName]++
		mu.Unlock()
		r.handleNodeAnnotationChange(nodeName, annotations)
	})

	// Start the informer
	stopCh := make(chan struct{})
	defer close(stopCh)

	go nodeInformer.Run(stopCh)

	// Wait for initial sync
	if !cache.WaitForCacheSync(stopCh, nodeInformer.HasSynced) {
		t.Fatalf("Failed to sync cache")
	}

	// Initial state verification
	totalGpu, cordonedMap, err := nodeInformer.GetGpuNodeCounts()
	if err != nil {
		t.Fatalf("Failed to get initial counts: %v", err)
	}
	if totalGpu != 2 {
		t.Errorf("Expected 2 GPU nodes, got %d", totalGpu)
	}
	if len(cordonedMap) != 1 || !cordonedMap["test-node-1"] {
		t.Errorf("Expected test-node-1 to be cordoned, got %v", cordonedMap)
	}

	// Simulate manual uncordon by updating the node
	quarantinedNode.Spec.Unschedulable = false
	_, err = fakeClient.CoreV1().Nodes().Update(ctx, quarantinedNode, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Failed to simulate manual uncordon: %v", err)
	}

	// Wait for the event to be processed
	time.Sleep(100 * time.Millisecond)

	// Verify manual uncordon was detected and handled
	mu.Lock()
	if !manualUncordonCalled {
		t.Error("Manual uncordon callback was not called")
	}
	if manualUncordonNodeName != "test-node-1" {
		t.Errorf("Expected manual uncordon for test-node-1, got %s", manualUncordonNodeName)
	}
	mu.Unlock()

	// Verify the node was updated with manual uncordon annotation
	updatedNode, err := fakeClient.CoreV1().Nodes().Get(ctx, "test-node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}

	// Check FQ annotations were removed
	if _, exists := updatedNode.Annotations[common.QuarantineHealthEventAnnotationKey]; exists {
		t.Error("QuarantineHealthEvent annotation should be removed")
	}
	if _, exists := updatedNode.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey]; exists {
		t.Error("QuarantineHealthEventAppliedTaints annotation should be removed")
	}
	if _, exists := updatedNode.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]; exists {
		t.Error("QuarantineHealthEventIsCordoned annotation should be removed")
	}

	// Check dgxc.nvidia.com/nvsentinel-state label was removed but expected labels are persisted
	if _, exists := updatedNode.Labels[statemanager.NVSentinelStateLabelKey]; exists {
		t.Errorf("%s label should be removed", statemanager.NVSentinelStateLabelKey)
	}
	if _, exists := updatedNode.Labels[informer.GpuNodeLabel]; !exists {
		t.Errorf("%s label should be persisted", informer.GpuNodeLabel)
	}

	// Check manual uncordon annotation was added
	if val := updatedNode.Annotations[common.QuarantinedNodeUncordonedManuallyAnnotationKey]; val != common.QuarantinedNodeUncordonedManuallyAnnotationValue {
		t.Errorf("Expected manual uncordon annotation to be 'True', got %s", val)
	}

	// Verify final state
	totalGpu, cordonedMap, err = nodeInformer.GetGpuNodeCounts()
	if err != nil {
		t.Fatalf("Failed to get final counts: %v", err)
	}
	if totalGpu != 2 {
		t.Errorf("Expected 2 GPU nodes, got %d", totalGpu)
	}
	if len(cordonedMap) != 0 {
		t.Errorf("Expected no cordoned nodes after manual uncordon, got %v", cordonedMap)
	}

	// Verify nodeInfo cache is consistent
	quarantinedNodes := r.nodeInfo.GetQuarantinedNodesCopy()
	if quarantinedNodes["test-node-1"] {
		t.Error("test-node-1 should not be in quarantined nodes cache")
	}

	// Verify annotation change callbacks were invoked
	mu.Lock()
	if annotationChanges["test-node-1"] < 1 {
		t.Error("Expected annotation change callback to be invoked for test-node-1")
	}

	// Verify at least 2 updates occurred (one for removing annotations, one for adding manual uncordon annotation)
	if updateCount < 2 {
		t.Errorf("Expected at least 2 node updates, got %d", updateCount)
	}
	mu.Unlock()
}

// TestNodeRequarantineAfterManualUncordon tests that when FQ quarantines a node again after manual uncordon,
// the manual uncordon annotation is removed
func TestNodeRequarantineAfterManualUncordon(t *testing.T) {
	ctx := context.Background()

	// Track annotations that were removed
	var removedAnnotations []string
	hasManualUncordonAnnotation := true

	// Create mock K8s client
	mockK8sClient := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			// Simulate node with manual uncordon annotation
			if hasManualUncordonAnnotation {
				return map[string]string{
					common.QuarantinedNodeUncordonedManuallyAnnotationKey: common.QuarantinedNodeUncordonedManuallyAnnotationValue,
					common.QuarantineHealthEventAnnotationKey:             "old-event",
				}, nil
			}
			return map[string]string{
				common.QuarantineHealthEventAnnotationKey: "old-event",
			}, nil
		},
		unTaintAndUnCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
			// Track which annotations were removed
			removedAnnotations = append(removedAnnotations, annotationKeys...)
			// Simulate removal
			for _, key := range annotationKeys {
				if key == common.QuarantinedNodeUncordonedManuallyAnnotationKey {
					hasManualUncordonAnnotation = false
				}
			}
			return nil
		},
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error {
			return nil
		},
		getK8sClientFn: func() kubernetes.Interface {
			return fake.NewSimpleClientset()
		},
	}

	// Create reconciler
	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: mockK8sClient}, nil)
	r.SetLabelKeys("k8s.nvidia.com/")

	// Build cache with the node having manual uncordon annotation
	r.nodeAnnotationsCache.Store("test-node", map[string]string{
		common.QuarantinedNodeUncordonedManuallyAnnotationKey: common.QuarantinedNodeUncordonedManuallyAnnotationValue,
		common.QuarantineHealthEventAnnotationKey:             "old-event",
	})

	// Simulate quarantine action that should remove manual uncordon annotation
	// This would normally be called from applyRule, but we'll test the specific part
	nodeAnnotations, err := r.getNodeQuarantineAnnotations(ctx, "test-node")
	if err != nil {
		t.Fatalf("Failed to get node annotations: %v", err)
	}

	if _, hasManualUncordon := nodeAnnotations[common.QuarantinedNodeUncordonedManuallyAnnotationKey]; hasManualUncordon {
		// Remove the manual uncordon annotation before applying quarantine
		if err := mockK8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(
			ctx,
			"test-node",
			nil,   // No taints to remove
			false, // Not uncordoning
			[]string{common.QuarantinedNodeUncordonedManuallyAnnotationKey}, // Remove manual uncordon annotation
			nil, // No labels to remove
			nil, // No labels to add
		); err == nil {
			// Update cache to remove the manual uncordon annotation
			r.updateCacheWithUnquarantineAnnotations("test-node",
				[]string{common.QuarantinedNodeUncordonedManuallyAnnotationKey})
		}
	}

	// Verify manual uncordon annotation was removed
	found := false
	for _, annotation := range removedAnnotations {
		if annotation == common.QuarantinedNodeUncordonedManuallyAnnotationKey {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("Expected manual uncordon annotation to be removed when FQ quarantines node again")
	}

	// Verify cache was updated
	cachedAnnotations, _ := r.getNodeQuarantineAnnotations(ctx, "test-node")
	if _, stillHasAnnotation := cachedAnnotations[common.QuarantinedNodeUncordonedManuallyAnnotationKey]; stillHasAnnotation {
		t.Errorf("Cache should not contain manual uncordon annotation after removal")
	}
}

// TestCircuitBreakerBasicFunctionality tests basic circuit breaker operations
func TestCircuitBreakerBasicFunctionality(t *testing.T) {
	ctx := context.Background()

	// Mock circuit breaker state storage
	breakerState := "CLOSED"
	cbStateReadCount := 0
	cbStateWriteCount := 0

	mockK8sClient := &mockK8sClient{
		ensureConfigMapFn: func(ctx context.Context, name, namespace string, initialStatus string) error {
			if initialStatus != "CLOSED" {
				t.Errorf("Expected initial state to be CLOSED, got %s", initialStatus)
			}
			return nil
		},
		readCBStateFn: func(ctx context.Context, name, namespace string) (string, error) {
			cbStateReadCount++
			return breakerState, nil
		},
		writeCBStateFn: func(ctx context.Context, name, namespace, status string) error {
			cbStateWriteCount++
			breakerState = status
			return nil
		},
		getTotalGpuNodesFn: func(ctx context.Context) (int, error) {
			return 10, nil // 10 total nodes for testing
		},
	}

	circuitBreakerConfig := CircuitBreakerConfig{
		Namespace:  "test-namespace",
		Name:       "test-circuit-breaker",
		Percentage: 50, // 50% threshold
		Duration:   5 * time.Minute,
	}

	cfg := ReconcilerConfig{
		K8sClient:             mockK8sClient,
		CircuitBreakerEnabled: true,
		CircuitBreaker:        circuitBreakerConfig,
	}

	r := NewReconciler(ctx, cfg, nil)

	// Test 1: Circuit breaker should be initialized in CLOSED state
	if r.cb == nil {
		t.Fatal("Circuit breaker should be initialized when enabled")
	}

	currentState := r.cb.CurrentState()
	if currentState != "CLOSED" {
		t.Errorf("Expected initial state to be CLOSED, got %s", currentState)
	}

	// Test 2: Circuit breaker should not be tripped initially
	isTripped, err := r.cb.IsTripped(ctx)
	if err != nil {
		t.Fatalf("Error checking if circuit breaker is tripped: %v", err)
	}
	if isTripped {
		t.Error("Circuit breaker should not be tripped initially")
	}

	// Test 3: Add cordon events below threshold (4 out of 10 nodes = 40% < 50%)
	for i := 0; i < 4; i++ {
		r.cb.AddCordonEvent(fmt.Sprintf("node-%d", i))
	}

	isTripped, err = r.cb.IsTripped(ctx)
	if err != nil {
		t.Fatalf("Error checking if circuit breaker is tripped: %v", err)
	}
	if isTripped {
		t.Error("Circuit breaker should not be tripped at 40% threshold")
	}

	// Test 4: Add one more cordon event to reach threshold (5 out of 10 nodes = 50%)
	r.cb.AddCordonEvent("node-4")

	isTripped, err = r.cb.IsTripped(ctx)
	if err != nil {
		t.Fatalf("Error checking if circuit breaker is tripped: %v", err)
	}
	if !isTripped {
		t.Error("Circuit breaker should be tripped at 50% threshold")
	}

	// Test 5: Verify state was persisted
	if cbStateWriteCount == 0 {
		t.Error("Circuit breaker state should have been written to ConfigMap")
	}
	if breakerState != "TRIPPED" {
		t.Errorf("Expected persisted state to be TRIPPED, got %s", breakerState)
	}

	// Test 6: Force state back to CLOSED
	err = r.cb.ForceState(ctx, "CLOSED")
	if err != nil {
		t.Fatalf("Error forcing circuit breaker state: %v", err)
	}

	currentState = r.cb.CurrentState()
	if currentState != "CLOSED" {
		t.Errorf("Expected state to be CLOSED after forcing, got %s", currentState)
	}

	// Note: IsTripped() will automatically trip the breaker again if the threshold is still exceeded
	// So we just verify the current state is CLOSED, but IsTripped() will return true due to the
	// cordon events still being in the sliding window
	isTripped, err = r.cb.IsTripped(ctx)
	if err != nil {
		t.Fatalf("Error checking if circuit breaker is tripped: %v", err)
	}
	// The breaker will be tripped again because the cordon events are still within the sliding window
	if !isTripped {
		t.Error("Circuit breaker should be tripped again due to cordon events still in sliding window")
	}
}

// TestCircuitBreakerSlidingWindow tests sliding window behavior
func TestCircuitBreakerSlidingWindow(t *testing.T) {
	ctx := context.Background()

	mockK8sClient := &mockK8sClient{
		ensureConfigMapFn: func(ctx context.Context, name, namespace string, initialStatus string) error {
			return nil
		},
		readCBStateFn: func(ctx context.Context, name, namespace string) (string, error) {
			return "CLOSED", nil
		},
		writeCBStateFn: func(ctx context.Context, name, namespace, status string) error {
			return nil
		},
		getTotalGpuNodesFn: func(ctx context.Context) (int, error) {
			return 10, nil // 10 total nodes
		},
	}

	circuitBreakerConfig := CircuitBreakerConfig{
		Namespace:  "test-namespace",
		Name:       "test-circuit-breaker",
		Percentage: 50,              // 50% threshold
		Duration:   2 * time.Second, // Short window for testing
	}

	cfg := ReconcilerConfig{
		K8sClient:             mockK8sClient,
		CircuitBreakerEnabled: true,
		CircuitBreaker:        circuitBreakerConfig,
	}

	r := NewReconciler(ctx, cfg, nil)

	// Add cordon events to reach threshold
	for i := 0; i < 5; i++ {
		r.cb.AddCordonEvent(fmt.Sprintf("node-%d", i))
	}

	// Should be tripped immediately
	isTripped, err := r.cb.IsTripped(ctx)
	if err != nil {
		t.Fatalf("Error checking if circuit breaker is tripped: %v", err)
	}
	if !isTripped {
		t.Error("Circuit breaker should be tripped after reaching threshold")
	}

	// Force state back to CLOSED to test sliding window
	err = r.cb.ForceState(ctx, "CLOSED")
	if err != nil {
		t.Fatalf("Error forcing circuit breaker state: %v", err)
	}

	// Wait for sliding window to expire (events should age out)
	time.Sleep(3 * time.Second)

	// Should not be tripped anymore due to sliding window
	isTripped, err = r.cb.IsTripped(ctx)
	if err != nil {
		t.Fatalf("Error checking if circuit breaker is tripped: %v", err)
	}
	if isTripped {
		t.Error("Circuit breaker should not be tripped after sliding window expires")
	}
}

// TestCircuitBreakerUniqueNodeTracking tests that duplicate cordon events for same node are handled correctly
func TestCircuitBreakerUniqueNodeTracking(t *testing.T) {
	ctx := context.Background()

	mockK8sClient := &mockK8sClient{
		ensureConfigMapFn: func(ctx context.Context, name, namespace string, initialStatus string) error {
			return nil
		},
		readCBStateFn: func(ctx context.Context, name, namespace string) (string, error) {
			return "CLOSED", nil
		},
		writeCBStateFn: func(ctx context.Context, name, namespace, status string) error {
			return nil
		},
		getTotalGpuNodesFn: func(ctx context.Context) (int, error) {
			return 10, nil // 10 total nodes
		},
	}

	circuitBreakerConfig := CircuitBreakerConfig{
		Namespace:  "test-namespace",
		Name:       "test-circuit-breaker",
		Percentage: 50, // 50% threshold (5 out of 10 nodes)
		Duration:   5 * time.Minute,
	}

	cfg := ReconcilerConfig{
		K8sClient:             mockK8sClient,
		CircuitBreakerEnabled: true,
		CircuitBreaker:        circuitBreakerConfig,
	}

	r := NewReconciler(ctx, cfg, nil)

	// Add same node multiple times - should only count once
	for i := 0; i < 10; i++ {
		r.cb.AddCordonEvent("node-1")
	}

	// Should not be tripped because only 1 unique node was cordoned
	isTripped, err := r.cb.IsTripped(ctx)
	if err != nil {
		t.Fatalf("Error checking if circuit breaker is tripped: %v", err)
	}
	if isTripped {
		t.Error("Circuit breaker should not be tripped with only 1 unique node cordoned")
	}

	// Add 4 more unique nodes to reach threshold
	for i := 2; i <= 5; i++ {
		r.cb.AddCordonEvent(fmt.Sprintf("node-%d", i))
	}

	// Now should be tripped (5 unique nodes = 50%)
	isTripped, err = r.cb.IsTripped(ctx)
	if err != nil {
		t.Fatalf("Error checking if circuit breaker is tripped: %v", err)
	}
	if !isTripped {
		t.Error("Circuit breaker should be tripped with 5 unique nodes cordoned")
	}
}

// TestBackwardCompatibilityAppendNewEvent tests that when an old single-event annotation exists
// and a new fatal event arrives, the system converts to the new format and appends the new event
func TestBackwardCompatibilityAppendNewEvent(t *testing.T) {
	ctx := context.Background()

	// Existing old format annotation (single event)
	existingOldEvent := &protos.HealthEvent{
		NodeName:       "node1",
		Agent:          "gpu-health-monitor",
		ComponentClass: "GPU",
		CheckName:      "GpuXidError",
		Version:        1,
		IsHealthy:      false,
		IsFatal:        true,
		Message:        "XID 62 error",
		ErrorCode:      []string{"62"},
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}

	oldAnnotationStr, _ := json.Marshal(existingOldEvent)
	annotationsMap := map[string]string{
		quarantineHealthEventAnnotationKey:              string(oldAnnotationStr),
		quarantineHealthEventIsCordonedAnnotationKey:    "True",
		quarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"gpu-xid-error","Value":"true","Effect":"NoSchedule"}]`,
	}

	updateCount := 0
	var capturedAnnotation string

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			return annotationsMap, nil
		},
		updateNodeAnnotationsFn: func(ctx context.Context, nodeName string, annotations map[string]string) error {
			updateCount++
			for k, v := range annotations {
				annotationsMap[k] = v
				if k == quarantineHealthEventAnnotationKey {
					capturedAnnotation = v
				}
			}
			return nil
		},
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error {
			// Already cordoned, shouldn't be called
			return nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{
		K8sClient: k8sMock,
	}, nil)
	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true, true)

	// New fatal event for a different GPU
	newEvent := &store.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			NodeName:       "node1",
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			CheckName:      "NVLinkError",
			Version:        1,
			IsHealthy:      false,
			IsFatal:        true,
			Message:        "NVLink down",
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "1"},
			},
		},
	}

	// Create mock evaluator
	mockEval := &mockEvaluator{
		name:           "ruleset1",
		ok:             true,
		ruleEvalResult: common.RuleEvaluationSuccess,
		priority:       10,
		version:        "v1",
	}
	ruleSetEvals := []evaluator.RuleSetEvaluatorIface{mockEval}

	defaultTaint := config.Taint{Key: "gpu-error", Value: "true", Effect: "NoSchedule"}
	rulesetsConf := rulesetsConfig{
		TaintConfigMap: map[string]*config.Taint{
			"ruleset1": &defaultTaint,
		},
		CordonConfigMap: map[string]bool{
			"ruleset1": true,
		},
		RuleSetPriorityMap: map[string]int{
			"ruleset1": 10,
		},
	}

	status, _ := r.handleEvent(ctx, newEvent, ruleSetEvals, rulesetsConf)

	// Verify conversion happened and new event was added
	if updateCount < 1 {
		t.Errorf("Expected annotation update for format conversion and new event addition")
	}

	// Verify the annotation was converted to new format
	var newFormatMap healthEventsAnnotation.HealthEventsAnnotationMap
	if err := json.Unmarshal([]byte(capturedAnnotation), &newFormatMap); err != nil {
		t.Fatalf("Failed to unmarshal new format annotation: %v", err)
	}

	// Should have both events tracked
	if newFormatMap.Count() != 2 {
		t.Errorf("Expected 2 events in new format (1 converted + 1 new), got %d", newFormatMap.Count())
	}

	// Verify both events are present
	if _, found := newFormatMap.GetEvent(existingOldEvent); !found {
		t.Errorf("Converted old event not found in new format")
	}
	if _, found := newFormatMap.GetEvent(newEvent.HealthEvent); !found {
		t.Errorf("New event not found in new format")
	}

	if status == nil || *status != store.AlreadyQuarantined {
		t.Errorf("Expected AlreadyQuarantined status, got %v", status)
	}
}

// TestBackwardCompatibilityHealthyEventRemoval tests that when an old single-event annotation exists
// and the corresponding healthy event arrives, the system converts format and handles recovery
func TestBackwardCompatibilityHealthyEventRemoval(t *testing.T) {
	ctx := context.Background()

	// Existing old format annotation (single unhealthy event)
	existingOldEvent := &protos.HealthEvent{
		NodeName:       "node1",
		Agent:          "gpu-health-monitor",
		ComponentClass: "GPU",
		CheckName:      "GpuXidError",
		Version:        1,
		IsHealthy:      false,
		IsFatal:        true,
		Message:        "XID 62 error",
		ErrorCode:      []string{"62"},
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}

	oldAnnotationStr, _ := json.Marshal(existingOldEvent)
	annotationsMap := map[string]string{
		quarantineHealthEventAnnotationKey:              string(oldAnnotationStr),
		quarantineHealthEventIsCordonedAnnotationKey:    "True",
		quarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"gpu-xid-error","Value":"true","Effect":"NoSchedule"}]`,
	}

	uncordonCalled := false
	updateCount := 0

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			return annotationsMap, nil
		},
		updateNodeAnnotationsFn: func(ctx context.Context, nodeName string, annotations map[string]string) error {
			updateCount++
			for k, v := range annotations {
				annotationsMap[k] = v
			}
			return nil
		},
		unTaintAndUnCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
			uncordonCalled = true
			// Clear annotations to simulate removal
			for _, key := range annotationKeys {
				delete(annotationsMap, key)
			}
			return nil
		},
	}

	r := NewReconciler(ctx, ReconcilerConfig{K8sClient: k8sMock}, nil)
	r.nodeInfo.MarkNodeQuarantineStatusCache("node1", true, true)

	// Corresponding healthy event
	healthyEvent := &store.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			NodeName:       "node1",
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			CheckName:      "GpuXidError",
			Version:        1,
			IsHealthy:      true,
			Message:        "GPU recovered",
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "0"},
			},
		},
	}

	status, _ := r.handleEvent(ctx, healthyEvent, nil, rulesetsConfig{})

	// Verify format conversion happened first
	if updateCount < 1 {
		t.Errorf("Expected at least one annotation update for format conversion")
	}

	// Verify uncordon was called
	if !uncordonCalled {
		t.Errorf("Expected node to be uncordoned after healthy event")
	}

	// Verify status
	if status == nil || *status != store.UnQuarantined {
		t.Errorf("Expected UnQuarantined status, got %v", status)
	}

	// Verify annotations were removed
	if _, exists := annotationsMap[quarantineHealthEventAnnotationKey]; exists {
		t.Errorf("Expected quarantine annotation to be removed after recovery")
	}
}

func TestEntityLevelQuarantineAndRecovery(t *testing.T) {
	ctx := context.Background()

	// Track what operations were called
	var updateAnnotationsCalled bool
	var uncordonCalled bool
	var lastAnnotations map[string]string

	k8sMock := &mockK8sClient{
		getNodeAnnotationsFn: func(ctx context.Context, nodeName string) (map[string]string, error) {
			// Initially no annotations
			if lastAnnotations == nil {
				return map[string]string{}, nil
			}
			return lastAnnotations, nil
		},
		taintAndCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error {
			// Store the annotations for future calls
			lastAnnotations = make(map[string]string)
			for k, v := range annotations {
				lastAnnotations[k] = v
			}
			return nil
		},
		updateNodeAnnotationsFn: func(ctx context.Context, nodeName string, annotations map[string]string) error {
			updateAnnotationsCalled = true
			// Update stored annotations
			if lastAnnotations == nil {
				lastAnnotations = make(map[string]string)
			}
			for k, v := range annotations {
				lastAnnotations[k] = v
			}
			return nil
		},
		unTaintAndUnCordonNodeFn: func(ctx context.Context, nodeName string, taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string, labelMap map[string]string) error {
			uncordonCalled = true
			// Clear annotations after uncordon
			lastAnnotations = map[string]string{}
			return nil
		},
	}
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
	r := NewReconciler(ctx, ReconcilerConfig{
		K8sClient:  k8sMock,
		TomlConfig: tomlConfig,
	}, nil)
	r.SetLabelKeys("k88s.nvidia.com/")

	// Create mock evaluators for rule evaluation
	mockEval1 := &mockEvaluator{
		name:           "ruleset1",
		ok:             true,
		ruleEvalResult: common.RuleEvaluationSuccess,
		priority:       10,
		version:        "v1",
	}

	mockEval2 := &mockEvaluator{
		name:           "ruleset2",
		ok:             true,
		ruleEvalResult: common.RuleEvaluationSuccess,
		priority:       5,
		version:        "v1",
	}

	ruleSetEvals := []evaluator.RuleSetEvaluatorIface{mockEval1, mockEval2}

	// Properly configure rulesetsConfig
	rulesetsConf := rulesetsConfig{
		TaintConfigMap: map[string]*config.Taint{
			"ruleset1": &tomlConfig.RuleSets[0].Taint,
			"ruleset2": &tomlConfig.RuleSets[1].Taint,
		},
		CordonConfigMap: map[string]bool{
			"ruleset1": tomlConfig.RuleSets[0].Cordon.ShouldCordon,
			"ruleset2": tomlConfig.RuleSets[1].Cordon.ShouldCordon,
		},
		RuleSetPriorityMap: map[string]int{
			"ruleset1": tomlConfig.RuleSets[0].Priority,
			"ruleset2": tomlConfig.RuleSets[1].Priority,
		},
	}

	// Test 1: Initial GPU 1 failure should quarantine node
	gpu1FailEvent := &store.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			CheckName:      "GpuXidError",
			NodeName:       "node1",
			Version:        1,
			IsFatal:        true,
			IsHealthy:      false,
			Message:        "XID error occurred",
			ErrorCode:      []string{"62"},
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "1"},
			},
		},
	}

	status1, _ := r.handleEvent(ctx, gpu1FailEvent, ruleSetEvals, rulesetsConf)
	if status1 == nil || *status1 != store.Quarantined {
		t.Errorf("Expected GPU 1 failure to quarantine node, got status: %v", status1)
	}

	// Verify annotation contains GPU 1 failure
	healthEventAnnotation := lastAnnotations[common.QuarantineHealthEventAnnotationKey]
	if !strings.Contains(healthEventAnnotation, `"entityValue":"1"`) {
		t.Errorf("Annotation should contain GPU 1 entity: %s", healthEventAnnotation)
	}

	// Test 2: GPU 2 failure should be added to existing quarantine
	updateAnnotationsCalled = false
	gpu2FailEvent := &store.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			CheckName:      "GpuXidError",
			NodeName:       "node1",
			Version:        1,
			IsFatal:        true,
			IsHealthy:      false,
			Message:        "XID error occurred",
			ErrorCode:      []string{"62"},
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "2"},
			},
		},
	}

	status2, _ := r.handleEvent(ctx, gpu2FailEvent, ruleSetEvals, rulesetsConf)
	if status2 == nil || *status2 != store.AlreadyQuarantined {
		t.Errorf("Expected GPU 2 failure to be added to quarantine, got status: %v", status2)
	}

	if !updateAnnotationsCalled {
		t.Errorf("UpdateNodeAnnotations should be called when adding GPU 2 failure")
	}

	// Verify annotation now contains both GPU 1 and GPU 2
	healthEventAnnotation = lastAnnotations[common.QuarantineHealthEventAnnotationKey]
	if !strings.Contains(healthEventAnnotation, `"entityValue":"1"`) {
		t.Errorf("Annotation should still contain GPU 1: %s", healthEventAnnotation)
	}
	if !strings.Contains(healthEventAnnotation, `"entityValue":"2"`) {
		t.Errorf("Annotation should now contain GPU 2: %s", healthEventAnnotation)
	}

	// Test 3: GPU 1 recovery should remove only GPU 1, node stays quarantined
	updateAnnotationsCalled = false
	uncordonCalled = false
	gpu1RecoveryEvent := &store.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			CheckName:      "GpuXidError",
			NodeName:       "node1",
			Version:        1,
			IsHealthy:      true,
			Message:        "No health failures",
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "1"}, // Only GPU 1 recovers
			},
		},
	}

	status3, _ := r.handleEvent(ctx, gpu1RecoveryEvent, ruleSetEvals, rulesetsConf)
	if status3 == nil || *status3 != store.AlreadyQuarantined {
		t.Errorf("Expected partial recovery to keep node quarantined, got status: %v", status3)
	}

	if !updateAnnotationsCalled {
		t.Errorf("UpdateNodeAnnotations should be called for partial recovery")
	}
	if uncordonCalled {
		t.Errorf("Node should not be uncordoned during partial recovery")
	}

	// Verify annotation now contains only GPU 2
	healthEventAnnotation = lastAnnotations[common.QuarantineHealthEventAnnotationKey]
	if strings.Contains(healthEventAnnotation, `"entityValue":"1"`) {
		t.Errorf("Annotation should not contain GPU 1 after recovery: %s", healthEventAnnotation)
	}
	if !strings.Contains(healthEventAnnotation, `"entityValue":"2"`) {
		t.Errorf("Annotation should still contain GPU 2: %s", healthEventAnnotation)
	}

	// Test 4: GPU 2 recovery should uncordon node
	updateAnnotationsCalled = false
	uncordonCalled = false
	gpu2RecoveryEvent := &store.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			Agent:          "gpu-health-monitor",
			ComponentClass: "GPU",
			CheckName:      "GpuXidError",
			NodeName:       "node1",
			Version:        1,
			IsHealthy:      true,
			Message:        "No health failures",
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "2"}, // GPU 2 recovers
			},
		},
	}

	status4, _ := r.handleEvent(ctx, gpu2RecoveryEvent, ruleSetEvals, rulesetsConf)
	if status4 == nil || *status4 != store.UnQuarantined {
		t.Errorf("Expected complete recovery to unquarantine node, got status: %v", status4)
	}

	if uncordonCalled != true {
		t.Errorf("Node should be uncordoned after all entities recover")
	}
}
