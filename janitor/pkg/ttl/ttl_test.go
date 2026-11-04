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

package ttl

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	janitorv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
)

// fakeClock is a deterministic Clock for tests.
type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time { return f.now }

func (f *fakeClock) advance(d time.Duration) { f.now = f.now.Add(d) }

// fixedNow is an arbitrary reference time used across tests.
var fixedNow = time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)

func newClock() *fakeClock { return &fakeClock{now: fixedNow} }

func newRebootNode(name string) *janitorv1alpha1.RebootNode {
	return &janitorv1alpha1.RebootNode{
		Name: name,
	}
}

// TestProcess covers the single-call behavior of Process across all the input
// shapes that matter: missing/present annotations, valid/invalid durations,
// preserve escape hatch, deletion timestamp, and the requeue cap. Multi-call
// scenarios (expiry persistence, clock advance past TTL) are exercised in
// separate tests below because they don't fit the single-input table shape.
func TestProcess(t *testing.T) {
	deletingObj := newRebootNode("deleting")
	deletionTime := metav1.NewTime(fixedNow)
	deletingObj.DeletionTimestamp = &deletionTime

	cases := []struct {
		name           string
		obj            *janitorv1alpha1.RebootNode
		initialAnnots  map[string]string
		defaultTTL     time.Duration
		expectedAction Action
		expectedChange bool
		// assertTTL, if non-empty, is the expected value of the TTL annotation
		// after Process runs.
		assertTTL string
		// assertHasExpiry indicates whether the ExpiryAnnotation should exist
		// after Process runs.
		assertHasExpiry bool
		// assertRequeueAtMost, if > 0, bounds the expected RequeueAfter.
		assertRequeueAtMost time.Duration
	}{
		{
			name:           "being deleted → Noop",
			obj:            deletingObj,
			defaultTTL:     time.Hour,
			expectedAction: ActionNoop,
		},
		{
			name:           "preserve annotation → Noop",
			obj:            newRebootNode("preserved"),
			initialAnnots:  map[string]string{PreserveAnnotation: "true"},
			defaultTTL:     time.Hour,
			expectedAction: ActionNoop,
		},
		{
			name:           "no TTL and no default → Noop",
			obj:            newRebootNode("bare"),
			defaultTTL:     0,
			expectedAction: ActionNoop,
		},
		{
			name:            "default TTL applied when missing",
			obj:             newRebootNode("default-applied"),
			defaultTTL:      14 * 24 * time.Hour,
			expectedAction:  ActionRequeue,
			expectedChange:  true,
			assertTTL:       "336h0m0s",
			assertHasExpiry: true,
			// TTL (14d) exceeds maxRequeueInterval; requeue should be capped.
			assertRequeueAtMost: maxRequeueInterval,
		},
		{
			name:            "existing TTL annotation not overwritten by default",
			obj:             newRebootNode("explicit-ttl"),
			initialAnnots:   map[string]string{TTLAnnotation: "1h"},
			defaultTTL:      336 * time.Hour,
			expectedAction:  ActionRequeue,
			expectedChange:  true, // expiry gets written on first pass
			assertTTL:       "1h",
			assertHasExpiry: true,
		},
		{
			name:           "invalid TTL annotation → Noop",
			obj:            newRebootNode("invalid-ttl"),
			initialAnnots:  map[string]string{TTLAnnotation: "not-a-duration"},
			defaultTTL:     0,
			expectedAction: ActionNoop,
		},
		{
			name:           "zero TTL annotation → Noop",
			obj:            newRebootNode("zero-ttl"),
			initialAnnots:  map[string]string{TTLAnnotation: "0"},
			defaultTTL:     0,
			expectedAction: ActionNoop,
		},
		{
			name: "invalid expiry is recomputed",
			obj:  newRebootNode("bad-expiry"),
			initialAnnots: map[string]string{
				TTLAnnotation:    "1h",
				ExpiryAnnotation: "not-a-time",
			},
			defaultTTL:      0,
			expectedAction:  ActionRequeue,
			expectedChange:  true,
			assertTTL:       "1h",
			assertHasExpiry: true,
		},
		{
			name: "expired-by-annotation returns ActionExpired",
			obj:  newRebootNode("expired"),
			initialAnnots: map[string]string{
				TTLAnnotation:    "1h",
				ExpiryAnnotation: fixedNow.Add(-5 * time.Minute).Format(time.RFC3339),
			},
			defaultTTL:     0,
			expectedAction: ActionExpired,
		},
		{
			name: "expiry at exact now → ActionExpired (boundary)",
			obj:  newRebootNode("edge"),
			initialAnnots: map[string]string{
				TTLAnnotation:    "1h",
				ExpiryAnnotation: fixedNow.Format(time.RFC3339),
			},
			defaultTTL:     0,
			expectedAction: ActionExpired,
		},
		{
			name:                "requeue capped at maxRequeueInterval",
			obj:                 newRebootNode("long-ttl"),
			defaultTTL:          30 * 24 * time.Hour,
			expectedAction:      ActionRequeue,
			expectedChange:      true,
			assertHasExpiry:     true,
			assertRequeueAtMost: maxRequeueInterval,
		},
		{
			name: "nil annotation map is initialized",
			// Don't seed annotations; Process should not panic and should
			// materialize a map when it needs to write.
			obj:             newRebootNode("nil-annots"),
			defaultTTL:      time.Hour,
			expectedAction:  ActionRequeue,
			expectedChange:  true,
			assertHasExpiry: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.initialAnnots != nil {
				tc.obj.Annotations = tc.initialAnnots
			}

			result, err := Process(context.Background(), tc.obj, tc.defaultTTL, newClock())

			require.NoError(t, err)
			assert.Equal(t, tc.expectedAction, result.Action)
			assert.Equal(t, tc.expectedChange, result.Changed)

			if tc.assertTTL != "" {
				assert.Equal(t, tc.assertTTL, tc.obj.Annotations[TTLAnnotation])
			}

			if tc.assertHasExpiry {
				assert.NotEmpty(t, tc.obj.Annotations[ExpiryAnnotation])
			}

			if tc.assertRequeueAtMost > 0 {
				assert.LessOrEqual(t, result.RequeueAfter, tc.assertRequeueAtMost)
			}
		})
	}
}

