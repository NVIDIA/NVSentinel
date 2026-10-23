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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/commons/pkg/cordonownership"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
)

func TestPerformUncordon_NoNewValidationKeepsTaintRecordsForExistingSession(t *testing.T) {
	ctx := t.Context()
	const name = "taint-records-existing-validation"
	owned := config.Taint{Key: "example.com/fault-b", Value: "true", Effect: "NoSchedule"}
	manual := config.Taint{Key: "example.com/manual", Value: "true", Effect: "NoSchedule", PreExisting: true}
	records, err := json.Marshal([]config.Taint{owned, manual})
	require.NoError(t, err)
	annotations := map[string]string{
		common.QuarantineHealthEventAnnotationKey:              `[{"id":"fault-b","agent":"gpu","checkName":"b"}]`,
		common.QuarantineHealthEventIsCordonedAnnotationKey:    "True",
		common.QuarantineHealthEventAppliedTaintsAnnotationKey: string(records),
	}
	taints := []corev1.Taint{
		{Key: owned.Key, Value: owned.Value, Effect: corev1.TaintEffect(owned.Effect)},
		{Key: manual.Key, Value: manual.Value, Effect: corev1.TaintEffect(manual.Effect)},
	}
	createE2ETestNode(ctx, t, name, annotations, nil, taints, true)
	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	claim := cordonownership.New(node)
	_, err = cordonownership.Store(node, claim)
	require.NoError(t, err)
	session, err := json.Marshal([]map[string]any{{
		"name": "validation-a", "nodeUID": node.UID, "requestUID": "validation-a-uid",
		"claimID": claim.ID, "cordonOwnership": "owned", "tests": []string{"smoke"},
	}})
	require.NoError(t, err)
	node.Annotations[cordonownership.ValidationSessionAnnotationKey] = string(session)
	node, err = e2eTestClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
	taints = append([]corev1.Taint(nil), node.Spec.Taints...)
	r := &Reconciler{k8sClient: &informer.FaultQuarantineClient{Clientset: e2eTestClient}}
	healthy := &protos.HealthEvent{NodeName: name, CheckName: "b", IsHealthy: true}
	stayQuarantined, err := r.performUncordon(ctx, healthy, node.Annotations)
	require.NoError(t, err)
	require.False(t, stayQuarantined)
	node, err = e2eTestClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, taints, node.Spec.Taints)
	require.Equal(t, string(records), node.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey])
	require.NotContains(t, node.Annotations, common.QuarantineHealthEventAnnotationKey)
	require.NotContains(t, node.Annotations, common.QuarantineValidationHealthEventAnnotationKey)
	require.Equal(t, string(session), node.Annotations[cordonownership.ValidationSessionAnnotationKey])
	require.True(t, node.Spec.Unschedulable)

	// Once the prior validation association ends, the retained record still
	// identifies the taint owned by FQ and protects the pre-existing taint.
	delete(node.Annotations, cordonownership.ValidationSessionAnnotationKey)
	node, err = e2eTestClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
	stayQuarantined, err = r.performUncordon(ctx, healthy, node.Annotations)
	require.NoError(t, err)
	require.False(t, stayQuarantined)
	node, err = e2eTestClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, taints[1:], node.Spec.Taints)
	require.NotContains(t, node.Annotations, common.QuarantineHealthEventAppliedTaintsAnnotationKey)
}
