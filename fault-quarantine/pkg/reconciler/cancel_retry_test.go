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
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/evaluator"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/nodecache"
)

// sessionEndTaintKey is only used by these tests, so they don't move the taint counters that
// other tests in the package compare.
const sessionEndTaintKey = "nvidia.com/session-end-test"

// cancelRecorder is an event watcher that records the cancels for a node. A cancel fails while
// failing is set, or while the node has failures left.
type cancelRecorder struct {
	MockEventWatcher

	mu        sync.Mutex
	failing   bool
	failures  map[string]int
	failed    map[string]int
	cancelled map[string][][]string
}

func newCancelRecorder(failures map[string]int) *cancelRecorder {
	if failures == nil {
		failures = map[string]int{}
	}

	recorder := &cancelRecorder{
		failures:  failures,
		failed:    map[string]int{},
		cancelled: map[string][][]string{},
	}

	recorder.CancelLatestQuarantiningEventsFn = func(_ context.Context, nodeName string, eventIDs []string, _ string) error {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()

		if recorder.failing || recorder.failures[nodeName] > 0 {
			if recorder.failures[nodeName] > 0 {
				recorder.failures[nodeName]--
			}

			recorder.failed[nodeName]++

			return errors.New("(NotWritablePrimary) not primary")
		}

		eventIDs = slices.Clone(eventIDs)
		slices.Sort(eventIDs)
		recorder.cancelled[nodeName] = append(recorder.cancelled[nodeName], eventIDs)

		return nil
	}

	return recorder
}

func (c *cancelRecorder) setFailing(failing bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.failing = failing
}

// snapshot returns how many cancels failed for the node and the event IDs of each one that was
// written.
func (c *cancelRecorder) snapshot(nodeName string) (int, [][]string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.failed[nodeName], slices.Clone(c.cancelled[nodeName])
}

// sessionEndRules cordons and taints a node for a GpuXidError.
func sessionEndRules() config.TomlConfig {
	return config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.QuarantineRuleSet{{
			Enabled:  true,
			Name:     "xid-rule",
			Version:  "1",
			Priority: 10,
			Match:    config.Match{Any: []config.Rule{{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError'"}}},
			Cordon:   config.Cordon{ShouldCordon: true},
			Taint:    config.Taint{Key: sessionEndTaintKey, Value: "true", Effect: "NoSchedule"},
		}},
	}
}

// createQuarantinedTestNode creates a node the way FQ leaves it after a fatal GpuXidError on
// GPU 0 with the given event ID. The node is cordoned unless uncordoned is set.
func createQuarantinedTestNode(ctx context.Context, t *testing.T, eventID string, uncordoned bool) string {
	t.Helper()

	nodeName := "test-node-" + generateShortTestID()

	quarantined := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	quarantined.AddOrUpdateEvent(&protos.HealthEvent{
		Id: eventID, NodeName: nodeName, Agent: "gpu-health-monitor", CheckName: "GpuXidError",
		ComponentClass: "GPU", Version: 1, EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
	})
	quarantinedBytes, err := json.Marshal(quarantined)
	require.NoError(t, err)

	createE2ETestNode(ctx, t, nodeName, map[string]string{
		common.QuarantineHealthEventAnnotationKey: string(quarantinedBytes),
		common.QuarantineHealthEventAppliedTaintsAnnotationKey: `[{"Key":"` + sessionEndTaintKey +
			`","Value":"true","Effect":"NoSchedule"}]`,
		common.QuarantineHealthEventIsCordonedAnnotationKey: "True",
	}, map[string]string{
		statemanager.NVSentinelStateLabelKey: string(statemanager.QuarantinedLabelValue),
	}, []corev1.Taint{{Key: sessionEndTaintKey, Value: "true", Effect: "NoSchedule"}}, !uncordoned)

	t.Cleanup(func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(context.Background(), nodeName, metav1.DeleteOptions{})
	})

	return nodeName
}

func updateTestNode(ctx context.Context, t *testing.T, nodeName string, change func(node *corev1.Node)) {
	t.Helper()

	require.NoError(t, retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		change(node)

		_, err = e2eTestClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})

		return err
	}))
}

// pendingSessionEnds returns how many nodes are still waiting for their cancel.
func pendingSessionEnds(r *Reconciler) int {
	r.pendingCancels.mu.Lock()
	defer r.pendingCancels.mu.Unlock()

	return len(r.pendingCancels.nodes)
}

func uncordonTestNode(node *corev1.Node) { node.Spec.Unschedulable = false }

func untaintTestNode(node *corev1.Node) {
	node.Spec.Taints = slices.DeleteFunc(node.Spec.Taints, func(taint corev1.Taint) bool {
		return taint.Key == sessionEndTaintKey
	})
}

func getTestNode(ctx context.Context, t *testing.T, nodeName string) *corev1.Node {
	t.Helper()

	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	return node
}

// sessionEventIDs returns the sorted event IDs in the node's quarantine annotation.
func sessionEventIDs(node *corev1.Node) []string {
	ids := quarantineEventIDs(node.Annotations[common.QuarantineHealthEventAnnotationKey])
	slices.Sort(ids)

	return ids
}

