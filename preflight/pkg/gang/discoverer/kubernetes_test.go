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
)

func makePodWithWorkloadRef(ns, workload, podGroup string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns},
	}

	if workload != "" {
		pod.Spec.WorkloadRef = &corev1.WorkloadReference{
			Name:     workload,
			PodGroup: podGroup,
		}
	}

	return pod
}

func TestWorkloadRefDiscoverer_CanHandle(t *testing.T) {
	d := NewWorkloadRefDiscoverer(nil)

	tests := []struct {
		name     string
		workload string
		want     bool
	}{
		{"has workloadRef", "my-workload", true},
		{"no workloadRef", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := makePodWithWorkloadRef("ns", tt.workload, "")

			if got := d.CanHandle(pod); got != tt.want {
				t.Errorf("CanHandle() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWorkloadRefDiscoverer_ExtractGangID(t *testing.T) {
	d := NewWorkloadRefDiscoverer(nil)

	tests := []struct {
		name     string
		ns       string
		workload string
		podGroup string
		want     string
	}{
		{"workload only", "ml", "train", "", "kubernetes-ml-train"},
		{"workload with podGroup", "ml", "train", "workers", "kubernetes-ml-train-workers"},
		{"no workloadRef", "ml", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := makePodWithWorkloadRef(tt.ns, tt.workload, tt.podGroup)

			if got := d.ExtractGangID(pod); got != tt.want {
				t.Errorf("ExtractGangID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorkloadRefDiscoverer_Name(t *testing.T) {
	d := NewWorkloadRefDiscoverer(nil)

	if got := d.Name(); got != "kubernetes" {
		t.Errorf("Name() = %q, want %q", got, "kubernetes")
	}
}
