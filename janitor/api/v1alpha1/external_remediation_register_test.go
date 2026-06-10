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

package v1alpha1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func newERR() *ExternalRemediationRequest {
	return &ExternalRemediationRequest{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "nvsentinel.dgxc.nvidia.com/v1",
			Kind:       "ExternalRemediationRequest",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "gpu0-xid79-node-01",
			Labels: map[string]string{
				"nvsentinel.dgxc.nvidia.com/node": "node-01.example-cluster.internal",
			},
			Finalizers: []string{"nvsentinel.dgxc.nvidia.com/external-remediation-cleanup"},
		},
		Spec: &protos.ExternalRemediationRequestSpec{
			HealthEvent: &protos.HealthEvent{
				Id:                "he-7f0b3e2c",
				NodeName:          "node-01.example-cluster.internal",
				IsFatal:           true,
				RecommendedAction: protos.RecommendedAction_CUSTOM,
				Message:           "GPU fell off the bus (XID 79)",
			},
		},
		Status: &protos.ExternalRemediationRequestStatus{
			Conditions: []*protos.Condition{
				{
					Type:               "NVSentinelOwnershipReleased",
					Status:             "True",
					LastTransitionTime: timestamppb.New(time.Date(2026, 5, 11, 20, 14, 9, 0, time.UTC)),
					Reason:             "ReleaseTaintApplied",
				},
			},
		},
	}
}

func TestAddNVSentinelToScheme_RegistersTypes(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	err := AddNVSentinelToScheme(scheme)
	require.NoError(t, err)

	gvk := NVSentinelGroupVersion.WithKind("ExternalRemediationRequest")
	obj, err := scheme.New(gvk)
	require.NoError(t, err)
	assert.IsType(t, &ExternalRemediationRequest{}, obj)

	listGVK := NVSentinelGroupVersion.WithKind("ExternalRemediationRequestList")
	listObj, err := scheme.New(listGVK)
	require.NoError(t, err)
	assert.IsType(t, &ExternalRemediationRequestList{}, listObj)
}

func TestAddNVSentinelToScheme_DoesNotConflictWithJanitorScheme(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, AddToScheme(scheme))           // janitor.dgxc.nvidia.com
	require.NoError(t, AddNVSentinelToScheme(scheme)) // nvsentinel.dgxc.nvidia.com

	// Both groups should resolve independently.
	_, err := scheme.New(GroupVersion.WithKind("RebootNode"))
	require.NoError(t, err, "RebootNode (janitor.dgxc.nvidia.com) must still resolve")

	_, err = scheme.New(NVSentinelGroupVersion.WithKind("ExternalRemediationRequest"))
	require.NoError(t, err, "ExternalRemediationRequest (nvsentinel.dgxc.nvidia.com) must resolve")
}

func TestExternalRemediationRequest_DeepCopyObject_NilSafe(t *testing.T) {
	t.Parallel()

	var nilERR *ExternalRemediationRequest

	assert.Nil(t, nilERR.DeepCopyObject())
	assert.Nil(t, nilERR.DeepCopy())
}

func TestExternalRemediationRequest_DeepCopy_FieldFidelity(t *testing.T) {
	t.Parallel()

	original := newERR()
	copied := original.DeepCopy()

	require.NotNil(t, copied)
	assert.Equal(t, original.Name, copied.Name)
	assert.Equal(t, original.Namespace, copied.Namespace)
	assert.Equal(t, original.Labels, copied.Labels)
	assert.Equal(t, original.Finalizers, copied.Finalizers)
	require.NotNil(t, copied.Spec.HealthEvent)
	assert.Equal(t, original.Spec.HealthEvent.Id, copied.Spec.HealthEvent.Id)
	assert.Equal(t, original.Spec.HealthEvent.NodeName, copied.Spec.HealthEvent.NodeName)
	assert.Equal(t, original.Spec.HealthEvent.Message, copied.Spec.HealthEvent.Message)
	require.Len(t, copied.Status.Conditions, 1)
	assert.Equal(t, original.Status.Conditions[0].Type, copied.Status.Conditions[0].Type)
	assert.Equal(t, original.Status.Conditions[0].Status, copied.Status.Conditions[0].Status)
}

// TestExternalRemediationRequest_DeepCopy_IsIndependent verifies that mutating
// the deep copy does not affect the original — the whole point of DeepCopy in
// controller-runtime caches.
func TestExternalRemediationRequest_DeepCopy_IsIndependent(t *testing.T) {
	t.Parallel()

	original := newERR()
	copied := original.DeepCopy()
	require.NotNil(t, copied)

	// Mutate ObjectMeta.
	copied.Labels["mutated"] = "yes"
	assert.NotContains(t, original.Labels, "mutated", "ObjectMeta must be independently deep-copied")

	// Mutate Spec.HealthEvent (proto-cloned).
	copied.Spec.HealthEvent.Message = "mutated"
	assert.Equal(t, "GPU fell off the bus (XID 79)", original.Spec.HealthEvent.Message,
		"Spec.HealthEvent must be independently proto-cloned")

	// Mutate Status.Conditions slice element.
	copied.Status.Conditions[0].Reason = "mutated"
	assert.Equal(t, "ReleaseTaintApplied", original.Status.Conditions[0].Reason,
		"Status.Conditions must be independently proto-cloned")
}

func TestExternalRemediationRequest_DeepCopyObject_ReturnsRuntimeObject(t *testing.T) {
	t.Parallel()

	original := newERR()
	obj := original.DeepCopyObject()

	require.IsType(t, &ExternalRemediationRequest{}, obj)
	copied := obj.(*ExternalRemediationRequest)
	assert.Equal(t, original.Name, copied.Name)
}

func TestExternalRemediationRequestList_DeepCopy_FieldFidelity(t *testing.T) {
	t.Parallel()

	original := &ExternalRemediationRequestList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "nvsentinel.dgxc.nvidia.com/v1",
			Kind:       "ExternalRemediationRequestList",
		},
		Items: []ExternalRemediationRequest{*newERR(), *newERR()},
	}

	copied := original.DeepCopy()

	require.NotNil(t, copied)
	require.Len(t, copied.Items, 2)
	assert.Equal(t, original.Items[0].Name, copied.Items[0].Name)
	assert.Equal(t, original.Items[1].Name, copied.Items[1].Name)
}

func TestExternalRemediationRequestList_DeepCopy_NilSafe(t *testing.T) {
	t.Parallel()

	var nilList *ExternalRemediationRequestList

	assert.Nil(t, nilList.DeepCopyObject())
	assert.Nil(t, nilList.DeepCopy())
}