func waitForQuarantinedNodeInCache(t *testing.T, r *Reconciler, nodeName string) {
	t.Helper()

	require.Eventually(t, func() bool {
		node, err := r.k8sClient.NodeInformer.GetNode(nodeName)
		return err == nil && node.Spec.Unschedulable && len(node.Spec.Taints) > 0
	}, eventuallyTimeout, eventuallyPollInterval, "the node informer should see the quarantined node")
}

// A manual uncordon or untaint whose datastore write fails. The node keeps its annotations while
// the write fails, and it's cleaned only after the worker's retry gets through.
func TestE2E_ManualChange_KeepsTheNodeUntilTheCancellationIsWritten(t *testing.T) {
	tests := []struct {
		name             string
		operatorAction   func(node *corev1.Node)
		manualAnnotation string
	}{
		{"manual uncordon", uncordonTestNode, common.QuarantinedNodeUncordonedManuallyAnnotationKey},
		{"manual untaint", untaintTestNode, common.QuarantinedNodeIsUntaintedManuallyAnnotationKey},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
			defer cancel()

			eventID := generateTestID()
			nodeName := createQuarantinedTestNode(ctx, t, eventID, false)

			recorder := newCancelRecorder(map[string]int{nodeName: 2})
			r, _, _, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
				TomlConfig:   config.TomlConfig{LabelPrefix: "k8s.nvidia.com/"},
				EventWatcher: recorder,
			})

			waitForQuarantinedNodeInCache(t, r, nodeName)
			updateTestNode(ctx, t, nodeName, tt.operatorAction)

			require.Eventually(t, func() bool {
				failed, _ := recorder.snapshot(nodeName)
				return failed > 0
			}, eventuallyTimeout, statusCheckPollInterval, "the cancellation should be tried")

			node := getTestNode(ctx, t, nodeName)
			assert.Equal(t, []string{eventID}, sessionEventIDs(node),
				"the node keeps its quarantine annotation while the cancellation is not written")
			assert.NotContains(t, node.Annotations, tt.manualAnnotation)
			assert.Contains(t, node.Annotations, common.QuarantineHealthEventAppliedTaintsAnnotationKey)
			_, pending := r.pendingCancels.get(nodeName)
			assert.True(t, pending, "the node waits for its cancellation")

			require.Eventually(t, func() bool {
				node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
				if err != nil {
					return false
				}

				_, quarantined := node.Annotations[common.QuarantineHealthEventAnnotationKey]

				return !quarantined && node.Annotations[tt.manualAnnotation] != ""
			}, 20*time.Second, eventuallyPollInterval, "the node should be cleaned once the cancellation is written")

			failed, cancelled := recorder.snapshot(nodeName)
			assert.Equal(t, 2, failed, "two failed attempts")
			assert.Equal(t, [][]string{{eventID}}, cancelled, "one cancellation, of the events the node named")

			node = getTestNode(ctx, t, nodeName)
			assert.NotContains(t, node.Annotations, common.QuarantineHealthEventIsCordonedAnnotationKey)
			assert.NotContains(t, node.Annotations, common.QuarantineHealthEventAppliedTaintsAnnotationKey)

			require.Eventually(t, func() bool { return pendingSessionEnds(r) == 0 },
				eventuallyTimeout, statusCheckPollInterval, "no node should wait for a cancellation any more")
		})
	}
}

// FQ restarts while a cancel is still owed, or the node is uncordoned while FQ is down. Either
// way the stale-state check queues the node at startup.
func TestE2E_PendingCancellation_IsFinishedAfterARestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	eventID := generateTestID()
	nodeName := createQuarantinedTestNode(ctx, t, eventID, true)

	recorder := newCancelRecorder(map[string]int{nodeName: 1})
	setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:   config.TomlConfig{LabelPrefix: "k8s.nvidia.com/"},
		EventWatcher: recorder,
	})

	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		_, quarantined := node.Annotations[common.QuarantineHealthEventAnnotationKey]

		return !quarantined && node.Annotations[common.QuarantinedNodeUncordonedManuallyAnnotationKey] != ""
	}, 20*time.Second, eventuallyPollInterval, "the session should be cancelled and the node cleaned after the restart")

	failed, cancelled := recorder.snapshot(nodeName)
	assert.Equal(t, 1, failed)
	assert.Equal(t, [][]string{{eventID}}, cancelled)
}

