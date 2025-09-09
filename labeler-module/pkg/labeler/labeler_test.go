// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestLabeler_handlePodEvent(t *testing.T) {
	tests := []struct {
		name                string
		pod                 *corev1.Pod
		existingPods        []*corev1.Pod
		existingNode        *corev1.Node
		expectedDCGMLabel   string
		expectedDriverLabel string
		expectError         bool
		expectUpdate        bool
		setupReactor        func(*fake.Clientset)
	}{
		{
			name: "DCGM 4.x new deployment adds version label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:4.1.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			expectedDCGMLabel:   "4.x",
			expectedDriverLabel: "",
			expectError:         false,
			expectUpdate:        true,
		},
		{
			name: "DCGM 3.x new deployment adds version label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:3.2.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			expectedDCGMLabel:   "3.x",
			expectedDriverLabel: "",
			expectError:         false,
			expectUpdate:        true,
		},
		{
			name: "DCGM pod with non-DCGM image new deployment does not add label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/other:1.0.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "",
			expectError:         false,
			expectUpdate:        false,
		},
		{
			name: "ready driver pod new deployment adds driver label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "driver-pod",
					Labels: map[string]string{"app": "nvidia-driver-daemonset"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			existingPods: []*corev1.Pod{},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "true",
			expectError:         false,
			expectUpdate:        true,
		},
		{
			name: "not ready driver pod new deployment does not add label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "driver-pod",
					Labels: map[string]string{"app": "nvidia-driver-daemonset"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionFalse},
					},
				},
			},
			existingPods: []*corev1.Pod{},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "",
			expectError:         false,
			expectUpdate:        false,
		},
		{
			name: "both DCGM and driver pods new deployment add both labels",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:3.2.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "driver-pod",
						Labels: map[string]string{"app": "nvidia-driver-daemonset"},
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{Type: corev1.PodReady, Status: corev1.ConditionTrue},
						},
					},
				},
			},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			expectedDCGMLabel:   "3.x",
			expectedDriverLabel: "true",
			expectError:         false,
			expectUpdate:        true,
		},
		{
			name: "node already has correct labels redeployment no update needed",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:4.1.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "dcgm-pod",
						Labels: map[string]string{"app": "nvidia-dcgm"},
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
						Containers: []corev1.Container{
							{
								Name:  "dcgm",
								Image: "nvcr.io/nvidia/dcgm:4.1.0",
							},
						},
					},
				},
			},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DCGMVersionLabel:     "4.x",
						DriverInstalledLabel: "",
					},
				},
			},
			expectedDCGMLabel:   "4.x",
			expectedDriverLabel: "",
			expectError:         false,
			expectUpdate:        false,
		},
		{
			name: "pod with no node assignment new deployment fails",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "",
				},
			},
			existingPods: []*corev1.Pod{},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			expectError:  true,
			expectUpdate: false,
		},
		{
			name: "node not found new deployment fails gracefully",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "non-existent-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:4.1.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{},
			existingNode: nil,
			expectError:  true,
			expectUpdate: false,
		},
		{
			name: "DCGM upgrade from 3.x to 4.x updates label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:4.2.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "dcgm-pod",
						Labels: map[string]string{"app": "nvidia-dcgm"},
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
						Containers: []corev1.Container{
							{
								Name:  "dcgm",
								Image: "nvcr.io/nvidia/dcgm:3.1.0",
							},
						},
					},
				},
			},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DCGMVersionLabel: "3.x",
					},
				},
			},
			expectedDCGMLabel:   "4.x",
			expectedDriverLabel: "",
			expectError:         false,
			expectUpdate:        true,
		},
		{
			name: "DCGM downgrade from 4.x to 3.x updates label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:3.3.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "dcgm-pod",
						Labels: map[string]string{"app": "nvidia-dcgm"},
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
						Containers: []corev1.Container{
							{
								Name:  "dcgm",
								Image: "nvcr.io/nvidia/dcgm:4.1.0",
							},
						},
					},
				},
			},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DCGMVersionLabel: "4.x",
					},
				},
			},
			expectedDCGMLabel:   "3.x",
			expectedDriverLabel: "",
			expectError:         false,
			expectUpdate:        true,
		},
		{
			name: "driver pod becomes not ready removes label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "driver-pod",
					Labels: map[string]string{"app": "nvidia-driver-daemonset"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionFalse},
					},
				},
			},
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "driver-pod",
						Labels: map[string]string{"app": "nvidia-driver-daemonset"},
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{Type: corev1.PodReady, Status: corev1.ConditionTrue},
						},
					},
				},
			},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DriverInstalledLabel: "true",
					},
				},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "",
			expectError:         false,
			expectUpdate:        true,
		},
		{
			name: "retry logic new deployment handles conflict errors gracefully",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dcgm-pod",
					Labels: map[string]string{"app": "nvidia-dcgm"},
				},
				Spec: corev1.PodSpec{
					NodeName: "test-node",
					Containers: []corev1.Container{
						{
							Name:  "dcgm",
							Image: "nvcr.io/nvidia/dcgm:4.1.0",
						},
					},
				},
			},
			existingPods: []*corev1.Pod{},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			expectedDCGMLabel:   "4.x",
			expectedDriverLabel: "",
			expectError:         false,
			expectUpdate:        true,
			setupReactor: func(clientset *fake.Clientset) {
				callCount := 0
				clientset.PrependReactor("update", "nodes", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
					callCount++
					if callCount < 3 {
						return true, nil, kerrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "test-node", nil)
					}
					return false, nil, nil
				})
			},
		},
		{
			name: "DCGM pod deletion removes version label",
			pod:  nil,
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "dcgm-pod",
						Labels: map[string]string{"app": "nvidia-dcgm"},
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
						Containers: []corev1.Container{
							{
								Name:  "dcgm",
								Image: "nvcr.io/nvidia/dcgm:4.1.0",
							},
						},
					},
				},
			},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DCGMVersionLabel: "4.x",
					},
				},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "",
			expectError:         false,
			expectUpdate:        true,
		},
		{
			name: "driver pod deletion removes driver label",
			pod:  nil,
			existingPods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "driver-pod",
						Labels: map[string]string{"app": "nvidia-driver-daemonset"},
					},
					Spec: corev1.PodSpec{
						NodeName: "test-node",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{Type: corev1.PodReady, Status: corev1.ConditionTrue},
						},
					},
				},
			},
			existingNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					Labels: map[string]string{
						DriverInstalledLabel: "true",
					},
				},
			},
			expectedDCGMLabel:   "",
			expectedDriverLabel: "",
			expectError:         false,
			expectUpdate:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := []runtime.Object{}
			if tt.existingNode != nil {
				objects = append(objects, tt.existingNode)
			}
			for _, pod := range tt.existingPods {
				objects = append(objects, pod)
			}

			clientset := fake.NewSimpleClientset(objects...)

			if tt.setupReactor != nil {
				tt.setupReactor(clientset)
			}

			labeler, err := NewLabeler(clientset, time.Minute, "nvidia-dcgm", "nvidia-driver-daemonset")
			require.NoError(t, err)

			for _, pod := range tt.existingPods {
				err := labeler.informer.GetIndexer().Add(pod)
				require.NoError(t, err)
			}

			// First, process events for existing pods to establish initial state
			for _, existingPod := range tt.existingPods {
				err = labeler.handlePodEvent(existingPod)
				if err != nil {
					t.Logf("Initial pod event failed: %v", err)
				}
			}

			// Update cache with new pod state to simulate transition
			if tt.pod != nil {
				err = labeler.informer.GetIndexer().Update(tt.pod)
				require.NoError(t, err)
				// Process the new pod event (the main test)
				err = labeler.handlePodEvent(tt.pod)
			} else {
				// For deletion scenarios, remove all existing pods and trigger reconciliation
				for _, existingPod := range tt.existingPods {
					err = labeler.informer.GetIndexer().Delete(existingPod)
					require.NoError(t, err)
					// Process deletion event by calling with the deleted pod
					err = labeler.handlePodEvent(existingPod)
				}
			}

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)

			if tt.expectUpdate && tt.existingNode != nil {
				actions := clientset.Actions()
				updateFound := false
				for _, action := range actions {
					if action.GetVerb() == "update" && action.GetResource().Resource == "nodes" {
						updateFound = true
						break
					}
				}
				assert.True(t, updateFound, "Expected node update to be called")

				updatedNode, err := clientset.CoreV1().Nodes().Get(context.Background(), tt.existingNode.Name, metav1.GetOptions{})
				require.NoError(t, err)

				if tt.expectedDCGMLabel != "" {
					assert.Equal(t, tt.expectedDCGMLabel, updatedNode.Labels[DCGMVersionLabel])
				} else {
					_, exists := updatedNode.Labels[DCGMVersionLabel]
					assert.False(t, exists, "DCGM version label should not exist")
				}

				if tt.expectedDriverLabel != "" {
					assert.Equal(t, tt.expectedDriverLabel, updatedNode.Labels[DriverInstalledLabel])
				} else {
					_, exists := updatedNode.Labels[DriverInstalledLabel]
					assert.False(t, exists, "Driver installed label should not exist")
				}
			} else if !tt.expectUpdate {
				actions := clientset.Actions()
				for _, action := range actions {
					assert.NotEqual(t, "update", action.GetVerb(), "Did not expect node update to be called")
				}
			}
		})
	}
}
