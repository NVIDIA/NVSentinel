// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"errors"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	cordonlabels "github.com/nvidia/nvsentinel/commons/pkg/labels"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/evaluator"
	healthEventsAnnotation "github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/nodecache"
	"github.com/nvidia/nvsentinel/store-client/pkg/testutils"
)

func TestUpdateSessionActionMetrics_CountsOnlyCordonsThatFaultQuarantineApplied(t *testing.T) {
	cordonRecorded := map[string]string{
		common.QuarantineHealthEventIsCordonedAnnotationKey: common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
	}

	testCases := []struct {
		name          string
		update        sessionActionUpdate
		expectedDelta float64
	}{
		{
			name:          "cordon applied by fault-quarantine",
			update:        sessionActionUpdate{cordon: true, annotations: cordonRecorded},
			expectedDelta: 1,
		},
		{
			name: "pre-existing cordon is recorded but not applied",
			update: sessionActionUpdate{annotations: map[string]string{
				common.QuarantineHealthEventIsCordonedAnnotationKey: common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
				common.QuarantineHealthEventCordonPreExistingAnnotationKey: common.QuarantineHealthEventCordonPreExistingAnnotationValue,
			}},
			expectedDelta: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			before := counterValue(t, metrics.CordonsApplied)

			(&Reconciler{}).updateSessionActionMetrics(tc.update)

			assert.InDelta(t, tc.expectedDelta, counterValue(t, metrics.CordonsApplied)-before, 0)
		})
	}
}

const sessionTestLabelPrefix = "k8s.nvidia.com/"

func appliedTaintsAnnotation(t *testing.T, taints ...config.Taint) string {
	t.Helper()

	taintsJSON, err := json.Marshal(taints)
	require.NoError(t, err)

	return string(taintsJSON)
}

func TestSessionTaintsToAdd(t *testing.T) {
	taintA := config.Taint{Key: "nvidia.com/a", Value: "true", Effect: "NoSchedule"}
	taintB := config.Taint{Key: "nvidia.com/b", Value: "true", Effect: "NoSchedule"}

	testCases := []struct {
		name        string
		recorded    string
		nodeTaints  []corev1.Taint
		desired     []config.Taint
		expected    []config.Taint
		expectedErr bool
	}{
		{
			name:     "a taint the node does not have is added",
			recorded: appliedTaintsAnnotation(t, taintA),
			nodeTaints: []corev1.Taint{
				{Key: taintA.Key, Value: taintA.Value, Effect: corev1.TaintEffectNoSchedule},
			},
			desired:  []config.Taint{taintB},
			expected: []config.Taint{taintB},
		},
		{
			name:     "a taint the session records with the same key and effect is left as it is",
			recorded: appliedTaintsAnnotation(t, taintA),
			nodeTaints: []corev1.Taint{
				{Key: taintA.Key, Value: taintA.Value, Effect: corev1.TaintEffectNoSchedule},
			},
			desired:  []config.Taint{{Key: taintA.Key, Value: "other", Effect: taintA.Effect}},
			expected: []config.Taint{},
		},
		{
			name: "a taint already on the node with the same value is recorded as pre-existing",
			nodeTaints: []corev1.Taint{
				{Key: taintB.Key, Value: taintB.Value, Effect: corev1.TaintEffectNoSchedule},
			},
			desired: []config.Taint{taintB},
			expected: []config.Taint{
				{Key: taintB.Key, Value: taintB.Value, Effect: taintB.Effect, PreExisting: true},
			},
		},
		{
			name: "a taint already on the node with another value is skipped",
			nodeTaints: []corev1.Taint{
				{Key: taintB.Key, Value: "owned-by-someone-else", Effect: corev1.TaintEffectNoSchedule},
			},
			desired:  []config.Taint{taintB},
			expected: []config.Taint{},
		},
		{
			name: "the same key with another effect is a different taint",
			nodeTaints: []corev1.Taint{
				{Key: taintB.Key, Value: taintB.Value, Effect: corev1.TaintEffectNoExecute},
			},
			desired:  []config.Taint{taintB},
			expected: []config.Taint{taintB},
		},
		{
			name:        "a malformed applied taints annotation is an error",
			recorded:    "not-json",
			desired:     []config.Taint{taintB},
			expectedErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node", Annotations: map[string]string{}},
				Spec:       corev1.NodeSpec{Taints: tc.nodeTaints},
			}
			if tc.recorded != "" {
				node.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey] = tc.recorded
			}

			taints, err := (&Reconciler{}).sessionTaintsToAdd(context.Background(), node, tc.desired)
			if tc.expectedErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.ElementsMatch(t, tc.expected, taints)
		})
	}
}

