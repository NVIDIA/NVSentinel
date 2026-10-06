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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/breaker"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
)

func dryRunRules(cordon bool, taint bool) config.TomlConfig {
	rule := config.QuarantineRuleSet{
		Enabled:  true,
		Name:     "dry-run-rule",
		Version:  "1",
		Priority: 10,
		Match:    config.Match{Any: []config.Rule{{Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError'"}}},
		Cordon:   config.Cordon{ShouldCordon: cordon},
	}
	if taint {
		rule.Taint = config.Taint{Key: "nvidia.com/dry-run-test", Value: "true", Effect: "NoSchedule"}
	}

	return config.TomlConfig{LabelPrefix: "k8s.nvidia.com/", RuleSets: []config.QuarantineRuleSet{rule}}
}

func dryRunEvent(nodeName string, healthy bool) (string, *TestEvent) {
	id := generateTestID()

	return id, &TestEvent{Data: createHealthEventBSON(id, nodeName, "GpuXidError", healthy, !healthy,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "0"}}, model.StatusInProgress)}
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()

	m := &dto.Metric{}
	require.NoError(t, c.Write(m))

	return m.GetCounter().GetValue()
}

func newDryRunTestNode(ctx context.Context, t *testing.T, prefix string) string {
	t.Helper()

	nodeName := prefix + generateShortTestID()
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	t.Cleanup(func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(context.Background(), nodeName, metav1.DeleteOptions{})
	})

	return nodeName
}

// quarantineInDryRun runs a dry-run reconciler and quarantines nodeName with it.
func quarantineInDryRun(ctx context.Context, t *testing.T, nodeName string, rules config.TomlConfig) {
	t.Helper()

	_, watcher, getStatus, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{TomlConfig: rules, DryRun: true})

	id, ev := dryRunEvent(nodeName, false)
	watcher.EventsChan <- ev

	require.Eventually(t, func() bool {
		s := getStatus(id)
		return s != nil && *s == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval)

	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	require.False(t, node.Spec.Unschedulable, "dry run must not cordon")
	require.Equal(t, common.QuarantineHealthEventDryRunAnnotationValue,
		node.Annotations[common.QuarantineHealthEventDryRunAnnotationKey], "dry run should mark its annotations")
}

func requireDryRunRecordKept(ctx context.Context, t *testing.T, nodeName string) {
	t.Helper()

	// The handlers run on informer events; give them time to act before checking they did not.
	time.Sleep(time.Second)

	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotEmpty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey], "dry-run record should be kept")
	assert.NotEmpty(t, node.Annotations[common.QuarantineHealthEventDryRunAnnotationKey])
	assert.NotContains(t, node.Annotations, common.QuarantinedNodeUncordonedManuallyAnnotationKey)
	assert.NotContains(t, node.Annotations, common.QuarantinedNodeIsUntaintedManuallyAnnotationKey)
}

func TestE2E_DryRunDoesNotFeedCircuitBreaker(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	base := "e2e-dryrun-cb-" + generateShortTestID()[:6]
	for i := range 10 {
		name := fmt.Sprintf("%s-%d", base, i)
		createE2ETestNode(ctx, t, name, nil, nil, nil, false)
		defer func(n string) {
			_ = e2eTestClient.CoreV1().Nodes().Delete(context.Background(), n, metav1.DeleteOptions{})
		}(name)
	}

	// A breaker is passed in, as a stale config could, to check the reconciler ignores it.
	r, watcher, getStatus, cb := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:           dryRunRules(true, false),
		CircuitBreakerConfig: &breaker.CircuitBreakerConfig{Namespace: "default", Percentage: 50, Duration: 5 * time.Minute},
		DryRun:               true,
	})
	require.Eventually(t, func() bool {
		total, _, err := r.k8sClient.NodeInformer.GetNodeCounts()
		return err == nil && total == 10
	}, statusCheckTimeout, statusCheckPollInterval)

	dryRunBefore := counterValue(t, metrics.DryRunActions.WithLabelValues(metrics.DryRunActionQuarantine))
	cordonsBefore := counterValue(t, metrics.CordonsApplied)

	// 6 distinct nodes is past the 50% trip threshold.
	for i := range 6 {
		id, ev := dryRunEvent(fmt.Sprintf("%s-%d", base, i), false)
		watcher.EventsChan <- ev
		require.Eventually(t, func() bool {
			s := getStatus(id)
			return s != nil && *s == model.Quarantined
		}, statusCheckTimeout, statusCheckPollInterval, "dry run must not be halted by the breaker")
	}

	assert.Equal(t, breaker.StateClosed, cb.CurrentState(), "dry-run cordons must not trip the breaker")

	cordoned, err := r.k8sClient.GetCordonedNodes(ctx)
	require.NoError(t, err)
	assert.Zero(t, cordoned, "dry-run nodes must not count as cordoned")

	assert.Equal(t, dryRunBefore+6, counterValue(t, metrics.DryRunActions.WithLabelValues(metrics.DryRunActionQuarantine)))
	assert.Equal(t, cordonsBefore, counterValue(t, metrics.CordonsApplied), "dry run must not count as applied cordons")

	for i := range 10 {
		node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, fmt.Sprintf("%s-%d", base, i), metav1.GetOptions{})
		require.NoError(t, err)
		assert.False(t, node.Spec.Unschedulable)
	}
}

