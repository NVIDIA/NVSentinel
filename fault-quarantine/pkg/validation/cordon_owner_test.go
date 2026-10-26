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
	"path/filepath"
	"testing"
	"text/template"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
)

func TestCreateValidationRequest_CarriesQuarantineCordonDecision(t *testing.T) {
	environment := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "distros", "kubernetes", "nvsentinel",
			"charts", "lifecycle-manager", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	dynamicClient, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	gvr := schema.GroupVersionResource{Group: "nvsentinel.nvidia.com", Version: "v1alpha1", Resource: "validationrequests"}
	client := &ValidationClient{
		k8sClient:   &informer.FaultQuarantineClient{DynamicClient: dynamicClient},
		resourceGVR: gvr,
		resourceTemplate: template.Must(template.New("validation").Parse(`apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: ValidationRequest
metadata:
  name: {{.NodeName}}-{{.SessionID}}
  annotations:
    example.com/retained: template-value
    nvsentinel.nvidia.com/cordon-pre-existing: template-value
spec:
  nodes:
  - name: {{.NodeName}}
  tests: [quick]
`)),
	}

	for _, tc := range []struct {
		name, cordoned, preExisting, cordonBy, want string
	}{
		{"owned", "True", "", "NVSentinel", "false"},
		{"pre-existing", "True", "True", "NVSentinel", "true"},
		{"taint-only", "", "", "NVSentinel", "true"},
		{"force-quarantine", "True", "", "custom-agent-creator", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: tc.name, UID: types.UID("node-" + tc.name),
				Labels: map[string]string{"example.com/cordon-by": tc.cordonBy},
				Annotations: map[string]string{
					common.QuarantineHealthEventIsCordonedAnnotationKey:        tc.cordoned,
					common.QuarantineHealthEventCordonPreExistingAnnotationKey: tc.preExisting,
				},
			}, Spec: corev1.NodeSpec{Unschedulable: true}}
			if tc.cordoned == "" {
				delete(node.Annotations, common.QuarantineHealthEventIsCordonedAnnotationKey)
			}
			if tc.preExisting == "" {
				delete(node.Annotations, common.QuarantineHealthEventCordonPreExistingAnnotationKey)
			}
			original := node.DeepCopy()
			require.NoError(t, client.CreateValidationRequest(t.Context(), node, []string{"quick"}, "session", "", ""))
			request, err := dynamicClient.Resource(gvr).Get(t.Context(), node.Name+"-session", metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, tc.want, request.GetAnnotations()[annotationCordonPreExisting])
			require.Equal(t, "template-value", request.GetAnnotations()["example.com/retained"])
			require.Equal(t, node.UID, request.GetOwnerReferences()[0].UID)
			require.Equal(t, original, node)
		})
	}
}