// A new fault on a node whose cancel isn't written yet. The event finishes the cancel first and
// then starts its own quarantine, and the worker's later retry leaves that one alone.
func TestE2E_NewFault_EndsTheEarlierSessionFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 60*time.Second)
	defer cancel()

	nodeName := "test-node-" + generateShortTestID()
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	t.Cleanup(func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(context.Background(), nodeName, metav1.DeleteOptions{})
	})

	recorder := newCancelRecorder(nil)
	r, watcher, getStatus, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:   sessionEndRules(),
		EventWatcher: recorder,
	})

	send := func(gpu string) string {
		id := generateTestID()
		watcher.EventsChan <- &TestEvent{Data: createHealthEventBSON(id, nodeName, "GpuXidError", false, true,
			[]*protos.Entity{{EntityType: "GPU", EntityValue: gpu}}, model.StatusInProgress)}

		return id
	}

	first := send("0")
	require.Eventually(t, func() bool {
		status := getStatus(first)
		return status != nil && *status == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval)
	waitForQuarantinedNodeInCache(t, r, nodeName)

	// The cancel fails until the worker has backed off to several seconds between attempts.
	recorder.setFailing(true)
	updateTestNode(ctx, t, nodeName, uncordonTestNode)

	require.Eventually(t, func() bool {
		failed, _ := recorder.snapshot(nodeName)
		return failed >= 3
	}, 15*time.Second, statusCheckPollInterval, "the worker should try the cancellation three times")

	assert.Equal(t, []string{first}, sessionEventIDs(getTestNode(ctx, t, nodeName)),
		"the node keeps the ended session while its cancellation is not written")

	recorder.setFailing(false)

	second := send("1")
	require.Eventually(t, func() bool {
		status := getStatus(second)
		return status != nil
	}, statusCheckTimeout, statusCheckPollInterval, "the new fault should be handled")
	assert.Equal(t, model.Quarantined, *getStatus(second), "the new fault starts a session of its own")

	_, cancelled := recorder.snapshot(nodeName)
	assert.Equal(t, [][]string{{first}}, cancelled, "the earlier session is cancelled, without the new fault")

	node := getTestNode(ctx, t, nodeName)
	assert.True(t, node.Spec.Unschedulable, "the new fault cordons the node again")
	assert.Equal(t, []string{second}, sessionEventIDs(node), "the new session holds only the new fault")
	assert.NotContains(t, node.Annotations, common.QuarantinedNodeUncordonedManuallyAnnotationKey)

	// The worker tries again a few seconds later and should find nothing left to do.
	assert.Never(t, func() bool {
		_, cancelled := recorder.snapshot(nodeName)
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})

		return len(cancelled) != 1 || err != nil || !node.Spec.Unschedulable ||
			!slices.Equal(sessionEventIDs(node), []string{second})
	}, 6*time.Second, eventuallyPollInterval, "the retry of the worker must leave the new session alone")
}

// A new fault while the cancel keeps failing. A failing write must not block the quarantine, so
// the node is cleaned without the cancel and the fault starts its own quarantine.
func TestE2E_NewFault_QuarantinesTheNodeWhileTheCancellationFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 60*time.Second)
	defer cancel()

	eventID := generateTestID()
	nodeName := createQuarantinedTestNode(ctx, t, eventID, false)

	recorder := newCancelRecorder(nil)
	recorder.setFailing(true)

	r, watcher, getStatus, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:   sessionEndRules(),
		EventWatcher: recorder,
	})

	waitForQuarantinedNodeInCache(t, r, nodeName)
	updateTestNode(ctx, t, nodeName, uncordonTestNode)

	require.Eventually(t, func() bool {
		failed, _ := recorder.snapshot(nodeName)
		return failed > 0
	}, eventuallyTimeout, statusCheckPollInterval, "the worker should try the cancellation")

	second := generateTestID()
	watcher.EventsChan <- &TestEvent{Data: createHealthEventBSON(second, nodeName, "GpuXidError", false, true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "1"}}, model.StatusInProgress)}

	require.Eventually(t, func() bool {
		status := getStatus(second)
		return status != nil
	}, statusCheckTimeout, statusCheckPollInterval, "the new fault should be handled although the cancellation fails")
	assert.Equal(t, model.Quarantined, *getStatus(second), "the new fault starts a session of its own")

	node := getTestNode(ctx, t, nodeName)
	assert.True(t, node.Spec.Unschedulable, "the new fault cordons the node")
	assert.Equal(t, []string{second}, sessionEventIDs(node), "the new session holds only the new fault")
	assert.NotContains(t, node.Annotations, common.QuarantinedNodeUncordonedManuallyAnnotationKey)

	// The cancel works again. The worker has to leave the new quarantine alone.
	recorder.setFailing(false)

	require.Eventually(t, func() bool { return pendingSessionEnds(r) == 0 },
		20*time.Second, statusCheckPollInterval, "the worker should find nothing left to end")

	_, cancelled := recorder.snapshot(nodeName)
	assert.Empty(t, cancelled, "nothing of the new session is cancelled")

	node = getTestNode(ctx, t, nodeName)
	assert.True(t, node.Spec.Unschedulable)
	assert.Equal(t, []string{second}, sessionEventIDs(node))
}