func TestBuildSessionActionUpdate(t *testing.T) {
	taintB := config.Taint{Key: "nvidia.com/b", Value: "true", Effect: "NoSchedule"}
	cordonedBy := cordonlabels.Key(sessionTestLabelPrefix, cordonlabels.CordonedBySuffix)
	cordonedReason := cordonlabels.Key(sessionTestLabelPrefix, cordonlabels.CordonedReasonSuffix)
	cordonedTimestamp := cordonlabels.Key(sessionTestLabelPrefix, cordonlabels.CordonedTimestampSuffix)

	testCases := []struct {
		name                string
		dryRun              bool
		nodeAnnotations     map[string]string
		nodeUnschedulable   bool
		requiresCordon      bool
		expectTaints        bool
		expectCordon        bool
		expectCordonRecord  bool
		expectPreExisting   bool
		expectCordonByLabel bool
	}{
		{
			name:                "the event's taint and cordon are added to a live session",
			requiresCordon:      true,
			expectTaints:        true,
			expectCordon:        true,
			expectCordonRecord:  true,
			expectCordonByLabel: true,
		},
		{
			name:         "an event that does not require a cordon does not cordon the node",
			expectTaints: true,
		},
		{
			name: "a session that already records a cordon is not cordoned again",
			nodeAnnotations: map[string]string{
				common.QuarantineHealthEventIsCordonedAnnotationKey: common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
			},
			nodeUnschedulable: true,
			requiresCordon:    true,
			expectTaints:      true,
		},
		{
			name:               "a cordon that someone else applied is recorded as pre-existing",
			nodeUnschedulable:  true,
			requiresCordon:     true,
			expectTaints:       true,
			expectCordonRecord: true,
			expectPreExisting:  true,
		},
		{
			name:           "dry run records nothing on a live session",
			dryRun:         true,
			requiresCordon: true,
		},
		{
			name:   "dry run records the actions on a dry-run session",
			dryRun: true,
			nodeAnnotations: map[string]string{
				common.QuarantineHealthEventDryRunAnnotationKey: common.QuarantineHealthEventDryRunAnnotationValue,
			},
			requiresCordon:      true,
			expectTaints:        true,
			expectCordon:        true,
			expectCordonRecord:  true,
			expectCordonByLabel: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reconciler{config: ReconcilerConfig{DryRun: tc.dryRun}}
			r.SetLabelKeys(sessionTestLabelPrefix)

			annotations := map[string]string{}
			for k, v := range tc.nodeAnnotations {
				annotations[k] = v
			}

			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node", Annotations: annotations},
				Spec:       corev1.NodeSpec{Unschedulable: tc.nodeUnschedulable},
			}

			labels := map[string]string{}
			if tc.requiresCordon {
				labels[cordonedReason] = "nic-fault"
			}

			update, err := r.buildSessionActionUpdate(context.Background(), node, sessionActionUpdate{
				taints: []config.Taint{taintB},
				cordon: tc.requiresCordon,
				labels: labels,
			})
			require.NoError(t, err)

			_, hasTaintsRecord := update.annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey]
			assert.Equal(t, tc.expectTaints, hasTaintsRecord, "applied taints annotation")
			assert.Equal(t, tc.expectTaints, len(update.taints) == 1, "taints to add")
			assert.Equal(t, tc.expectCordon, update.cordon, "cordon")

			_, hasCordonRecord := update.annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]
			assert.Equal(t, tc.expectCordonRecord, hasCordonRecord, "cordon annotation")

			_, hasPreExisting := update.annotations[common.QuarantineHealthEventCordonPreExistingAnnotationKey]
			assert.Equal(t, tc.expectPreExisting, hasPreExisting, "cordon pre-existing annotation")

			if tc.expectCordonByLabel {
				assert.Equal(t, cordonlabels.ServiceName, update.labels[cordonedBy])
				assert.Equal(t, "nic-fault", update.labels[cordonedReason])
				assert.NotEmpty(t, update.labels[cordonedTimestamp])
			} else {
				assert.NotContains(t, update.labels, cordonedBy)
				assert.NotContains(t, update.labels, cordonedReason)
				assert.NotContains(t, update.labels, cordonedTimestamp)
			}
		})
	}
}

