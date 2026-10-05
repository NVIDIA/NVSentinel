// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
)

type recoveryMetricSink struct {
	db                       *recoveryTestDB
	calls, storeOn, rejectOn int
}

func (s *recoveryMetricSink) HealthEventOccurredV1(_ context.Context, events *protos.HealthEvents,
	_ ...grpc.CallOption) (*emptypb.Empty, error) {
	s.calls++
	if s.rejectOn > 0 && s.calls >= s.rejectOn {
		return nil, status.Error(codes.InvalidArgument, "rejected recovery")
	}
	if s.storeOn > 0 && s.calls >= s.storeOn {
		for _, event := range events.Events {
			s.db.append(event)
		}
	}
	return &emptypb.Empty{}, nil
}

func TestRecoveryEventsPublished_CountsAcceptedSends(t *testing.T) {
	storeFailure := errors.New("store unavailable")
	for _, tc := range []struct {
		name              string
		storeOn, rejectOn int
		storeError        error
		wantError         error
		wantPublished     float64
	}{
		{name: "stored on first send", storeOn: 1, wantPublished: 1},
		{name: "stored after republish", storeOn: 2, wantPublished: 2},
		{name: "rejected first send", rejectOn: 1, wantError: healthpub.ErrPublishRejected},
		{name: "rejected republish", rejectOn: 2, wantError: healthpub.ErrPublishRejected, wantPublished: 1},
		{name: "accepted but store check failed", storeError: storeFailure, wantError: storeFailure, wantPublished: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &recoveryTestDB{failure: tc.storeError}
			sink := &recoveryMetricSink{db: db, storeOn: tc.storeOn, rejectOn: tc.rejectOn}
			r := &Reconciler{
				config: HealthEventsAnalyzerReconcilerConfig{
					Publisher: publisher.NewPublisher(sink, protos.ProcessingStrategy_EXECUTE_REMEDIATION),
				},
				databaseClient: db, recoveryPoll: time.Millisecond, recoveryRepublish: time.Millisecond,
			}
			rule := annotationRule()
			rule.Name = t.Name()
			event := testRecoveryFault("metric-node", "GPU-a")
			event.CheckName = rule.Name
			event.Metadata = map[string]string{annotationRequestKey: "metric-request"}
			identity, ok := recoveryIdentityForEvent(rule, event)
			require.True(t, ok)
			counter := recoveryEventsPublishedTotal.WithLabelValues(rule.Name)
			before := counterValue(t, counter)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := r.publishRecoveryUntilStored(ctx, event, rule, identity)
			if tc.wantError != nil {
				require.ErrorIs(t, err, tc.wantError)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantPublished, counterValue(t, counter)-before)
		})
	}
}

func TestRecoveryEventsPublished_RetainedRequestDoesNotDoubleCount(t *testing.T) {
	db := &recoveryTestDB{}
	sink := &recoveryTestSink{database: db, captured: make(chan *protos.HealthEvent, 4)}
	r := newRecoveryReconciler(db, sink)
	rule := annotationRule()
	rule.Name = t.Name()
	for _, gpu := range []string{"GPU-a", "GPU-b"} {
		fault := testRecoveryFault("metric-replay", gpu)
		fault.CheckName = rule.Name
		db.append(fault)
	}
	now := time.Now().UTC()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "metric-replay", UID: "metric-uid", CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
	}}
	request, err := parseAnnotationRecovery(node, rule, now.Format(time.RFC3339Nano), now)
	require.NoError(t, err)
	counter := recoveryEventsPublishedTotal.WithLabelValues(rule.Name)
	before := counterValue(t, counter)
	for range 2 {
		recovered, err := r.recoverFromAnnotation(t.Context(), request, rule)
		require.NoError(t, err)
		require.Equal(t, 2, recovered)
		require.Equal(t, before+2, counterValue(t, counter))
	}

	// The retained request must not count or recover a new fault.
	newFault := testRecoveryFault(node.Name, "GPU-a")
	newFault.CheckName = rule.Name
	newFault.GeneratedTimestamp = timestamppb.New(now.Add(time.Hour))
	db.append(newFault)
	recovered, err := r.recoverFromAnnotation(t.Context(), request, rule)
	require.NoError(t, err)
	require.Equal(t, 1, recovered, "only the already recovered GPU-b matches the old request")
	require.Equal(t, before+2, counterValue(t, counter))
	require.EqualValues(t, 2, sink.calls.Load())
}

func TestNewReconciler_InitializesRecoveryCounter(t *testing.T) {
	rule := annotationRule()
	rule.Name = t.Name()
	NewReconciler(HealthEventsAnalyzerReconcilerConfig{
		HealthEventsAnalyzerRules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{rule}},
	})
	registry := prometheus.NewRegistry()
	registry.MustRegister(recoveryEventsPublishedTotal)
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "recovery_events_published_total" {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "rule_name" && label.GetValue() == rule.Name {
					require.Len(t, metric.Label, 1, "the counter must not add node or entity labels")
					require.Zero(t, metric.GetCounter().GetValue())
					return
				}
			}
		}
	}
	t.Fatal("configured recovery rule must be exposed with a zero counter before its first publication")
}
