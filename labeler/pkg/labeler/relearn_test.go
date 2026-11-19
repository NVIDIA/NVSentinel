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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nvidia/nvsentinel/labeler/pkg/devicecounts"
)

func TestTransformNodeForCache_KeepsRelearnAnnotations(t *testing.T) {
	relearnKey := devicecounts.RelearnAnnotationKey("gpu")
	node := &corev1.Node{
		Name: "node-a",
		Annotations: map[string]string{
			DCGMBootstrapCompletedAnnotation: "true",
			relearnKey:                       ratchetRelearnID + "/4",
			"drop":                           "annotation",
		},
	}

	transformed, err := transformNodeForCache(true, relearnKey)(node.DeepCopy())
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		DCGMBootstrapCompletedAnnotation: "true",
		relearnKey:                       ratchetRelearnID + "/4",
	}, transformed.(*corev1.Node).Annotations)

	transformed, err = transformNodeForCache(true)(node.DeepCopy())
	require.NoError(t, err)
	require.Equal(t, map[string]string{DCGMBootstrapCompletedAnnotation: "true"},
		transformed.(*corev1.Node).Annotations, "without a relearn ID the annotation is dropped")
}

func TestLabelerNodeRequiresReconciliation_RelearnCertificationLost(t *testing.T) {
	config := testDeviceCountConfig()
	config.Classes[0].RelearnID = ratchetRelearnID

	labeler, err := NewLabeler(
		fake.NewSimpleClientset(),
		time.Minute,
		"nvidia-dcgm",
		"nvidia-driver-daemonset",
		"nvidia-driver-installer",
		"",
		false,
		false,
		config,
		false,
	)
	require.NoError(t, err)

	relearnKey := devicecounts.RelearnAnnotationKey("gpu")
	oldNode := &corev1.Node{
		Name: "node-a",
		Labels: map[string]string{
			"nvidia.com/gpu.count":     "4",
			"test.nvsentinel/current":  "4",
			"test.nvsentinel/expected": "4",
		},
		Annotations: map[string]string{relearnKey: ratchetRelearnID + "/4"},
	}

	t.Run("reconciles when someone else changes a certified label", func(t *testing.T) {
		newNode := oldNode.DeepCopy()
		newNode.Labels["test.nvsentinel/expected"] = "8"

		require.True(t, labeler.nodeRequiresReconciliation(oldNode, newNode))
	})

	t.Run("ignores its own write of a label and its annotation", func(t *testing.T) {
		newNode := oldNode.DeepCopy()
		newNode.Labels["test.nvsentinel/expected"] = "6"
		newNode.Annotations[relearnKey] = ratchetRelearnID + "/6"

		require.False(t, labeler.nodeRequiresReconciliation(oldNode, newNode))
	})
}