// sessionActionsTestConfig has one rule set per check. GpuFault taints the node with taint A only. NicFault taints it
// with taint B and, when nicCordons is set, cordons it.
func sessionActionsTestConfig(nicCordons bool) config.TomlConfig {
	return config.TomlConfig{
		LabelPrefix: sessionTestLabelPrefix,
		RuleSets: []config.QuarantineRuleSet{
			{
				Enabled:  true,
				Name:     "gpu-fault",
				Version:  "1",
				Priority: 10,
				Match: config.Match{Any: []config.Rule{{
					Kind: "HealthEvent", Expression: "event.checkName == 'GpuFault'",
				}}},
				Taint: config.Taint{Key: "nvidia.com/gpu-fault", Value: "true", Effect: "NoSchedule"},
			},
			{
				Enabled:  true,
				Name:     "nic-fault",
				Version:  "1",
				Priority: 10,
				Match: config.Match{Any: []config.Rule{{
					Kind: "HealthEvent", Expression: "event.checkName == 'NicFault'",
				}}},
				Taint:  config.Taint{Key: "nvidia.com/nic-fault", Value: "true", Effect: "NoSchedule"},
				Cordon: config.Cordon{ShouldCordon: nicCordons},
			},
		},
	}
}

func sendSessionTestEvent(mockWatcher *testutils.MockChangeStreamWatcher, nodeName, checkName, entityValue string,
	isHealthy bool) string {
	eventID := generateTestID()
	mockWatcher.EventsChan <- &TestEvent{Data: createHealthEventBSON(
		eventID, nodeName, checkName, isHealthy, !isHealthy,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: entityValue}}, model.StatusInProgress,
	)}

	return eventID
}

func nodeHasTaint(node *corev1.Node, key string) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == key {
			return true
		}
	}

	return false
}

func sessionAppliedTaints(t *testing.T, node *corev1.Node) []config.Taint {
	t.Helper()

	var taints []config.Taint
	require.NoError(t, json.Unmarshal(
		[]byte(node.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey]), &taints,
	))

	return taints
}

func TestE2E_LaterEventOnQuarantinedNode_AppliesItsTaintAndCordon(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := "e2e-session-actions-" + generateShortTestID()
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	_, mockWatcher, getStatus, _ := setupE2EReconciler(t, ctx, sessionActionsTestConfig(true), nil)

	getNode := func() *corev1.Node {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		require.NoError(t, err)

		return node
	}

	t.Log("The GPU fault starts the session with taint A and no cordon")
	gpuID := sendSessionTestEvent(mockWatcher, nodeName, "GpuFault", "0", false)
	require.Eventually(t, func() bool {
		status := getStatus(gpuID)
		return status != nil && *status == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval)

	node := getNode()
	require.True(t, nodeHasTaint(node, "nvidia.com/gpu-fault"))
	require.False(t, node.Spec.Unschedulable)

	t.Log("The NIC fault on the quarantined node adds taint B and the cordon")
	nicID := sendSessionTestEvent(mockWatcher, nodeName, "NicFault", "1", false)
	require.Eventually(t, func() bool {
		status := getStatus(nicID)
		return status != nil && *status == model.AlreadyQuarantined
	}, statusCheckTimeout, statusCheckPollInterval)

	require.Eventually(t, func() bool {
		node := getNode()
		return node.Spec.Unschedulable && nodeHasTaint(node, "nvidia.com/nic-fault")
	}, eventuallyTimeout, eventuallyPollInterval, "The later event's taint and cordon should be applied")

	node = getNode()
	assert.True(t, nodeHasTaint(node, "nvidia.com/gpu-fault"), "The first event's taint should remain")
	assert.Equal(t, common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
		node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey])
	assert.NotContains(t, node.Annotations, common.QuarantineHealthEventCordonPreExistingAnnotationKey)
	assert.Equal(t, cordonlabels.ServiceName,
		node.Labels[cordonlabels.Key(sessionTestLabelPrefix, cordonlabels.CordonedBySuffix)])
	assert.ElementsMatch(t, []config.Taint{
		{Key: "nvidia.com/gpu-fault", Value: "true", Effect: "NoSchedule"},
		{Key: "nvidia.com/nic-fault", Value: "true", Effect: "NoSchedule"},
	}, sessionAppliedTaints(t, node))

	t.Log("Both faults recover and fault-quarantine removes every action it applied")
	sendSessionTestEvent(mockWatcher, nodeName, "GpuFault", "0", true)
	sendSessionTestEvent(mockWatcher, nodeName, "NicFault", "1", true)

	require.Eventually(t, func() bool {
		node := getNode()
		_, quarantined := node.Annotations[common.QuarantineHealthEventAnnotationKey]

		return !quarantined && !node.Spec.Unschedulable &&
			!nodeHasTaint(node, "nvidia.com/gpu-fault") && !nodeHasTaint(node, "nvidia.com/nic-fault")
	}, eventuallyTimeout, eventuallyPollInterval, "The node should be untainted and uncordoned")
}