// A healthy event on a node whose cancel keeps failing. The event is handled against the
// quarantine as it is and ends it, and the worker then finds nothing to do.
func TestE2E_HealthyEvent_WhileTheCancellationFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 60*time.Second)
	defer cancel()

	eventID := generateTestID()
	nodeName := createQuarantinedTestNode(ctx, t, eventID, false)

	recorder := newCancelRecorder(nil)
	recorder.setFailing(true)

	r, watcher, getStatus, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:   sessionEndRules(),
		EventWatcher: recorder,
	})

	waitForQuarantinedNodeInCache(t, r, nodeName)
	updateTestNode(ctx, t, nodeName, uncordonTestNode)

	require.Eventually(t, func() bool {
		failed, _ := recorder.snapshot(nodeName)
		return failed > 0
	}, eventuallyTimeout, statusCheckPollInterval, "the worker should try the cancellation")

	healthy := generateTestID()
	watcher.EventsChan <- &TestEvent{Data: createHealthEventBSON(healthy, nodeName, "GpuXidError", true, false,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}}, model.StatusInProgress)}

	require.Eventually(t, func() bool {
		status := getStatus(healthy)
		return status != nil && *status == model.UnQuarantined
	}, statusCheckTimeout, statusCheckPollInterval, "the healthy event should end the session")

	recorder.setFailing(false)

	require.Eventually(t, func() bool { return pendingSessionEnds(r) == 0 },
		20*time.Second, statusCheckPollInterval, "the worker should find nothing left to end")

	_, cancelled := recorder.snapshot(nodeName)
	assert.Empty(t, cancelled, "a session that a healthy event ended is not cancelled")
	assert.NotContains(t, getTestNode(ctx, t, nodeName).Annotations, common.QuarantineHealthEventAnnotationKey)
}

// The operator uncordons the node and cordons it again while the cancel is failing. The node
// looks quarantined, but the events still get cancelled, the annotations go and the operator's
// cordon stays.
func TestE2E_ReCordonBeforeTheCancellationIsWritten_StillEndsTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 60*time.Second)
	defer cancel()

	eventID := generateTestID()
	nodeName := createQuarantinedTestNode(ctx, t, eventID, false)

	recorder := newCancelRecorder(nil)
	recorder.setFailing(true)

	r, _, _, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:   config.TomlConfig{LabelPrefix: "k8s.nvidia.com/"},
		EventWatcher: recorder,
	})

	waitForQuarantinedNodeInCache(t, r, nodeName)
	updateTestNode(ctx, t, nodeName, uncordonTestNode)

	require.Eventually(t, func() bool {
		failed, _ := recorder.snapshot(nodeName)
		return failed > 0
	}, eventuallyTimeout, statusCheckPollInterval, "the worker should try the cancellation")

	updateTestNode(ctx, t, nodeName, func(node *corev1.Node) { node.Spec.Unschedulable = true })
	recorder.setFailing(false)

	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		_, quarantined := node.Annotations[common.QuarantineHealthEventAnnotationKey]

		return !quarantined && node.Annotations[common.QuarantinedNodeUncordonedManuallyAnnotationKey] != ""
	}, 20*time.Second, eventuallyPollInterval, "the session should end although the node is cordoned again")

	_, cancelled := recorder.snapshot(nodeName)
	assert.Equal(t, [][]string{{eventID}}, cancelled)

	node := getTestNode(ctx, t, nodeName)
	assert.True(t, node.Spec.Unschedulable, "the cordon of the operator stays")
	assert.NotContains(t, node.Annotations, common.QuarantineHealthEventIsCordonedAnnotationKey)
}

// A new fault on a node that was uncordoned and cordoned again while the cancel was failing.
// Only the queued entry shows the quarantine ended, and the fault has to finish that first.
func TestE2E_NewFault_AfterAReCordon_EndsTheEarlierSessionFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 60*time.Second)
	defer cancel()

	eventID := generateTestID()
	nodeName := createQuarantinedTestNode(ctx, t, eventID, false)

	recorder := newCancelRecorder(nil)
	recorder.setFailing(true)

	r, watcher, getStatus, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:   sessionEndRules(),
		EventWatcher: recorder,
	})

	waitForQuarantinedNodeInCache(t, r, nodeName)
	updateTestNode(ctx, t, nodeName, uncordonTestNode)

	// After three failures the worker waits several seconds for its next attempt.
	require.Eventually(t, func() bool {
		failed, _ := recorder.snapshot(nodeName)
		return failed >= 3
	}, 15*time.Second, statusCheckPollInterval, "the worker should try the cancellation three times")

	updateTestNode(ctx, t, nodeName, func(node *corev1.Node) { node.Spec.Unschedulable = true })
	waitForQuarantinedNodeInCache(t, r, nodeName)

	recorder.setFailing(false)

	second := generateTestID()
	watcher.EventsChan <- &TestEvent{Data: createHealthEventBSON(second, nodeName, "GpuXidError", false, true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "1"}}, model.StatusInProgress)}

	require.Eventually(t, func() bool {
		status := getStatus(second)
		return status != nil
	}, statusCheckTimeout, statusCheckPollInterval, "the new fault should be handled")
	assert.Equal(t, model.Quarantined, *getStatus(second), "the new fault starts a session of its own")

	_, cancelled := recorder.snapshot(nodeName)
	assert.Equal(t, [][]string{{eventID}}, cancelled, "the earlier session is cancelled, without the new fault")

	node := getTestNode(ctx, t, nodeName)
	assert.True(t, node.Spec.Unschedulable)
	assert.Equal(t, []string{second}, sessionEventIDs(node), "the new session holds only the new fault")
}

