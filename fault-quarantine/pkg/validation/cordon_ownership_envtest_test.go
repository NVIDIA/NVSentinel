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
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"
	"text/template"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/commons/pkg/cordonownership"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
)

func TestCreateValidationRequest_PersistsCordonOwnershipAndNodeIdentity(t *testing.T) {
	environment := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "distros", "kubernetes", "nvsentinel", "charts", "lifecycle-manager", "crds")}, ErrorIfCRDPathMissing: true}
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	cfg, err := environment.Start()
	require.NoError(t, err)
	clientset, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	dynamicClient, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	gvr := schema.GroupVersionResource{Group: "nvsentinel.nvidia.com", Version: "v1alpha1", Resource: "validationrequests"}
	tmpl, err := template.New("request").Parse(`apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: ValidationRequest
metadata:
  name: {{.NodeName}}-{{.SessionID}}
  annotations:
    nvsentinel.nvidia.com/cordon-ownership: owned
    nvsentinel.nvidia.com/cordon-claim-id: template-cannot-grant
    example.com/keep: unchanged
spec:
  nodes:
  - name: {{.NodeName}}
  tests:
  {{range .Tests}}- {{.}}
  {{end}}
`)
	require.NoError(t, err)
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		want        string
		claimMode   string
	}{
		{"owned", map[string]string{common.QuarantineHealthEventIsCordonedAnnotationKey: "True"}, common.CordonOwnershipOwned, ""},
		{"pre-existing", map[string]string{common.QuarantineHealthEventIsCordonedAnnotationKey: "True", common.QuarantineHealthEventCordonPreExistingAnnotationKey: "True"}, common.CordonOwnershipPreExisting, ""},
		{"missing", nil, common.CordonOwnershipUnknown, ""},
		{"malformed", map[string]string{common.QuarantineHealthEventIsCordonedAnnotationKey: "True", common.QuarantineHealthEventCordonPreExistingAnnotationKey: "false"}, common.CordonOwnershipUnknown, ""},
		{"claim-owned", map[string]string{common.QuarantineHealthEventIsCordonedAnnotationKey: "True"}, common.CordonOwnershipOwned, "owned"},
		{"claim-preserved", map[string]string{common.QuarantineHealthEventIsCordonedAnnotationKey: "True"}, common.CordonOwnershipUnknown, "preserved"},
		{"claim-manual", map[string]string{common.QuarantineHealthEventIsCordonedAnnotationKey: "True", common.QuarantineHealthEventCordonPreExistingAnnotationKey: "True"}, common.CordonOwnershipPreExisting, "preserved"},
		{"claim-invalid", map[string]string{common.QuarantineHealthEventIsCordonedAnnotationKey: "True"}, common.CordonOwnershipUnknown, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			const source = `[{"id":"source-event","tests":["quick"]}]`
			if tc.annotations == nil {
				tc.annotations = make(map[string]string)
			}
			tc.annotations[common.QuarantineValidationHealthEventAnnotationKey] = source
			node, err := clientset.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: "handoff-" + tc.name, Annotations: tc.annotations, Labels: map[string]string{"k8saas.nvidia.com/cordoned-by": "fault-quarantine"},
			}, Spec: corev1.NodeSpec{Unschedulable: true}}, metav1.CreateOptions{})
			require.NoError(t, err)
			claimID := ""
			if tc.claimMode != "" {
				claim := cordonownership.New(node)
				claim.PreserveCordon = tc.claimMode == "preserved"
				_, err = cordonownership.Store(node, claim)
				require.NoError(t, err)
				if tc.claimMode == "invalid" {
					node.Annotations[cordonownership.AnnotationKey] = `{"id":"old","nodeUID":"replaced"}`
				} else {
					claimID = claim.ID
				}
				node, err = clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
				require.NoError(t, err)
			}
			client := &ValidationClient{k8sClient: &informer.FaultQuarantineClient{DynamicClient: dynamicClient},
				resourceGVR: gvr, resourceTemplate: tmpl, persistHandoff: true}
			require.Error(t, client.CreateValidationRequest(ctx, node, []string{"quick"}, "stale", "trace", "span", source+" "))
			_, err = dynamicClient.Resource(gvr).Get(ctx, node.Name+"-stale", metav1.GetOptions{})
			require.True(t, apierrors.IsNotFound(err), "a changed source must not create a request")
			require.NoError(t, client.CreateValidationRequest(ctx, node, []string{"quick"}, "session", "trace", "span", source))
			// Clearing quarantine annotations does not discard the handoff's release decision.
			node.Annotations = nil
			_, err = clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			require.NoError(t, err)
			request, err := dynamicClient.Resource(gvr).Get(ctx, node.Name+"-session", metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, tc.want, request.GetAnnotations()[common.ValidationCordonOwnershipAnnotationKey])
			require.Equal(t, "unchanged", request.GetAnnotations()["example.com/keep"])
			require.Equal(t, claimID, request.GetAnnotations()[cordonownership.RequestClaimIDAnnotationKey])
			require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(source))),
				request.GetAnnotations()[common.ValidationSourceHashAnnotationKey])
			require.Len(t, request.GetOwnerReferences(), 1)
			require.Equal(t, node.UID, request.GetOwnerReferences()[0].UID)
			// A new Node with the same name cannot adopt this already-created request.
			require.NoError(t, clientset.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{}))
			replacement, err := clientset.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Annotations: tc.annotations}}, metav1.CreateOptions{})
			require.NoError(t, err)
			restarted := &ValidationClient{k8sClient: &informer.FaultQuarantineClient{DynamicClient: dynamicClient},
				resourceGVR: gvr, resourceTemplate: tmpl, persistHandoff: true}
			require.Error(t, restarted.CreateValidationRequest(ctx, replacement, []string{"quick"}, "session", "trace", "span", source))
		})
	}
}
