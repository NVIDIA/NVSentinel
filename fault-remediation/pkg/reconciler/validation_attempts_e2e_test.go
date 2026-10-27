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
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	annotationutil "github.com/nvidia/nvsentinel/commons/pkg/annotation"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/annotation"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// validationStateLabel is the label lifecycle-manager keeps on a node while validation holds it.
const validationStateLabel = "nvsentinel.nvidia.com/validation-state"

// handOffToValidation models fault-quarantine handing the node to validation: the node stays
// cordoned, and FR processes the UnQuarantined event after LM registered the Node session. An
// empty validationState models a hand-off before that registration.
func handOffToValidation(ctx context.Context, t *testing.T, r *FaultRemediationReconciler,
	nodeName, validationState string) {
	t.Helper()

	setValidationState(ctx, t, nodeName, validationState, statemanager.NVSentinelStateLabelKey)

	_, err := r.reconcileEvent(ctx, &datastore.EventWithToken{
		Event:       createUnquarantineEvent(nodeName),
		ResumeToken: []byte(nodeName + "-handoff"),
	})
	require.NoError(t, err)

	// The reconciler reads the Node through the informer cache. Wait until the cache holds the
	// kept counts, so the next event does not read the state from before the hand-off.
	require.Eventually(t, func() bool {
		state, _, err := r.annotationManager.GetRemediationState(ctx, nodeName)

		return err == nil && len(state.ValidationAttempts) > 0
	}, 10*time.Second, 50*time.Millisecond, "kept attempts must be visible through the cache")
}

// unquarantineToService models a quarantine that ends without validation: fault-quarantine
// uncordons the node, and FR processes the UnQuarantined event.
func unquarantineToService(ctx context.Context, t *testing.T, r *FaultRemediationReconciler, nodeName string) {
	t.Helper()

	setNodeUnschedulable(ctx, t, nodeName, false)
	setValidationState(ctx, t, nodeName, "", statemanager.NVSentinelStateLabelKey)

	_, err := r.reconcileEvent(ctx, &datastore.EventWithToken{
		Event:       createUnquarantineEvent(nodeName),
		ResumeToken: []byte(nodeName + "-unquarantine"),
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		state, _, err := r.annotationManager.GetRemediationState(ctx, nodeName)

		return err == nil && len(state.EquivalenceGroups) == 0 && len(state.ValidationAttempts) == 0
	}, 10*time.Second, 50*time.Millisecond, "the cleared state must be visible through the cache")
}

// releaseFromValidation models LM releasing the node: the session ends and the node is uncordoned.
func releaseFromValidation(ctx context.Context, t *testing.T, nodeName string) {
	t.Helper()

	setValidationState(ctx, t, nodeName, "")
	setNodeUnschedulable(ctx, t, nodeName, false)
}

// setNodeUnschedulable cordons the node, as fault-quarantine does when it quarantines the node,
// or uncordons it, as fault-quarantine and lifecycle-manager do when they return it to service.
func setNodeUnschedulable(ctx context.Context, t *testing.T, nodeName string, unschedulable bool) {
	t.Helper()

	node, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	node.Spec.Unschedulable = unschedulable
	_, err = testClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func setValidationState(ctx context.Context, t *testing.T, nodeName, validationState string,
	removeLabels ...string) {
	t.Helper()

	setValidationRequests(ctx, t, nodeName, validationState, []string{"validation-" + nodeName}, removeLabels...)
}

// setValidationRequests models the session and its derived labels in one Node update, as LM does.
func setValidationRequests(ctx context.Context, t *testing.T, nodeName, validationState string,
	requests []string, removeLabels ...string) {
	t.Helper()

	node, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	for _, label := range removeLabels {
		delete(node.Labels, label)
	}

	if validationState == "" {
		delete(node.Labels, validationStateLabel)
		delete(node.Annotations, annotationutil.ValidationSessionKey)
	} else {
		node.Labels[validationStateLabel] = validationState
		entries := make([]map[string]any, 0, len(requests))
		for _, name := range requests {
			entries = append(entries, map[string]any{
				"name": name, "tests": []string{"dcgm-diag"}, "failed": validationState == "validation-failed",
			})
		}

		encoded, err := json.Marshal(entries)
		require.NoError(t, err)
		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		node.Annotations[annotationutil.ValidationSessionKey] = string(encoded)
	}
	delete(node.Annotations, "nvsentinel.nvidia.com/active-validation-request")
	if validationState == "validating" && len(requests) > 0 {
		node.Annotations["nvsentinel.nvidia.com/active-validation-request"] = requests[0]
	}

	_, err = testClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func prepareValidationRepair(t *testing.T, nodeName string, cap int) (*FaultRemediationReconciler, string) {
	t.Helper()

	ctx := t.Context()
	r, _, _ := newCappedReconciler(t, cap)
	prepareQuarantinedNode(ctx, t, r, nodeName)
	setNodeUnschedulable(ctx, t, nodeName, true)
	cleanupValidationBudgetNode(t, nodeName)
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, nodeName+"-fault-1"))
	crName := currentCR(ctx, t, r, nodeName)
	require.NotEmpty(t, crName)
	updateRebootNodeStatus(ctx, t, crName, "Succeeded")

	return r, crName
}

