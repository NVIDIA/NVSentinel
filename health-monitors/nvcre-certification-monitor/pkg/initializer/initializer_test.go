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

package initializer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/health-monitors/nvcre-certification-monitor/pkg/state"
)

func TestStripNodeForCache_KeepsOnlyReconcilerFields(t *testing.T) {
	in := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "gpu-01",
			UID:             "uid-1",
			ResourceVersion: "42",
			Labels:          map[string]string{state.LabelKey: "true", "topology.kubernetes.io/zone": "a"},
			Annotations: map[string]string{
				state.AnnotationKey: `["nccl-all-gather/WorkloadFailed"]`,
				"other.io/big-blob": "should be dropped",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubelet"}},
		},
		Spec:   corev1.NodeSpec{PodCIDR: "10.0.0.0/24"},
		Status: corev1.NodeStatus{Images: []corev1.ContainerImage{{Names: []string{"img"}}}},
	}

	out, err := stripNodeForCache(in)
	require.NoError(t, err)

	node, ok := out.(*corev1.Node)
	require.True(t, ok)

	assert.Equal(t, "gpu-01", node.Name)
	assert.Equal(t, "42", node.ResourceVersion)
	assert.Equal(t, in.Labels, node.Labels)
	assert.Equal(t, map[string]string{state.AnnotationKey: `["nccl-all-gather/WorkloadFailed"]`}, node.Annotations)
	assert.Nil(t, node.ManagedFields)
	assert.Empty(t, node.Spec.PodCIDR)
	assert.Empty(t, node.Status.Images)
}

func TestStripNodeForCache_NoAnnotationYieldsNilMap(t *testing.T) {
	out, err := stripNodeForCache(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-02"}})
	require.NoError(t, err)
	assert.Nil(t, out.(*corev1.Node).Annotations)
}

func TestStripNodeForCache_RejectsNonNode(t *testing.T) {
	_, err := stripNodeForCache(&corev1.ConfigMap{})
	require.Error(t, err)
}
