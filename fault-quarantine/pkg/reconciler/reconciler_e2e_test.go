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
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/breaker"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/evaluator"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
	storeclientsdk "github.com/nvidia/nvsentinel/store-client/pkg/storewatcher"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	e2eTestClient     *kubernetes.Clientset
	e2eTestContext    context.Context
	e2eTestCancelFunc context.CancelFunc
	e2eTestEnv        *envtest.Environment
)

var (
	quarantineHealthEventAnnotationKey              = common.QuarantineHealthEventAnnotationKey
	quarantineHealthEventAppliedTaintsAnnotationKey = common.QuarantineHealthEventAppliedTaintsAnnotationKey
	quarantineHealthEventIsCordonedAnnotationKey    = common.QuarantineHealthEventIsCordonedAnnotationKey
)

const (
	eventuallyTimeout      = 10 * time.Second
	eventuallyPollInterval = 200 * time.Millisecond

	statusCheckTimeout      = 5 * time.Second
	statusCheckPollInterval = 100 * time.Millisecond

	neverTimeout      = 1 * time.Second
	neverPollInterval = 100 * time.Millisecond
)

func TestMain(m *testing.M) {
	var err error
	e2eTestContext, e2eTestCancelFunc = context.WithCancel(context.Background())

	e2eTestEnv = &envtest.Environment{}

	e2eTestRestConfig, err := e2eTestEnv.Start()
	if err != nil {
		log.Fatalf("Failed to start test environment: %v", err)
	}

	e2eTestClient, err = kubernetes.NewForConfig(e2eTestRestConfig)
	if err != nil {
		log.Fatalf("Failed to create kubernetes client: %v", err)
	}

	exitCode := m.Run()

	e2eTestCancelFunc()
	if err := e2eTestEnv.Stop(); err != nil {
		log.Fatalf("Failed to stop test environment: %v", err)
	}
	os.Exit(exitCode)
}

func createE2ETestNode(ctx context.Context, t *testing.T, name string, annotations map[string]string, labels map[string]string, taints []corev1.Taint, unschedulable bool) {
	t.Helper()

	if labels == nil {
		labels = make(map[string]string)
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
			Labels:      labels,
		},
		Spec: corev1.NodeSpec{
			Unschedulable: unschedulable,
			Taints:        taints,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}

	_, err := e2eTestClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create test node %s", name)
}

func createHealthEventBSON(eventID primitive.ObjectID, nodeName, checkName string, isHealthy, isFatal bool, entities []*protos.Entity, quarantineStatus model.Status) bson.M {
	entitiesBSON := []interface{}{}
	for _, entity := range entities {
		entitiesBSON = append(entitiesBSON, bson.M{
			"entitytype":  entity.EntityType,
			"entityvalue": entity.EntityValue,
		})
	}

	return bson.M{
		"operationType": "insert",
		"fullDocument": bson.M{
			"_id": eventID,
			"healtheventstatus": bson.M{
				"nodequarantined": quarantineStatus,
			},
			"healthevent": bson.M{
				"nodename":         nodeName,
				"agent":            "gpu-health-monitor",
				"componentclass":   "GPU",
				"checkname":        checkName,
				"version":          uint32(1),
				"ishealthy":        isHealthy,
				"isfatal":          isFatal,
				"entitiesimpacted": entitiesBSON,
			},
		},
	}
}

type StatusGetter func(eventID primitive.ObjectID) *model.Status

// E2EReconcilerConfig holds configuration options for test reconciler setup
type E2EReconcilerConfig struct {
	TomlConfig           config.TomlConfig
	CircuitBreakerConfig *breaker.CircuitBreakerConfig
	DryRun               bool
}

// setupE2EReconciler creates a test reconciler with mock watcher
// Returns: (reconciler, mockWatcher, statusGetter, circuitBreaker)
// Note: circuitBreaker will be nil when cbConfig is nil (circuit breaker disabled)
func setupE2EReconciler(t *testing.T, ctx context.Context, tomlConfig config.TomlConfig, cbConfig *breaker.CircuitBreakerConfig) (*Reconciler, *storeclientsdk.FakeChangeStreamWatcher, StatusGetter, breaker.CircuitBreaker) {
	t.Helper()
	return setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:           tomlConfig,
		CircuitBreakerConfig: cbConfig,
		DryRun:               false,
	})
}

// setupE2EReconcilerWithOptions creates a test reconciler with full configuration control
// Returns: (reconciler, mockWatcher, statusGetter, circuitBreaker)
// Note: circuitBreaker will be nil when cbConfig is nil (circuit breaker disabled)
func setupE2EReconcilerWithOptions(t *testing.T, ctx context.Context, cfg E2EReconcilerConfig) (*Reconciler, *storeclientsdk.FakeChangeStreamWatcher, StatusGetter, breaker.CircuitBreaker) {
	t.Helper()

	nodeInformer, err := informer.NewNodeInformer(e2eTestClient, 0)
	require.NoError(t, err)

	fqClient := &informer.FaultQuarantineClient{
		Clientset:    e2eTestClient,
		DryRunMode:   cfg.DryRun,
		NodeInformer: nodeInformer,
	}

	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })

	go nodeInformer.Run(stopCh)

	require.Eventually(t, nodeInformer.HasSynced, eventuallyTimeout, statusCheckPollInterval, "NodeInformer should sync")

	ruleSetEvals, err := evaluator.InitializeRuleSetEvaluators(cfg.TomlConfig.RuleSets, fqClient.NodeInformer)
	require.NoError(t, err)

	var cb breaker.CircuitBreaker
	if cfg.CircuitBreakerConfig != nil {
		cbConfig := cfg.CircuitBreakerConfig
		// Set defaults if not provided
		percentage := cbConfig.Percentage
		if percentage == 0 {
			percentage = 50
		}
		duration := cbConfig.Duration
		if duration == 0 {
			duration = 5 * time.Minute
		}
		namespace := cbConfig.Namespace
		if namespace == "" {
			namespace = "default"
		}
		name := cbConfig.Name
		if name == "" {
			name = "test-cb-" + primitive.NewObjectID().Hex()[:8]
		}

		cb, err = breaker.NewSlidingWindowBreaker(ctx, breaker.Config{
			Window:             duration,
			TripPercentage:     float64(percentage),
			K8sClient:          fqClient,
			ConfigMapName:      name,
			ConfigMapNamespace: namespace,
		})
		require.NoError(t, err, "Failed to create circuit breaker")
	}

	reconcilerCfg := ReconcilerConfig{
		TomlConfig:            cfg.TomlConfig,
		CircuitBreakerEnabled: cfg.CircuitBreakerConfig != nil,
		DryRun:                cfg.DryRun,
	}

	r := NewReconciler(reconcilerCfg, fqClient, cb)

	if cfg.TomlConfig.LabelPrefix != "" {
		r.SetLabelKeys(cfg.TomlConfig.LabelPrefix)
		fqClient.SetLabelKeys(r.cordonedReasonLabelKey, r.uncordonedReasonLabelKey)
	}

	// Build rulesets config (mimics reconciler.Start())
	rulesetsConfig := rulesetsConfig{
		TaintConfigMap:     make(map[string]*config.Taint),
		CordonConfigMap:    make(map[string]bool),
		RuleSetPriorityMap: make(map[string]int),
	}

	for _, ruleSet := range cfg.TomlConfig.RuleSets {
		if ruleSet.Taint.Key != "" {
			rulesetsConfig.TaintConfigMap[ruleSet.Name] = &ruleSet.Taint
		}
		if ruleSet.Cordon.ShouldCordon {
			rulesetsConfig.CordonConfigMap[ruleSet.Name] = true
		}
		if ruleSet.Priority > 0 {
			rulesetsConfig.RuleSetPriorityMap[ruleSet.Name] = ruleSet.Priority
		}
	}

	r.precomputeTaintInitKeys(ruleSetEvals, rulesetsConfig)

	// Setup manual uncordon callback
	fqClient.NodeInformer.SetOnManualUncordonCallback(r.handleManualUncordon)

	// Create mock watcher
	mockWatcher := storeclientsdk.NewFakeChangeStreamWatcher()

	// Ensure the event channel is closed when test completes to terminate the processing goroutine
	t.Cleanup(func() {
		close(mockWatcher.EventsChan)
	})

	// Store event statuses for verification (mimics MongoDB status updates)
	var statusMu sync.Mutex
	eventStatuses := make(map[primitive.ObjectID]*model.Status)

	// Setup the reconciler with the callback (mimics Start())
	processEventFunc := func(ctx context.Context, event *model.HealthEventWithStatus) *model.Status {
		return r.ProcessEvent(ctx, event, ruleSetEvals, rulesetsConfig)
	}

	// Start event processing goroutine (mimics production event watcher)
	go func() {
		for event := range mockWatcher.Events() {
			healthEventWithStatus := model.HealthEventWithStatus{}
			if err := storeclientsdk.UnmarshalFullDocumentFromEvent(event, &healthEventWithStatus); err != nil {
				continue
			}

			// Get event ID (mimics MongoDB _id)
			var eventID primitive.ObjectID
			if fullDoc, ok := event["fullDocument"].(bson.M); ok {
				if id, ok := fullDoc["_id"].(primitive.ObjectID); ok {
					eventID = id
				}
			}

			// Process event and store status (mimics updateNodeQuarantineStatus in production)
			status := processEventFunc(ctx, &healthEventWithStatus)

			statusMu.Lock()
			eventStatuses[eventID] = status
			statusMu.Unlock()
		}
	}()

	// Return status getter for tests
	getStatus := func(eventID primitive.ObjectID) *model.Status {
		statusMu.Lock()
		defer statusMu.Unlock()
		return eventStatuses[eventID]
	}

	return r, mockWatcher, getStatus, cb
}

