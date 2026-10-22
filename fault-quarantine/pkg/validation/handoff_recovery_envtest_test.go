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

package validation

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"text/template"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/commons/pkg/cordonownership"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
)

const recoveryTemplate = `apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: ValidationRequest
metadata:
  name: {{.NodeName}}-{{.SessionID}}
spec:
  nodes:
  - name: {{.NodeName}}
  tests:
  {{range .Tests}}- {{.}}
  {{end}}
`

type recoveryFixture struct {
	client *ValidationClient
	node   *corev1.Node
	name   string
}

func TestEnsurePendingHandoff_ReplaysOnlyUnregisteredSource(t *testing.T) {
	environment := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "distros", "kubernetes", "nvsentinel", "charts", "lifecycle-manager", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	clientset, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	dynamicClient, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	gvr := schema.GroupVersionResource{Group: "nvsentinel.nvidia.com", Version: "v1alpha1", Resource: "validationrequests"}
	requests := dynamicClient.Resource(gvr)
	tmpl := template.Must(template.New("request").Parse(recoveryTemplate))
	newFixture := func(t *testing.T, name string, owned bool) recoveryFixture {
		t.Helper()
		source := fmt.Sprintf(`[{"nodeName":%q,"id":%q,"tests":["quick"]}]`, name, "event-"+name)
		node, err := clientset.CoreV1().Nodes().Create(t.Context(), &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{common.QuarantineValidationHealthEventAnnotationKey: source}},
			Spec:       corev1.NodeSpec{Unschedulable: true},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
		if owned {
			_, err = cordonownership.Store(node, cordonownership.New(node))
			require.NoError(t, err)
			node, err = clientset.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
			require.NoError(t, err)
		}
		return recoveryFixture{
			client: &ValidationClient{k8sClient: &informer.FaultQuarantineClient{Clientset: clientset, DynamicClient: dynamicClient}, resourceGVR: gvr, resourceTemplate: tmpl, persistHandoff: true},
			node:   node, name: name + "-" + sessionID([]string{"event-" + name}),
		}
	}

	t.Run("recreates request deleted before any finalizer without event history", func(t *testing.T) {
		f := newFixture(t, "recovery-deleted", true)
		// No HealthEventStore is provided: replay must use the already-approved source.
		pending, err := f.client.EnsurePendingHandoff(t.Context(), f.node.Name)
		require.NoError(t, err)
		require.True(t, pending)
		first, err := requests.Get(t.Context(), f.name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Empty(t, first.GetFinalizers())
		require.Equal(t, common.CordonOwnershipOwned, first.GetAnnotations()[common.ValidationCordonOwnershipAnnotationKey])
		require.NoError(t, requests.Delete(t.Context(), f.name, metav1.DeleteOptions{}))
		pending, err = f.client.EnsurePendingHandoff(t.Context(), f.node.Name)
		require.NoError(t, err)
		require.True(t, pending)
		restored, err := requests.Get(t.Context(), f.name, metav1.GetOptions{})
		require.NoError(t, err)
		require.NotEqual(t, first.GetUID(), restored.GetUID())
		require.Equal(t, first.GetAnnotations(), restored.GetAnnotations())
		node, err := clientset.CoreV1().Nodes().Get(t.Context(), f.node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, f.node.Annotations, node.Annotations)
		require.True(t, node.Spec.Unschedulable)
		delete(node.Annotations, common.QuarantineValidationHealthEventAnnotationKey)
		_, err = clientset.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.NoError(t, requests.Delete(t.Context(), f.name, metav1.DeleteOptions{}))
		pending, err = f.client.EnsurePendingHandoff(t.Context(), f.node.Name)
		require.NoError(t, err)
		require.False(t, pending)
		_, err = requests.Get(t.Context(), f.name, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(err), "a consumed source must not rerun validation")
	})

	t.Run("keeps completed request unchanged", func(t *testing.T) {
		f := newFixture(t, "recovery-completed", true)
		_, err := f.client.EnsurePendingHandoff(t.Context(), f.node.Name)
		require.NoError(t, err)
		request, err := requests.Get(t.Context(), f.name, metav1.GetOptions{})
		require.NoError(t, err)
		request.Object["status"] = map[string]any{"phase": "Succeeded"}
		request, err = requests.UpdateStatus(t.Context(), request, metav1.UpdateOptions{})
		require.NoError(t, err)
		_, err = f.client.EnsurePendingHandoff(t.Context(), f.node.Name)
		require.NoError(t, err)
		unchanged, err := requests.Get(t.Context(), f.name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, request, unchanged)
	})

	t.Run("keeps existing pre-existing snapshot after quarantine cleanup", func(t *testing.T) {
		f := newFixture(t, "recovery-manual", false)
		f.node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] = "True"
		f.node.Annotations[common.QuarantineHealthEventCordonPreExistingAnnotationKey] = "True"
		_, err := f.client.CreateValidationRequest(t.Context(), f.node, []string{"quick"}, sessionID([]string{"event-" + f.node.Name}), "", "", f.node.Annotations[common.QuarantineValidationHealthEventAnnotationKey])
		require.NoError(t, err)
		_, err = f.client.EnsurePendingHandoff(t.Context(), f.node.Name)
		require.NoError(t, err)
		request, err := requests.Get(t.Context(), f.name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, common.CordonOwnershipPreExisting, request.GetAnnotations()[common.ValidationCordonOwnershipAnnotationKey])
		require.NoError(t, requests.Delete(t.Context(), f.name, metav1.DeleteOptions{}))
		_, err = f.client.EnsurePendingHandoff(t.Context(), f.node.Name)
		require.NoError(t, err)
		request, err = requests.Get(t.Context(), f.name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, common.CordonOwnershipUnknown, request.GetAnnotations()[common.ValidationCordonOwnershipAnnotationKey])
	})

	for _, change := range []string{"source-consumed", "source-replaced", "node-replaced", "new-quarantine", "claim-preserved"} {
		t.Run(change+" during missing-request lookup", func(t *testing.T) {
			f := newFixture(t, "recovery-"+change, true)
			raced := false
			f.client.k8sClient.DynamicClient = &recoveryLookupClient{Interface: dynamicClient, afterMissing: func() {
				if raced {
					return
				}
				raced = true
				node, err := clientset.CoreV1().Nodes().Get(t.Context(), f.node.Name, metav1.GetOptions{})
				require.NoError(t, err)
				switch change {
				case "source-consumed":
					delete(node.Annotations, common.QuarantineValidationHealthEventAnnotationKey)
				case "source-replaced":
					node.Annotations[common.QuarantineValidationHealthEventAnnotationKey] = `[{"id":"new-source","tests":["quick"]}]`
				case "new-quarantine":
					node.Annotations[common.QuarantineHealthEventAnnotationKey] = `[{"id":"new-fault"}]`
				case "node-replaced":
					require.NoError(t, clientset.CoreV1().Nodes().Delete(t.Context(), node.Name, metav1.DeleteOptions{}))
					_, err = clientset.CoreV1().Nodes().Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Annotations: node.Annotations}, Spec: node.Spec}, metav1.CreateOptions{})
					require.NoError(t, err)
					return
				case "claim-preserved":
					claim, ok := cordonownership.Read(node)
					require.True(t, ok)
					claim.PreserveCordon = true
					_, err = cordonownership.Store(node, claim)
					require.NoError(t, err)
				}
				_, err = clientset.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
				require.NoError(t, err)
			}}
			_, err := f.client.EnsurePendingHandoff(t.Context(), f.node.Name)
			require.NoError(t, err)
			require.True(t, raced)
			request, err := requests.Get(t.Context(), f.name, metav1.GetOptions{})
			if change == "claim-preserved" {
				require.NoError(t, err)
				require.Equal(t, common.CordonOwnershipUnknown, request.GetAnnotations()[common.ValidationCordonOwnershipAnnotationKey])
			} else {
				require.True(t, apierrors.IsNotFound(err))
			}
		})
	}

	for _, mode := range []string{"disabled", "dry-run", "healthy", "quarantined", "dry-run-source"} {
		t.Run(mode+" does not create validation", func(t *testing.T) {
			f := newFixture(t, "recovery-noop-"+mode, true)
			switch mode {
			case "disabled":
				f.client.persistHandoff = false
			case "dry-run":
				f.client.k8sClient.DryRunMode = true
			case "healthy":
				f.node.Annotations = nil
				f.node.Spec.Unschedulable = false
			case "quarantined":
				f.node.Annotations[common.QuarantineHealthEventAnnotationKey] = `[{"id":"fault"}]`
			case "dry-run-source":
				f.node.Annotations[common.QuarantineHealthEventDryRunAnnotationKey] = "True"
			}
			_, err := clientset.CoreV1().Nodes().Update(t.Context(), f.node, metav1.UpdateOptions{})
			require.NoError(t, err)
			pending, err := f.client.EnsurePendingHandoff(t.Context(), f.node.Name)
			require.NoError(t, err)
			require.False(t, pending)
			_, err = requests.Get(t.Context(), f.name, metav1.GetOptions{})
			require.True(t, apierrors.IsNotFound(err))
		})
	}
}

type recoveryLookupClient struct {
	dynamic.Interface
	afterMissing func()
}

func (c *recoveryLookupClient) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &recoveryLookupResource{NamespaceableResourceInterface: c.Interface.Resource(gvr), afterMissing: c.afterMissing}
}

type recoveryLookupResource struct {
	dynamic.NamespaceableResourceInterface
	afterMissing func()
}

func (r *recoveryLookupResource) Get(ctx context.Context, name string, opts metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	obj, err := r.NamespaceableResourceInterface.Get(ctx, name, opts, subresources...)
	if apierrors.IsNotFound(err) {
		r.afterMissing()
	}
	return obj, err
}
