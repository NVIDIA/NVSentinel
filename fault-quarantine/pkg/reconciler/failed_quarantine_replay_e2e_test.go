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
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/evaluator"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/eventwatcher"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/testutils"
)

// internalErrorTransport answers the next PATCH requests to one node with the HTTP 500 that an API
// server returns when an admission webhook of the node times out. The client does not retry it.
type internalErrorTransport struct {
	next      http.RoundTripper
	path      string
	remaining atomic.Int32
	failed    atomic.Int32
}

func (f *internalErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPatch || !strings.HasSuffix(req.URL.Path, f.path) || f.remaining.Add(-1) < 0 {
		return f.next.RoundTrip(req)
	}

	f.failed.Add(1)

	body := `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure",` +
		`"message":"Internal error occurred: failed calling webhook \"nodes.policy.example.com\": ` +
		`context deadline exceeded","reason":"InternalError","code":500}`

	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    req,
	}, nil
}

// checkpointCountingWatcher counts the events that the event watcher marks as processed.
type checkpointCountingWatcher struct {
	*testutils.MockChangeStreamWatcher
	checkpoints atomic.Int32
}

func (w *checkpointCountingWatcher) MarkProcessed(ctx context.Context, token []byte) error {
	w.checkpoints.Add(1)

	return w.MockChangeStreamWatcher.MarkProcessed(ctx, token)
}

// liveEventDatabaseClient adds the lookup that the live path makes for an event from before the start.
type liveEventDatabaseClient struct {
	*coldStartDatabaseClient
}

func (c *liveEventDatabaseClient) CountDocuments(context.Context, any, *client.CountOptions) (int64, error) {
	return 0, nil
}

// failedQuarantineTestConfig cordons and taints a node for a fatal GpuNvlinkWatch health event. It
// applies no label, so these tests leave the label metrics of the other tests as they are.
func failedQuarantineTestConfig() config.TomlConfig {
	return config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.QuarantineRuleSet{{
			Enabled: true, Name: "nvlink-failure", Version: "1", Priority: 10,
			Match: config.Match{Any: []config.Rule{{
				Kind: "HealthEvent", Expression: "event.checkName == 'GpuNvlinkWatch' && event.isFatal == true",
			}}},
			Taint:  config.Taint{Key: "nvidia.com/nvlink-failure", Value: "true", Effect: "NoSchedule"},
			Cordon: config.Cordon{ShouldCordon: true},
		}},
	}
}

type liveWatcherFixture struct {
	reconciler   *Reconciler
	watcher      *eventwatcher.EventWatcher
	changeStream *checkpointCountingWatcher
	dbClient     *liveEventDatabaseClient
	transport    *internalErrorTransport
	nodeName     string
	// stopped gets the error that the event watcher stops with.
	stopped chan error
}

// startLiveWatcher runs the real event watcher and the real reconciler against the envtest API server.
// The API calls of fault-quarantine go through a transport that can fail the PATCH of the node.
func startLiveWatcher(t *testing.T, ctx context.Context, tomlConfig config.TomlConfig) *liveWatcherFixture {
	t.Helper()

	nodeName := "e2e-live-failure-" + generateShortTestID()
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	t.Cleanup(func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(context.Background(), nodeName, metav1.DeleteOptions{})
	})

	transport := &internalErrorTransport{path: "/nodes/" + nodeName}
	restConfig := rest.CopyConfig(e2eTestEnv.Config)
	restConfig.WrapTransport = func(next http.RoundTripper) http.RoundTripper {
		transport.next = next

		return transport
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)

	r, _, _, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{TomlConfig: tomlConfig, Clientset: clientset})

	fixture := &liveWatcherFixture{
		reconciler:   r,
		changeStream: &checkpointCountingWatcher{MockChangeStreamWatcher: testutils.NewMockChangeStreamWatcher()},
		dbClient:     &liveEventDatabaseClient{&coldStartDatabaseClient{statuses: make(map[string]string)}},
		transport:    transport,
		nodeName:     nodeName,
		stopped:      make(chan error, 1),
	}

	ruleSetEvals, err := evaluator.InitializeRuleSetEvaluators(quarantineRuleSetMetas(r.config.TomlConfig.RuleSets),
		r.k8sClient.NodeInformer)
	require.NoError(t, err)

	rulesets := r.buildRulesetsConfig()

	fixture.watcher = eventwatcher.NewEventWatcher(fixture.changeStream, fixture.dbClient, time.Minute, r)
	fixture.watcher.SetProcessEventCallback(
		func(ctx context.Context, event *model.HealthEventWithStatus) (*model.Status, error) {
			return r.ProcessEvent(ctx, event, ruleSetEvals, rulesets)
		},
	)

	go func() {
		fixture.stopped <- fixture.watcher.Start(ctx)
	}()

	return fixture
}

func (f *liveWatcherFixture) sendLiveEvent(eventID, checkName string) {
	f.changeStream.EventsChan <- &TestEvent{
		Data: createHealthEventBSON(eventID, f.nodeName, checkName, false, true, nil, model.StatusInProgress),
	}
}

// waitForStop returns the error that the event watcher stops with.
func (f *liveWatcherFixture) waitForStop(t *testing.T) error {
	t.Helper()

	var stopErr error

	require.Eventually(t, func() bool {
		select {
		case stopErr = <-f.stopped:
			return true
		default:
			return false
		}
	}, 15*time.Second, 50*time.Millisecond, "the event watcher went on past an event that it left with no status")

	return stopErr
}