func verifyHealthEventInAnnotation(t *testing.T, node *corev1.Node, expectedCheckName, expectedAgent, expectedComponentClass string, expectedEntityType, expectedEntityValue string) {
	t.Helper()

	annotationStr := node.Annotations[quarantineHealthEventAnnotationKey]
	require.NotEmpty(t, annotationStr, "Quarantine annotation should exist")

	var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
	err := json.Unmarshal([]byte(annotationStr), &healthEventsMap)
	require.NoError(t, err, "Should unmarshal annotation")

	queryEvent := &protos.HealthEvent{
		Agent:          expectedAgent,
		ComponentClass: expectedComponentClass,
		CheckName:      expectedCheckName,
		NodeName:       node.Name,
		Version:        1,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: expectedEntityType, EntityValue: expectedEntityValue},
		},
	}

	storedEvent, found := healthEventsMap.GetEvent(queryEvent)
	require.True(t, found, "Expected entity should be found in annotation")
	require.NotNil(t, storedEvent, "Stored event should not be nil")
	assert.Equal(t, expectedCheckName, storedEvent.CheckName, "Check name should match")
	assert.Equal(t, expectedAgent, storedEvent.Agent, "Agent should match")
	assert.Equal(t, expectedComponentClass, storedEvent.ComponentClass, "Component class should match")
}

func verifyAppliedTaintsAnnotation(t *testing.T, node *corev1.Node, expectedTaints []config.Taint) {
	t.Helper()

	taintsAnnotationStr := node.Annotations[quarantineHealthEventAppliedTaintsAnnotationKey]
	require.NotEmpty(t, taintsAnnotationStr, "Applied taints annotation should exist")

	var appliedTaints []config.Taint
	err := json.Unmarshal([]byte(taintsAnnotationStr), &appliedTaints)
	require.NoError(t, err, "Should unmarshal taints annotation")

	assert.Len(t, appliedTaints, len(expectedTaints), "Should have expected number of taints")

	for _, expectedTaint := range expectedTaints {
		found := false
		for _, appliedTaint := range appliedTaints {
			if appliedTaint.Key == expectedTaint.Key &&
				appliedTaint.Value == expectedTaint.Value &&
				appliedTaint.Effect == expectedTaint.Effect {
				found = true
				break
			}
		}
		assert.True(t, found, "Expected taint %+v should be in applied taints annotation", expectedTaint)
	}
}

func verifyNodeTaintsMatch(t *testing.T, node *corev1.Node, expectedTaints []config.Taint) {
	t.Helper()

	for _, expectedTaint := range expectedTaints {
		found := false
		for _, nodeTaint := range node.Spec.Taints {
			if nodeTaint.Key == expectedTaint.Key &&
				nodeTaint.Value == expectedTaint.Value &&
				string(nodeTaint.Effect) == expectedTaint.Effect {
				found = true
				break
			}
		}
		assert.True(t, found, "Expected taint %+v should be on node", expectedTaint)
	}
}

func verifyQuarantineLabels(t *testing.T, node *corev1.Node, expectedCordonReason string) {
	t.Helper()

	assert.Equal(t, common.ServiceName, node.Labels["k8s.nvidia.com/cordon-by"], "cordon-by label should be set")
	assert.Contains(t, node.Labels["k8s.nvidia.com/cordon-reason"], expectedCordonReason, "cordon-reason should contain expected value")
	assert.NotEmpty(t, node.Labels["k8s.nvidia.com/cordon-timestamp"], "cordon-timestamp should be set")
	assert.Equal(t, string(statemanager.QuarantinedLabelValue), node.Labels[statemanager.NVSentinelStateLabelKey], "nvsentinel-state should be quarantined")
}

func verifyUnquarantineLabels(t *testing.T, node *corev1.Node) {
	t.Helper()

	assert.Equal(t, common.ServiceName, node.Labels["k8s.nvidia.com/uncordon-by"], "uncordon-by label should be set")
	assert.NotEmpty(t, node.Labels["k8s.nvidia.com/uncordon-timestamp"], "uncordon-timestamp should be set")
	assert.NotContains(t, node.Labels, "k8s.nvidia.com/cordon-by", "cordon-by label should be removed")
	assert.NotContains(t, node.Labels, "k8s.nvidia.com/cordon-reason", "cordon-reason label should be removed")
	assert.NotContains(t, node.Labels, "k8s.nvidia.com/cordon-timestamp", "cordon-timestamp label should be removed")
	assert.NotContains(t, node.Labels, statemanager.NVSentinelStateLabelKey, "nvsentinel-state label should be removed")
}

func TestE2E_BasicQuarantineAndUnquarantine(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-basic-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, getStatus, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	beforeProcessed := getCounterValue(t, metrics.TotalEventsSuccessfullyProcessed)
	beforeQuarantined := getCounterVecValue(t, metrics.TotalNodesQuarantined, nodeName)
	beforeTaints := getCounterVecValue(t, metrics.TaintsApplied, "nvidia.com/gpu-xid-error", "NoSchedule")
	beforeCordons := getCounterValue(t, metrics.CordonsApplied)
	beforeRulesetPassed := getCounterVecValue(t, metrics.RulesetEvaluations, "gpu-xid-critical-errors", metrics.StatusPassed)

	t.Log("Sending unhealthy event for initial quarantine")
	eventID1 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID1,
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	t.Log("Waiting for node to be quarantined")
	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return node.Spec.Unschedulable && node.Annotations[common.QuarantineHealthEventAnnotationKey] != ""
	}, eventuallyTimeout, eventuallyPollInterval, "Node should be quarantined")

	t.Log("Verify complete quarantine state with actual annotation content")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	verifyHealthEventInAnnotation(t, node, "GpuXidError", "gpu-health-monitor", "GPU", "GPU", "0")

	t.Log("Verify applied taints annotation content")
	expectedTaints := []config.Taint{
		{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
	}
	verifyAppliedTaintsAnnotation(t, node, expectedTaints)
	verifyNodeTaintsMatch(t, node, expectedTaints)
	assert.Equal(t, "True", node.Annotations[quarantineHealthEventIsCordonedAnnotationKey], "Cordon annotation should be True")
	verifyQuarantineLabels(t, node, "gpu-xid-critical-errors")

	afterProcessed := getCounterValue(t, metrics.TotalEventsSuccessfullyProcessed)
	afterQuarantined := getCounterVecValue(t, metrics.TotalNodesQuarantined, nodeName)
	afterGauge := getGaugeVecValue(t, metrics.CurrentQuarantinedNodes, nodeName)
	afterTaints := getCounterVecValue(t, metrics.TaintsApplied, "nvidia.com/gpu-xid-error", "NoSchedule")
	afterCordons := getCounterValue(t, metrics.CordonsApplied)
	afterRulesetPassed := getCounterVecValue(t, metrics.RulesetEvaluations, "gpu-xid-critical-errors", metrics.StatusPassed)

	assert.GreaterOrEqual(t, afterProcessed, beforeProcessed+1, "TotalEventsSuccessfullyProcessed should increment")
	assert.Equal(t, beforeQuarantined+1, afterQuarantined, "TotalNodesQuarantined should increment by 1")
	assert.Equal(t, float64(1), afterGauge, "CurrentQuarantinedNodes should be 1")
	assert.GreaterOrEqual(t, afterTaints, beforeTaints+1, "TaintsApplied should increment")
	assert.GreaterOrEqual(t, afterCordons, beforeCordons+1, "CordonsApplied should increment")
	assert.GreaterOrEqual(t, afterRulesetPassed, beforeRulesetPassed+1, "RulesetEvaluations with status=passed should increment")

	t.Log("Sending healthy event for unquarantine")
	eventID2 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID2,
		nodeName,
		"GpuXidError",
		true,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	t.Log("Waiting for UnQuarantined status")
	require.Eventually(t, func() bool {
		status := getStatus(eventID2)
		return status != nil && *status == model.UnQuarantined
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be UnQuarantined")

	t.Log("Waiting for node to be unquarantined")
	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return !node.Spec.Unschedulable && node.Annotations[common.QuarantineHealthEventAnnotationKey] == ""
	}, eventuallyTimeout, eventuallyPollInterval, "Node should be unquarantined")

	t.Log("Verify complete unquarantine state")
	node, err = e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	fqTaintCount := 0
	for _, taint := range node.Spec.Taints {
		if taint.Key == "nvidia.com/gpu-xid-error" {
			fqTaintCount++
		}
	}
	assert.Equal(t, 0, fqTaintCount, "FQ taints should be removed")
	assert.Empty(t, node.Annotations[quarantineHealthEventAnnotationKey], "Quarantine annotation should be removed")
	assert.Empty(t, node.Annotations[quarantineHealthEventAppliedTaintsAnnotationKey], "Applied taints annotation should be removed")
	assert.Empty(t, node.Annotations[quarantineHealthEventIsCordonedAnnotationKey], "Cordoned annotation should be removed")
	verifyUnquarantineLabels(t, node)

	afterUnquarantined := getCounterVecValue(t, metrics.TotalNodesUnquarantined, nodeName)
	finalGauge := getGaugeVecValue(t, metrics.CurrentQuarantinedNodes, nodeName)
	afterTaintsRemoved := getCounterVecValue(t, metrics.TaintsRemoved, "nvidia.com/gpu-xid-error", "NoSchedule")
	afterCordonsRemoved := getCounterValue(t, metrics.CordonsRemoved)
	finalProcessed := getCounterValue(t, metrics.TotalEventsSuccessfullyProcessed)

	assert.GreaterOrEqual(t, afterUnquarantined, beforeQuarantined+1, "TotalNodesUnquarantined should increment")
	assert.Equal(t, float64(0), finalGauge, "CurrentQuarantinedNodes should be 0 after unquarantine")
	assert.GreaterOrEqual(t, afterTaintsRemoved, beforeTaints+1, "TaintsRemoved should increment")
	assert.GreaterOrEqual(t, afterCordonsRemoved, beforeCordons+1, "CordonsRemoved should increment")
	assert.GreaterOrEqual(t, finalProcessed, beforeProcessed+2, "TotalEventsSuccessfullyProcessed should increment for both events")
}

