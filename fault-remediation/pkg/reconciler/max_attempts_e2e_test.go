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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/remediation"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// These tests drive the attempt cap through the reconciler against envtest, which is the only
// level where it can be verified: the cap depends on how checkExistingCRStatus rewrites the
// node annotation between events, so an annotation-level unit test would pass while the
// end-to-end cap never engaged.

// newCappedReconciler builds a reconciler whose config caps remediation attempts per group.
func newCappedReconciler(t *testing.T, maxAttempts int, enableLogCollector ...bool) (*FaultRemediationReconciler,
	*MockChangeStreamWatcher, *MockHealthEventStore) {
	t.Helper()

	logCollectorEnabled := len(enableLogCollector) > 0 && enableLogCollector[0]
	remediationClient, err := remediation.NewRemediationClient(ctrlRuntimeClient, false, config.TomlConfig{
		Template:               config.Template{MountPath: "./templates"},
		RemediationActions:     restartRemediationActions,
		MaxRemediationAttempts: maxAttempts,
	})
	require.NoError(t, err)

	store := &MockHealthEventStore{}
	store.UpdateHealthEventStatusFn = func(ctx context.Context, id string, status datastore.HealthEventStatus) error {
		return nil
	}

	watcher := NewMockChangeStreamWatcher()

	cfg := ReconcilerConfig{
		RemediationClient:  remediationClient,
		EnableLogCollector: logCollectorEnabled,
		StateManager:       statemanager.NewStateManager(testClient),
		NodeReader:         ctrlRuntimeAPIReader,
		UpdateMaxRetries:   3,
		UpdateRetryDelay:   100 * time.Millisecond,
	}

	return NewFaultRemediationReconciler(nil, watcher, store, cfg, false), watcher, store
}

// prepareQuarantinedNode creates a node in the state fault-quarantine and node-drainer leave
// behind, which is what fault-remediation expects to act on.
func prepareQuarantinedNode(ctx context.Context, t *testing.T, r *FaultRemediationReconciler, nodeName string) {
	t.Helper()

	createTestNode(ctx, nodeName, nil, map[string]string{"test": "label"})
	cleanupNodeAnnotations(ctx, t, nodeName)
	applyQuarantineLabels(ctx, t, r, nodeName)
}

// applyQuarantineLabels walks the node back through the labels fault-quarantine and
// node-drainer set, without recreating it: a second quarantine session reuses the same node.
func applyQuarantineLabels(ctx context.Context, t *testing.T, r *FaultRemediationReconciler, nodeName string) {
	t.Helper()

	for _, label := range []statemanager.NVSentinelStateLabelValue{
		statemanager.QuarantinedLabelValue,
		statemanager.DrainingLabelValue,
		statemanager.DrainSucceededLabelValue,
	} {
		_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx, nodeName, label, false)
		require.NoError(t, err)
	}
}

// reconcileQuarantineEvent feeds one quarantine event through the full reconcile path.
func reconcileQuarantineEvent(ctx context.Context, t *testing.T, r *FaultRemediationReconciler,
	nodeName, eventID string) error {
	t.Helper()

	event := createQuarantineEvent(eventID, nodeName, protos.RecommendedAction_RESTART_BM)
	eventToken := datastore.EventWithToken{
		Event:       map[string]any(event),
		ResumeToken: []byte(eventID),
	}

	_, err := r.Reconcile(ctx, &eventToken)

	return err
}

// currentCR returns the CR recorded for the restart group, or "" when none is recorded.
func currentCR(ctx context.Context, t *testing.T, r *FaultRemediationReconciler, nodeName string) string {
	t.Helper()

	state, _, err := r.annotationManager.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)

	return state.EquivalenceGroups["restart"].MaintenanceCR
}

func nodeStateLabel(ctx context.Context, t *testing.T, nodeName string) string {
	t.Helper()

	node, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	return node.Labels[statemanager.NVSentinelStateLabelKey]
}

func TestLogCollectorRequeuesDoNotConsumeAttemptBudget(t *testing.T) {
	ctx := t.Context()
	t.Setenv(remediation.LogCollectorManifestPathEnv, "../remediation/templates/log-collector-job.yaml")
	_, err := testClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		require.NoError(t, testClient.CoreV1().Namespaces().Delete(cleanupCtx, "test", metav1.DeleteOptions{}))
	})
	nodeName := "log-collector-attempt-budget"
	r, _, _ := newCappedReconciler(t, 1, true)
	prepareQuarantinedNode(ctx, t, r, nodeName)
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		require.NoError(t, testClient.CoreV1().Nodes().Delete(cleanupCtx, nodeName, metav1.DeleteOptions{}))
	})

	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, "log-collector-event"))
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, "log-collector-event"))

	state, _, err := r.annotationManager.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	assert.Zero(t, state.EquivalenceGroups["restart"].AttemptCount)
	assert.Empty(t, currentCR(ctx, t, r, nodeName))
}

