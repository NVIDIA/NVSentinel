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

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/config"
)

var (
	gpuAllocatableCriteria = []v1alpha1.CriteriaSpec{
		{
			Name: "gpu-allocatable",
			Expression: `has(node.status.allocatable) && "nvidia.com/gpu" in node.status.allocatable &&
				quantity(node.status.allocatable["nvidia.com/gpu"]) > 0`,
		},
	}
	cordonedCriteria = []v1alpha1.CriteriaSpec{
		{
			Name:       "cordoned",
			Expression: `has(node.spec.unschedulable) && node.spec.unschedulable`,
		},
	}
	notUnderQuarantineCriteria = []v1alpha1.CriteriaSpec{
		{
			Name: "not-under-quarantine",
			Expression: `!(has(node.metadata.annotations) &&
				"quarantineHealthEvent" in node.metadata.annotations)`,
		},
	}
	readyLabelCriteria = []v1alpha1.CriteriaSpec{
		{
			Name:       "test-criterion",
			Expression: `has(node.metadata.labels) && "ready" in node.metadata.labels`,
		},
	}
)

func TestEvaluateNodeReadinessCriteria(t *testing.T) {
	tests := []struct {
		name               string
		criteria           []v1alpha1.CriteriaSpec
		node               *corev1.Node
		wantErr            bool
		wantFailedCriteria string
	}{
		{
			name:     "gpu-allocatable: GPUs present",
			criteria: gpuAllocatableCriteria,
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
				},
			},
			wantFailedCriteria: "",
		},
		{
			name:     "gpu-allocatable: GPUs missing (quantity zero)",
			criteria: gpuAllocatableCriteria,
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("0")},
				},
			},
			wantFailedCriteria: "gpu-allocatable",
		},
		{
			name:     "gpu-allocatable: allocatable section missing entirely",
			criteria: gpuAllocatableCriteria,
			node: &corev1.Node{
				Status: corev1.NodeStatus{Allocatable: nil},
			},
			wantFailedCriteria: "gpu-allocatable",
		},
		{
			name:     "gpu-allocatable: allocatable present without gpu resource type",
			criteria: gpuAllocatableCriteria,
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Allocatable: corev1.ResourceList{"cpu": resource.MustParse("4")},
				},
			},
			wantFailedCriteria: "gpu-allocatable",
		},
		{
			name:     "cordoned: node is schedulable",
			criteria: cordonedCriteria,
			node: &corev1.Node{
				Spec: corev1.NodeSpec{Unschedulable: false},
			},
			wantFailedCriteria: "cordoned",
		},
		{
			name:     "cordoned: node is cordoned",
			criteria: cordonedCriteria,
			node: &corev1.Node{
				Spec: corev1.NodeSpec{Unschedulable: true},
			},
			wantFailedCriteria: "",
		},
		{
			name:     "not-under-quarantine: no annotations",
			criteria: notUnderQuarantineCriteria,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Annotations: nil},
			},
			wantFailedCriteria: "",
		},
		{
			name:     "not-under-quarantine: other annotations present",
			criteria: notUnderQuarantineCriteria,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"some-other-annotation": "value"}},
			},
			wantFailedCriteria: "",
		},
		{
			name:     "not-under-quarantine: quarantine annotation present",
			criteria: notUnderQuarantineCriteria,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"quarantineHealthEvent": "{}"},
				},
			},
			wantFailedCriteria: "not-under-quarantine",
		},
		{
			name:     "test-criterion: ready label present",
			criteria: readyLabelCriteria,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"ready": "true"}},
			},
			wantFailedCriteria: "",
		},
		{
			name:     "test-criterion: no labels",
			criteria: readyLabelCriteria,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Labels: nil},
			},
			wantFailedCriteria: "test-criterion",
		},
		{
			name:     "test-criterion: other labels present without ready",
			criteria: readyLabelCriteria,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"other-label": "value"}},
			},
			wantFailedCriteria: "test-criterion",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Validation: &v1alpha1.ValidationConfiguration{
					Spec: v1alpha1.ValidationConfigurationSpec{ReadinessCriteria: tt.criteria},
				},
			}

			reconciler, err := NewValidationRequestReconciler(nil, nil, nil, cfg, "")
			if err != nil {
				t.Fatalf("failed to construct reconciler: %v", err)
			}

			failedCriterion, err := reconciler.evaluateNodeReadinessCriteria(tt.node, tt.criteria)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got none")
				}

				return
			}

			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}

			if failedCriterion != tt.wantFailedCriteria {
				t.Fatalf("failedCriterion = %q, want %q", failedCriterion, tt.wantFailedCriteria)
			}
		})
	}
}