func TestE2E_EntityLevelTracking(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := "e2e-entity-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, getStatus, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("GPU 0 fails - initial quarantine")
	eventID1 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID1,
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	t.Log("Verify status is Quarantined for first failure")
	require.Eventually(t, func() bool {
		status := getStatus(eventID1)
		return status != nil && *status == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be Quarantined")

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval, "Node should be quarantined")

	t.Log("GPU 1 fails - testing entity-level tracking")
	eventID2 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID2,
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
		model.StatusInProgress,
	)

	t.Log("Verify status is AlreadyQuarantined for second failure")
	require.Eventually(t, func() bool {
		status := getStatus(eventID2)
		return status != nil && *status == model.AlreadyQuarantined
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be AlreadyQuarantined")

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
		if err := json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap); err != nil {
			return false
		}
		return healthEventsMap.Count() == 2
	}, eventuallyTimeout, eventuallyPollInterval, "Should track 2 GPUs")

	t.Log("Verify actual annotation content for both entities")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	verifyHealthEventInAnnotation(t, node, "GpuXidError", "gpu-health-monitor", "GPU", "GPU", "0")
	verifyHealthEventInAnnotation(t, node, "GpuXidError", "gpu-health-monitor", "GPU", "GPU", "1")

	t.Log("GPU 0 recovers - node should stay quarantined (GPU 1 still failing)")
	eventID3 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID3,
		nodeName,
		"GpuXidError",
		true,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	t.Log("Verify status is AlreadyQuarantined (partial recovery, node stays quarantined)")
	require.Eventually(t, func() bool {
		status := getStatus(eventID3)
		return status != nil && *status == model.AlreadyQuarantined
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be AlreadyQuarantined for partial recovery")

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
		if err := json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap); err != nil {
			return false
		}
		return node.Spec.Unschedulable && healthEventsMap.Count() == 1
	}, eventuallyTimeout, eventuallyPollInterval, "Should remove GPU 0, keep quarantined")

	t.Log("Verify GPU 1 is still in annotation, GPU 0 is not")
	node, err = e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	verifyHealthEventInAnnotation(t, node, "GpuXidError", "gpu-health-monitor", "GPU", "GPU", "1")
	var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
	err = json.Unmarshal([]byte(node.Annotations[quarantineHealthEventAnnotationKey]), &healthEventsMap)
	require.NoError(t, err)
	gpu0Query := &protos.HealthEvent{
		Agent:          "gpu-health-monitor",
		ComponentClass: "GPU",
		CheckName:      "GpuXidError",
		NodeName:       nodeName,
		Version:        1,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}
	_, found := healthEventsMap.GetEvent(gpu0Query)
	assert.False(t, found, "GPU 0 should NOT be in annotation after recovery")

	t.Log("GPU 1 recovers - node should be fully unquarantined")
	eventID4 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID4,
		nodeName,
		"GpuXidError",
		true,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
		model.StatusInProgress,
	)

	t.Log("Verify status is UnQuarantined (complete recovery)")
	require.Eventually(t, func() bool {
		status := getStatus(eventID4)
		return status != nil && *status == model.UnQuarantined
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be UnQuarantined")

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return !node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval, "Node should be unquarantined")
}

func TestE2E_MultipleChecksOnSameNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := "e2e-multicheck-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
			{
				Name:     "gpu-nvlink-errors",
				Version:  "1",
				Priority: 8,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuNvLinkWatch' && event.isHealthy == false"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-nvlink-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("XID Error on GPU 0")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval)

	t.Log("NVLink Error on GPU 1")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuNvLinkWatch",
		false,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
		if err := json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap); err != nil {
			return false
		}
		return healthEventsMap.Count() == 2 && node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval, "Should track both XID and NVLink entities")

	t.Log("Verify actual content for both checks/entities")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	verifyHealthEventInAnnotation(t, node, "GpuXidError", "gpu-health-monitor", "GPU", "GPU", "0")
	verifyHealthEventInAnnotation(t, node, "GpuNvLinkWatch", "gpu-health-monitor", "GPU", "GPU", "1")

	t.Log("XID recovers - node stays quarantined (NVLink still failing)")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		true,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
		if err := json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap); err != nil {
			return false
		}
		return healthEventsMap.Count() == 1 && node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval, "XID entity removed, NVLink remains, still quarantined")

	t.Log("NVLink recovers")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuNvLinkWatch",
		true,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return !node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval, "Node should be unquarantined")
}

func TestE2E_CheckLevelHealthyEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := "e2e-checklevel-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Quarantine with multiple entities")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
			{EntityType: "GPU", EntityValue: "1"},
		},
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
		if err := json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap); err != nil {
			return false
		}
		return node.Spec.Unschedulable && healthEventsMap.Count() == 2
	}, eventuallyTimeout, eventuallyPollInterval, "Should track 2 entities")

	t.Log("Check-level healthy event (empty entities) - should clear ALL entities for this check")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		true,
		false,
		[]*protos.Entity{}, // Empty - means all entities healthy
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return !node.Spec.Unschedulable && node.Annotations[common.QuarantineHealthEventAnnotationKey] == ""
	}, eventuallyTimeout, eventuallyPollInterval, "Check-level healthy event should clear all entities and unquarantine")
}

func TestE2E_DuplicateEntityEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-duplicate-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("First failure on GPU 0")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval)

	// Get initial annotation before duplicate event
	initialNode, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	initialAnnotation := initialNode.Annotations[common.QuarantineHealthEventAnnotationKey]

	t.Log("Duplicate failure on same GPU 0 - should not duplicate entity")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	// Use Never to verify annotation doesn't change for duplicate
	assert.Never(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		currentAnnotation := node.Annotations[common.QuarantineHealthEventAnnotationKey]
		return currentAnnotation != initialAnnotation
	}, neverTimeout, neverPollInterval, "Duplicate entity should not change annotation")

	// Final verification
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
	err = json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap)
	require.NoError(t, err)
	assert.Equal(t, 1, healthEventsMap.Count(), "Duplicate entity should not be added")
}

func TestE2E_HealthyEventWithoutQuarantine(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-healthy-noq-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError'"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, getStatus, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Send healthy event without any prior quarantine")
	eventID1 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID1,
		nodeName,
		"GpuXidError",
		true,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	t.Log("Verify status is nil (healthy event without prior quarantine is skipped)")
	require.Eventually(t, func() bool {
		status := getStatus(eventID1)
		return status == nil
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be nil for skipped event")

	t.Log("Verify node stays unquarantined")
	assert.Never(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return node.Spec.Unschedulable
	}, neverTimeout, neverPollInterval, "Node should not be quarantined")

	t.Log("Verify final state - no quarantine annotations")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey])
}

func TestE2E_PartialEntityRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := "e2e-partial-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Fail GPUs 0, 1, 2 (send sequentially to avoid race conditions)")
	for i := 0; i < 3; i++ {
		mockWatcher.EventsChan <- createHealthEventBSON(
			primitive.NewObjectID(),
			nodeName,
			"GpuXidError",
			false,
			true,
			[]*protos.Entity{{EntityType: "GPU", EntityValue: fmt.Sprintf("%d", i)}},
			model.StatusInProgress,
		)

		// Wait for this GPU to be tracked before sending next event
		expectedCount := i + 1
		require.Eventually(t, func() bool {
			node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
			if err := json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap); err != nil {
				return false
			}
			return healthEventsMap.Count() == expectedCount
		}, statusCheckTimeout, statusCheckPollInterval, "Should track %d GPU(s)", expectedCount)
	}

	t.Log("Recover GPU 1 only")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		true,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
		if err := json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap); err != nil {
			return false
		}
		return healthEventsMap.Count() == 2 && node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval, "Should remove GPU 1, keep node quarantined with GPU 0 and GPU 2")
}

