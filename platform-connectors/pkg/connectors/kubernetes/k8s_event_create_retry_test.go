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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clientretry "k8s.io/client-go/util/retry"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// ambiguousEventCreateTransport injects failures around real API commits and lookups.
// A successful response is drained before it is lost, proving the Event was persisted.
type ambiguousEventCreateTransport struct {
	base             http.RoundTripper
	mu               sync.Mutex
	calls            []string
	posts            int
	loseBeforeCommit bool
	alreadyExists    bool
	lookupFailures   int
	lookupForbidden  bool
	mutate           func(*corev1.Event)
	afterCommit      func(context.Context, *corev1.Event) error
}

func (f *ambiguousEventCreateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.Contains(req.URL.Path, "/events") {
		return f.base.RoundTrip(req)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req.Method)
	if req.Method == http.MethodGet && f.posts > 0 {
		if f.lookupForbidden {
			return eventCreateStatusResponse(req, http.StatusForbidden, metav1.StatusReasonForbidden)
		}
		if f.lookupFailures > 0 {
			f.lookupFailures--
			return eventCreateStatusResponse(req, http.StatusServiceUnavailable, metav1.StatusReasonServiceUnavailable)
		}
	}
	if req.Method != http.MethodPost {
		return f.base.RoundTrip(req)
	}
	f.posts++
	if f.posts != 1 {
		return f.base.RoundTrip(req)
	}
	if f.loseBeforeCommit {
		if err := req.Body.Close(); err != nil {
			return nil, err
		}
		return nil, io.ErrUnexpectedEOF
	}
	if f.mutate != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if err := req.Body.Close(); err != nil {
			return nil, err
		}
		var event corev1.Event
		if err := json.Unmarshal(body, &event); err != nil {
			return nil, err
		}
		f.mutate(&event)
		body, err = json.Marshal(&event)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	resp, err := f.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		return resp, err
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if f.afterCommit != nil {
		var created corev1.Event
		if err := json.Unmarshal(body, &created); err != nil {
			return nil, err
		}
		if err := f.afterCommit(req.Context(), &created); err != nil {
			return nil, err
		}
	}
	if f.alreadyExists {
		return eventCreateStatusResponse(req, http.StatusConflict, metav1.StatusReasonAlreadyExists)
	}
	return nil, io.ErrUnexpectedEOF
}

// eventCreateStatusResponse preserves Kubernetes status semantics for injected failures.
func eventCreateStatusResponse(req *http.Request, code int, reason metav1.StatusReason) (*http.Response, error) {
	body, err := json.Marshal(metav1.Status{Status: metav1.StatusFailure, Code: int32(code), Reason: reason})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
}