// TestProcess_ExpiryPersistedAndReused exercises two back-to-back calls to
// Process: the first writes the expiry annotation; the second (within TTL,
// after a clock advance) reuses it without mutating anything.
// This scenario doesn't fit the single-input table in TestProcess.
func TestProcess_ExpiryPersistedAndReused(t *testing.T) {
	rn := newRebootNode("persistent")
	clk := newClock()

	result, err := Process(context.Background(), rn, time.Hour, clk)
	require.NoError(t, err)
	require.Equal(t, ActionRequeue, result.Action)

	firstExpiry := rn.Annotations[ExpiryAnnotation]
	require.NotEmpty(t, firstExpiry)

	clk.advance(10 * time.Minute)

	result, err = Process(context.Background(), rn, time.Hour, clk)
	require.NoError(t, err)
	assert.Equal(t, ActionRequeue, result.Action)
	assert.False(t, result.Changed, "expiry should not be recomputed on subsequent calls")
	assert.Equal(t, firstExpiry, rn.Annotations[ExpiryAnnotation])
}

// TestProcess_ExpiredAfterClockAdvance verifies that the same object, given
// enough time, transitions from Requeue to Expired.
func TestProcess_ExpiredAfterClockAdvance(t *testing.T) {
	rn := newRebootNode("lives-and-dies")
	clk := newClock()

	result, err := Process(context.Background(), rn, time.Hour, clk)
	require.NoError(t, err)
	require.Equal(t, ActionRequeue, result.Action)

	clk.advance(2 * time.Hour)

	result, err = Process(context.Background(), rn, time.Hour, clk)
	require.NoError(t, err)
	assert.Equal(t, ActionExpired, result.Action)
}