// The event must not go on until the informer cache shows the cleaned node, because it reads
// the node from that cache. The informer is stopped here, so the cache never catches up.
func TestEndSessionBeforeEvent_WaitsForTheNodeInformer(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	eventID := generateTestID()
	nodeName := createQuarantinedTestNode(ctx, t, eventID, true)

	nodeInformer, err := informer.NewNodeInformer(e2eTestClient, 0, informer.GPUNodeLabel, informer.GPUNodeLabelValue,
		nodecache.Derive(config.TomlConfig{}, nodecache.Operational{GPUNodeLabelKey: informer.GPUNodeLabel}))
	require.NoError(t, err)

	recorder := newCancelRecorder(nil)
	r := NewReconciler(ReconcilerConfig{TomlConfig: config.TomlConfig{LabelPrefix: "k8s.nvidia.com/"}},
		&informer.FaultQuarantineClient{
			Clientset: e2eTestClient, DynamicClient: e2eTestDynamicClient, NodeInformer: nodeInformer,
		}, nil)
	r.eventWatcher = recorder

	// No worker runs here, so only the event can finish the cancel.
	nodeInformer.SetOnManualUncordonCallback(func(string) error { return nil })
	nodeInformer.SetOnManualUntaintCallback(func(string) error { return nil })

	stopInformer := make(chan struct{})
	informerStopped := make(chan struct{})

	go func() {
		defer close(informerStopped)

		_ = nodeInformer.Run(stopInformer)
	}()

	require.Eventually(t, func() bool {
		_, err := nodeInformer.GetNode(nodeName)
		return nodeInformer.HasSynced() && err == nil
	}, eventuallyTimeout, statusCheckPollInterval, "the node informer should see the node")

	close(stopInformer)
	<-informerStopped

	eventCtx, stopEvent := context.WithTimeout(ctx, time.Second)
	defer stopEvent()

	err = r.endSessionBeforeEvent(eventCtx, &protos.HealthEvent{NodeName: nodeName, IsFatal: true})
	require.ErrorIs(t, err, context.DeadlineExceeded, "the event must wait for the node informer")

	_, cancelled := recorder.snapshot(nodeName)
	assert.Equal(t, [][]string{{eventID}}, cancelled, "the session itself is ended")
	assert.NotContains(t, getTestNode(ctx, t, nodeName).Annotations, common.QuarantineHealthEventAnnotationKey)
}

// Dry run is switched off and the datastore write fails. The dry-run record stays on the node
// until the worker's retry gets through.
func TestE2E_DryRunSwitchedOff_KeepsTheRecordUntilTheCancellationIsWritten(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := newDryRunTestNode(ctx, t, "e2e-dryrun-cancel-")
	quarantineInDryRun(ctx, t, nodeName, dryRunRules(true, false))

	recorded := sessionEventIDs(getTestNode(ctx, t, nodeName))
	require.Len(t, recorded, 1)

	// Restart with dry run switched off.
	recorder := newCancelRecorder(map[string]int{nodeName: 2})
	setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:   dryRunRules(true, false),
		DryRun:       false,
		EventWatcher: recorder,
	})

	require.Eventually(t, func() bool {
		failed, _ := recorder.snapshot(nodeName)
		return failed > 0
	}, eventuallyTimeout, statusCheckPollInterval, "the cancellation should be tried")

	node := getTestNode(ctx, t, nodeName)
	assert.Equal(t, recorded, sessionEventIDs(node), "the record stays while the cancellation is not written")
	assert.Contains(t, node.Annotations, common.QuarantineHealthEventDryRunAnnotationKey)

	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		_, quarantined := node.Annotations[common.QuarantineHealthEventAnnotationKey]
		_, marked := node.Annotations[common.QuarantineHealthEventDryRunAnnotationKey]

		return !quarantined && !marked
	}, 20*time.Second, eventuallyPollInterval, "the record should be discarded once the cancellation is written")

	failed, cancelled := recorder.snapshot(nodeName)
	assert.Equal(t, 2, failed)
	assert.Equal(t, [][]string{recorded}, cancelled)
	assert.NotContains(t, getTestNode(ctx, t, nodeName).Annotations, common.QuarantinedNodeUncordonedManuallyAnnotationKey,
		"discarding a dry-run quarantine is not a manual uncordon")
}