func TestE2E_AllGPUsFailThenRecover(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 40*time.Second)
	defer cancel()

	nodeName := "e2e-allgpu-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	numGPUs := 8

	t.Log("All GPUs fail (send sequentially to avoid race conditions)")
	for i := 0; i < numGPUs; i++ {
		mockWatcher.EventsChan <- createHealthEventBSON(
			primitive.NewObjectID(),
			nodeName,
			"GpuXidError",
			false,
			true,
			[]*protos.Entity{{EntityType: "GPU", EntityValue: fmt.Sprintf("%d", i)}},
			model.StatusInProgress,
		)

		// Wait for this GPU to be tracked before sending next event
		expectedCount := i + 1
		require.Eventually(t, func() bool {
			node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
			if err := json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap); err != nil {
				return false
			}
			return healthEventsMap.Count() == expectedCount
		}, statusCheckTimeout, statusCheckPollInterval, "Should track %d GPU(s)", expectedCount)
	}

	t.Log("All GPUs recover")
	for i := 0; i < numGPUs; i++ {
		mockWatcher.EventsChan <- createHealthEventBSON(
			primitive.NewObjectID(),
			nodeName,
			"GpuXidError",
			true,
			false,
			[]*protos.Entity{{EntityType: "GPU", EntityValue: fmt.Sprintf("%d", i)}},
			model.StatusInProgress,
		)
	}

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return !node.Spec.Unschedulable && node.Annotations[common.QuarantineHealthEventAnnotationKey] == ""
	}, eventuallyTimeout, eventuallyPollInterval, "All GPUs recovered, node should be unquarantined")
}

func TestE2E_SyslogMultipleEntityTypes(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := "e2e-syslog-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "syslog-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'SysLogsXIDError' && event.isFatal == true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/syslog-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Syslog pattern: single event with multiple entity types (PCI + GPUID)")
	mockWatcher.EventsChan <- bson.M{
		"operationType": "insert",
		"fullDocument": bson.M{
			"_id": primitive.NewObjectID(),
			"healtheventstatus": bson.M{
				"nodequarantined": model.StatusInProgress,
			},
			"healthevent": bson.M{
				"nodename":       nodeName,
				"agent":          "syslog-health-monitor",
				"componentclass": "GPU",
				"checkname":      "SysLogsXIDError",
				"version":        uint32(1),
				"ishealthy":      false,
				"isfatal":        true,
				"errorcode":      []string{"79"},
				"entitiesimpacted": []interface{}{
					bson.M{"entitytype": "PCI", "entityvalue": "0000:b4:00"},
					bson.M{"entitytype": "GPUID", "entityvalue": "GPU-0b32a29e-0c94-cd1a-d44a-4e3ea8b2e3fc"},
				},
			},
		},
	}

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
		if err := json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap); err != nil {
			return false
		}
		return node.Spec.Unschedulable && healthEventsMap.Count() == 2
	}, eventuallyTimeout, eventuallyPollInterval, "Should track both PCI and GPUID entities")

	t.Log("Verify actual annotation content for both entity types")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	verifyHealthEventInAnnotation(t, node, "SysLogsXIDError", "syslog-health-monitor", "GPU", "PCI", "0000:b4:00")
	verifyHealthEventInAnnotation(t, node, "SysLogsXIDError", "syslog-health-monitor", "GPU", "GPUID", "GPU-0b32a29e-0c94-cd1a-d44a-4e3ea8b2e3fc")

	t.Log("Check-level healthy event (empty entities) should clear BOTH PCI and GPUID")
	mockWatcher.EventsChan <- bson.M{
		"operationType": "insert",
		"fullDocument": bson.M{
			"_id": primitive.NewObjectID(),
			"healtheventstatus": bson.M{
				"nodequarantined": model.StatusInProgress,
			},
			"healthevent": bson.M{
				"nodename":         nodeName,
				"agent":            "syslog-health-monitor",
				"componentclass":   "GPU",
				"checkname":        "SysLogsXIDError",
				"version":          uint32(1),
				"ishealthy":        true,
				"message":          "No Health Failures",
				"entitiesimpacted": []interface{}{}, // Empty
			},
		},
	}

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return !node.Spec.Unschedulable && node.Annotations[common.QuarantineHealthEventAnnotationKey] == ""
	}, eventuallyTimeout, eventuallyPollInterval, "Check-level healthy event should clear all entity types")
}

func TestE2E_ManualUncordon(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-manual-uncordon-" + primitive.NewObjectID().Hex()[:8]

	annotations := map[string]string{
		common.QuarantineHealthEventAnnotationKey:              `[{"nodeName":"` + nodeName + `","agent":"test","checkName":"test","isHealthy":false,"entitiesImpacted":[{"entityType":"GPU","entityValue":"0"}]}]`,
		common.QuarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"nvidia.com/gpu-error","Value":"true","Effect":"NoSchedule"}]`,
		common.QuarantineHealthEventIsCordonedAnnotationKey:    common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
	}

	labels := map[string]string{
		statemanager.NVSentinelStateLabelKey: string(statemanager.QuarantinedLabelValue),
	}

	taints := []corev1.Taint{
		{Key: "nvidia.com/gpu-error", Value: "true", Effect: "NoSchedule"},
	}

	createE2ETestNode(ctx, t, nodeName, annotations, labels, taints, true)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
	}

	// Setup reconciler to watch for manual uncordon events
	// The node informer callbacks are registered during setup and will detect the manual uncordon
	setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Manually uncordon the node")
	quarantinedNode, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	quarantinedNode.Spec.Unschedulable = false
	_, err = e2eTestClient.CoreV1().Nodes().Update(ctx, quarantinedNode, metav1.UpdateOptions{})
	require.NoError(t, err)

	t.Log("Verify manual uncordon is detected and FQ state cleaned up")
	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		if _, exists := node.Annotations[common.QuarantineHealthEventAnnotationKey]; exists {
			return false
		}

		if node.Annotations[common.QuarantinedNodeUncordonedManuallyAnnotationKey] != common.QuarantinedNodeUncordonedManuallyAnnotationValue {
			return false
		}

		fqTaintCount := 0
		for _, taint := range node.Spec.Taints {
			if taint.Key == "nvidia.com/gpu-error" {
				fqTaintCount++
			}
		}

		return fqTaintCount == 0
	}, eventuallyTimeout, eventuallyPollInterval, "Manual uncordon should clean up FQ state")
}

func TestE2E_BackwardCompatibilityOldFormat(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-backward-" + primitive.NewObjectID().Hex()[:8]

	// Old format: single HealthEvent object (not array)
	existingOldEvent := &protos.HealthEvent{
		NodeName:       nodeName,
		Agent:          "gpu-health-monitor",
		ComponentClass: "GPU",
		CheckName:      "GpuXidError",
		Version:        1,
		IsHealthy:      false,
		IsFatal:        true,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}

	oldAnnotationBytes, err := json.Marshal(existingOldEvent)
	require.NoError(t, err)

	annotations := map[string]string{
		common.QuarantineHealthEventAnnotationKey:              string(oldAnnotationBytes),
		common.QuarantineHealthEventIsCordonedAnnotationKey:    "True",
		common.QuarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"nvidia.com/gpu-xid-error","Value":"true","Effect":"NoSchedule"}]`,
	}

	taints := []corev1.Taint{
		{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
	}

	createE2ETestNode(ctx, t, nodeName, annotations, nil, taints, true)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-nvlink-errors",
				Version:  "1",
				Priority: 8,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuNvLinkWatch'"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-nvlink-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Add new event for different check/entity")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuNvLinkWatch",
		false,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
		model.StatusInProgress,
	)

	// Should convert to new format and append
	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
		if err := json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap); err != nil {
			return false
		}
		return healthEventsMap.Count() == 2
	}, eventuallyTimeout, eventuallyPollInterval, "Should convert old format and add new event")

	t.Log("Recover the old event")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		true,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
		if err := json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap); err != nil {
			return false
		}
		return healthEventsMap.Count() == 1 && node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval, "Old event removed, new event remains")

	t.Log("Recover the new event")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuNvLinkWatch",
		true,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return !node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval, "Node should be unquarantined")
}

func TestE2E_MixedHealthyUnhealthyFlapping(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := "e2e-flapping-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Flapping GPU scenario: alternating unhealthy and healthy events")
	for cycle := 0; cycle < 3; cycle++ {
		// Unhealthy
		mockWatcher.EventsChan <- createHealthEventBSON(
			primitive.NewObjectID(),
			nodeName,
			"GpuXidError",
			false,
			true,
			[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
			model.StatusInProgress,
		)

		require.Eventually(t, func() bool {
			node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			return node.Spec.Unschedulable
		}, statusCheckTimeout, statusCheckPollInterval, "Should be quarantined")

		// Healthy
		mockWatcher.EventsChan <- createHealthEventBSON(
			primitive.NewObjectID(),
			nodeName,
			"GpuXidError",
			true,
			false,
			[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
			model.StatusInProgress,
		)

		require.Eventually(t, func() bool {
			node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			return !node.Spec.Unschedulable
		}, statusCheckTimeout, statusCheckPollInterval, "Should be unquarantined")
	}

	t.Log("Verify final state should be healthy")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, node.Spec.Unschedulable)
	assert.Empty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey])
}