func TestE2E_LaterEventOnQuarantinedNode_WithoutCordonRule_DoesNotCordon(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := "e2e-session-no-cordon-" + generateShortTestID()
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	_, mockWatcher, getStatus, _ := setupE2EReconciler(t, ctx, sessionActionsTestConfig(false), nil)

	gpuID := sendSessionTestEvent(mockWatcher, nodeName, "GpuFault", "0", false)
	require.Eventually(t, func() bool {
		status := getStatus(gpuID)
		return status != nil && *status == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval)

	nicID := sendSessionTestEvent(mockWatcher, nodeName, "NicFault", "1", false)
	require.Eventually(t, func() bool {
		status := getStatus(nicID)
		return status != nil && *status == model.AlreadyQuarantined
	}, statusCheckTimeout, statusCheckPollInterval)

	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return err == nil && nodeHasTaint(node, "nvidia.com/nic-fault")
	}, eventuallyTimeout, eventuallyPollInterval, "The later event's taint should be applied")

	require.Never(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		_, cordonRecorded := node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]

		return node.Spec.Unschedulable || cordonRecorded
	}, neverTimeout, neverPollInterval, "No rule requires a cordon, so the node must stay schedulable")
}

func TestE2E_LaterEventOnQuarantinedNode_PreExistingTaintAndCordon_SurviveRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := "e2e-session-pre-existing-" + generateShortTestID()
	userTaint := corev1.Taint{Key: "nvidia.com/nic-fault", Value: "true", Effect: corev1.TaintEffectNoSchedule}
	createE2ETestNode(ctx, t, nodeName, nil, nil, []corev1.Taint{userTaint}, true)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	_, mockWatcher, getStatus, _ := setupE2EReconciler(t, ctx, sessionActionsTestConfig(true), nil)

	getNode := func() *corev1.Node {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		require.NoError(t, err)

		return node
	}

	gpuID := sendSessionTestEvent(mockWatcher, nodeName, "GpuFault", "0", false)
	require.Eventually(t, func() bool {
		status := getStatus(gpuID)
		return status != nil && *status == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval)

	t.Log("The NIC fault requires a taint and a cordon that the node already has")
	nicID := sendSessionTestEvent(mockWatcher, nodeName, "NicFault", "1", false)
	require.Eventually(t, func() bool {
		status := getStatus(nicID)
		return status != nil && *status == model.AlreadyQuarantined
	}, statusCheckTimeout, statusCheckPollInterval)

	require.Eventually(t, func() bool {
		return getNode().Annotations[common.QuarantineHealthEventCordonPreExistingAnnotationKey] ==
			common.QuarantineHealthEventCordonPreExistingAnnotationValue
	}, eventuallyTimeout, eventuallyPollInterval, "The existing cordon should be recorded as pre-existing")

	assert.Contains(t, sessionAppliedTaints(t, getNode()), config.Taint{
		Key: userTaint.Key, Value: userTaint.Value, Effect: string(userTaint.Effect), PreExisting: true,
	})

	t.Log("Both faults recover; fault-quarantine removes only its own taint")
	sendSessionTestEvent(mockWatcher, nodeName, "GpuFault", "0", true)
	sendSessionTestEvent(mockWatcher, nodeName, "NicFault", "1", true)

	require.Eventually(t, func() bool {
		node := getNode()
		_, quarantined := node.Annotations[common.QuarantineHealthEventAnnotationKey]

		return !quarantined && !nodeHasTaint(node, "nvidia.com/gpu-fault")
	}, eventuallyTimeout, eventuallyPollInterval, "The session should end and taint A should be removed")

	node := getNode()
	assert.True(t, node.Spec.Unschedulable, "The cordon that existed before the session should remain")
	assert.True(t, nodeHasTaint(node, userTaint.Key), "The taint that existed before the session should remain")
}