// A real fault on a node whose dry-run record still waits for its cancel. The record lists a
// cordon that was never applied, so the record is discarded first and the fault quarantines
// the node for real.
func TestE2E_DryRunSwitchedOff_NewFaultBeforeTheRecordIsDiscarded(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 60*time.Second)
	defer cancel()

	nodeName := newDryRunTestNode(ctx, t, "e2e-dryrun-fault-")
	quarantineInDryRun(ctx, t, nodeName, dryRunRules(true, false))

	recorded := sessionEventIDs(getTestNode(ctx, t, nodeName))
	require.Len(t, recorded, 1)

	// Restart with dry run switched off, while the cancellation fails.
	recorder := newCancelRecorder(nil)
	recorder.setFailing(true)

	_, watcher, getStatus, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:   dryRunRules(true, false),
		DryRun:       false,
		EventWatcher: recorder,
	})

	require.Eventually(t, func() bool {
		failed, _ := recorder.snapshot(nodeName)
		return failed > 0
	}, eventuallyTimeout, statusCheckPollInterval, "the worker should try the cancellation")

	id := generateTestID()
	watcher.EventsChan <- &TestEvent{Data: createHealthEventBSON(id, nodeName, "GpuXidError", false, true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "1"}}, model.StatusInProgress)}

	require.Eventually(t, func() bool {
		status := getStatus(id)
		return status != nil && *status == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval, "the real fault should start a session of its own")

	node := getTestNode(ctx, t, nodeName)
	assert.True(t, node.Spec.Unschedulable, "the real fault cordons the node")
	assert.Equal(t, []string{id}, sessionEventIDs(node), "the dry-run record is gone")
	assert.NotContains(t, node.Annotations, common.QuarantineHealthEventDryRunAnnotationKey)
}

// A restart that stays in dry run keeps the record and cancels nothing.
func TestE2E_DryRunRestart_CancelsNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := newDryRunTestNode(ctx, t, "e2e-dryrun-keep-")
	quarantineInDryRun(ctx, t, nodeName, dryRunRules(true, false))

	recorded := sessionEventIDs(getTestNode(ctx, t, nodeName))

	recorder := newCancelRecorder(nil)
	r, _, _, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:   dryRunRules(true, false),
		DryRun:       true,
		EventWatcher: recorder,
	})

	// The stale-state check queues the node, and the worker finds nothing to do.
	require.Eventually(t, func() bool { return pendingSessionEnds(r) == 0 },
		eventuallyTimeout, statusCheckPollInterval, "the worker should look at the node")

	assert.Never(t, func() bool {
		failed, cancelled := recorder.snapshot(nodeName)
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})

		return failed > 0 || len(cancelled) > 0 || err != nil || !slices.Equal(sessionEventIDs(node), recorded)
	}, neverTimeout, neverPollInterval, "in dry run the record and its events are kept")
}

// A retry after a failed node cleanup doesn't cancel again, unless the annotation changed.
func TestCancelEndedSession_NotRepeatedForTheSameAnnotation(t *testing.T) {
	const nodeName = "node-a"

	annotation := func(ids ...string) map[string]string {
		events := healthEventsAnnotation.NewHealthEventsAnnotationMap()
		for gpu, id := range ids {
			events.AddOrUpdateEvent(&protos.HealthEvent{
				Id: id, NodeName: nodeName, Agent: "gpu-health-monitor", CheckName: "GpuXidError", ComponentClass: "GPU",
				Version: 1, EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: string(rune('0' + gpu))}},
			})
		}

		value, err := json.Marshal(events)
		require.NoError(t, err)

		return map[string]string{common.QuarantineHealthEventAnnotationKey: string(value)}
	}

	recorder := newCancelRecorder(nil)
	r := &Reconciler{eventWatcher: recorder}
	change := informer.ManualChange{Uncordoned: true}
	first, second := generateTestID(), generateTestID()

	r.pendingCancels.add(nodeName, change)

	require.NoError(t, r.cancelEndedSession(context.Background(), nodeName, annotation(first), change, time.Second))
	require.NoError(t, r.cancelEndedSession(context.Background(), nodeName, annotation(first), change, time.Second))

	_, cancelled := recorder.snapshot(nodeName)
	assert.Len(t, cancelled, 1, "the same session is cancelled once")

	// An event that joined the quarantine in between gets cancelled as well.
	require.NoError(t, r.cancelEndedSession(context.Background(), nodeName, annotation(first, second), change, time.Second))

	_, cancelled = recorder.snapshot(nodeName)
	assert.Len(t, cancelled, 2)

	// A failed cancel is tried again.
	r.pendingCancels.remove(nodeName)
	r.pendingCancels.add(nodeName, change)
	recorder.setFailing(true)
	require.Error(t, r.cancelEndedSession(context.Background(), nodeName, annotation(first), change, time.Second))
	recorder.setFailing(false)
	require.NoError(t, r.cancelEndedSession(context.Background(), nodeName, annotation(first), change, time.Second))

	_, cancelled = recorder.snapshot(nodeName)
	assert.Len(t, cancelled, 3)
}

// matchEveryEvent is a ruleset evaluator that matches every event.
type matchEveryEvent struct{ name string }

func (e matchEveryEvent) Evaluate(context.Context, *protos.HealthEvent) (common.RuleEvaluationResult, error) {
	return common.RuleEvaluationSuccess, nil
}

func (e matchEveryEvent) GetName() string { return e.name }

