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
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/nvidia/nvsentinel/commons/pkg/cordonownership"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
)

func TestValidateRecoveredValidationRequest_RequiresSamePersistedHandoff(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*unstructured.Unstructured)
		wantError bool
	}{
		{"same", func(_ *unstructured.Unstructured) {}, false},
		{"pre-existing-to-unknown", func(v *unstructured.Unstructured) {
			a := v.GetAnnotations()
			a[common.ValidationCordonOwnershipAnnotationKey] = common.CordonOwnershipPreExisting
			v.SetAnnotations(a)
		}, false},
		{"owned-to-unknown", func(v *unstructured.Unstructured) {
			a := v.GetAnnotations()
			a[common.ValidationCordonOwnershipAnnotationKey] = common.CordonOwnershipOwned
			v.SetAnnotations(a)
		}, true},
		{"legacy", func(v *unstructured.Unstructured) {
			a := v.GetAnnotations()
			delete(a, common.ValidationCordonOwnershipAnnotationKey)
			v.SetAnnotations(a)
		}, true},
		{"missing-source", func(v *unstructured.Unstructured) {
			a := v.GetAnnotations()
			delete(a, common.ValidationSourceHashAnnotationKey)
			v.SetAnnotations(a)
		}, true},
		{"different-source", func(v *unstructured.Unstructured) {
			a := v.GetAnnotations()
			a[common.ValidationSourceHashAnnotationKey] = "other-source"
			v.SetAnnotations(a)
		}, true},
		{"different-claim", func(v *unstructured.Unstructured) {
			a := v.GetAnnotations()
			a[cordonownership.RequestClaimIDAnnotationKey] = "other-claim"
			v.SetAnnotations(a)
		}, true},
		{"recreated-node", func(v *unstructured.Unstructured) {
			owners := v.GetOwnerReferences()
			owners[0].UID = "another-node"
			v.SetOwnerReferences(owners)
		}, true},
		{"different-tests", func(v *unstructured.Unstructured) {
			v.Object["spec"].(map[string]any)["tests"] = []any{"other-test"}
		}, true},
		{"deleting", func(v *unstructured.Unstructured) {
			now := metav1.Now()
			v.SetDeletionTimestamp(&now)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "nvsentinel.nvidia.com/v1alpha1", "kind": "ValidationRequest",
				"metadata": map[string]any{"name": "validation-a"},
				"spec": map[string]any{"nodes": []any{map[string]any{"name": "gpu-node"}},
					"tests": []any{"quick"}},
			}}
			desired.SetAnnotations(map[string]string{
				common.ValidationCordonOwnershipAnnotationKey: common.CordonOwnershipUnknown,
				common.ValidationSourceHashAnnotationKey:      "source-hash",
				cordonownership.RequestClaimIDAnnotationKey:   "claim-a",
			})
			desired.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: "gpu-node", UID: "node-a"}})
			existing := desired.DeepCopy()
			tc.mutate(existing)
			original := existing.DeepCopy()
			err := ValidateRecoveredValidationRequest(existing, desired)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, original, existing, "recovery must preserve the persisted ownership decision")
		})
	}
}
