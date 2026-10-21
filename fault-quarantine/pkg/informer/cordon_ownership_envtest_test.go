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
	"net/http"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/commons/pkg/cordonownership"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
)

func ownershipQuarantineAnnotations() map[string]string {
	return map[string]string{
		common.QuarantineHealthEventAnnotationKey:           `{}`,
		common.QuarantineHealthEventIsCordonedAnnotationKey: "True",
	}
}

func TestQuarantineCordonOwnership_PreservesActualDecision(t *testing.T) {
	for _, tc := range []struct {
		name             string
		preCordoned      bool
		stalePreExisting bool
		want             string
	}{
		{"new-cordon", false, false, common.CordonOwnershipOwned},
		{"already-cordoned", true, false, common.CordonOwnershipPreExisting},
		{"stale-pre-existing-snapshot", false, true, common.CordonOwnershipOwned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeName := "ownership-" + tc.name
			createTestNode(t.Context(), t, nodeName, nil, map[string]string{"k8saas.nvidia.com/cordoned-by": "fault-quarantine"}, nil, tc.preCordoned)
			client := &FaultQuarantineClient{Clientset: testClient}
			incoming := ownershipQuarantineAnnotations()
			if tc.stalePreExisting {
				incoming[common.QuarantineHealthEventCordonPreExistingAnnotationKey] = "True"
			}
			_, err := client.QuarantineNodeAndSetAnnotations(t.Context(), nodeName, nil, true, incoming, nil)
			require.NoError(t, err)
			node, err := testClient.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
			require.NoError(t, err)
			require.True(t, node.Spec.Unschedulable)
			require.Equal(t, tc.want, common.CordonOwnership(node.Annotations))
			require.Equal(t, tc.stalePreExisting, incoming[common.QuarantineHealthEventCordonPreExistingAnnotationKey] == "True",
				"caller snapshot must not mutate across retries")
		})
	}
}

func TestQuarantineCordonOwnership_ConcurrentManualCordonIsPreserved(t *testing.T) {
	const nodeName = "ownership-cordon-conflict"
	ctx := t.Context()
	annotations := ownershipQuarantineAnnotations()
	annotations[common.QuarantineHealthEventAnnotationKey] = `{"persisted":{"checkName":"gpu"}}`
	annotations[common.QuarantineHealthEventDryRunAnnotationKey] = "True"
	createTestNode(ctx, t, nodeName, annotations, nil, nil, false)
	config := rest.CopyConfig(testConfig)
	var once sync.Once
	var injectedErr error
	patches := 0
	config.Wrap(func(delegate http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			if request.Method == http.MethodPatch {
				patches++
				once.Do(func() {
					node, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
					if err != nil {
						injectedErr = err
						return
					}
					node.Spec.Unschedulable = true
					_, injectedErr = testClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
				})
				if injectedErr != nil {
					return nil, injectedErr
				}
			}
			return delegate.RoundTrip(request)
		})
	})
	clientset, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)
	client := &FaultQuarantineClient{Clientset: clientset}
	_, err = client.QuarantineNodeAndSetAnnotations(ctx, nodeName, nil, true, ownershipQuarantineAnnotations(), nil)
	require.NoError(t, err)
	require.Equal(t, 2, patches, "real API server must reject the stale first cordon")
	node, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, common.CordonOwnershipPreExisting, common.CordonOwnership(node.Annotations))
	require.NotContains(t, node.Annotations, common.QuarantineHealthEventDryRunAnnotationKey)
}

func TestQuarantineCordonOwnership_DryRunRequiresActualCordon(t *testing.T) {
	for _, tc := range []struct {
		name        string
		marker      string
		preCordoned bool
		want        string
	}{
		{"schedulable", "True", false, common.CordonOwnershipOwned},
		{"manual", "True", true, common.CordonOwnershipPreExisting},
		{"empty-marker", "", true, common.CordonOwnershipPreExisting},
		{"malformed-marker", "false", true, common.CordonOwnershipPreExisting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			annotations := ownershipQuarantineAnnotations()
			annotations[common.QuarantineHealthEventAnnotationKey] = `{"persisted":{"checkName":"gpu"}}`
			annotations[common.QuarantineHealthEventDryRunAnnotationKey] = tc.marker
			nodeName := "ownership-promote-" + tc.name
			createTestNode(t.Context(), t, nodeName, annotations, nil, nil, tc.preCordoned)
			client := &FaultQuarantineClient{Clientset: testClient}
			_, err := client.QuarantineNodeAndSetAnnotations(t.Context(), nodeName, nil, true,
				ownershipQuarantineAnnotations(), nil)
			require.NoError(t, err)
			node, err := testClient.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
			require.NoError(t, err)
			require.True(t, node.Spec.Unschedulable)
			require.NotContains(t, node.Annotations, common.QuarantineHealthEventDryRunAnnotationKey)
			require.Equal(t, tc.want, common.CordonOwnership(node.Annotations))
		})
	}
}