func TestE2E_DryRunRestartKeepsQuarantineRecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := newDryRunTestNode(ctx, t, "e2e-dryrun-restart-")
	quarantineInDryRun(ctx, t, nodeName, dryRunRules(true, false))

	before := counterValue(t, metrics.TotalNodesManuallyUncordoned.WithLabelValues(nodeName))

	// Restart, still in dry run. The ADD-time stale check sees an annotated node that is not cordoned.
	setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{TomlConfig: dryRunRules(true, false), DryRun: true})

	requireDryRunRecordKept(ctx, t, nodeName)
	assert.Equal(t, before, counterValue(t, metrics.TotalNodesManuallyUncordoned.WithLabelValues(nodeName)),
		"a dry-run quarantine is not a manual uncordon")
}

func TestE2E_DryRunRestartKeepsTaintOnlyQuarantineRecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := newDryRunTestNode(ctx, t, "e2e-dryrun-untaint-")
	quarantineInDryRun(ctx, t, nodeName, dryRunRules(false, true))

	before := counterValue(t, metrics.TotalNodesManuallyUntainted.WithLabelValues(nodeName))

	// The recorded taint is absent from the node, which the ADD-time check reads as a manual untaint.
	setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{TomlConfig: dryRunRules(false, true), DryRun: true})

	requireDryRunRecordKept(ctx, t, nodeName)
	assert.Equal(t, before, counterValue(t, metrics.TotalNodesManuallyUntainted.WithLabelValues(nodeName)),
		"a dry-run quarantine is not a manual untaint")
}

func TestE2E_DryRunExternalCordonChangeKeepsQuarantineRecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := newDryRunTestNode(ctx, t, "e2e-dryrun-external-")
	_, watcher, getStatus, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig: dryRunRules(true, false),
		DryRun:     true,
	})

	id, ev := dryRunEvent(nodeName, false)
	watcher.EventsChan <- ev
	require.Eventually(t, func() bool {
		s := getStatus(id)
		return s != nil && *s == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval)

	before := counterValue(t, metrics.TotalNodesManuallyUncordoned.WithLabelValues(nodeName))

	// Something other than fault-quarantine cordons and then uncordons the node.
	for _, unschedulable := range []bool{true, false} {
		patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, unschedulable)
		_, err := e2eTestClient.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
		require.NoError(t, err)
		time.Sleep(500 * time.Millisecond)
	}

	requireDryRunRecordKept(ctx, t, nodeName)
	assert.Equal(t, before, counterValue(t, metrics.TotalNodesManuallyUncordoned.WithLabelValues(nodeName)))
}

func TestE2E_DryRunSwitchedOffDiscardsQuarantineRecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := newDryRunTestNode(ctx, t, "e2e-dryrun-switch-")
	quarantineInDryRun(ctx, t, nodeName, dryRunRules(true, false))

	before := counterValue(t, metrics.TotalNodesManuallyUncordoned.WithLabelValues(nodeName))

	var (
		mu        sync.Mutex
		cancelled []string
	)
	eventWatcher := &MockEventWatcher{
		CancelLatestQuarantiningEventsFn: func(_ context.Context, node string, _ string) error {
			mu.Lock()
			defer mu.Unlock()
			cancelled = append(cancelled, node)

			return nil
		},
	}

	// Restart with dry run switched off.
	_, watcher, getStatus, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig:   dryRunRules(true, false),
		DryRun:       false,
		EventWatcher: eventWatcher,
	})

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(cancelled) == 1 && cancelled[0] == nodeName
	}, statusCheckTimeout, statusCheckPollInterval, "the dry-run quarantine's events should be cancelled")

	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	for _, key := range []string{
		common.QuarantineHealthEventAnnotationKey,
		common.QuarantineHealthEventIsCordonedAnnotationKey,
		common.QuarantineHealthEventDryRunAnnotationKey,
		common.QuarantinedNodeUncordonedManuallyAnnotationKey,
	} {
		assert.NotContains(t, node.Annotations, key)
	}

	assert.Equal(t, before, counterValue(t, metrics.TotalNodesManuallyUncordoned.WithLabelValues(nodeName)),
		"discarding a dry-run quarantine is not a manual uncordon")

	// The next fault is a fresh, real quarantine.
	id, ev := dryRunEvent(nodeName, false)
	watcher.EventsChan <- ev

	require.Eventually(t, func() bool {
		s := getStatus(id)
		return s != nil && *s == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval)

	require.Eventually(t, func() bool {
		n, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return err == nil && n.Spec.Unschedulable && n.Annotations[common.QuarantineHealthEventDryRunAnnotationKey] == ""
	}, statusCheckTimeout, statusCheckPollInterval, "the node should be cordoned with no dry-run marker")
}