// TestProcess_AfterCompletion covers the completion gate used for kinds whose
// TTL must not start until they finish (ExternalRemediationRequest).
func TestProcess_AfterCompletion(t *testing.T) {
	completedAt := fixedNow.Add(-30 * time.Minute)
	longAgo := fixedNow.Add(-48 * time.Hour)
	expiredAnnotation := fixedNow.Add(-5 * time.Minute).Format(time.RFC3339)

	cases := []struct {
		name           string
		initialAnnots  map[string]string
		completedAt    *time.Time
		defaultTTL     time.Duration
		expectedAction Action
		expectedChange bool
		// expectedAnnots, if non-nil, must equal the annotations after Process.
		expectedAnnots map[string]string
	}{
		{
			name:           "in progress with an expired expiry → Noop, annotations untouched",
			initialAnnots:  map[string]string{TTLAnnotation: "1h", ExpiryAnnotation: expiredAnnotation},
			completedAt:    nil,
			defaultTTL:     time.Hour,
			expectedAction: ActionNoop,
			expectedAnnots: map[string]string{TTLAnnotation: "1h", ExpiryAnnotation: expiredAnnotation},
		},
		{
			name:           "in progress without annotations → Noop, default TTL not written",
			completedAt:    nil,
			defaultTTL:     time.Hour,
			expectedAction: ActionNoop,
		},
		{
			name:           "completed recently → expiry calculated from completion",
			completedAt:    &completedAt,
			defaultTTL:     time.Hour,
			expectedAction: ActionRequeue,
			expectedChange: true,
			expectedAnnots: map[string]string{
				TTLAnnotation:    "1h0m0s",
				ExpiryAnnotation: completedAt.Add(time.Hour).Format(time.RFC3339),
			},
		},
		{
			name:           "completed longer ago than the TTL → Expired",
			completedAt:    &longAgo,
			defaultTTL:     time.Hour,
			expectedAction: ActionExpired,
			expectedChange: true,
		},
		{
			name: "expiry written before completion is recomputed from completion",
			initialAnnots: map[string]string{
				TTLAnnotation:    "336h",
				ExpiryAnnotation: completedAt.Add(-time.Hour).Format(time.RFC3339),
			},
			completedAt:    &completedAt,
			expectedAction: ActionRequeue,
			expectedChange: true,
			expectedAnnots: map[string]string{
				TTLAnnotation:    "336h",
				ExpiryAnnotation: completedAt.Add(336 * time.Hour).Format(time.RFC3339),
			},
		},
		{
			name: "first-reconcile expiry later than completion is recomputed from completion",
			initialAnnots: map[string]string{
				TTLAnnotation:    "336h",
				ExpiryAnnotation: completedAt.Add(time.Minute).Format(time.RFC3339),
			},
			completedAt:    &completedAt,
			expectedAction: ActionRequeue,
			expectedChange: true,
			expectedAnnots: map[string]string{
				TTLAnnotation:    "336h",
				ExpiryAnnotation: completedAt.Add(336 * time.Hour).Format(time.RFC3339),
			},
		},
		{
			name: "expiry equal to completion + ttl is kept",
			initialAnnots: map[string]string{
				TTLAnnotation:    "336h",
				ExpiryAnnotation: completedAt.Add(336 * time.Hour).Format(time.RFC3339),
			},
			completedAt:    &completedAt,
			expectedAction: ActionRequeue,
			expectedChange: false,
		},
		{
			name: "ttl shortened after completion → expiry follows the new ttl",
			initialAnnots: map[string]string{
				TTLAnnotation:    "24h",
				ExpiryAnnotation: longAgo.Add(336 * time.Hour).Format(time.RFC3339),
			},
			completedAt:    &longAgo,
			expectedAction: ActionExpired,
			expectedChange: true,
			expectedAnnots: map[string]string{
				TTLAnnotation:    "24h",
				ExpiryAnnotation: longAgo.Add(24 * time.Hour).Format(time.RFC3339),
			},
		},
		{
			name: "preserve annotation wins over completion",
			initialAnnots: map[string]string{
				TTLAnnotation:      "1h",
				ExpiryAnnotation:   expiredAnnotation,
				PreserveAnnotation: "true",
			},
			completedAt:    &longAgo,
			expectedAction: ActionNoop,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := newRebootNode("gated")
			obj.Annotations = tc.initialAnnots

			result, err := Process(context.Background(), obj, tc.defaultTTL, newClock(), AfterCompletion(tc.completedAt))

			require.NoError(t, err)
			assert.Equal(t, tc.expectedAction, result.Action)
			assert.Equal(t, tc.expectedChange, result.Changed)

			if tc.expectedAnnots != nil {
				assert.Equal(t, tc.expectedAnnots, obj.Annotations)
			}

			if tc.completedAt == nil {
				assert.Equal(t, tc.initialAnnots, obj.Annotations, "an in-progress object must not be mutated")
			}
		})
	}
}

// TestProcess_AfterCompletion_StableAcrossReconciles guards against an update
// loop: recalculating from a sub-second completion time must produce the same
// stored expiry on the next call.
func TestProcess_AfterCompletion_StableAcrossReconciles(t *testing.T) {
	completedAt := fixedNow.Add(-10*time.Minute + 750*time.Millisecond)
	obj := newRebootNode("stable")

	first, err := Process(context.Background(), obj, time.Hour, newClock(), AfterCompletion(&completedAt))
	require.NoError(t, err)
	require.True(t, first.Changed)

	second, err := Process(context.Background(), obj, time.Hour, newClock(), AfterCompletion(&completedAt))
	require.NoError(t, err)
	assert.Equal(t, ActionRequeue, second.Action)
	assert.False(t, second.Changed, "expiry must not be recomputed on the next call")
}

// TestKindFromType confirms the reflect-based kind extraction works for
// both our CRDs and standard core/v1 types, guarding against a future
// refactor that accidentally breaks the helper.
func TestKindFromType(t *testing.T) {
	assert.Equal(t, "RebootNode", kindFromType[*janitorv1alpha1.RebootNode]())
	assert.Equal(t, "ConfigMap", kindFromType[*corev1.ConfigMap]())
}

func TestNewZeroRef_ReturnsNonNilPointer(t *testing.T) {
	got := newZeroRef[*janitorv1alpha1.RebootNode]()
	require.NotNil(t, got)
	assert.Empty(t, got.Name)
}