var rebootNodeGVR = schema.GroupVersionResource{
	Group:    "janitor.dgxc.nvidia.com",
	Version:  "v1alpha1",
	Resource: "rebootnodes",
}

// TestMaxAttempts_CapHoldsAcrossFailedCRs is the regression test for the loop in #1543: a
// failed CR clears the group from the annotation, so before this fix the attempt count was
// rebuilt from scratch on every event and no cap above 1 could ever be reached.
func TestMaxAttempts_CapHoldsAcrossFailedCRs(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext, 60*time.Second)
	defer cancel()

	r, watcher, _ := newCappedReconciler(t, 2)
	nodeName := "test-node-max-attempts-cap"

	prepareQuarantinedNode(ctx, t, r, nodeName)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	// Attempt 1 of 2.
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, "cap-event-1"))
	crName1 := currentCR(ctx, t, r, nodeName)
	require.NotEmpty(t, crName1, "first event must create a CR")

	defer func() {
		_ = testDynamic.Resource(rebootNodeGVR).Delete(ctx, crName1, metav1.DeleteOptions{})
	}()

	// The remediation fails, which is what restarts the loop this feature must stop.
	updateRebootNodeStatus(ctx, t, crName1, "Failed")

	// Attempt 2 of 2: still within budget, a new CR is expected.
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, "cap-event-2"))

	crName2 := currentCR(ctx, t, r, nodeName)
	require.NotEmpty(t, crName2, "second event must still be allowed by a cap of 2")
	require.NotEqual(t, crName1, crName2, "second event must create a new CR")

	defer func() {
		_ = testDynamic.Resource(rebootNodeGVR).Delete(ctx, crName2, metav1.DeleteOptions{})
	}()

	updateRebootNodeStatus(ctx, t, crName2, "Failed")

	// Attempt 3 exceeds the cap: no CR, and the node is handed to an operator.
	_, markedBefore, _, _ := watcher.GetCallCounts()
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, "cap-event-3"))

	_, markedAfter, _, _ := watcher.GetCallCounts()

	assert.Empty(t, currentCR(ctx, t, r, nodeName),
		"no CR may be created once the attempt budget is spent")
	assert.Equal(t, string(statemanager.RemediationFailedLabelValue), nodeStateLabel(ctx, t, nodeName),
		"a capped node must be labelled remediation-failed so operators find it")
	assert.Greater(t, markedAfter, markedBefore,
		"the capped event must be marked processed, not left to be redelivered forever")

	crList, err := testDynamic.Resource(rebootNodeGVR).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)

	created := 0

	for _, cr := range crList.Items {
		if spec, ok := cr.Object["spec"].(map[string]any); ok && spec["nodeName"] == nodeName {
			created++
		}
	}

	assert.Equal(t, 2, created, "exactly maxRemediationAttempts CRs may exist for the node")
}

// TestMaxAttempts_StaysCappedAndRelabels covers the label being re-stamped: fault-quarantine
// and node-drainer rewrite the state label as new events arrive, so a one-time write would be
// silently overwritten and the node would no longer look failed.
func TestMaxAttempts_StaysCappedAndRelabels(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext, 60*time.Second)
	defer cancel()

	r, _, _ := newCappedReconciler(t, 1)
	nodeName := "test-node-max-attempts-relabel"

	prepareQuarantinedNode(ctx, t, r, nodeName)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, "relabel-event-1"))
	crName := currentCR(ctx, t, r, nodeName)
	require.NotEmpty(t, crName)

	defer func() {
		_ = testDynamic.Resource(rebootNodeGVR).Delete(ctx, crName, metav1.DeleteOptions{})
	}()

	updateRebootNodeStatus(ctx, t, crName, "Failed")

	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, "relabel-event-2"))
	require.Equal(t, string(statemanager.RemediationFailedLabelValue), nodeStateLabel(ctx, t, nodeName))

	// Another controller re-stamps the label, as happens when new events flow in.
	_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx, nodeName,
		statemanager.RemediatingLabelValue, false)
	require.NoError(t, err)

	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, "relabel-event-3"))

	assert.Empty(t, currentCR(ctx, t, r, nodeName), "the group must stay capped")
	assert.Equal(t, string(statemanager.RemediationFailedLabelValue), nodeStateLabel(ctx, t, nodeName),
		"the remediation-failed label must be rewritten on every capped event")
}