func TestQuarantineCordonOwnership_TaintOnlyPromotionKeepsManualCordon(t *testing.T) {
	const nodeName = "ownership-taint-promotion"
	annotations := ownershipQuarantineAnnotations()
	annotations[common.QuarantineHealthEventDryRunAnnotationKey] = "True"
	annotations[common.QuarantineHealthEventAnnotationKey] = `{"persisted":{"checkName":"gpu"}}`
	createTestNode(t.Context(), t, nodeName, annotations, nil, nil, true)
	client := &FaultQuarantineClient{Clientset: testClient}
	_, err := client.QuarantineNodeAndSetAnnotations(t.Context(), nodeName,
		[]config.Taint{{Key: "nvidia.com/test", Value: "true", Effect: "NoSchedule"}}, false,
		map[string]string{common.QuarantineHealthEventAnnotationKey: `{}`}, nil)
	require.NoError(t, err)
	node, err := testClient.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	require.True(t, node.Spec.Unschedulable)
	require.NotContains(t, node.Annotations, common.QuarantineHealthEventDryRunAnnotationKey)
	require.NotContains(t, node.Annotations, common.QuarantineHealthEventIsCordonedAnnotationKey)
	require.Equal(t, common.CordonOwnershipUnknown, common.CordonOwnership(node.Annotations))
}

func TestQuarantineCordonOwnership_RestartRetainsExistingQuarantine(t *testing.T) {
	for _, tc := range []struct{ name, preExisting, want string }{
		{"owned", "", common.CordonOwnershipOwned},
		{"pre-existing", "True", common.CordonOwnershipPreExisting},
		{"unknown", "malformed", common.CordonOwnershipUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			annotations := ownershipQuarantineAnnotations()
			annotations[common.QuarantineHealthEventAnnotationKey] = `{"persisted":{"checkName":"gpu"}}`
			if tc.preExisting != "" {
				annotations[common.QuarantineHealthEventCordonPreExistingAnnotationKey] = tc.preExisting
			}
			nodeName := "ownership-restart-" + tc.name
			createTestNode(t.Context(), t, nodeName, annotations, nil, nil, true)
			client := &FaultQuarantineClient{Clientset: testClient}
			incoming := ownershipQuarantineAnnotations()
			incoming[common.QuarantineHealthEventCordonPreExistingAnnotationKey] = "True"
			_, err := client.QuarantineNodeAndSetAnnotations(t.Context(), nodeName, nil, true, incoming, nil)
			require.NoError(t, err)
			node, err := testClient.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, tc.want, common.CordonOwnership(node.Annotations))
		})
	}
}

func TestValidationRequestAlreadyExists_RequiresSameHandoff(t *testing.T) {
	_, err := envtest.InstallCRDs(testConfig, envtest.CRDInstallOptions{
		Paths:              []string{filepath.Join("..", "..", "..", "distros", "kubernetes", "nvsentinel", "charts", "lifecycle-manager", "crds", "nvsentinel.nvidia.com_validationrequests.yaml")},
		ErrorIfPathMissing: true,
	})
	require.NoError(t, err)
	client, err := dynamic.NewForConfig(testConfig)
	require.NoError(t, err)
	fq := &FaultQuarantineClient{DynamicClient: client}
	gvr := schema.GroupVersionResource{Group: "nvsentinel.nvidia.com", Version: "v1alpha1", Resource: "validationrequests"}
	for _, tc := range []struct {
		name      string
		change    func(*unstructured.Unstructured)
		wantError bool
	}{
		{"same", func(_ *unstructured.Unstructured) {}, false},
		{"claim-differs", func(v *unstructured.Unstructured) {
			a := v.GetAnnotations()
			a[cordonownership.RequestClaimIDAnnotationKey] = "other-claim"
			v.SetAnnotations(a)
		}, true},
		{"recreated-node", func(v *unstructured.Unstructured) {
			owners := v.GetOwnerReferences()
			owners[0].UID = types.UID("different-node")
			v.SetOwnerReferences(owners)
		}, true},
		{"different-target", func(v *unstructured.Unstructured) {
			v.Object["spec"].(map[string]any)["nodes"] = []any{map[string]any{"name": "other"}}
		}, true},
		{"different-tests", func(v *unstructured.Unstructured) { v.Object["spec"].(map[string]any)["tests"] = []any{"other"} }, true},
		{"different-ownership", func(v *unstructured.Unstructured) {
			v.SetAnnotations(map[string]string{common.ValidationCordonOwnershipAnnotationKey: common.CordonOwnershipPreExisting})
		}, true},
		{"missing-ownership", func(v *unstructured.Unstructured) { v.SetAnnotations(nil) }, true},
		{"different-source", func(v *unstructured.Unstructured) {
			annotations := v.GetAnnotations()
			annotations[common.ValidationSourceHashAnnotationKey] = "another-quarantine-source"
			v.SetAnnotations(annotations)
		}, true},
		{"missing-owner", func(v *unstructured.Unstructured) { v.SetOwnerReferences(nil) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "nvsentinel.nvidia.com/v1alpha1", "kind": "ValidationRequest",
				"metadata": map[string]any{"name": "handoff-" + tc.name},
				"spec":     map[string]any{"nodes": []any{map[string]any{"name": "gpu-node"}}, "tests": []any{"quick"}},
			}}
			desired.SetAnnotations(map[string]string{common.ValidationCordonOwnershipAnnotationKey: common.CordonOwnershipOwned})
			desired.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: "gpu-node", UID: types.UID("verified-node")}})
			existing := desired.DeepCopy()
			tc.change(existing)
			_, err := client.Resource(gvr).Create(t.Context(), existing, metav1.CreateOptions{})
			require.NoError(t, err)
			err = fq.CreateValidationRequestResource(t.Context(), gvr, desired)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			persisted, err := client.Resource(gvr).Get(t.Context(), desired.GetName(), metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, existing.GetAnnotations(), persisted.GetAnnotations(), "retry must not upgrade or rewrite ownership")
		})
	}
}