// restart runs cold start the way the next start of fault-quarantine does. The datastore still has
// each event that got no status.
func (f *liveWatcherFixture) restart(t *testing.T, ctx context.Context, unresolved map[string]string) {
	t.Helper()

	store := &coldStartHealthEventStore{
		findBatched: func(
			_ context.Context, _ datastore.QueryBuilder, _ int, fn func([]datastore.HealthEventWithStatus) error,
		) error {
			var events []datastore.HealthEventWithStatus

			for eventID, checkName := range unresolved {
				if f.dbClient.status(eventID) == "" {
					events = append(events, datastore.HealthEventWithStatus{
						RawEvent: directStoredHealthEvent(eventID, f.nodeName, checkName, false, true),
					})
				}
			}

			return fn(events)
		},
	}

	completed, err := coldstart.Handle(ctx, coldstart.Dependencies{
		HealthEventStore: store,
		EventProcessor:   newColdStartEventProcessor(t, f.reconciler, f.dbClient),
	})
	require.NoError(t, err)
	require.True(t, completed)
}

func (f *liveWatcherFixture) node(t *testing.T, ctx context.Context) *corev1.Node {
	t.Helper()

	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, f.nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	return node
}

// One HTTP 500 on the cordon of a fatal event must not leave the node schedulable while
// fault-quarantine goes on. The event gets no status, and only cold start replays such an event.
func TestE2E_QuarantineFailsOnOneInternalError_WatcherStopsAndColdStartQuarantinesTheNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 60*time.Second)
	defer cancel()

	f := startLiveWatcher(t, ctx, failedQuarantineTestConfig())
	f.transport.remaining.Store(1)

	eventID := generateTestID()
	f.sendLiveEvent(eventID, "GpuNvlinkWatch")

	stopErr := f.waitForStop(t)
	require.ErrorContains(t, stopErr, "live event requires replay")
	assert.Equal(t, int32(1), f.transport.failed.Load())
	assert.Zero(t, f.changeStream.checkpoints.Load(), "the failed event must not be checkpointed")
	assert.Empty(t, f.dbClient.status(eventID))
	assert.False(t, f.node(t, ctx).Spec.Unschedulable)

	f.restart(t, ctx, map[string]string{eventID: "GpuNvlinkWatch"})

	node := f.node(t, ctx)
	assert.True(t, node.Spec.Unschedulable)
	assert.Contains(t, node.Spec.Taints, corev1.Taint{
		Key: "nvidia.com/nvlink-failure", Value: "true", Effect: corev1.TaintEffectNoSchedule,
	})
	assert.Equal(t, string(model.Quarantined), f.dbClient.status(eventID))
}

// The same failure on a node that is already quarantined loses the taint of the later event.
func TestE2E_LaterEventFailsOnOneInternalError_WatcherStopsAndColdStartAppliesItsTaint(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 60*time.Second)
	defer cancel()

	tomlConfig := failedQuarantineTestConfig()
	tomlConfig.RuleSets = append(tomlConfig.RuleSets, config.QuarantineRuleSet{
		Enabled: true, Name: "xid-failure", Version: "1", Priority: 20,
		Match: config.Match{Any: []config.Rule{{
			Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true",
		}}},
		Taint: config.Taint{Key: "nvidia.com/xid-failure", Value: "true", Effect: "NoExecute"},
	})

	f := startLiveWatcher(t, ctx, tomlConfig)

	firstEventID := generateTestID()
	f.sendLiveEvent(firstEventID, "GpuNvlinkWatch")
	require.Eventually(t, func() bool {
		return f.dbClient.status(firstEventID) == string(model.Quarantined) && f.changeStream.checkpoints.Load() == 1
	}, eventuallyTimeout, statusCheckPollInterval)

	f.transport.remaining.Store(1)

	laterEventID := generateTestID()
	f.sendLiveEvent(laterEventID, "GpuXidError")

	stopErr := f.waitForStop(t)
	require.ErrorContains(t, stopErr, "live event requires replay")
	assert.Equal(t, int32(1), f.changeStream.checkpoints.Load(), "only the first event is checkpointed")
	assert.Empty(t, f.dbClient.status(laterEventID))

	xidTaint := corev1.Taint{Key: "nvidia.com/xid-failure", Value: "true", Effect: corev1.TaintEffectNoExecute}
	assert.NotContains(t, f.node(t, ctx).Spec.Taints, xidTaint)

	f.restart(t, ctx, map[string]string{laterEventID: "GpuXidError"})

	assert.Contains(t, f.node(t, ctx).Spec.Taints, xidTaint)
	assert.Equal(t, string(model.AlreadyQuarantined), f.dbClient.status(laterEventID))
}

// With no failure the event watcher quarantines the node, checkpoints the event and goes on.
func TestE2E_QuarantineSucceeds_WatcherCheckpointsAndGoesOn(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 60*time.Second)
	defer cancel()

	f := startLiveWatcher(t, ctx, failedQuarantineTestConfig())

	eventID := generateTestID()
	f.sendLiveEvent(eventID, "GpuNvlinkWatch")

	require.Eventually(t, func() bool {
		return f.dbClient.status(eventID) == string(model.Quarantined) && f.changeStream.checkpoints.Load() == 1
	}, eventuallyTimeout, statusCheckPollInterval)
	assert.True(t, f.node(t, ctx).Spec.Unschedulable)

	require.Never(t, func() bool {
		select {
		case <-f.stopped:
			return true
		default:
			return false
		}
	}, 2*time.Second, 50*time.Millisecond, "the event watcher stopped after an event that it processed")
}