// TestMaxAttempts_SurvivesReconcilerRestart proves the budget lives in the node annotation and
// not in memory: a fresh reconciler over the same cluster must still refuse to remediate.
func TestMaxAttempts_SurvivesReconcilerRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext, 60*time.Second)
	defer cancel()

	r, _, _ := newCappedReconciler(t, 1)
	nodeName := "test-node-max-attempts-restart"

	prepareQuarantinedNode(ctx, t, r, nodeName)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, "restart-event-1"))
	crName := currentCR(ctx, t, r, nodeName)
	require.NotEmpty(t, crName)

	defer func() {
		_ = testDynamic.Resource(rebootNodeGVR).Delete(ctx, crName, metav1.DeleteOptions{})
	}()

	updateRebootNodeStatus(ctx, t, crName, "Failed")

	// The pod restarts: a brand new reconciler, no in-memory state carried over.
	restarted, _, _ := newCappedReconciler(t, 1)

	require.NoError(t, reconcileQuarantineEvent(ctx, t, restarted, nodeName, "restart-event-2"))

	assert.Empty(t, currentCR(ctx, t, restarted, nodeName),
		"a restarted reconciler must still see the spent budget in the node annotation")
	assert.Equal(t, string(statemanager.RemediationFailedLabelValue), nodeStateLabel(ctx, t, nodeName))
}

// TestMaxAttempts_CancellationResetsBudget checks the budget is scoped to one quarantine
// session: once the node leaves quarantine, a later fault must be remediable again.
func TestMaxAttempts_CancellationResetsBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext, 60*time.Second)
	defer cancel()

	r, _, _ := newCappedReconciler(t, 1)
	nodeName := "test-node-max-attempts-cancel"

	prepareQuarantinedNode(ctx, t, r, nodeName)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, "cancel-event-1"))
	crName1 := currentCR(ctx, t, r, nodeName)
	require.NotEmpty(t, crName1)

	defer func() {
		_ = testDynamic.Resource(rebootNodeGVR).Delete(ctx, crName1, metav1.DeleteOptions{})
	}()

	updateRebootNodeStatus(ctx, t, crName1, "Failed")

	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, "cancel-event-2"))
	require.Empty(t, currentCR(ctx, t, r, nodeName), "budget must be spent before the cancellation")

	// The quarantine session ends.
	cancelled := createCancelledEvent("cancel-event-3", nodeName, protos.RecommendedAction_RESTART_BM)
	cancelledToken := datastore.EventWithToken{
		Event:       map[string]any(cancelled),
		ResumeToken: []byte("cancel-event-3"),
	}
	_, err := r.Reconcile(ctx, &cancelledToken)
	require.NoError(t, err)

	// A new session starts on the same node, so it must be remediable again.
	applyQuarantineLabels(ctx, t, r, nodeName)

	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, "cancel-event-4"))

	crName2 := currentCR(ctx, t, r, nodeName)
	assert.NotEmpty(t, crName2, "a new quarantine session must start from a fresh attempt budget")

	if crName2 != "" {
		defer func() {
			_ = testDynamic.Resource(rebootNodeGVR).Delete(ctx, crName2, metav1.DeleteOptions{})
		}()
	}
}

// TestMaxAttempts_SucceededCRThenOutOfSessionEventIsNotCapped guards the boundary the other
// way round: a successful remediation followed by a later, unrelated fault must still be
// allowed while the group has budget left.
func TestMaxAttempts_SucceededCRThenOutOfSessionEventIsNotCapped(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext, 60*time.Second)
	defer cancel()

	r, _, _ := newCappedReconciler(t, 2)
	nodeName := "test-node-max-attempts-succeeded"

	prepareQuarantinedNode(ctx, t, r, nodeName)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, "succeeded-event-1"))
	crName1 := currentCR(ctx, t, r, nodeName)
	require.NotEmpty(t, crName1)

	defer func() {
		_ = testDynamic.Resource(rebootNodeGVR).Delete(ctx, crName1, metav1.DeleteOptions{})
	}()

	updateRebootNodeStatus(ctx, t, crName1, "Succeeded")

	// A fault raised after the CR completed belongs to a new remediation attempt, not to the
	// session the CR covered, so it must be remediated while budget remains.
	event := createQuarantineEventCreatedAt("succeeded-event-2", nodeName,
		protos.RecommendedAction_RESTART_BM, time.Now().Add(time.Minute))
	eventToken := datastore.EventWithToken{
		Event:       map[string]any(event),
		ResumeToken: []byte("succeeded-event-2"),
	}
	_, err := r.Reconcile(ctx, &eventToken)
	require.NoError(t, err)

	crName2 := currentCR(ctx, t, r, nodeName)
	assert.NotEmpty(t, crName2, "an out-of-session event must not be capped while budget remains")
	assert.NotEqual(t, crName1, crName2)

	if crName2 != "" {
		defer func() {
			_ = testDynamic.Resource(rebootNodeGVR).Delete(ctx, crName2, metav1.DeleteOptions{})
		}()
	}
}