// A later event whose node update fails must keep no status, as the first quarantine does, so that cold start
// replays it. With an AlreadyQuarantined status, the replay skips it and its actions are never applied.
func TestHandleAlreadyQuarantinedNode_NodeUpdateFails_WritesNoStatus(t *testing.T) {
	trackedEvent := &protos.HealthEvent{
		Version: 1, Agent: "test-monitor", ComponentClass: "GPU", CheckName: "GpuFault", NodeName: "node",
		IsFatal: true, ErrorCode: []string{"79"},
		EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
	}
	newEvent := &protos.HealthEvent{
		Version: 1, Agent: "test-monitor", ComponentClass: "GPU", CheckName: "GpuFault", NodeName: "node",
		IsFatal: true, ErrorCode: []string{"79"},
		EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
	}

	testCases := []struct {
		name           string
		event          *protos.HealthEvent
		failNodeWrites bool
		deferRecovery  bool
		expectNoStatus bool
	}{
		{
			name:           "the health events annotation update fails",
			event:          newEvent,
			failNodeWrites: true,
			expectNoStatus: true,
		},
		{
			name:           "the session actions update fails",
			event:          trackedEvent,
			failNodeWrites: true,
			expectNoStatus: true,
		},
		{
			name:           "cold start defers the session actions after a transient evaluation error",
			event:          trackedEvent,
			deferRecovery:  true,
			expectNoStatus: true,
		},
		{
			name:  "the node update succeeds",
			event: newEvent,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			annotation := healthEventsAnnotation.NewHealthEventsAnnotationMap()
			require.True(t, annotation.AddOrUpdateEvent(trackedEvent))
			annotationJSON, err := json.Marshal(annotation)
			require.NoError(t, err)

			clientset := fake.NewSimpleClientset(&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node",
					Labels:      map[string]string{informer.GPUNodeLabel: informer.GPUNodeLabelValue},
					Annotations: map[string]string{common.QuarantineHealthEventAnnotationKey: string(annotationJSON)},
				},
			})
			if tc.failNodeWrites {
				failWrite := func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("API server unavailable")
				}
				clientset.PrependReactor("update", "nodes", failWrite)
				clientset.PrependReactor("patch", "nodes", failWrite)
			}

			nodeInformer, err := informer.NewNodeInformer(clientset, 0, informer.GPUNodeLabel,
				informer.GPUNodeLabelValue,
				nodecache.Derive(config.TomlConfig{}, nodecache.Operational{GPUNodeLabelKey: informer.GPUNodeLabel}))
			require.NoError(t, err)
			go func() { _ = nodeInformer.Run(ctx.Done()) }()
			require.True(t, nodeInformer.WaitForSync(ctx))

			r := NewReconciler(ReconcilerConfig{},
				&informer.FaultQuarantineClient{Clientset: clientset, NodeInformer: nodeInformer}, nil)
			r.SetLabelKeys(sessionTestLabelPrefix)

			evaluators := []evaluator.RuleSetEvaluatorIface{
				&stubRuleSetEvaluator{name: "all", result: common.RuleEvaluationSuccess},
			}
			if tc.deferRecovery {
				ctx = coldstart.WithRecoveryContext(ctx)
				evaluators = append(evaluators, &stubRuleSetEvaluator{
					name: "flaky", result: common.RuleEvaluationFailed, err: errors.New("transient evaluation error"),
				})
			}

			status, err := r.handleAlreadyQuarantinedNode(ctx, tc.event, evaluators,
				rulesetsConfig{CordonConfigMap: map[string]bool{"all": true}})

			if tc.expectNoStatus {
				require.ErrorIs(t, err, errQuarantinedNodeNotUpdated)
				assert.Nil(t, status, "an event whose node update failed must stay unprocessed")

				return
			}

			require.NoError(t, err)
			require.NotNil(t, status)
			assert.Equal(t, model.AlreadyQuarantined, *status)
		})
	}
}
