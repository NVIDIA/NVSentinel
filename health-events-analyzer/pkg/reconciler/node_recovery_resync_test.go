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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestAnnotationRecovery_DelayedFaultStorage(t *testing.T) {
	server := &envtest.Environment{}
	restConfig, err := server.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Stop()) })
	kube, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)
	node := createRecoveryNode(t, kube, "recovery-delayed-storage")

	// Both faults were generated before verification, but neither has reached
	// storage when the controller first processes the annotation.
	first := testRecoveryFault(node.Name, "GPU-a")
	second := testRecoveryFault(node.Name, "GPU-b")
	verified := time.Now().UTC()
	request := verified.Format(time.RFC3339Nano)
	annotateRecoveryNode(t, kube, node, request)
	before, err := kube.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	db := &recoveryTestDB{}
	sink := &recoveryTestSink{database: db, captured: make(chan *protos.HealthEvent, 20)}
	r := newRecoveryReconciler(db, sink)
	stop := startRecoveryControllerWithResync(t, kube, r, time.Second)
	requireRecoveryEvent(t, kube, node, "RecoverySkipped")
	require.Zero(t, sink.calls.Load(), "an empty store must not create a recovery boundary")

	// Model a fault still in flight when the analyzer started. Persisting it
	// does not change the Kubernetes Node or enqueue another source event.
	db.append(first)
	requireRecoveryEvent(t, kube, node, "RecoveryCompleted")
	require.EqualValues(t, 1, sink.calls.Load())

	// Completion for one GPU must not abandon an older fault on another GPU
	// that reaches storage later under the same node-wide request.
	db.append(second)
	require.Eventually(t, func() bool { return sink.calls.Load() == 2 }, 5*time.Second, 10*time.Millisecond)
	for _, fault := range []*protos.HealthEvent{first, second} {
		identity, ok := recoveryIdentityForEvent(annotationRule(), fault)
		require.True(t, ok)
		require.Eventually(t, func() bool {
			boundary, err := r.latestRecoveryTime(t.Context(), annotationRule(), identity)
			return err == nil && boundary.Equal(verified)
		}, time.Second, 10*time.Millisecond)
	}
	after, err := kube.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, before.ResourceVersion, after.ResourceVersion, "recovery must work without any Node update")
	require.Equal(t, request, after.Annotations[annotationRule().Recovery.AnnotationKey])

	// A later fault needs a new verification even while this annotation keeps
	// being retried. Observe two more store reads, not an arbitrary sleep.
	db.append(testRecoveryFault(node.Name, "GPU-a"))
	reads := db.finds.Load()
	require.Eventually(t, func() bool { return db.finds.Load() >= reads+2 }, 5*time.Second, 10*time.Millisecond)
	stop()
	require.EqualValues(t, 2, sink.calls.Load(), "old verification must not clear a new fault")
	states, err := r.derivedStatesForNode(t.Context(), annotationRule(), node.Name)
	require.NoError(t, err)
	for _, state := range states {
		require.Equal(t, state.HealthEvent.EntitiesImpacted[0].EntityValue == "GPU-b", state.HealthEvent.IsHealthy)
	}

	// The verification boundary is durable across controller restarts.
	restarted := newRecoveryReconciler(db, sink)
	startRecoveryController(t, kube, restarted)
	identity, ok := recoveryIdentityForEvent(annotationRule(), first)
	require.True(t, ok)
	boundary, err := restarted.latestRecoveryTime(t.Context(), annotationRule(), identity)
	require.NoError(t, err)
	require.True(t, boundary.Equal(verified))
}