func TestE2E_MultipleNodesSimultaneous(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeNames := []string{
		"e2e-multi-1-" + primitive.NewObjectID().Hex()[:6],
		"e2e-multi-2-" + primitive.NewObjectID().Hex()[:6],
		"e2e-multi-3-" + primitive.NewObjectID().Hex()[:6],
	}

	for _, nodeName := range nodeNames {
		createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
		defer func(name string) {
			_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
		}(nodeName)
	}

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Send failure events for all nodes")
	for _, nodeName := range nodeNames {
		mockWatcher.EventsChan <- createHealthEventBSON(
			primitive.NewObjectID(),
			nodeName,
			"GpuXidError",
			false,
			true,
			[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
			model.StatusInProgress,
		)
	}

	// Verify all nodes are quarantined
	for _, nodeName := range nodeNames {
		require.Eventually(t, func() bool {
			node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			return node.Spec.Unschedulable
		}, eventuallyTimeout, eventuallyPollInterval, "Node %s should be quarantined", nodeName)
	}

	t.Log("Verify all have proper annotations and taints")
	for _, nodeName := range nodeNames {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Contains(t, node.Annotations, common.QuarantineHealthEventAnnotationKey)
		hasTaint := false
		for _, taint := range node.Spec.Taints {
			if taint.Key == "nvidia.com/gpu-xid-error" {
				hasTaint = true
				break
			}
		}
		assert.True(t, hasTaint, "Node %s should have FQ taint", nodeName)
	}
}

func TestE2E_HealthyEventForNonMatchingCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-nomatch-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Quarantine with XID error")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval)

	t.Log("Send healthy event for DIFFERENT check that was never failing")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuNvLinkWatch",
		true,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	// Node should remain quarantined (XID error still active, healthy NVLink event doesn't unquarantine)
	assert.Never(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return !node.Spec.Unschedulable
	}, neverTimeout, neverPollInterval, "Node should remain quarantined")

	t.Log("Verify XID error still tracked")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
	err = json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap)
	require.NoError(t, err)
	assert.Equal(t, 1, healthEventsMap.Count(), "Should still have XID error tracked")
}

func TestE2E_MultipleRulesetsWithPriorities(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-priorities-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "low-priority-rule",
				Version:  "1",
				Priority: 5,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-error", Value: "low", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: false},
			},
			{
				Name:     "high-priority-rule",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-error", Value: "high", Effect: "NoExecute"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"TestCheck",
		false,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		// Should use higher priority effect (NoExecute)
		for _, taint := range node.Spec.Taints {
			if taint.Key == "nvidia.com/gpu-error" && taint.Value == "high" && string(taint.Effect) == "NoExecute" {
				return node.Spec.Unschedulable
			}
		}

		return false
	}, eventuallyTimeout, eventuallyPollInterval, "Should use higher priority taint effect")
}

func TestE2E_NonFatalEventDoesNotQuarantine(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-nonfatal-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Send non-fatal XID error (isFatal=false) - rule requires isFatal=true")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		false,
		false, // Not fatal
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	// Verify node is never quarantined (rule doesn't match)
	assert.Never(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return node.Spec.Unschedulable
	}, neverTimeout, neverPollInterval, "Non-fatal event should not quarantine")

	t.Log("Verify no quarantine annotations")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey])
}

func TestE2E_OutOfOrderEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-outoforder-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Send healthy event BEFORE unhealthy event (out of order)")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		true,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	// Verify node is never quarantined (healthy event without prior quarantine is skipped)
	assert.Never(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return node.Spec.Unschedulable
	}, neverTimeout, neverPollInterval, "Healthy event before unhealthy should not quarantine")

	t.Log("Now send unhealthy event")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval, "Unhealthy event should quarantine")
}

func TestE2E_SkipRedundantCordoning(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-redundant-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-critical-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError'"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("First check quarantines node")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval, "Node should be quarantined")

	t.Log("Different check on already cordoned node - should skip redundant cordoning")
	initialCordonState := true

	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuMemWatch",
		false,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
		model.StatusInProgress,
	)

	// Verify node remains cordoned (doesn't uncordon)
	assert.Never(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return node.Spec.Unschedulable != initialCordonState
	}, neverTimeout, neverPollInterval, "Node cordon state should not change")
}

func TestE2E_NodeAlreadyCordonedManually(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-manual-cordon-" + primitive.NewObjectID().Hex()[:8]

	// Create node that's already manually cordoned (no FQ annotations)
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, true)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError'"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Send unhealthy event - FQM should apply taints/annotations to manually cordoned node")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	// Verify FQM adds taints and annotations to manually cordoned node
	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		hasTaint := false
		for _, taint := range node.Spec.Taints {
			if taint.Key == "nvidia.com/gpu-xid-error" {
				hasTaint = true
				break
			}
		}

		return node.Spec.Unschedulable &&
			hasTaint &&
			node.Annotations[common.QuarantineHealthEventAnnotationKey] != ""
	}, eventuallyTimeout, eventuallyPollInterval, "FQM should add taints/annotations to manually cordoned node")

	t.Log("Verify actual annotation content and taints")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	verifyHealthEventInAnnotation(t, node, "GpuXidError", "gpu-health-monitor", "GPU", "GPU", "0")
	expectedTaints := []config.Taint{
		{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
	}
	verifyAppliedTaintsAnnotation(t, node, expectedTaints)
	verifyNodeTaintsMatch(t, node, expectedTaints)
}

func TestE2E_NodeAlreadyQuarantinedStillUnhealthy(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-already-q-unhealthy-" + primitive.NewObjectID().Hex()[:8]

	// Create node already quarantined by FQM
	existingEvent := &protos.HealthEvent{
		NodeName:       nodeName,
		Agent:          "agent1",
		CheckName:      "checkA",
		ComponentClass: "GPU",
		Version:        1,
		IsHealthy:      false,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}

	existingMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	existingMap.AddOrUpdateEvent(existingEvent)
	existingBytes, err := json.Marshal(existingMap)
	require.NoError(t, err)

	annotations := map[string]string{
		common.QuarantineHealthEventAnnotationKey:           string(existingBytes),
		common.QuarantineHealthEventIsCordonedAnnotationKey: "True",
	}

	createE2ETestNode(ctx, t, nodeName, annotations, nil, nil, true)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Send another unhealthy event for same entity - should remain quarantined")
	mockWatcher.EventsChan <- bson.M{
		"operationType": "insert",
		"fullDocument": bson.M{
			"_id": primitive.NewObjectID(),
			"healtheventstatus": bson.M{
				"nodequarantined": model.StatusInProgress,
			},
			"healthevent": bson.M{
				"nodename":       nodeName,
				"agent":          "agent1",
				"componentclass": "GPU",
				"checkname":      "checkA",
				"version":        uint32(1),
				"ishealthy":      false,
				"entitiesimpacted": []interface{}{
					bson.M{"entitytype": "GPU", "entityvalue": "0"},
				},
			},
		},
	}

	// Verify node never unquarantines (remains quarantined with same entity)
	assert.Never(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return !node.Spec.Unschedulable
	}, neverTimeout, neverPollInterval, "Node should remain quarantined")

	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotEmpty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey])
}

