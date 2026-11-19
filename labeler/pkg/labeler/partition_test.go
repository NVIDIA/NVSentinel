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

package labeler

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// The node cache keeps the partition annotation of every class. Without it
// every cached node would look like one whose label was written before the
// annotation existed, and its label would count in any partition.
func TestNodeCache_KeepsThePartitionAnnotation(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.Node{
		Name: "node-a",
		Annotations: map[string]string{
			ratchetPartitionAnnotation: "default",
			"drop":                     "annotation",
		},
	})

	labeler, _ := startTransformTestLabeler(t, clientset, testDeviceCountConfig())

	cached, err := labeler.getNodeFromCache("node-a")
	require.NoError(t, err)
	require.Equal(t, map[string]string{ratchetPartitionAnnotation: "default"}, cached.Annotations)
}

// The expected label, the partition annotation and, under a relearn ID, the
// relearn annotation reach the API server in the same patch, so no reader
// sees a label without the annotations that say whether it counts.
func TestReconcileNodeLabels_ExpectedLabelAndItsAnnotationsInOnePatch(t *testing.T) {
	config := testDeviceCountConfig()
	config.Classes[0].RelearnID = ratchetRelearnID

	clientset := fake.NewSimpleClientset(&corev1.Node{
		Name:   "node-a",
		Labels: map[string]string{"nvidia.com/gpu.count": "4"},
	})

	startTransformTestLabeler(t, clientset, config)

	patches := 0

	for _, action := range clientset.Actions() {
		patch, ok := action.(k8stesting.PatchAction)
		if !ok || action.GetResource().Resource != "nodes" {
			continue
		}

		var body struct {
			Metadata struct {
				Labels      map[string]any `json:"labels"`
				Annotations map[string]any `json:"annotations"`
			} `json:"metadata"`
		}
		require.NoError(t, json.Unmarshal(patch.GetPatch(), &body))

		_, label := body.Metadata.Labels["test.nvsentinel/expected"]
		_, partition := body.Metadata.Annotations[ratchetPartitionAnnotation]
		_, relearn := body.Metadata.Annotations[ratchetAnnotation]

		if label || partition || relearn {
			require.True(t, label && partition && relearn, "patch %s", patch.GetPatch())

			patches++
		}
	}

	require.Equal(t, 1, patches)

	node, err := clientset.CoreV1().Nodes().Get(t.Context(), "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "4", node.Labels["test.nvsentinel/expected"])
	require.Equal(t, "default", node.Annotations[ratchetPartitionAnnotation])
	require.Equal(t, ratchetRelearnID+"/4", node.Annotations[ratchetAnnotation])
}