// A node already cordoned by someone else is still unschedulable when dry run annotates it.
func TestE2E_DryRunOnCordonedNodeIsNotCountedAndIsDiscardedOnSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := "e2e-dryrun-cordoned-" + generateShortTestID()
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, true)
	t.Cleanup(func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(context.Background(), nodeName, metav1.DeleteOptions{})
	})

	r, watcher, getStatus, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig: dryRunRules(true, false),
		DryRun:     true,
	})

	id, ev := dryRunEvent(nodeName, false)
	watcher.EventsChan <- ev
	require.Eventually(t, func() bool {
		s := getStatus(id)
		return s != nil && *s == model.Quarantined
	}, statusCheckTimeout, statusCheckPollInterval)

	require.Eventually(t, func() bool {
		_, quarantined, err := r.k8sClient.NodeInformer.GetNodeCounts()
		n, getErr := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return err == nil && getErr == nil && n.Annotations[common.QuarantineHealthEventDryRunAnnotationKey] != "" &&
			!quarantined[nodeName]
	}, statusCheckTimeout, statusCheckPollInterval, "a dry-run quarantine must not count even on a cordoned node")

	var (
		mu        sync.Mutex
		cancelled []string
	)

	// Switch dry run off. The node stays cordoned, so the stale-uncordon check does not apply.
	setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig: dryRunRules(true, false),
		DryRun:     false,
		EventWatcher: &MockEventWatcher{
			CancelLatestQuarantiningEventsFn: func(_ context.Context, node string, _ string) error {
				mu.Lock()
				defer mu.Unlock()
				cancelled = append(cancelled, node)

				return nil
			},
		},
	})

	require.Eventually(t, func() bool {
		n, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return err == nil && n.Annotations[common.QuarantineHealthEventDryRunAnnotationKey] == "" &&
			n.Annotations[common.QuarantineHealthEventAnnotationKey] == ""
	}, statusCheckTimeout, statusCheckPollInterval, "the dry-run record should be discarded on the switch")

	n, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, n.Spec.Unschedulable, "the existing cordon is not fault-quarantine's to remove")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{nodeName}, cancelled)
}

// A marker left on a node, for example by a rollback to a version that does not know it,
// must not make a later real quarantine look like a dry run.
func TestE2E_LiveQuarantineRemovesLeftoverDryRunMarker(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
	defer cancel()

	nodeName := newDryRunTestNode(ctx, t, "e2e-dryrun-leftover-")
	r, watcher, getStatus, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig: dryRunRules(true, false),
		DryRun:     false,
	})

	addMarker := func() {
		patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`,
			common.QuarantineHealthEventDryRunAnnotationKey, common.QuarantineHealthEventDryRunAnnotationValue)
		_, err := e2eTestClient.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
		require.NoError(t, err)
	}

	send := func(gpu string, want model.Status) {
		id := generateTestID()
		watcher.EventsChan <- &TestEvent{Data: createHealthEventBSON(id, nodeName, "GpuXidError", false, true,
			[]*protos.Entity{{EntityType: "GPU", EntityValue: gpu}}, model.StatusInProgress)}
		require.Eventually(t, func() bool {
			s := getStatus(id)
			return s != nil && *s == want
		}, statusCheckTimeout, statusCheckPollInterval)
	}

	markerGone := func() bool {
		n, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return err == nil && n.Spec.Unschedulable && n.Annotations[common.QuarantineHealthEventDryRunAnnotationKey] == ""
	}

	// Fresh quarantine path.
	addMarker()
	send("0", model.Quarantined)
	require.Eventually(t, markerGone, statusCheckTimeout, statusCheckPollInterval,
		"a real quarantine should remove a leftover marker")
	require.Eventually(t, func() bool {
		_, quarantined, err := r.k8sClient.NodeInformer.GetNodeCounts()
		return err == nil && quarantined[nodeName]
	}, statusCheckTimeout, statusCheckPollInterval, "the real quarantine should be counted")

	// Already-quarantined path.
	addMarker()
	send("1", model.AlreadyQuarantined)
	require.Eventually(t, markerGone, statusCheckTimeout, statusCheckPollInterval,
		"a further real event should remove a leftover marker")

	// A restart must keep the real quarantine.
	setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{TomlConfig: dryRunRules(true, false), DryRun: false})
	time.Sleep(time.Second)

	n, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, n.Spec.Unschedulable)
	assert.NotEmpty(t, n.Annotations[common.QuarantineHealthEventAnnotationKey], "the real quarantine record should survive a restart")
}
