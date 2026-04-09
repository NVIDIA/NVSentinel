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

package discoverer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testConfig() PodGroupConfig {
	return PodGroupConfig{
		Name:           "test-scheduler",
		AnnotationKeys: []string{"test.io/pod-group", "scheduling.k8s.io/group-name"},
		LabelKeys:      []string{"test.io/job-name"},
		MinCountExpr:   "podGroup.spec.minMember",
		PodGroupGVK: schema.GroupVersionKind{
			Group:   "scheduling.test.io",
			Version: "v1",
			Kind:    "PodGroup",
		},
	}
}

func TestPodGroupDiscoverer_CanHandle(t *testing.T) {
	tests := []struct {
		name   string
		config PodGroupConfig
		pod    *corev1.Pod
		want   bool
	}{
		{
			name:   "matches annotation",
			config: testConfig(),
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"test.io/pod-group": "my-pg"},
				},
			},
			want: true,
		},
		{
			name:   "matches label fallback",
			config: testConfig(),
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"test.io/job-name": "my-job"},
				},
			},
			want: true,
		},
		{
			name:   "no matching annotation or label",
			config: testConfig(),
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"some-label": "value"},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := NewPodGroupDiscoverer(nil, tt.config)
			if err != nil {
				t.Fatalf("NewPodGroupDiscoverer() error = %v", err)
			}

			if got := d.CanHandle(tt.pod); got != tt.want {
				t.Errorf("CanHandle() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodGroupDiscoverer_ExtractGangID(t *testing.T) {
	tests := []struct {
		name   string
		config PodGroupConfig
		pod    *corev1.Pod
		want   string
	}{
		{
			name:   "gang ID format",
			config: testConfig(),
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:   "default",
					Annotations: map[string]string{"test.io/pod-group": "pg-123"},
				},
			},
			want: "test-scheduler-default-pg-123",
		},
		{
			name:   "no matching annotation returns empty",
			config: testConfig(),
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "test"},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := NewPodGroupDiscoverer(nil, tt.config)
			if err != nil {
				t.Fatalf("NewPodGroupDiscoverer() error = %v", err)
			}

			if got := d.ExtractGangID(tt.pod); got != tt.want {
				t.Errorf("ExtractGangID() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- DiscoverPeers tests ---

func makePodGroupCRD(namespace, name string, minMember int64) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "scheduling.test.io",
		Version: "v1",
		Kind:    "PodGroup",
	})
	obj.SetNamespace(namespace)
	obj.SetName(name)
	_ = unstructured.SetNestedField(obj.Object, minMember, "spec", "minMember")
	return obj
}

func makePodInGroup(name, namespace, podGroupName, ip string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: map[string]string{"test.io/pod-group": podGroupName},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "c", Image: "img"}},
		},
		Status: corev1.PodStatus{
			Phase: phase,
			PodIP: ip,
		},
	}
}

