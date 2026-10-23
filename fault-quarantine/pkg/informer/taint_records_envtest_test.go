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

package informer

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/commons/pkg/cordonownership"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
)

func taintRecordsJSON(t *testing.T, records ...config.Taint) string {
	t.Helper()
	encoded, err := json.Marshal(records)
	require.NoError(t, err)
	return string(encoded)
}

func TestUnquarantine_ValidationKeepsTaintRecordsUntilFinalRemoval(t *testing.T) {
	node, claim := claimedNode(t, "taint-record-session")
	owned := config.Taint{Key: "example.com/owned", Value: "true", Effect: "NoSchedule"}
	manual := config.Taint{Key: "example.com/manual", Value: "true", Effect: "NoSchedule", PreExisting: true}
	missing := config.Taint{Key: "example.com/missing", Value: "true", Effect: "NoSchedule"}
	node.Annotations[common.QuarantineHealthEventAnnotationKey] = claimQuarantine
	node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] = "True"
	node.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey] = taintRecordsJSON(t, owned, manual, missing)
	node.Annotations[cordonownership.ValidationSessionAnnotationKey] = ownedClaimSession(t, node, claim)
	node.Spec.Taints = []corev1.Taint{
		{Key: owned.Key, Value: owned.Value, Effect: corev1.TaintEffect(owned.Effect)},
		{Key: manual.Key, Value: manual.Value, Effect: corev1.TaintEffect(manual.Effect)},
	}
	node, err := testClient.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	client := &FaultQuarantineClient{Clientset: testClient}
	keys := []string{common.QuarantineHealthEventAnnotationKey, common.QuarantineHealthEventIsCordonedAnnotationKey,
		common.QuarantineHealthEventAppliedTaintsAnnotationKey, cordonownership.AnnotationKey}
	require.NoError(t, client.UnQuarantineNodeAndRemoveAnnotations(t.Context(), node.Name, []config.Taint{owned}, true, keys, nil, nil))
	node, err = testClient.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.True(t, node.Spec.Unschedulable)
	require.True(t, hasTaint(node, owned))
	require.True(t, hasTaint(node, manual))
	require.NotContains(t, node.Annotations, common.QuarantineHealthEventAnnotationKey)
	require.JSONEq(t, taintRecordsJSON(t, owned, manual), node.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey])

	delete(node.Annotations, cordonownership.ValidationSessionAnnotationKey)
	_, err = testClient.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, client.UnQuarantineNodeAndRemoveAnnotations(t.Context(), node.Name, []config.Taint{owned}, false, keys, nil, nil))
	node, err = testClient.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.False(t, hasTaint(node, owned))
	require.True(t, hasTaint(node, manual))
	require.NotContains(t, node.Annotations, common.QuarantineHealthEventAppliedTaintsAnnotationKey)
}