// The same new fault, replayed by a cold start. The replay reads the node once per event, so
// the event has to read it again after the cancel is finished.
func TestE2E_NewFault_ReplayedAtColdStart_EndsTheEarlierSessionFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 60*time.Second)
	defer cancel()

	eventID := generateTestID()
	nodeName := createQuarantinedTestNode(ctx, t, eventID, true)

	recorder := newCancelRecorder(nil)
	recorder.setFailing(true)

	rules := sessionEndRules()
	r, _, _, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{TomlConfig: rules, EventWatcher: recorder})

	// The worker tries at the start and then waits several seconds for its next attempt.
	require.Eventually(t, func() bool {
		failed, _ := recorder.snapshot(nodeName)
		return failed >= 3
	}, 15*time.Second, statusCheckPollInterval, "the worker should try the cancellation three times")

	recorder.setFailing(false)

	rule := rules.RuleSets[0]
	second := generateTestID()

	status, err := r.handleEvent(coldstart.WithRecoveryContext(ctx), &model.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{
			Id: second, NodeName: nodeName, Agent: "gpu-health-monitor", CheckName: "GpuXidError", ComponentClass: "GPU",
			Version: 1, IsFatal: true, EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
		},
		HealthEventStatus: &protos.HealthEventStatus{},
	}, []evaluator.RuleSetEvaluatorIface{matchEveryEvent{name: rule.Name}}, rulesetsConfig{
		TaintConfigMap:     map[string]*config.Taint{rule.Name: &rule.Taint},
		LabelConfigMap:     map[string]*config.Label{},
		CordonConfigMap:    map[string]bool{rule.Name: true},
		RuleSetPriorityMap: map[string]int{rule.Name: rule.Priority},
		RuleSetOrderMap:    map[string]int{rule.Name: 0},
	})
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Equal(t, model.Quarantined, *status, "the replayed fault starts a session of its own")

	_, cancelled := recorder.snapshot(nodeName)
	assert.Equal(t, [][]string{{eventID}}, cancelled, "the earlier session is cancelled, without the new fault")

	node := getTestNode(ctx, t, nodeName)
	assert.True(t, node.Spec.Unschedulable, "the new fault cordons the node again")
	assert.Equal(t, []string{second}, sessionEventIDs(node), "the new session holds only the new fault")
}

// A second caller for the same node waits for the first and gives up at its deadline, so a
// write that hangs can't hold an event forever. Another node doesn't wait.
func TestEndSession_OneAtATime(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	eventID := generateTestID()
	nodeName := createQuarantinedTestNode(ctx, t, eventID, true)

	entered := make(chan struct{})
	release := make(chan struct{})

	var calls atomic.Int32

	watcher := &MockEventWatcher{
		CancelLatestQuarantiningEventsFn: func(_ context.Context, _ string, _ []string, _ string) error {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
			}

			return nil
		},
	}

	r, _, _, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:   config.TomlConfig{LabelPrefix: "k8s.nvidia.com/"},
		EventWatcher: watcher,
	})

	// The worker picks up the node at the start and is held inside the cancel.
	select {
	case <-entered:
	case <-ctx.Done():
		require.FailNow(t, "the worker should start the cancellation")
	}

	_, err := r.endSession(ctx, nodeName, sessionEndOptions{
		lockTimeout: 200 * time.Millisecond, cancelTimeout: cancelAttemptTimeout,
	})
	require.ErrorIs(t, err, context.DeadlineExceeded, "the second call waits and stops at its deadline")
	assert.Equal(t, int32(1), calls.Load(), "the second call must not run next to the first")

	// Another node doesn't wait for this one.
	otherNode := createQuarantinedTestNode(ctx, t, generateTestID(), true)
	_, err = r.endSession(ctx, otherNode, sessionEndOptions{
		lockTimeout: 200 * time.Millisecond, cancelTimeout: cancelAttemptTimeout,
	})
	require.NoError(t, err, "another node is not held up")
	assert.Equal(t, int32(2), calls.Load())

	close(release)

	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		_, quarantined := node.Annotations[common.QuarantineHealthEventAnnotationKey]

		return !quarantined
	}, eventuallyTimeout, eventuallyPollInterval, "the first call should finish and clean the node")

	// The node is clean now, so a later call finds nothing to do.
	_, err = r.endSession(ctx, nodeName, sessionEndOptions{
		lockTimeout: cancelAttemptTimeout, cancelTimeout: cancelAttemptTimeout,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(2), calls.Load())
}