func TestPodGroupDiscoverer_DiscoverPeers(t *testing.T) {
	pgGVK := schema.GroupVersionKind{
		Group: "scheduling.test.io", Version: "v1", Kind: "PodGroup",
	}

	t.Run("discovers peers in same PodGroup", func(t *testing.T) {
		pg := makePodGroupCRD("default", "my-pg", 3)
		pods := []runtime.Object{
			makePodInGroup("pod-0", "default", "my-pg", "10.0.0.1", corev1.PodRunning),
			makePodInGroup("pod-1", "default", "my-pg", "10.0.0.2", corev1.PodRunning),
			makePodInGroup("pod-2", "default", "my-pg", "10.0.0.3", corev1.PodPending),
			makePodInGroup("other-pod", "default", "other-pg", "10.0.0.4", corev1.PodRunning),
		}

		c := fake.NewClientBuilder().WithRuntimeObjects(append(pods, pg)...).Build()
		cfg := testConfig()
		cfg.PodGroupGVK = pgGVK
		d, err := NewPodGroupDiscoverer(c, cfg)
		require.NoError(t, err)

		requestPod := makePodInGroup("pod-0", "default", "my-pg", "10.0.0.1", corev1.PodRunning)
		info, err := d.DiscoverPeers(context.Background(), requestPod)
		require.NoError(t, err)
		require.NotNil(t, info)
		assert.Len(t, info.Peers, 3)
		assert.Equal(t, 3, info.ExpectedMinCount)
	})

	t.Run("filters by phase", func(t *testing.T) {
		pg := makePodGroupCRD("default", "phase-pg", 4)
		pods := []runtime.Object{
			makePodInGroup("p-running", "default", "phase-pg", "10.0.0.1", corev1.PodRunning),
			makePodInGroup("p-pending", "default", "phase-pg", "10.0.0.2", corev1.PodPending),
			makePodInGroup("p-succeeded", "default", "phase-pg", "10.0.0.3", corev1.PodSucceeded),
			makePodInGroup("p-failed", "default", "phase-pg", "10.0.0.4", corev1.PodFailed),
		}

		c := fake.NewClientBuilder().WithRuntimeObjects(append(pods, pg)...).Build()
		cfg := testConfig()
		cfg.PodGroupGVK = pgGVK
		d, err := NewPodGroupDiscoverer(c, cfg)
		require.NoError(t, err)

		info, err := d.DiscoverPeers(context.Background(), makePodInGroup("p-running", "default", "phase-pg", "10.0.0.1", corev1.PodRunning))
		require.NoError(t, err)
		require.NotNil(t, info)
		assert.Len(t, info.Peers, 2, "only Running and Pending should be included")
	})

	t.Run("extracts minCount via CEL", func(t *testing.T) {
		pg := makePodGroupCRD("default", "cel-pg", 8)
		pods := []runtime.Object{
			makePodInGroup("pod-0", "default", "cel-pg", "10.0.0.1", corev1.PodRunning),
		}

		c := fake.NewClientBuilder().WithRuntimeObjects(append(pods, pg)...).Build()
		cfg := testConfig()
		cfg.PodGroupGVK = pgGVK
		d, err := NewPodGroupDiscoverer(c, cfg)
		require.NoError(t, err)

		info, err := d.DiscoverPeers(context.Background(), makePodInGroup("pod-0", "default", "cel-pg", "10.0.0.1", corev1.PodRunning))
		require.NoError(t, err)
		require.NotNil(t, info)
		assert.Equal(t, 8, info.ExpectedMinCount)
	})

	t.Run("returns nil when no peers", func(t *testing.T) {
		pg := makePodGroupCRD("default", "empty-pg", 2)

		c := fake.NewClientBuilder().WithRuntimeObjects(pg).Build()
		cfg := testConfig()
		cfg.PodGroupGVK = pgGVK
		d, err := NewPodGroupDiscoverer(c, cfg)
		require.NoError(t, err)

		// The requesting pod is not in the fake client's pod list
		requestPod := makePodInGroup("orphan", "default", "empty-pg", "10.0.0.1", corev1.PodRunning)
		info, err := d.DiscoverPeers(context.Background(), requestPod)
		require.NoError(t, err)
		assert.Nil(t, info)
	})

	t.Run("PodGroup not found returns error", func(t *testing.T) {
		pods := []runtime.Object{
			makePodInGroup("pod-0", "default", "missing-pg", "10.0.0.1", corev1.PodRunning),
		}

		c := fake.NewClientBuilder().WithRuntimeObjects(pods...).Build()
		cfg := testConfig()
		cfg.PodGroupGVK = pgGVK
		d, err := NewPodGroupDiscoverer(c, cfg)
		require.NoError(t, err)

		_, err = d.DiscoverPeers(context.Background(), makePodInGroup("pod-0", "default", "missing-pg", "10.0.0.1", corev1.PodRunning))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "PodGroup")
	})

	t.Run("pod without annotation returns nil", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		cfg := testConfig()
		cfg.PodGroupGVK = pgGVK
		d, err := NewPodGroupDiscoverer(c, cfg)
		require.NoError(t, err)

		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}}
		info, err := d.DiscoverPeers(context.Background(), pod)
		require.NoError(t, err)
		assert.Nil(t, info)
	})
}
