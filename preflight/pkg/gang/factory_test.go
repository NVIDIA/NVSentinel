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
	"context"
	"testing"

	"github.com/nvidia/nvsentinel/preflight/pkg/config"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang/discoverer"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestNewDiscovererFromConfig(t *testing.T) {
	// Create fake client
	fakeClient := fake.NewClientBuilder().Build()

	// Create REST mapper with required GVKs registered
	restMapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "scheduling.k8s.io", Version: "v1alpha2"},
		{Group: "scheduling.volcano.sh", Version: "v1beta1"},
	})
	// K8s 1.36+ native PodGroup API
	restMapper.Add(schema.GroupVersionKind{
		Group:   "scheduling.k8s.io",
		Version: "v1alpha2",
		Kind:    "PodGroup",
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

func TestNewDiscovererFromConfig_NativeKubernetesVersionSelection(t *testing.T) {
	fakeClient := fake.NewClientBuilder().Build()

	// The PodGroup version must reach the discoverer, so check the owner
	// reference it builds, not only the discoverer type.
	for _, version := range []string{"v1alpha2", "v1beta1"} { // K8s 1.36, K8s 1.37
		t.Run("prefers PodGroup "+version+" over Workload", func(t *testing.T) {
			podGroupGVK := discoverer.PodGroupGroupKind.WithVersion(version)
			restMapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
				podGroupGVK.GroupVersion(),
				discoverer.WorkloadGVK.GroupVersion(),
			})
			restMapper.Add(podGroupGVK, meta.RESTScopeNamespace)
			restMapper.Add(discoverer.WorkloadGVK, meta.RESTScopeNamespace)

			podGroup := &unstructured.Unstructured{}
			podGroup.SetGroupVersionKind(podGroupGVK)
			podGroup.SetNamespace("default")
			podGroup.SetName("training")
			c := fake.NewClientBuilder().WithRuntimeObjects(podGroup).Build()

			got, err := NewDiscovererFromConfig(config.GangDiscoveryConfig{}, c, restMapper)
			require.NoError(t, err)
			native, ok := got.(*discoverer.KubernetesDiscoverer)
			require.True(t, ok, "NewDiscovererFromConfig() = %T, want *discoverer.KubernetesDiscoverer", got)

			ownerRef, err := native.OwnerReference(context.Background(), schedulingGroupPod("default", "training"))
			require.NoError(t, err)
			require.NotNil(t, ownerRef)
			assert.Equal(t, podGroupGVK.GroupVersion().String(), ownerRef.APIVersion)
		})
	}

	t.Run("falls back to K8s 1.35 Workload API", func(t *testing.T) {
		restMapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
			{Group: "scheduling.k8s.io", Version: "v1alpha1"},
		})
		restMapper.Add(discoverer.WorkloadGVK, meta.RESTScopeNamespace)

		got, err := NewDiscovererFromConfig(config.GangDiscoveryConfig{}, fakeClient, restMapper)
		if err != nil {
			t.Fatalf("NewDiscovererFromConfig() error = %v", err)
		}

		if _, ok := got.(*discoverer.WorkloadRefDiscoverer); !ok {
			t.Fatalf("NewDiscovererFromConfig() = %T, want *discoverer.WorkloadRefDiscoverer", got)
		}
	})

	t.Run("errors when no native K8s gang API is available", func(t *testing.T) {
		restMapper := meta.NewDefaultRESTMapper(nil)

		_, err := NewDiscovererFromConfig(config.GangDiscoveryConfig{}, fakeClient, restMapper)
		if err == nil {
			t.Fatal("NewDiscovererFromConfig() expected error, got nil")
		}
	})
}

// TestNewDiscovererFromConfig_NativeKubernetes137 runs native discovery against
// a K8s 1.37 API server. K8s 1.37 serves PodGroup only as v1beta1, behind the
// GenericWorkload feature gate.
func TestNewDiscovererFromConfig_NativeKubernetes137(t *testing.T) {
	testEnvironment := &envtest.Environment{}
	apiServerArgs := testEnvironment.ControlPlane.GetAPIServer().Configure()
	apiServerArgs.Append("feature-gates", "GenericWorkload=true")
	apiServerArgs.Append("runtime-config", "scheduling.k8s.io/v1beta1=true")

	testConfig, err := testEnvironment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnvironment.Stop()) })

	httpClient, err := rest.HTTPClientFor(testConfig)
	require.NoError(t, err)
	restMapper, err := apiutil.NewDynamicRESTMapper(testConfig, httpClient)
	require.NoError(t, err)
	c, err := client.New(testConfig, client.Options{Mapper: restMapper})
	require.NoError(t, err)

	ctx := context.Background()
	podGroup := &unstructured.Unstructured{}
	podGroup.SetGroupVersionKind(discoverer.PodGroupGroupKind.WithVersion("v1beta1"))
	podGroup.SetNamespace("default")
	podGroup.SetName("training")
	require.NoError(t, unstructured.SetNestedField(
		podGroup.Object, int64(2), "spec", "schedulingPolicy", "gang", "minCount"))
	require.NoError(t, c.Create(ctx, podGroup))

	got, err := NewDiscovererFromConfig(config.GangDiscoveryConfig{}, c, restMapper)
	require.NoError(t, err)
	native, ok := got.(*discoverer.KubernetesDiscoverer)
	require.True(t, ok, "NewDiscovererFromConfig() = %T, want *discoverer.KubernetesDiscoverer", got)

	ownerRef, err := native.OwnerReference(ctx, schedulingGroupPod("default", "training"))
	require.NoError(t, err)
	require.NotNil(t, ownerRef)

	// The version is whichever one the API server prefers, so it can move to
	// v1 when PodGroup reaches GA. The UID proves the discoverer read the
	// PodGroup created above.
	ownerGV, err := schema.ParseGroupVersion(ownerRef.APIVersion)
	require.NoError(t, err)
	assert.Equal(t, discoverer.PodGroupGroupKind.Group, ownerGV.Group)
	assert.Equal(t, discoverer.PodGroupGroupKind.Kind, ownerRef.Kind)
	assert.Equal(t, podGroup.GetUID(), ownerRef.UID)
}

func schedulingGroupPod(namespace, podGroup string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "worker-0"},
		Spec: corev1.PodSpec{
			SchedulingGroup: &corev1.PodSchedulingGroup{PodGroupName: new(podGroup)},
		},
	}
}
