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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