func cleanupValidationBudgetNode(t *testing.T, nodeName string) {
	t.Helper()

	t.Cleanup(func() {
		ctx := context.Background()

		crs, err := testDynamic.Resource(rebootNodeGVR).List(ctx, metav1.ListOptions{})
		require.NoError(t, err)

		for _, cr := range crs.Items {
			crNode, _, err := unstructured.NestedString(cr.Object, "spec", "nodeName")
			require.NoError(t, err)

			if crNode == nodeName {
				require.NoError(t, testDynamic.Resource(rebootNodeGVR).Delete(ctx, cr.GetName(),
					metav1.DeleteOptions{}))
			}
		}

		require.NoError(t, testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{}))
	})
}

// TestMaxAttempts_FaultDuringValidationContinuesBudget covers a node that faults again before
// post-remediation validation passes. The quarantine session ended at the hand-off, but the node
// never returned to service, so the new fault continues the spent budget.
func TestMaxAttempts_FaultDuringValidationContinuesBudget(t *testing.T) {
	for _, validationState := range []string{"validation-pending", "validating", "validation-failed"} {
		t.Run(validationState, func(t *testing.T) {
			ctx := t.Context()
			nodeName := "validation-budget-" + validationState

			r, _ := prepareValidationRepair(t, nodeName, 1)

			handOffToValidation(ctx, t, r, nodeName, validationState)

			applyQuarantineLabels(ctx, t, r, nodeName)
			require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, nodeName+"-fault-2"))

			assert.Empty(t, currentCR(ctx, t, r, nodeName), "the hand-off must not renew the spent budget")
			assert.Equal(t, string(statemanager.RemediationFailedLabelValue), nodeStateLabel(ctx, t, nodeName))

			// Validation finishing later does not lift the cap of the running quarantine.
			releaseFromValidation(ctx, t, nodeName)
			require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, nodeName+"-fault-3"))
			assert.Empty(t, currentCR(ctx, t, r, nodeName), "the quarantine stays capped")
		})
	}
}

// TestMaxAttempts_ValidationRegisteredAfterHandOffContinuesBudget covers a hand-off that
// fault-remediation processes before lifecycle-manager registers the new request in the node's
// validation session. The request holds the node by the time the next fault arrives.
func TestMaxAttempts_ValidationRegisteredAfterHandOffContinuesBudget(t *testing.T) {
	ctx := t.Context()
	nodeName := "validation-budget-late-registration"

	r, _ := prepareValidationRepair(t, nodeName, 1)

	handOffToValidation(ctx, t, r, nodeName, "")
	setValidationState(ctx, t, nodeName, "validation-failed")

	applyQuarantineLabels(ctx, t, r, nodeName)
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, nodeName+"-fault-2"))

	assert.Empty(t, currentCR(ctx, t, r, nodeName), "the hand-off must not renew the spent budget")
	assert.Equal(t, string(statemanager.RemediationFailedLabelValue), nodeStateLabel(ctx, t, nodeName))
}