func TestValidationHandoff_KeepsTaintOwnersAcrossHandoffModes(t *testing.T) {
	for _, mode := range []string{"legacy", "persisted", "adopted-legacy"} {
		name := "taint-record-handoff-" + mode
		t.Run(name, func(t *testing.T) {
			owned := config.Taint{Key: "example.com/owned", Value: "true", Effect: "NoSchedule"}
			manual := config.Taint{Key: "example.com/manual", Value: "true", Effect: "NoSchedule", PreExisting: true}
			annotations := ownershipQuarantineAnnotations()
			annotations[common.QuarantineValidationHealthEventAnnotationKey] = claimSource
			annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey] = taintRecordsJSON(t, owned, manual)
			createTestNode(t.Context(), t, name, annotations, nil, []corev1.Taint{
				{Key: owned.Key, Value: owned.Value, Effect: corev1.TaintEffect(owned.Effect)},
				{Key: manual.Key, Value: manual.Value, Effect: corev1.TaintEffect(manual.Effect)},
			}, true)
			node, err := testClient.CoreV1().Nodes().Get(t.Context(), name, metav1.GetOptions{})
			require.NoError(t, err)
			client := &FaultQuarantineClient{Clientset: testClient}
			switch mode {
			case "persisted":
				require.NoError(t, client.CompleteValidationHandoff(t.Context(), node, common.QuarantineAnnotationKeys, nil))
			case "adopted-legacy":
				require.NoError(t, client.CompleteValidationHandoff(t.Context(), node, common.QuarantineAnnotationKeys, nil, false))
			default:
				require.NoError(t, client.UnQuarantineNodeAndRemoveAnnotations(t.Context(), name, nil, false,
					common.QuarantineAnnotationKeys, nil, nil, true))
			}
			actual, err := testClient.CoreV1().Nodes().Get(t.Context(), name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, node.Spec.Taints, actual.Spec.Taints)
			require.Equal(t, node.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey],
				actual.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey])
			require.NotContains(t, actual.Annotations, common.QuarantineHealthEventAnnotationKey)
			if mode == "persisted" {
				require.Equal(t, claimSource, actual.Annotations[common.QuarantineValidationHealthEventAnnotationKey])
			} else {
				require.NotContains(t, actual.Annotations, common.QuarantineValidationHealthEventAnnotationKey)
			}
		})
	}
}

func TestQuarantine_RetainedValidationTaintKeepsOriginalOwner(t *testing.T) {
	for _, tc := range []struct {
		name, association, currentValue string
		previouslyOwned                 bool
		wasDryRun                       bool
		wantPreExisting                 bool
	}{
		{"owned-session", `[ {"name":"validation-a"} ]`, "true", true, false, false},
		{"pre-existing-session", `[ {"name":"validation-a"} ]`, "true", false, false, true},
		{"detached-record", "", "true", true, false, true},
		{"malformed-session", "broken", "true", true, false, true},
		{"changed-taint-value", `[ {"name":"validation-a"} ]`, "new", true, false, true},
		{"dry-run-promotion", `[ {"name":"validation-a"} ]`, "true", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := config.Taint{Key: "example.com/validation", Value: "true", Effect: "NoSchedule", PreExisting: !tc.previouslyOwned}
			incoming := config.Taint{Key: previous.Key, Value: tc.currentValue, Effect: previous.Effect, PreExisting: true}
			annotations := map[string]string{common.QuarantineHealthEventAppliedTaintsAnnotationKey: taintRecordsJSON(t, previous)}
			if tc.association != "" {
				annotations[cordonownership.ValidationSessionAnnotationKey] = tc.association
			}
			if tc.wasDryRun {
				annotations[common.QuarantineHealthEventDryRunAnnotationKey] = "True"
			}
			name := "taint-owner-" + tc.name
			createTestNode(t.Context(), t, name, annotations, nil, []corev1.Taint{
				{Key: incoming.Key, Value: incoming.Value, Effect: corev1.TaintEffect(incoming.Effect)},
			}, false)
			client := &FaultQuarantineClient{Clientset: testClient}
			_, err := client.QuarantineNodeAndSetAnnotations(t.Context(), name, []config.Taint{incoming}, false,
				map[string]string{common.QuarantineHealthEventAnnotationKey: claimQuarantine,
					common.QuarantineHealthEventAppliedTaintsAnnotationKey: taintRecordsJSON(t, incoming)}, nil)
			require.NoError(t, err)
			node, err := testClient.CoreV1().Nodes().Get(t.Context(), name, metav1.GetOptions{})
			require.NoError(t, err)
			records, err := parseAppliedTaintsAnnotation(node.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey])
			require.NoError(t, err)
			require.Len(t, records, 1)
			require.Equal(t, tc.wantPreExisting, records[0].PreExisting)
			require.True(t, incoming.PreExisting, "caller ownership snapshot must stay unchanged")
			require.False(t, node.Spec.Unschedulable, "a taint-only quarantine must not cordon")
		})
	}
}
