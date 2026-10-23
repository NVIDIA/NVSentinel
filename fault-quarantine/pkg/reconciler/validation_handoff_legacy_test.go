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
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/commons/pkg/cordonownership"
	"github.com/nvidia/nvsentinel/commons/pkg/templates"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/validation"
)

func TestPerformUncordon_AdoptedLegacyRequestFinishesWithoutPersistentSource(t *testing.T) {
	environment := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "distros", "kubernetes", "nvsentinel", "charts", "lifecycle-manager", "crds")}, ErrorIfCRDPathMissing: true}
	restConfig, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	clientset, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	require.NoError(t, err)
	for _, scenario := range []string{"adoption", "new-source", "new-quarantine"} {
		t.Run(scenario, func(t *testing.T) {
			testAdoptedLegacyHandoff(t, clientset, dynamicClient, scenario)
		})
	}
}

func testAdoptedLegacyHandoff(t *testing.T, clientset kubernetes.Interface, dynamicClient dynamic.Interface, scenario string) {
	t.Helper()
	ctx := t.Context()
	name := "legacy-handoff-" + scenario
	eventID := map[string]string{"adoption": "0123456789abcdef01234567", "new-source": "0123456789abcdef01234568", "new-quarantine": "0123456789abcdef01234569"}[scenario]
	source := fmt.Sprintf(`[{"id":%q,"nodeName":%q,"agent":"gpu","checkName":"fault","tests":["smoke"]}]`, eventID, name)
	records, err := json.Marshal([]config.Taint{{Key: "example.com/fault", Effect: "NoSchedule", Value: "true"}})
	require.NoError(t, err)
	node, err := clientset.CoreV1().Nodes().Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{
			common.QuarantineHealthEventAnnotationKey:                  `[{"id":"0123456789abcdef01234567","agent":"gpu","checkName":"fault"}]`,
			common.QuarantineHealthEventIsCordonedAnnotationKey:        "True",
			common.QuarantineHealthEventCordonPreExistingAnnotationKey: "True",
			common.QuarantineHealthEventAppliedTaintsAnnotationKey:     string(records),
			common.QuarantineValidationHealthEventAnnotationKey:        source,
		}}, Spec: corev1.NodeSpec{Unschedulable: true, Taints: []corev1.Taint{{Key: "example.com/fault", Value: "true", Effect: corev1.TaintEffectNoSchedule}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	fq := &informer.FaultQuarantineClient{Clientset: clientset, DynamicClient: dynamicClient}
	cfg := config.TomlConfig{Validation: validationConfig([]config.ValidationRuleSet{{
		RuleSetMeta: config.RuleSetMeta{Enabled: true, Name: "smoke", Version: "1", Match: config.Match{Any: []config.Rule{{Kind: "HealthEvent", Expression: "true"}}}},
		Tests:       []string{"smoke"},
	}})}
	cfg.Validation.PersistHandoff = true
	validator, err := validation.NewValidationClient(cfg, fq, mockHealthEventStoreWithDrainStatus(t, true))
	require.NoError(t, err)
	tests, id, err := validator.FetchValidationTestsFromQuarantineSession(ctx, name, source)
	require.NoError(t, err)
	legacy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "nvsentinel.nvidia.com/v1alpha1", "kind": "ValidationRequest",
		"metadata": map[string]any{"name": "vr-" + id},
		"spec":     map[string]any{"nodes": []any{map[string]any{"name": name}}, "tests": []any{tests[0]}},
	}}
	templates.SetNodeOwnerRef(legacy, node)
	legacy, err = dynamicClient.Resource(validationRequestGVR).Create(ctx, legacy, metav1.CreateOptions{})
	require.NoError(t, err)
	var concurrent *corev1.Node
	if scenario != "adoption" {
		fq.DynamicClient = &legacyHandoffLookupClient{Interface: dynamicClient, afterGet: func() {
			current, err := clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
			require.NoError(t, err)
			if scenario == "new-source" {
				current.Annotations[common.QuarantineValidationHealthEventAnnotationKey] = source + " "
			} else {
				current.Annotations[common.QuarantineHealthEventAnnotationKey] = `[{"id":"new-fault"}]`
			}
			concurrent, err = clientset.CoreV1().Nodes().Update(ctx, current, metav1.UpdateOptions{})
			require.NoError(t, err)
		}}
	}
	r := NewReconciler(ReconcilerConfig{TomlConfig: cfg}, fq, nil)
	r.validationClient = validator
	r.SetLabelKeys("nvsentinel.nvidia.com/")
	retained, err := r.performUncordon(ctx, &protos.HealthEvent{NodeName: name, IsHealthy: true}, node.Annotations)
	if scenario != "adoption" {
		require.ErrorContains(t, err, "quarantine changed during validation handoff")
		require.True(t, retained)
		current, err := clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, concurrent, current, "legacy cleanup must preserve a concurrently changed quarantine and source")
		return
	}
	require.NoError(t, err)
	require.False(t, retained)
	current, err := clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotContains(t, current.Annotations, common.QuarantineHealthEventAnnotationKey)
	require.NotContains(t, current.Annotations, common.QuarantineValidationHealthEventAnnotationKey)
	require.Equal(t, string(records), current.Annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey])
	require.Equal(t, node.Spec.Taints, current.Spec.Taints)
	require.True(t, current.Spec.Unschedulable)
	actual, err := dynamicClient.Resource(validationRequestGVR).Get(ctx, legacy.GetName(), metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, legacy, actual, "adoption must not add ownership or source annotations to the legacy object")
	require.NotContains(t, actual.GetAnnotations(), cordonownership.RequestClaimIDAnnotationKey)
	pending, err := validator.EnsurePendingHandoff(ctx, name)
	require.NoError(t, err)
	require.False(t, pending, "legacy handoffs must not enter an unacknowledgeable replay loop")
}

// This forwarding client changes the real Node after the existing request is
// read, exposing the interval between legacy adoption and the Node handoff.
type legacyHandoffLookupClient struct {
	dynamic.Interface
	afterGet func()
}

func (c *legacyHandoffLookupClient) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &legacyHandoffLookupResource{NamespaceableResourceInterface: c.Interface.Resource(gvr), afterGet: c.afterGet}
}

type legacyHandoffLookupResource struct {
	dynamic.NamespaceableResourceInterface
	afterGet func()
}

func (r *legacyHandoffLookupResource) Get(ctx context.Context, name string, options metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	object, err := r.NamespaceableResourceInterface.Get(ctx, name, options, subresources...)
	if err == nil {
		r.afterGet()
	}
	return object, err
}