// The normal case. An event on a node that's still cordoned and tainted joins the quarantine
// and nothing is cancelled.
func TestE2E_QuarantinedNode_NextEventDoesNotEndTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	eventID := generateTestID()
	nodeName := createQuarantinedTestNode(ctx, t, eventID, false)

	recorder := newCancelRecorder(nil)
	r, watcher, getStatus, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:   sessionEndRules(),
		EventWatcher: recorder,
	})

	waitForQuarantinedNodeInCache(t, r, nodeName)

	second := generateTestID()
	watcher.EventsChan <- &TestEvent{Data: createHealthEventBSON(second, nodeName, "GpuXidError", false, true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "1"}}, model.StatusInProgress)}

	require.Eventually(t, func() bool {
		status := getStatus(second)
		return status != nil && *status == model.AlreadyQuarantined
	}, statusCheckTimeout, statusCheckPollInterval, "the event should join the session of the node")

	assert.Never(t, func() bool {
		failed, cancelled := recorder.snapshot(nodeName)
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})

		return failed > 0 || len(cancelled) > 0 || err != nil || !node.Spec.Unschedulable ||
			!slices.Equal(sessionEventIDs(node), slices.Sorted(slices.Values([]string{eventID, second})))
	}, neverTimeout, neverPollInterval, "a node that is still quarantined must keep its session")
}

// One update both uncordons and untaints the node. The events are cancelled once, and the node
// gets both manual annotations and both metrics as before.
func TestE2E_UncordonAndUntaintInOneUpdate_EndsTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	eventID := generateTestID()
	nodeName := createQuarantinedTestNode(ctx, t, eventID, false)

	recorder := newCancelRecorder(nil)
	r, _, _, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:   config.TomlConfig{LabelPrefix: "k8s.nvidia.com/"},
		EventWatcher: recorder,
	})

	waitForQuarantinedNodeInCache(t, r, nodeName)

	uncordonsBefore := getCounterVecValue(t, metrics.TotalNodesManuallyUncordoned, nodeName)
	untaintsBefore := getCounterVecValue(t, metrics.TotalNodesManuallyUntainted, nodeName)

	updateTestNode(ctx, t, nodeName, func(node *corev1.Node) {
		uncordonTestNode(node)
		untaintTestNode(node)
	})

	require.Eventually(t, func() bool {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		_, quarantined := node.Annotations[common.QuarantineHealthEventAnnotationKey]

		return !quarantined &&
			node.Annotations[common.QuarantinedNodeUncordonedManuallyAnnotationKey] != "" &&
			node.Annotations[common.QuarantinedNodeIsUntaintedManuallyAnnotationKey] != ""
	}, eventuallyTimeout, eventuallyPollInterval, "the node should be cleaned with both manual annotations")

	_, cancelled := recorder.snapshot(nodeName)
	assert.Equal(t, [][]string{{eventID}}, cancelled, "the session is cancelled once")

	assert.Equal(t, uncordonsBefore+1, getCounterVecValue(t, metrics.TotalNodesManuallyUncordoned, nodeName))
	assert.Equal(t, untaintsBefore+1, getCounterVecValue(t, metrics.TotalNodesManuallyUntainted, nodeName))
}

func TestPendingCancellations_SetTheGauge(t *testing.T) {
	gauge := func() float64 {
		metric := &dto.Metric{}
		require.NoError(t, metrics.NodesPendingCancellation.Write(metric))

		return metric.GetGauge().GetValue()
	}

	var pending pendingCancellations

	pending.add("node-a", informer.ManualChange{Uncordoned: true})
	pending.add("node-b", informer.ManualChange{Untainted: true})
	pending.add("node-a", informer.ManualChange{Untainted: true})
	assert.Equal(t, float64(2), gauge(), "a node counts once")

	nodeA, _ := pending.get("node-a")
	assert.Equal(t, informer.ManualChange{Uncordoned: true, Untainted: true}, nodeA.seen,
		"what the node informer saw of a node adds up")

	pending.remove("node-a")
	assert.Equal(t, float64(1), gauge())

	pending.remove("node-b")
	pending.remove("node-b")
	assert.Equal(t, float64(0), gauge())
}

func TestQuarantineEventIDs(t *testing.T) {
	events := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	for gpu, id := range map[string]string{"0": "6741f2a0c3b1d4e5f6a7b8c9", "1": "6741f2a0c3b1d4e5f6a7b8ca", "2": ""} {
		events.AddOrUpdateEvent(&protos.HealthEvent{
			Id: id, NodeName: "node", Agent: "gpu-health-monitor", CheckName: "GpuXidError", ComponentClass: "GPU",
			Version: 1, EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: gpu}},
		})
	}

	eventsJSON, err := json.Marshal(events)
	require.NoError(t, err)

	// Older FQ versions wrote a single event here, not a list.
	single, err := json.Marshal(&protos.HealthEvent{
		Id: "12345", NodeName: "node", Agent: "gpu-health-monitor", CheckName: "GpuXidError", ComponentClass: "GPU",
		Version: 1, EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
	})
	require.NoError(t, err)

	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{"events with and without an ID", string(eventsJSON), []string{"6741f2a0c3b1d4e5f6a7b8c9", "6741f2a0c3b1d4e5f6a7b8ca"}},
		{"single event of an older version", string(single), []string{"12345"}},
		{"no annotation", "", nil},
		{"empty list", "[]", nil},
		{"not JSON", "quarantined", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := quarantineEventIDs(tt.value)
			slices.Sort(got)
			assert.Equal(t, tt.want, got)
		})
	}
}
