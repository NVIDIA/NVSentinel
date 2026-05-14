// Copyright 2026 k8s-gpu-mcp-server contributors
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

package tools_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/tools"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func failedPodOnNode(ns, name, node string, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "worker"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "worker", RestartCount: restarts, LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: "Error", Message: "CUDA driver init failed"},
				}},
			},
		},
	}
}

func runningPodOnNode(ns, name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "worker"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "worker", RestartCount: 0}}},
	}
}

func podK8sEvent(ns, podName, reason, message, eventType string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: podName + "." + reason, Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: podName},
		Reason:         reason,
		Message:        message,
		Type:           eventType,
		Count:          1,
		FirstTimestamp: metav1.Now(),
		LastTimestamp:  metav1.Now(),
	}
}

func podStoreEvent(t time.Time, ns, podName, msg string, healthy bool, codes ...string) datastore.HealthEventWithStatus {
	return at(t, &protos.HealthEvent{
		NodeName:       invTestNode,
		ComponentClass: "POD",
		CheckName:      "pod_failure",
		IsHealthy:      healthy,
		Message:        msg,
		ErrorCode:      codes,
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "Pod", EntityValue: ns + "/" + podName},
		},
	})
}

// TestPodFailure_FailedPodPopulatesAllLists asserts that a failed pod yields
// the phase, restart count, K8s events tied to the pod, and store health
// events that name the pod in entitiesImpacted. Mirrors the construction
// pattern of NewGPUInventoryHandler. Deleting any of the three data-source
// stitching paths makes distinct assertions fail.
func TestPodFailure_FailedPodPopulatesAllLists(t *testing.T) {
	t0 := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	r := store.NewFakeReader()
	r.SeedNodeEvents(invTestNode,
		podStoreEvent(t0, "ns-a", "pod-x", "GPU fault impacting pod", false, "XID-79"),
		podStoreEvent(t0.Add(time.Hour), "ns-a", "pod-x", "Pod evicted", false),
		podStoreEvent(t0, "ns-a", "other-pod", "ignored", false), // different pod, must not surface
	)

	k := fake.NewSimpleClientset(
		failedPodOnNode("ns-a", "pod-x", invTestNode, 5),
		podK8sEvent("ns-a", "pod-x", "BackOff", "Back-off restarting failed container", "Warning"),
		podK8sEvent("ns-a", "other-pod", "Pulled", "Pulled image", "Normal"), // must be filtered out
	)

	h := tools.NewPodFailureHandler(r, k)
	out, err := h.Handle(context.Background(), tools.PodFailureInput{Pod: "pod-x", Namespace: "ns-a"})

	require.NoError(t, err)
	require.Equal(t, "pod-x", out.Pod)
	require.Equal(t, "ns-a", out.Namespace)
	require.Equal(t, invTestNode, out.Node)
	require.Equal(t, "Failed", out.Phase)
	require.Equal(t, 5, out.RestartCount)

	require.Len(t, out.RecentEvents, 1, "only pod-x K8s events; other-pod's must be excluded")
	require.Equal(t, "BackOff", out.RecentEvents[0].Reason)
	require.Equal(t, "Warning", out.RecentEvents[0].Type)

	require.Len(t, out.RelatedHealthEvents, 2, "both pod-x store events; other-pod's must be excluded")
}

// TestPodFailure_RunningPodEmptyLists asserts a healthy running pod returns
// zero restart count and empty event lists, not an error. Catches the bug
// "happy-path pod treated as failed".
func TestPodFailure_RunningPodEmptyLists(t *testing.T) {
	k := fake.NewSimpleClientset(runningPodOnNode("ns", "p", invTestNode))

	h := tools.NewPodFailureHandler(store.NewFakeReader(), k)
	out, err := h.Handle(context.Background(), tools.PodFailureInput{Pod: "p", Namespace: "ns"})

	require.NoError(t, err)
	require.Equal(t, "Running", out.Phase)
	require.Equal(t, 0, out.RestartCount)
	require.Empty(t, out.RecentEvents)
	require.Empty(t, out.RelatedHealthEvents)
}

// TestPodFailure_PodNotFound_ReturnsError asserts a missing pod surfaces a
// clear error (not an empty response). Catches the bug "missing pod silently
// returns success with empty fields".
func TestPodFailure_PodNotFound_ReturnsError(t *testing.T) {
	k := fake.NewSimpleClientset()

	h := tools.NewPodFailureHandler(store.NewFakeReader(), k)
	_, err := h.Handle(context.Background(), tools.PodFailureInput{Pod: "ghost", Namespace: "nope"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

// TestPodFailure_MissingPodOrNamespace_ReturnsValidationError asserts both
// inputs are required.
func TestPodFailure_MissingPodOrNamespace_ReturnsValidationError(t *testing.T) {
	h := tools.NewPodFailureHandler(store.NewFakeReader(), fake.NewSimpleClientset())

	_, err1 := h.Handle(context.Background(), tools.PodFailureInput{Pod: "", Namespace: "ns"})
	require.Error(t, err1)
	require.Contains(t, err1.Error(), "pod")

	_, err2 := h.Handle(context.Background(), tools.PodFailureInput{Pod: "p", Namespace: ""})
	require.Error(t, err2)
	require.Contains(t, err2.Error(), "namespace")
}

// TestPodFailure_NilK8sClient_ReturnsError asserts the tool errors when no
// K8s client is configured.
func TestPodFailure_NilK8sClient_ReturnsError(t *testing.T) {
	h := tools.NewPodFailureHandler(store.NewFakeReader(), nil)

	_, err := h.Handle(context.Background(), tools.PodFailureInput{Pod: "p", Namespace: "ns"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "k8s API")
}