// TestMaxAttempts_BudgetContinuesAcrossRepeatedHandOffs follows a node through two hand-offs. The
// second request passes, but the first one keeps failing and holds the node, so the third fault
// continues the budget of both repairs.
func TestMaxAttempts_BudgetContinuesAcrossRepeatedHandOffs(t *testing.T) {
	ctx := t.Context()
	nodeName := "validation-budget-repeated"
	first, second := "validation-"+nodeName, "second-validation-"+nodeName

	r, _ := prepareValidationRepair(t, nodeName, 2)
	handOffToValidation(ctx, t, r, nodeName, "validation-failed")

	applyQuarantineLabels(ctx, t, r, nodeName)
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, nodeName+"-fault-2"))
	crName2 := currentCR(ctx, t, r, nodeName)
	require.NotEmpty(t, crName2, "the second attempt is still within the budget")
	updateRebootNodeStatus(ctx, t, crName2, "Succeeded")

	setValidationRequests(ctx, t, nodeName, "validation-pending", []string{first, second},
		statemanager.NVSentinelStateLabelKey)
	_, err := r.reconcileEvent(ctx, &datastore.EventWithToken{
		Event:       createUnquarantineEvent(nodeName),
		ResumeToken: []byte(nodeName + "-handoff-2"),
	})
	require.NoError(t, err)

	setValidationRequests(ctx, t, nodeName, "validation-failed", []string{first})

	applyQuarantineLabels(ctx, t, r, nodeName)
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, nodeName+"-fault-3"))

	assert.Empty(t, currentCR(ctx, t, r, nodeName), "the third attempt exceeds the budget")
	assert.Equal(t, string(statemanager.RemediationFailedLabelValue), nodeStateLabel(ctx, t, nodeName))
}

// TestMaxAttempts_FaultAfterValidationReleaseStartsFreshBudget covers the boundary the other way
// round: once validation released the node, a later fault starts a fresh budget.
func TestMaxAttempts_FaultAfterValidationReleaseStartsFreshBudget(t *testing.T) {
	ctx := t.Context()
	nodeName := "validation-budget-released"

	r, crName1 := prepareValidationRepair(t, nodeName, 1)

	handOffToValidation(ctx, t, r, nodeName, "validating")
	releaseFromValidation(ctx, t, nodeName)

	applyQuarantineLabels(ctx, t, r, nodeName)
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, nodeName+"-fault-2"))

	crName2 := currentCR(ctx, t, r, nodeName)
	assert.NotEmpty(t, crName2, "a node that returned to service starts a fresh budget")
	assert.NotEqual(t, crName1, crName2)

	state, _, err := r.annotationManager.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	assert.Equal(t, 1, state.EquivalenceGroups["restart"].AttemptCount)
	assert.Empty(t, state.ValidationAttempts)
}

// TestMaxAttempts_UnquarantineWithoutValidationClearsState covers a quarantine that ends without
// validation: fault-quarantine uncordons the node, so the state is cleared as before, and a
// validation that an operator starts later does not pick up the spent count.
func TestMaxAttempts_UnquarantineWithoutValidationClearsState(t *testing.T) {
	ctx := t.Context()
	nodeName := "validation-budget-no-validation"

	r, crName1 := prepareValidationRepair(t, nodeName, 1)
	unquarantineToService(ctx, t, r, nodeName)

	node, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, node.Annotations, annotation.AnnotationKey)

	setValidationRequests(ctx, t, nodeName, "validating", []string{"manual-" + nodeName})
	setNodeUnschedulable(ctx, t, nodeName, true)

	applyQuarantineLabels(ctx, t, r, nodeName)
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, nodeName+"-fault-2"))

	crName2 := currentCR(ctx, t, r, nodeName)
	assert.NotEmpty(t, crName2, "a validation started after the node returned to service starts a fresh budget")
	assert.NotEqual(t, crName1, crName2)
}

