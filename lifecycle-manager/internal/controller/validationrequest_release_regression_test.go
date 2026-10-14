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

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

func TestReleaseNodeFromSessionPreservesNewQuarantine(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
			Annotations: map[string]string{
				annotationValidationSession:       `[{"name":"old-validation","tests":["smoke-test"]}]`,
				annotationActiveValidationRequest: "old-validation",
				"quarantineHealthEvent":           `{"new-fatal-event":{}}`,
			},
		},
		Spec: corev1.NodeSpec{Unschedulable: true},
	}
	gate := &v1alpha1.SchedulingGateConfig{
		Cordon: v1alpha1.CordonConfig{Remove: true},
	}

	_, err := releaseNodeFromSession(node, "old-validation", nil, gate)
	if err != nil {
		t.Fatal(err)
	}
	if !node.Spec.Unschedulable {
		t.Fatal("old validation uncordoned a node with a new quarantineHealthEvent")
	}
}