// TestProcessHealthEventsWithRetry_AmbiguousEventCreate_ReconcilesPersistedOccurrence
// proves response-loss recovery, outer-retry continuity, and safe rejection of name collisions.
func TestProcessHealthEventsWithRetry_AmbiguousEventCreate_ReconcilesPersistedOccurrence(t *testing.T) {
	testEnv, reader := setupEnvtest(t)
	t.Cleanup(func() { require.NoError(t, testEnv.Stop()) })
	for i, tc := range []struct {
		name             string
		before           bool
		alreadyExists    bool
		deleted          bool
		lookupFailures   int
		forbidden        bool
		mutate           func(*corev1.Event)
		wantPosts        int
		wantOuterRetries int
		wantError        string
	}{
		{name: "committed create loses response", wantPosts: 1},
		{name: "create failed before commit", before: true, wantPosts: 2},
		{name: "matching AlreadyExists", alreadyExists: true, wantPosts: 1},
		{name: "AlreadyExists races deletion", alreadyExists: true, deleted: true, wantPosts: 2},
		{name: "lookup recovers across outer retry", lookupFailures: clientretry.DefaultRetry.Steps, wantPosts: 1, wantOuterRetries: 1},
		{name: "forbidden lookup does not create again", forbidden: true, wantPosts: 1, wantError: "Forbidden"},
		{name: "different message", mutate: func(e *corev1.Event) { e.Message = "another occurrence" }, wantPosts: 1, wantError: "does not match"},
		{name: "different node identity", mutate: func(e *corev1.Event) { e.InvolvedObject.UID = "other-node" }, wantPosts: 1, wantError: "does not match"},
		{name: "different reporter", mutate: func(e *corev1.Event) { e.ReportingController = "other-monitor" }, wantPosts: 1, wantError: "does not match"},
		// Stable names aggregate occurrences; an older FirstTimestamp is valid.
		{name: "earlier first occurrence of the same fault", mutate: func(e *corev1.Event) { e.FirstTimestamp = metav1.NewTime(e.FirstTimestamp.Add(-time.Minute)) }, wantPosts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			transport := &ambiguousEventCreateTransport{loseBeforeCommit: tc.before, alreadyExists: tc.alreadyExists,
				lookupFailures: tc.lookupFailures, lookupForbidden: tc.forbidden, mutate: tc.mutate}
			if tc.deleted {
				transport.afterCommit = func(ctx context.Context, event *corev1.Event) error {
					return reader.CoreV1().Events(DefaultNamespace).Delete(ctx, event.Name, metav1.DeleteOptions{})
				}
			}
			cfg := rest.CopyConfig(testEnv.Config)
			cfg.ContentType, cfg.AcceptContentTypes = "application/json", "application/json"
			cfg.WrapTransport = func(base http.RoundTripper) http.RoundTripper { transport.base = base; return transport }
			client, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err)
			connector := NewK8sConnector(client, nil, nil, ctx, defaultConnectorConfig)
			connector.retryBaseDelay, connector.retryMaxDelay = time.Nanosecond, time.Nanosecond
			node := fmt.Sprintf("ambiguous-create-%d", i)
			events := &protos.HealthEvents{Events: []*protos.HealthEvent{{NodeName: node, Agent: "retry-test", CheckName: "GPUWarning",
				GeneratedTimestamp: timestamppb.Now()}}}
			beforeDrops := testutil.ToFloat64(droppedWritesCounter.WithLabelValues("node_event", "permanent_error", "false"))
			beforeBatches := testutil.ToFloat64(droppedBatchesCounter.WithLabelValues("permanent_error"))
			retries, err := connector.processHealthEventsWithRetry(ctx, events)
			switch {
			case tc.forbidden:
				require.True(t, apierrors.IsForbidden(err), "lookup must preserve Forbidden: %v", err)
			case tc.wantError != "":
				require.ErrorContains(t, err, tc.wantError)
			default:
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantOuterRetries, retries)
			transport.mu.Lock()
			calls := append([]string(nil), transport.calls...)
			posts := transport.posts
			transport.mu.Unlock()
			require.Equal(t, tc.wantPosts, posts, "GET must reconcile an uncertain create before another POST: %v", calls)
			require.GreaterOrEqual(t, len(calls), 2)
			require.Equal(t, []string{http.MethodPost, http.MethodGet}, calls[:2])
			stored, err := reader.CoreV1().Events(DefaultNamespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + node})
			require.NoError(t, err)
			require.Len(t, stored.Items, 1)
			require.EqualValues(t, 1, stored.Items[0].Count, "reconciling the saved occurrence must not increment it")
			expected := connector.createK8sEvent(ctx, events.Events[0])
			_, cached := connector.rememberedNodeEvent(node, expected)
			name := expected.Name
			if tc.wantError != "" {
				require.False(t, cached, "unverified or mismatched events must not populate the cache")
				require.Equal(t, beforeDrops+1, testutil.ToFloat64(droppedWritesCounter.WithLabelValues("node_event", "permanent_error", "false")))
				require.Equal(t, beforeBatches+1, testutil.ToFloat64(droppedBatchesCounter.WithLabelValues("permanent_error")))
				return
			}
			require.True(t, cached)
			require.Equal(t, stored.Items[0].Name, name)
			require.Equal(t, beforeDrops, testutil.ToFloat64(droppedWritesCounter.WithLabelValues("node_event", "permanent_error", "false")))
			require.Equal(t, beforeBatches, testutil.ToFloat64(droppedBatchesCounter.WithLabelValues("permanent_error")))
			// A repeat inside main's refresh interval makes no API call.
			_, err = connector.processHealthEventsWithRetry(ctx, events)
			require.NoError(t, err)
			transport.mu.Lock()
			require.Len(t, transport.calls, len(calls))
			transport.mu.Unlock()
			// After the interval a later report refreshes the same Event once.
			ageRememberedEvent(t, connector, node, expected)
			events.Events[0].GeneratedTimestamp = timestamppb.New(events.Events[0].GeneratedTimestamp.AsTime().Add(time.Minute))
			_, err = connector.processHealthEventsWithRetry(ctx, events)
			require.NoError(t, err)
			later, err := reader.CoreV1().Events(DefaultNamespace).Get(ctx, name, metav1.GetOptions{})
			require.NoError(t, err)
			require.EqualValues(t, 2, later.Count)
		})
	}
}