// TestMaxAttempts_TaintGatedValidationContinuesBudget covers a quarantine that gates the node with
// taints instead of a cordon: the validation session alone shows that the node is still held.
func TestMaxAttempts_TaintGatedValidationContinuesBudget(t *testing.T) {
	ctx := t.Context()
	nodeName := "validation-budget-taint-gated"

	r, _ := prepareValidationRepair(t, nodeName, 1)
	setNodeUnschedulable(ctx, t, nodeName, false)

	handOffToValidation(ctx, t, r, nodeName, "validation-pending")

	applyQuarantineLabels(ctx, t, r, nodeName)
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, nodeName+"-fault-2"))

	assert.Empty(t, currentCR(ctx, t, r, nodeName), "the hand-off must not renew the spent budget")
	assert.Equal(t, string(statemanager.RemediationFailedLabelValue), nodeStateLabel(ctx, t, nodeName))
}

// TestMaxAttempts_IndependentValidationAfterReleaseStartsFreshBudget covers a node that validation
// released before a later, unrelated request started validating it. The later request holds the
// node, but it does not carry the budget of the repair that was already released.
func TestMaxAttempts_IndependentValidationAfterReleaseStartsFreshBudget(t *testing.T) {
	ctx := t.Context()
	nodeName := "validation-budget-independent"

	r, crName1 := prepareValidationRepair(t, nodeName, 1)

	handOffToValidation(ctx, t, r, nodeName, "validation-failed")
	releaseFromValidation(ctx, t, nodeName)
	setValidationRequests(ctx, t, nodeName, "validating", []string{"independent-" + nodeName})

	applyQuarantineLabels(ctx, t, r, nodeName)
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, nodeName+"-fault-2"))

	crName2 := currentCR(ctx, t, r, nodeName)
	assert.NotEmpty(t, crName2, "a released repair must not charge a fault during unrelated validation")
	assert.NotEqual(t, crName1, crName2)
}

// staleNodeClient serves an old copy of one Node for the first read, as a lagging informer cache
// does, and then reads the API server.
type staleNodeClient struct {
	client.Client
	snapshot *corev1.Node
	served   bool
}

func (c *staleNodeClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	if node, ok := obj.(*corev1.Node); ok && key.Name == c.snapshot.Name && !c.served {
		c.served = true
		*node = *c.snapshot.DeepCopy()

		return nil
	}

	return ctrlRuntimeAPIReader.Get(ctx, key, obj, opts...)
}

// TestMaxAttempts_StaleCacheDoesNotRejectFaultAfterRelease checks the cap read before log
// collection: a cache that still shows the released validation must not end a new fault as
// capped. The decision is written back, so the stale read fails on the Node's resourceVersion.
func TestMaxAttempts_StaleCacheDoesNotRejectFaultAfterRelease(t *testing.T) {
	ctx := t.Context()
	nodeName := "validation-budget-stale-cache"

	r, _ := prepareValidationRepair(t, nodeName, 1)
	handOffToValidation(ctx, t, r, nodeName, "validation-failed")

	snapshot, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	releaseFromValidation(ctx, t, nodeName)
	applyQuarantineLabels(ctx, t, r, nodeName)

	r.annotationManager = annotation.NewNodeAnnotationManager(&staleNodeClient{
		Client: ctrlRuntimeClient, snapshot: snapshot,
	})

	event := datastore.EventWithToken{
		Event:       createQuarantineEvent(nodeName+"-fault-2", nodeName, protos.RecommendedAction_RESTART_BM),
		ResumeToken: []byte(nodeName + "-fault-2"),
	}
	_, err, done := r.tryStopIfAlreadyCapped(ctx, nodeName, "restart", 1, event, r.Watcher, r.healthEventStore)
	require.NoError(t, err)
	assert.False(t, done, "validation already released the node")
	assert.NotEqual(t, string(statemanager.RemediationFailedLabelValue), nodeStateLabel(ctx, t, nodeName))
}

