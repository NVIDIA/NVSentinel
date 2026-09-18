// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubernetes

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clientretry "k8s.io/client-go/util/retry"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type mergeRoundTripper func(*http.Request) (*http.Response, error)

func (f mergeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestProcessBatch_ExistingFault_ReconcilesReplayAndLaterReports combines stable
// Event names with lost responses, without counting a persisted report twice.
func TestProcessBatch_ExistingFault_ReconcilesReplayAndLaterReports(t *testing.T) {
	testEnv, reader := setupEnvtest(t)
	t.Cleanup(func() { require.NoError(t, testEnv.Stop()) })
	for i, tc := range []struct {
		name        string
		lastOffset  time.Duration
		losePost    bool
		loseUpdate  bool
		wantCount   int32
		wantUpdates int
	}{
		{name: "restart replays the same report", wantCount: 1},
		{name: "another replica reports a later occurrence", lastOffset: -time.Minute, wantCount: 2, wantUpdates: 1},
		{name: "AlreadyExists response is lost", lastOffset: -time.Minute, losePost: true, wantCount: 2, wantUpdates: 1},
		{name: "refresh response is lost", lastOffset: -time.Minute, loseUpdate: true, wantCount: 2, wantUpdates: 1},
		{name: "a newer report is already persisted", lastOffset: time.Minute, wantCount: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now().Truncate(time.Second)
			node := fmt.Sprintf("merge-existing-%d", i)
			report := &protos.HealthEvent{NodeName: node, Agent: "retry-test", CheckName: "GPUWarning",
				GeneratedTimestamp: timestamppb.New(now)}
			seed := (&K8sConnector{}).createK8sEvent(ctx, report)
			seed.FirstTimestamp = metav1.NewTime(now.Add(-2 * time.Minute))
			seed.LastTimestamp = metav1.NewTime(now.Add(tc.lastOffset))
			saved, err := reader.CoreV1().Events(DefaultNamespace).Create(ctx, seed, metav1.CreateOptions{})
			require.NoError(t, err)

			posts, updates := 0, 0
			lost := false
			cfg := rest.CopyConfig(testEnv.Config)
			cfg.ContentType, cfg.AcceptContentTypes = "application/json", "application/json"
			cfg.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
				return mergeRoundTripper(func(req *http.Request) (*http.Response, error) {
					resp, err := base.RoundTrip(req)
					if err != nil || !strings.Contains(req.URL.Path, "/events") {
						return resp, err
					}
					if req.Method == http.MethodPost {
						posts++
					}
					if req.Method == http.MethodPut {
						updates++
					}
					drop := tc.losePost && req.Method == http.MethodPost && resp.StatusCode == http.StatusConflict ||
						tc.loseUpdate && req.Method == http.MethodPut && resp.StatusCode == http.StatusOK
					if !drop || lost {
						return resp, nil
					}
					lost = true
					_, readErr := io.Copy(io.Discard, resp.Body)
					closeErr := resp.Body.Close()
					if readErr != nil {
						return nil, readErr
					}
					if closeErr != nil {
						return nil, closeErr
					}
					return nil, io.ErrUnexpectedEOF
				})
			}
			client, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err)
			connector := NewK8sConnector(client, nil, nil, ctx, defaultConnectorConfig)
			require.NoError(t, connector.ProcessBatch(ctx, batch(report)))
			existing, err := reader.CoreV1().Events(DefaultNamespace).Get(ctx, seed.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, saved.UID, existing.UID, "all replicas must retain one Event")
			require.Equal(t, saved.FirstTimestamp, existing.FirstTimestamp)
			require.Equal(t, tc.wantCount, existing.Count)
			require.Equal(t, 1, posts, "an uncertain response must be reconciled before another create")
			require.Equal(t, tc.wantUpdates, updates, "a committed refresh must not be repeated")
			require.Equal(t, tc.losePost || tc.loseUpdate, lost)
			wantLast := max(now.Unix(), seed.LastTimestamp.Unix())
			require.Equal(t, wantLast, existing.LastTimestamp.Unix())
			_, known := connector.rememberedNodeEvent(node, seed)
			require.True(t, known)
		})
	}
}

// TestProcessHealthEventsWithRetry_FaultRecoveryRecurrence_PreservesOrderAndRefresh
// proves an earlier retry cannot absorb the recovery that should announce a recurrence.
func TestProcessHealthEventsWithRetry_FaultRecoveryRecurrence_PreservesOrderAndRefresh(t *testing.T) {
	testEnv, reader := setupEnvtest(t)
	t.Cleanup(func() { require.NoError(t, testEnv.Stop()) })
	ctx := t.Context()
	node := "merge-ordered-recurrence"
	_, err := reader.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}}, metav1.CreateOptions{})
	require.NoError(t, err)
	transport := &ambiguousEventCreateTransport{lookupFailures: clientretry.DefaultRetry.Steps}
	cfg := rest.CopyConfig(testEnv.Config)
	cfg.ContentType, cfg.AcceptContentTypes = "application/json", "application/json"
	cfg.WrapTransport = func(base http.RoundTripper) http.RoundTripper { transport.base = base; return transport }
	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	connector := NewK8sConnector(client, nil, nil, ctx, defaultConnectorConfig)
	connector.retryBaseDelay, connector.retryMaxDelay = time.Nanosecond, time.Nanosecond
	now := time.Now().Truncate(time.Second)
	report := func(offset time.Duration, healthy bool) *protos.HealthEvent {
		return &protos.HealthEvent{NodeName: node, Agent: "retry-test", CheckName: "GPUWarning",
			GeneratedTimestamp: timestamppb.New(now.Add(offset)), IsHealthy: healthy}
	}
	first, recovery, recurrence := report(0, false), report(time.Minute, true), report(2*time.Minute, false)
	retries, err := connector.processHealthEventsWithRetry(ctx, batch(recurrence, first, recovery))
	require.NoError(t, err)
	require.Equal(t, 1, retries, "the first committed create must cross the outer retry boundary")
	name := connector.createK8sEvent(ctx, first).Name
	event, err := reader.CoreV1().Events(DefaultNamespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	require.EqualValues(t, 2, event.Count, "recovery must clear suppression before the later fault")
	require.Equal(t, now.Unix(), event.FirstTimestamp.Unix())
	require.Equal(t, now.Add(2*time.Minute).Unix(), event.LastTimestamp.Unix())
	calls := len(transport.calls)
	_, err = connector.processHealthEventsWithRetry(ctx, batch(recurrence))
	require.NoError(t, err)
	require.Len(t, transport.calls, calls, "a repeat must still use main's no-write suppression")
	require.Equal(t, 2, transport.posts)
}
