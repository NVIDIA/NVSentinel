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

package informers

import (
	"fmt"
	"regexp"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var cachedPolicyPod any

// BenchmarkExcludedPodTransform_PolicyLabelRetention measures cache-object allocation
// with a fixed source pod. It excludes API decoding and the source label strings.
func BenchmarkExcludedPodTransform_PolicyLabelRetention(b *testing.B) {
	pod := richDrainEligiblePod("workloads", "worker", "node-a")
	pod.Labels = make(map[string]string)
	var keys []string
	for index := range 50 {
		key := fmt.Sprintf("example.com/label-%02d", index)
		pod.Labels[key] = fmt.Sprintf("value-%02d", index)
		keys = append(keys, key)
	}
	systemPod := pod.DeepCopy()
	systemPod.Namespace = "kube-system"
	daemonPod := pod.DeepCopy()
	daemonPod.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet"}}
	for _, scenario := range []struct {
		name string
		pod  *v1.Pod
		keys []string
	}{
		{"no_policies", pod, nil},
		{"absent_keys", pod, []string{"absent-a", "absent-b", "absent-c"}},
		{"one_key", pod, keys[:1]},
		{"three_keys", pod, keys[:3]},
		{"ten_keys", pod, keys[:10]},
		{"system_no_policies", systemPod, nil},
		{"system_three_keys", systemPod, keys[:3]},
		{"daemonset_no_policies", daemonPod, nil},
		{"daemonset_three_keys", daemonPod, keys[:3]},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			transform := excludedPodTransform(regexp.MustCompile(`^kube-system$`), scenario.keys...)
			b.ReportAllocs()
			for b.Loop() {
				cached, err := transform(scenario.pod)
				if err != nil {
					b.Fatal(err)
				}
				cachedPolicyPod = cached
			}
		})
	}
	cachedPolicyPod = nil
}
