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

// Package gang provides gang scheduling discovery and coordination for multi-node workloads.
package gang

import (
	"testing"

	"github.com/nvidia/nvsentinel/preflight/pkg/config"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNewDiscovererFromConfig(t *testing.T) {
	// Create fake client
	fakeClient := fake.NewClientBuilder().Build()

	// Create REST mapper with required GVKs registered
	restMapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "scheduling.k8s.io", Version: "v1alpha1"},
		{Group: "scheduling.volcano.sh", Version: "v1beta1"},
	})
	// K8s 1.35+ native Workload API
	restMapper.Add(schema.GroupVersionKind{
		Group:   "scheduling.k8s.io",
		Version: "v1alpha1",
		Kind:    "Workload",
	}, meta.RESTScopeNamespace)
	// Volcano PodGroup
	restMapper.Add(schema.GroupVersionKind{
		Group:   "scheduling.volcano.sh",
		Version: "v1beta1",
		Kind:    "PodGroup",
	}, meta.RESTScopeNamespace)

	tests := []struct {
		name      string
		cfg       config.GangDiscoveryConfig
		wantName  string
		wantError bool
	}{
		{
			name:     "default to kubernetes native",
			cfg:      config.GangDiscoveryConfig{},
			wantName: "kubernetes",
		},
		{
			name: "PodGroup-based scheduler",
			cfg: config.GangDiscoveryConfig{
				Name:           "volcano",
				AnnotationKeys: []string{"volcano.sh/pod-group"},
				MinCountExpr:   "podGroup.spec.minMember",
				PodGroupGVR: config.GVRConfig{
					Group:    "scheduling.volcano.sh",
					Version:  "v1beta1",
					Resource: "podgroups",
				},
			},
			wantName: "volcano",
		},
		{
			name: "missing annotation keys",
			cfg: config.GangDiscoveryConfig{
				Name: "my-scheduler",
				PodGroupGVR: config.GVRConfig{
					Group: "my.io", Version: "v1", Resource: "podgroups",
				},
			},
			wantError: true,
		},
		{
			name: "missing podGroupGVR",
			cfg: config.GangDiscoveryConfig{
				Name:           "my-scheduler",
				AnnotationKeys: []string{"my.io/pod-group"},
			},
			wantError: true,
		},
		{
			name: "GVR not found in cluster",
			cfg: config.GangDiscoveryConfig{
				Name:           "unknown-scheduler",
				AnnotationKeys: []string{"unknown.io/pod-group"},
				PodGroupGVR: config.GVRConfig{
					Group:    "unknown.io",
					Version:  "v1",
					Resource: "podgroups",
				},
			},
			wantError: true,
		},
		{
			name: "PodGroup fields without name",
			cfg: config.GangDiscoveryConfig{
				AnnotationKeys: []string{"volcano.sh/pod-group"},
				PodGroupGVR: config.GVRConfig{
					Group:    "scheduling.volcano.sh",
					Version:  "v1beta1",
					Resource: "podgroups",
				},
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewDiscovererFromConfig(tt.cfg, fakeClient, restMapper)

			if tt.wantError {
				if err == nil {
					t.Error("NewDiscovererFromConfig() expected error, got nil")
				}

				return
			}

			if err != nil {
				t.Fatalf("NewDiscovererFromConfig() error = %v", err)
			}

			if got.Name() != tt.wantName {
				t.Errorf("Discoverer.Name() = %q, want %q", got.Name(), tt.wantName)
			}
		})
	}
}