// TestMaxAttempts_RecordAttemptResumesKeptCounts covers recording an attempt without the cap read
// before log collection, which falls through when it cannot read the Node.
func TestMaxAttempts_RecordAttemptResumesKeptCounts(t *testing.T) {
	ctx := t.Context()
	nodeName := "validation-budget-record"

	r, _ := prepareValidationRepair(t, nodeName, 1)
	handOffToValidation(ctx, t, r, nodeName, "validation-failed")

	attempts, err := r.annotationManager.RecordRemediationAttempt(ctx, nodeName, "restart")
	require.NoError(t, err)
	assert.Equal(t, 2, attempts)
}

// TestMaxAttempts_ResumedBudgetOutlivesValidationRelease checks that the decision is made once,
// when the new session records its first attempt: validation finishing later does not reset the
// budget of the session that is already running.
func TestMaxAttempts_ResumedBudgetOutlivesValidationRelease(t *testing.T) {
	ctx := t.Context()
	nodeName := "validation-budget-resumed"

	r, _ := prepareValidationRepair(t, nodeName, 2)

	handOffToValidation(ctx, t, r, nodeName, "validation-failed")

	applyQuarantineLabels(ctx, t, r, nodeName)
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, nodeName+"-fault-2"))
	crName2 := currentCR(ctx, t, r, nodeName)
	require.NotEmpty(t, crName2, "the second attempt is still within the budget")

	releaseFromValidation(ctx, t, nodeName)
	updateRebootNodeStatus(ctx, t, crName2, "Failed")

	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, nodeName+"-fault-3"))
	assert.Empty(t, currentCR(ctx, t, r, nodeName), "the third attempt exceeds the budget")
	assert.Equal(t, string(statemanager.RemediationFailedLabelValue), nodeStateLabel(ctx, t, nodeName))
}

// TestMaxAttempts_CancellationDuringValidationResetsBudget keeps explicit cancellation as a reset,
// even while validation still holds the node.
func TestMaxAttempts_CancellationDuringValidationResetsBudget(t *testing.T) {
	ctx := t.Context()
	nodeName := "validation-budget-cancelled"

	r, crName1 := prepareValidationRepair(t, nodeName, 1)

	setValidationState(ctx, t, nodeName, "validation-failed")

	_, err := r.reconcileEvent(ctx, &datastore.EventWithToken{
		Event:       createCancelledEvent(nodeName+"-cancel", nodeName, protos.RecommendedAction_RESTART_BM),
		ResumeToken: []byte(nodeName + "-cancel"),
	})
	require.NoError(t, err)

	applyQuarantineLabels(ctx, t, r, nodeName)
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, nodeName+"-fault-2"))

	crName2 := currentCR(ctx, t, r, nodeName)
	assert.NotEmpty(t, crName2, "cancellation starts a fresh budget")
	assert.NotEqual(t, crName1, crName2)
}

// TestMaxAttempts_FaultDuringValidationAtCapSkipsLogCollector checks the cap read before log
// collection also counts the attempts kept through the hand-off.
func TestMaxAttempts_FaultDuringValidationAtCapSkipsLogCollector(t *testing.T) {
	ctx := t.Context()
	nodeName := "validation-budget-log-collector"
	useLogCollectorNamespace(ctx, t, nodeName)

	r, _, _ := newCappedReconciler(t, 1, true)
	prepareQuarantinedNode(ctx, t, r, nodeName)
	setNodeUnschedulable(ctx, t, nodeName, true)
	cleanupValidationBudgetNode(t, nodeName)

	_, err := r.annotationManager.RecordRemediationAttempt(ctx, nodeName, "restart")
	require.NoError(t, err)

	handOffToValidation(ctx, t, r, nodeName, "validation-failed")

	applyQuarantineLabels(ctx, t, r, nodeName)
	require.NoError(t, reconcileQuarantineEvent(ctx, t, r, nodeName, nodeName+"-fault-2"))

	jobs, err := testClient.BatchV1().Jobs(nodeName).List(ctx, metav1.ListOptions{
		LabelSelector: "dgxc.nvidia.com/node-name=" + nodeName,
	})
	require.NoError(t, err)
	assert.Empty(t, jobs.Items)
	assert.Equal(t, string(statemanager.RemediationFailedLabelValue), nodeStateLabel(ctx, t, nodeName))
}