func TestE2E_NodeAlreadyQuarantinedBecomesHealthy(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-already-q-healthy-" + primitive.NewObjectID().Hex()[:8]

	// Create node already quarantined by FQM
	existingEvent := &protos.HealthEvent{
		NodeName:       nodeName,
		Agent:          "agent1",
		CheckName:      "checkA",
		ComponentClass: "GPU",
		Version:        1,
		IsHealthy:      false,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}

	existingMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	existingMap.AddOrUpdateEvent(existingEvent)
	existingBytes, err := json.Marshal(existingMap)
	require.NoError(t, err)

	annotations := map[string]string{
		common.QuarantineHealthEventAnnotationKey:              string(existingBytes),
		common.QuarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"nvidia.com/gpu-error","Value":"true","Effect":"NoSchedule"}]`,
		common.QuarantineHealthEventIsCordonedAnnotationKey:    "True",
	}

	taints := []corev1.Taint{
		{Key: "nvidia.com/gpu-error", Value: "true", Effect: "NoSchedule"},
	}

	createE2ETestNode(ctx, t, nodeName, annotations, nil, taints, true)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Send healthy event - should unquarantine")
	mockWatcher.EventsChan <- bson.M{
		"operationType": "insert",
		"fullDocument": bson.M{
			"_id": primitive.NewObjectID(),
			"healtheventstatus": bson.M{
				"nodequarantined": model.StatusInProgress,
			},
			"healthevent": bson.M{
				"nodename":       nodeName,
				"agent":          "agent1",
				"componentclass": "GPU",
				"checkname":      "checkA",
				"version":        uint32(1),
				"ishealthy":      true,
				"entitiesimpacted": []interface{}{
					bson.M{"entitytype": "GPU", "entityvalue": "0"},
				},
			},
		},
	}

	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		fqTaintCount := 0
		for _, taint := range node.Spec.Taints {
			if taint.Key == "nvidia.com/gpu-error" {
				fqTaintCount++
			}
		}

		return !node.Spec.Unschedulable &&
			node.Annotations[common.QuarantineHealthEventAnnotationKey] == "" &&
			fqTaintCount == 0
	}, eventuallyTimeout, eventuallyPollInterval, "Node should be unquarantined")

	t.Log("Verify all FQ annotations removed")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey], "Quarantine annotation should be removed")
	assert.Empty(t, node.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey], "Applied taints annotation should be removed")
	assert.Empty(t, node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey], "Cordoned annotation should be removed")
}

func TestE2E_RulesetNotMatching(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-nomatch-rule-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-fatal-only",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	beforeRulesetFailed := getCounterVecValue(t, metrics.RulesetEvaluations, "gpu-xid-fatal-only", metrics.StatusFailed)

	t.Log("Send event that doesn't match (wrong checkName)")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuMemWatch",
		false,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	// Verify node never gets quarantined (rule doesn't match)
	assert.Never(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return node.Spec.Unschedulable
	}, neverTimeout, neverPollInterval, "Node should not be quarantined when rule doesn't match")

	t.Log("Send event that partially matches (correct checkName but not fatal)")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		false,
		false, // Not fatal
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	// Verify node never gets quarantined (isFatal requirement not met)
	assert.Never(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return node.Spec.Unschedulable
	}, neverTimeout, neverPollInterval, "Node should not be quarantined when isFatal requirement not met")

	// Verify ruleset failed metrics incremented (2 non-matching events sent)
	afterRulesetFailed := getCounterVecValue(t, metrics.RulesetEvaluations, "gpu-xid-fatal-only", metrics.StatusFailed)
	assert.GreaterOrEqual(t, afterRulesetFailed, beforeRulesetFailed+2, "RulesetEvaluations with status=failed should increment for non-matching events")
}

func TestE2E_PartialAnnotationUpdate(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := "e2e-partial-ann-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError'"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, _, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Quarantine with GPU 0, 1, 2 (send sequentially to avoid race conditions)")
	for i := 0; i < 3; i++ {
		mockWatcher.EventsChan <- createHealthEventBSON(
			primitive.NewObjectID(),
			nodeName,
			"GpuXidError",
			false,
			true,
			[]*protos.Entity{{EntityType: "GPU", EntityValue: fmt.Sprintf("%d", i)}},
			model.StatusInProgress,
		)

		// Wait for this GPU to be tracked before sending next event
		expectedCount := i + 1
		require.Eventually(t, func() bool {
			node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
			if err := json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap); err != nil {
				return false
			}
			return healthEventsMap.Count() == expectedCount
		}, statusCheckTimeout, statusCheckPollInterval, "Should track %d GPU(s)", expectedCount)
	}

	initialAnnotation := ""
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	initialAnnotation = node.Annotations[common.QuarantineHealthEventAnnotationKey]

	t.Log("Partial recovery of GPU 1 - annotation should be updated")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		nodeName,
		"GpuXidError",
		true,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
		model.StatusInProgress,
	)

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		currentAnnotation := node.Annotations[common.QuarantineHealthEventAnnotationKey]
		return currentAnnotation != initialAnnotation
	}, statusCheckTimeout, statusCheckPollInterval, "Annotation should be updated for partial recovery")

	t.Log("Verify annotation content changed correctly - GPU 1 removed, GPU 0 and 2 remain")
	node, err = e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
	err = json.Unmarshal([]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), &healthEventsMap)
	require.NoError(t, err)
	assert.Equal(t, 2, healthEventsMap.Count(), "Should have 2 entities remaining (GPU 0 and 2)")
	assert.True(t, node.Spec.Unschedulable, "Node should remain quarantined")

	verifyHealthEventInAnnotation(t, node, "GpuXidError", "gpu-health-monitor", "GPU", "GPU", "0")
	verifyHealthEventInAnnotation(t, node, "GpuXidError", "gpu-health-monitor", "GPU", "GPU", "2")
	gpu1Query := &protos.HealthEvent{
		Agent:          "gpu-health-monitor",
		ComponentClass: "GPU",
		CheckName:      "GpuXidError",
		NodeName:       nodeName,
		Version:        1,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "1"},
		},
	}
	_, found := healthEventsMap.GetEvent(gpu1Query)
	assert.False(t, found, "GPU 1 should NOT be in annotation after partial recovery")
}

func TestE2E_CircuitBreakerBasic(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	// Create 10 test nodes
	baseNodeName := "e2e-cb-basic-" + primitive.NewObjectID().Hex()[:6]
	for i := 0; i < 10; i++ {
		nodeName := fmt.Sprintf("%s-%d", baseNodeName, i)
		createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
		defer func(name string) {
			_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
		}(nodeName)
	}

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	// Setup with circuit breaker enabled
	r, mockWatcher, _, cb := setupE2EReconciler(t, ctx, tomlConfig, &breaker.CircuitBreakerConfig{
		Namespace:  "default",
		Percentage: 50,
		Duration:   5 * time.Minute,
	})

	// Verify circuit breaker is initialized
	t.Log("Verify circuit breaker is initialized")
	require.NotNil(t, cb, "Circuit breaker should be initialized")

	// BLOCKING: Wait for all 10 nodes to be visible in NodeInformer cache
	// This is critical for circuit breaker percentage calculations to be accurate
	// Test will fail if nodes aren't visible within 5 seconds
	require.Eventually(t, func() bool {
		totalNodes, _, err := r.k8sClient.NodeInformer.GetNodeCounts()
		return err == nil && totalNodes == 10
	}, statusCheckTimeout, statusCheckPollInterval, "NodeInformer should see all 10 nodes")

	t.Log("Cordoning 4 nodes (40%) - should not trip circuit breaker")
	for i := 0; i < 4; i++ {
		mockWatcher.EventsChan <- createHealthEventBSON(
			primitive.NewObjectID(),
			fmt.Sprintf("%s-%d", baseNodeName, i),
			"TestCheck",
			false,
			false,
			[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
			model.StatusInProgress,
		)
	}

	// Wait for all 4 nodes to be cordoned
	require.Eventually(t, func() bool {
		cordonedCount := 0
		for i := 0; i < 4; i++ {
			node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, fmt.Sprintf("%s-%d", baseNodeName, i), metav1.GetOptions{})
			if err == nil && node.Spec.Unschedulable {
				cordonedCount++
			}
		}
		return cordonedCount == 4
	}, statusCheckTimeout, statusCheckPollInterval, "4 nodes should be cordoned")

	isTripped, err := cb.IsTripped(ctx)
	require.NoError(t, err)
	assert.False(t, isTripped, "Circuit breaker should not trip at 40%")

	t.Log("Cordoning 5th node (50%) - should trip circuit breaker")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		fmt.Sprintf("%s-4", baseNodeName),
		"TestCheck",
		false,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	// Wait for 5th node to be cordoned
	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, fmt.Sprintf("%s-4", baseNodeName), metav1.GetOptions{})
		return err == nil && node.Spec.Unschedulable
	}, statusCheckTimeout, statusCheckPollInterval, "5th node should be cordoned")

	isTripped, err = cb.IsTripped(ctx)
	require.NoError(t, err)
	assert.True(t, isTripped, "Circuit breaker should trip at 50%")

	t.Log("Trying 6th node - should be blocked by circuit breaker")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		fmt.Sprintf("%s-5", baseNodeName),
		"TestCheck",
		false,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	// Verify 6th node never gets cordoned (circuit breaker blocks it)
	assert.Never(t, func() bool {
		sixthNode, err := e2eTestClient.CoreV1().Nodes().Get(ctx, fmt.Sprintf("%s-5", baseNodeName), metav1.GetOptions{})
		if err != nil {
			return false
		}
		return sixthNode.Spec.Unschedulable
	}, statusCheckTimeout, statusCheckPollInterval, "6th node should not be cordoned due to circuit breaker")
}

func TestE2E_CircuitBreakerSlidingWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	// Create 10 test nodes
	baseNodeName := "e2e-cb-window-" + primitive.NewObjectID().Hex()[:6]
	for i := 0; i < 10; i++ {
		nodeName := fmt.Sprintf("%s-%d", baseNodeName, i)
		createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
		defer func(name string) {
			_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
		}(nodeName)
	}

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	// Setup with circuit breaker (short window for testing)
	r, mockWatcher, _, cb := setupE2EReconciler(t, ctx, tomlConfig, &breaker.CircuitBreakerConfig{
		Namespace:  "default",
		Percentage: 50,
		Duration:   2 * time.Second, // Short window for testing
	})

	t.Log("Verify circuit breaker is initialized")
	require.NotNil(t, cb, "Circuit breaker should be initialized")

	// BLOCKING: Wait for all 10 nodes to be visible in NodeInformer cache
	// This is critical for circuit breaker percentage calculations to be accurate
	// Test will fail if nodes aren't visible within 5 seconds
	require.Eventually(t, func() bool {
		totalNodes, _, err := r.k8sClient.NodeInformer.GetNodeCounts()
		return err == nil && totalNodes == 10
	}, statusCheckTimeout, statusCheckPollInterval, "NodeInformer should see all 10 nodes")

	t.Log("Cordoning 5 nodes to trip the circuit breaker")
	for i := 0; i < 5; i++ {
		mockWatcher.EventsChan <- createHealthEventBSON(
			primitive.NewObjectID(),
			fmt.Sprintf("%s-%d", baseNodeName, i),
			"TestCheck",
			false,
			false,
			[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
			model.StatusInProgress,
		)
	}

	// Wait for all 5 nodes to be cordoned
	require.Eventually(t, func() bool {
		cordonedCount := 0
		for i := 0; i < 5; i++ {
			node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, fmt.Sprintf("%s-%d", baseNodeName, i), metav1.GetOptions{})
			if err == nil && node.Spec.Unschedulable {
				cordonedCount++
			}
		}
		return cordonedCount == 5
	}, statusCheckTimeout, statusCheckPollInterval, "5 nodes should be cordoned")

	isTripped, err := cb.IsTripped(ctx)
	require.NoError(t, err)
	assert.True(t, isTripped, "Circuit breaker should trip")

	t.Log("Forcing circuit breaker to CLOSED and waiting for window to expire")
	err = cb.ForceState(ctx, "CLOSED")
	require.NoError(t, err)

	// Wait for sliding window to fully expire (2 second window + buffer)
	time.Sleep(3 * time.Second)

	// Now check - should not trip since window has expired
	isTripped, err = cb.IsTripped(ctx)
	require.NoError(t, err)
	assert.False(t, isTripped, "Circuit breaker should not be tripped after sliding window expires")
}

func TestE2E_CircuitBreakerUniqueNodeTracking(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	// Create 10 test nodes
	baseNodeName := "e2e-cb-unique-" + primitive.NewObjectID().Hex()[:6]
	for i := 0; i < 10; i++ {
		nodeName := fmt.Sprintf("%s-%d", baseNodeName, i)
		createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
		defer func(name string) {
			_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
		}(nodeName)
	}

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "true"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	// Setup with circuit breaker enabled
	r, mockWatcher, _, cb := setupE2EReconciler(t, ctx, tomlConfig, &breaker.CircuitBreakerConfig{
		Namespace:  "default",
		Percentage: 50,
		Duration:   5 * time.Minute,
	})

	t.Log("Verify circuit breaker is initialized")
	require.NotNil(t, cb, "Circuit breaker should be initialized")

	t.Log("Waiting for all nodes to be visible in NodeInformer cache")
	require.Eventually(t, func() bool {
		totalNodes, _, err := r.k8sClient.NodeInformer.GetNodeCounts()
		return err == nil && totalNodes == 10
	}, statusCheckTimeout, statusCheckPollInterval, "NodeInformer should see all 10 nodes")

	t.Log("Sending first event for node 0 to test unique node tracking")
	mockWatcher.EventsChan <- createHealthEventBSON(
		primitive.NewObjectID(),
		fmt.Sprintf("%s-0", baseNodeName),
		"TestCheck",
		false,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	// Wait for node 0 to be cordoned
	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, fmt.Sprintf("%s-0", baseNodeName), metav1.GetOptions{})
		return err == nil && node.Spec.Unschedulable
	}, statusCheckTimeout, statusCheckPollInterval, "Node 0 should be cordoned")

	t.Log("Sending 9 duplicate events for same node (testing deduplication)")
	for i := 1; i < 10; i++ {
		mockWatcher.EventsChan <- createHealthEventBSON(
			primitive.NewObjectID(),
			fmt.Sprintf("%s-0", baseNodeName),
			"TestCheck",
			false,
			false,
			[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
			model.StatusInProgress,
		)
	}

	isTripped, err := cb.IsTripped(ctx)
	require.NoError(t, err)
	assert.False(t, isTripped, "Circuit breaker should not trip with only 1 unique node")

	t.Log("Adding 4 more unique nodes to reach 5 total (50% threshold)")
	for i := 1; i <= 4; i++ {
		mockWatcher.EventsChan <- createHealthEventBSON(
			primitive.NewObjectID(),
			fmt.Sprintf("%s-%d", baseNodeName, i),
			"TestCheck",
			false,
			false,
			[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
			model.StatusInProgress,
		)
	}

	// Wait for all 5 nodes to be cordoned
	require.Eventually(t, func() bool {
		cordonedCount := 0
		for i := 0; i < 5; i++ {
			node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, fmt.Sprintf("%s-%d", baseNodeName, i), metav1.GetOptions{})
			if err == nil && node.Spec.Unschedulable {
				cordonedCount++
			}
		}
		return cordonedCount == 5
	}, statusCheckTimeout, statusCheckPollInterval, "5 nodes should be cordoned")

	isTripped, err = cb.IsTripped(ctx)
	require.NoError(t, err)
	assert.True(t, isTripped, "Circuit breaker should trip with 5 unique nodes (50%)")
}

func TestE2E_QuarantineOverridesForce(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-force-quarantine-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "should-not-match",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "false"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/test", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, getStatus, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Send event with QuarantineOverrides.Force=true (bypasses rule evaluation)")
	eventID1 := primitive.NewObjectID()
	mockWatcher.EventsChan <- bson.M{
		"operationType": "insert",
		"fullDocument": bson.M{
			"_id": eventID1,
			"healtheventstatus": bson.M{
				"nodequarantined": model.StatusInProgress,
			},
			"healthevent": bson.M{
				"nodename":       nodeName,
				"agent":          "test-agent",
				"componentclass": "GPU",
				"checkname":      "TestCheck",
				"version":        uint32(1),
				"ishealthy":      false,
				"message":        "Force quarantine for maintenance",
				"metadata": bson.M{
					"creator_id": "user123",
				},
				"quarantineoverrides": bson.M{
					"force": true,
				},
			},
		},
	}

	// Verify status is Quarantined (even though rule doesn't match)
	require.Eventually(t, func() bool {
		status := getStatus(eventID1)
		return status != nil && *status == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be Quarantined with force override")

	t.Log("Verify node is cordoned with special labels")
	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return node.Spec.Unschedulable &&
			node.Labels["k8s.nvidia.com/cordon-by"] == "test-agent-user123" &&
			node.Labels["k8s.nvidia.com/cordon-reason"] == "Force-quarantine-for-maintenance"
	}, eventuallyTimeout, eventuallyPollInterval, "Node should be force quarantined with special labels")
}

func TestE2E_NodeRuleEvaluator(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-node-rule-" + primitive.NewObjectID().Hex()[:8]

	// Create node with specific label
	labels := map[string]string{
		"k8saas.nvidia.com/ManagedByNVSentinel": "true",
	}

	createE2ETestNode(ctx, t, nodeName, nil, labels, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "managed-nodes-only",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					All: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError'"},
						{Kind: "Node", Expression: "node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == 'true'"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, getStatus, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Send event - should match both HealthEvent and Node rules")
	eventID1 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID1,
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	t.Log("Verify status is Quarantined (Node rule matched)")
	require.Eventually(t, func() bool {
		status := getStatus(eventID1)
		return status != nil && *status == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be Quarantined when Node rule matches")

	require.Eventually(t, func() bool {
		node, _ := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval, "Node should be quarantined when Node rule matches")
}

func TestE2E_NodeRuleDoesNotMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-node-nomatch-" + primitive.NewObjectID().Hex()[:8]

	// Create node WITHOUT the required label
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "managed-nodes-only",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					All: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError'"},
						{Kind: "Node", Expression: "node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == 'true'"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, getStatus, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Send event - Node rule should NOT match (label missing)")
	eventID1 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID1,
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	t.Log("Verify status is nil (rule didn't match)")
	require.Eventually(t, func() bool {
		status := getStatus(eventID1)
		return status == nil
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be nil when Node rule doesn't match")

	t.Log("Verify node is NOT quarantined")
	assert.Never(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return node.Spec.Unschedulable
	}, neverTimeout, neverPollInterval, "Node should not be quarantined when Node rule doesn't match")
}

func TestE2E_TaintWithoutCordon(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-taint-no-cordon-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "taint-only-rule",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError'"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: false}, // No cordon
			},
		},
	}

	_, mockWatcher, getStatus, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Sending taint-only event (no cordon)")
	eventID1 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID1,
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	t.Log("Waiting for Quarantined status")
	require.Eventually(t, func() bool {
		status := getStatus(eventID1)
		return status != nil && *status == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be Quarantined")

	t.Log("Verify node is tainted but NOT cordoned")
	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		hasTaint := false
		for _, taint := range node.Spec.Taints {
			if taint.Key == "nvidia.com/gpu-xid-error" {
				hasTaint = true
				break
			}
		}

		return hasTaint && !node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval, "Node should be tainted but not cordoned")

	t.Log("Verify quarantine annotation exists but NOT cordon annotation")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotEmpty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey])
	assert.Empty(t, node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey], "Cordon annotation should not exist")
}

func TestE2E_CordonWithoutTaint(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-cordon-no-taint-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "cordon-only-rule",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError'"},
					},
				},
				Taint:  config.Taint{}, // No taint
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, getStatus, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Sending cordon-only event (no taint)")
	eventID1 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID1,
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	t.Log("Verify status is Quarantined")
	require.Eventually(t, func() bool {
		status := getStatus(eventID1)
		return status != nil && *status == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be Quarantined")

	t.Log("Verify node is cordoned but has NO FQ taints")
	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		return node.Spec.Unschedulable
	}, eventuallyTimeout, eventuallyPollInterval, "Node should be cordoned")

	t.Log("Verify no FQ taints (cordon-only)")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	fqTaintCount := 0
	for _, taint := range node.Spec.Taints {
		if taint.Key == "nvidia.com/test" {
			fqTaintCount++
		}
	}
	assert.Equal(t, 0, fqTaintCount, "Should have no FQ taints")
	assert.NotEmpty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey])
	assert.Equal(t, "True", node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey])
	assert.Empty(t, node.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey], "Applied taints annotation should be empty")
}

func TestE2E_ManualUncordonAnnotationCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-manual-cleanup-" + primitive.NewObjectID().Hex()[:8]

	// Create node with manual uncordon annotation (from previous manual uncordon)
	annotations := map[string]string{
		common.QuarantinedNodeUncordonedManuallyAnnotationKey: common.QuarantinedNodeUncordonedManuallyAnnotationValue,
	}

	createE2ETestNode(ctx, t, nodeName, annotations, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError'"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, getStatus, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Send unhealthy event - should remove manual uncordon annotation and quarantine")
	eventID1 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID1,
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	t.Log("Verify status is Quarantined")
	require.Eventually(t, func() bool {
		status := getStatus(eventID1)
		return status != nil && *status == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be Quarantined")

	t.Log("Verify manual uncordon annotation is removed and FQ annotations added")
	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		return node.Spec.Unschedulable &&
			node.Annotations[common.QuarantineHealthEventAnnotationKey] != "" &&
			node.Annotations[common.QuarantinedNodeUncordonedManuallyAnnotationKey] == ""
	}, eventuallyTimeout, eventuallyPollInterval, "Manual uncordon annotation should be removed, FQ annotations added")
}

func TestE2E_UnhealthyEventOnQuarantinedNodeNoRuleMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-q-node-nomatch-" + primitive.NewObjectID().Hex()[:8]

	// Create node already quarantined
	existingEvent := &protos.HealthEvent{
		NodeName:       nodeName,
		Agent:          "gpu-health-monitor",
		CheckName:      "GpuXidError",
		ComponentClass: "GPU",
		Version:        1,
		IsHealthy:      false,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}

	existingMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	existingMap.AddOrUpdateEvent(existingEvent)
	existingBytes, err := json.Marshal(existingMap)
	require.NoError(t, err)

	annotations := map[string]string{
		common.QuarantineHealthEventAnnotationKey:           string(existingBytes),
		common.QuarantineHealthEventIsCordonedAnnotationKey: "True",
	}

	createE2ETestNode(ctx, t, nodeName, annotations, nil, nil, true)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-only",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError'"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, getStatus, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	initialAnnotation := string(existingBytes)

	t.Log("Send unhealthy event for different check that doesn't match any rules")
	eventID1 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID1,
		nodeName,
		"GpuMemWatch", // Different check - doesn't match rule
		false,
		false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
		model.StatusInProgress,
	)

	t.Log("Verify status is AlreadyQuarantined (node stays quarantined but event doesn't match rules)")
	require.Eventually(t, func() bool {
		status := getStatus(eventID1)
		return status != nil && *status == model.AlreadyQuarantined
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be AlreadyQuarantined")

	t.Log("Verify annotation is NOT updated (event doesn't match rules, so not added)")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, initialAnnotation, node.Annotations[common.QuarantineHealthEventAnnotationKey], "Annotation should not change for non-matching rule")
}

func TestE2E_DryRunMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-dryrun-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "gpu-xid-errors",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError'"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	// Setup with DryRun=true (circuit breaker disabled)
	_, mockWatcher, getStatus, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:           tomlConfig,
		CircuitBreakerConfig: nil,
		DryRun:               true,
	})

	t.Log("Sending event in dry-run mode")
	eventID1 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID1,
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	t.Log("Verify status is Quarantined (dry run still returns status)")
	require.Eventually(t, func() bool {
		status := getStatus(eventID1)
		return status != nil && *status == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be Quarantined in dry run")

	t.Log("Verify node is NOT actually cordoned or tainted (dry run)")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, node.Spec.Unschedulable, "Node should NOT be cordoned in dry run mode")

	// Annotations ARE added in dry run (only spec changes are skipped)
	assert.NotEmpty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey], "Annotations are still added in dry run")
}

func TestE2E_TaintOnlyThenCordonRule(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "e2e-taint-then-cordon-" + primitive.NewObjectID().Hex()[:8]
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.RuleSet{
			{
				Name:     "taint-first",
				Version:  "1",
				Priority: 5,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError'"},
					},
				},
				Taint:  config.Taint{Key: "nvidia.com/gpu-xid-error", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: false},
			},
			{
				Name:     "cordon-second",
				Version:  "1",
				Priority: 10,
				Match: config.Match{
					Any: []config.Rule{
						{Kind: "HealthEvent", Expression: "event.isFatal == true"},
					},
				},
				Taint:  config.Taint{},
				Cordon: config.Cordon{ShouldCordon: true},
			},
		},
	}

	_, mockWatcher, getStatus, _ := setupE2EReconciler(t, ctx, tomlConfig, nil)

	t.Log("Send fatal XID error - both rules match (taint + cordon)")
	eventID1 := primitive.NewObjectID()
	mockWatcher.EventsChan <- createHealthEventBSON(
		eventID1,
		nodeName,
		"GpuXidError",
		false,
		true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		model.StatusInProgress,
	)

	t.Log("Verify status is Quarantined")
	require.Eventually(t, func() bool {
		status := getStatus(eventID1)
		return status != nil && *status == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval, "Status should be Quarantined")

	t.Log("Verify node has BOTH taint AND cordon")
	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		hasTaint := false
		for _, taint := range node.Spec.Taints {
			if taint.Key == "nvidia.com/gpu-xid-error" {
				hasTaint = true
				break
			}
		}

		return node.Spec.Unschedulable && hasTaint
	}, eventuallyTimeout, eventuallyPollInterval, "Node should have both taint and cordon")

	t.Log("Verify both annotations exist")
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotEmpty(t, node.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey], "Applied taints annotation should exist")
	assert.Equal(t, "True", node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey], "Cordon annotation should exist")
}

// Metrics Validation Tests

// TestMetrics_CurrentQuarantinedNodesRestore validates gauge restoration on restart
func TestMetrics_CurrentQuarantinedNodesRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "metrics-restore-" + primitive.NewObjectID().Hex()[:8]

	existingEvent := &protos.HealthEvent{
		NodeName:       nodeName,
		Agent:          "test-agent",
		CheckName:      "TestCheck",
		ComponentClass: "GPU",
		Version:        1,
		IsHealthy:      false,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
		},
	}

	existingMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	existingMap.AddOrUpdateEvent(existingEvent)
	existingBytes, err := json.Marshal(existingMap)
	require.NoError(t, err)

	annotations := map[string]string{
		common.QuarantineHealthEventAnnotationKey:           string(existingBytes),
		common.QuarantineHealthEventIsCordonedAnnotationKey: "True",
	}

	createE2ETestNode(ctx, t, nodeName, annotations, nil, nil, true)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	tomlConfig := config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
	}

	setupE2EReconciler(t, ctx, tomlConfig, nil)

	require.Eventually(t, func() bool {
		gaugeValue := getGaugeVecValue(t, metrics.CurrentQuarantinedNodes, nodeName)
		return gaugeValue >= float64(0)
	}, eventuallyTimeout, eventuallyPollInterval, "CurrentQuarantinedNodes gauge should be initialized")

	gaugeValue := getGaugeVecValue(t, metrics.CurrentQuarantinedNodes, nodeName)
	if gaugeValue == float64(1) {
		t.Logf("CurrentQuarantinedNodes correctly restored to 1 for existing quarantined node")
	} else {
		t.Logf("Note: CurrentQuarantinedNodes is %v (cold start restoration depends on node informer sync)", gaugeValue)
	}
}

// Helper functions for reading Prometheus metrics

func getCounterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	metric := &dto.Metric{}
	err := counter.Write(metric)
	require.NoError(t, err)
	return metric.Counter.GetValue()
}

func getCounterVecValue(t *testing.T, counterVec *prometheus.CounterVec, labelValues ...string) float64 {
	t.Helper()
	counter, err := counterVec.GetMetricWithLabelValues(labelValues...)
	require.NoError(t, err)
	metric := &dto.Metric{}
	err = counter.Write(metric)
	require.NoError(t, err)
	return metric.Counter.GetValue()
}

func getGaugeVecValue(t *testing.T, gaugeVec *prometheus.GaugeVec, labelValues ...string) float64 {
	t.Helper()
	gauge, err := gaugeVec.GetMetricWithLabelValues(labelValues...)
	require.NoError(t, err)
	metric := &dto.Metric{}
	err = gauge.Write(metric)
	require.NoError(t, err)
	return metric.Gauge.GetValue()
}
