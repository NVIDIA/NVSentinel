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

package main

import (
	"context"
	"fmt"
	"testing"

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

func TestDriverWatcher_buildPatchData(t *testing.T) {
	tests := []struct {
		name        string
		shouldLabel bool
		expected    string
	}{
		{
			name:        "add label",
			shouldLabel: true,
			expected:    `{"metadata":{"labels":{"nvsentinel.dgxc.nvidia.com/driver.installed":"true"}}}`,
		},
		{
			name:        "remove label",
			shouldLabel: false,
			expected:    `{"metadata":{"labels":{"nvsentinel.dgxc.nvidia.com/driver.installed":null}}}`,
		},
	}

	watcher := &DriverWatcher{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := watcher.buildPatchData(tt.shouldLabel)
			assert.Equal(t, tt.expected, string(result))
		})
	}
}

func TestDriverWatcher_isNonRetryableError(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		shouldLabel bool
		expected    bool
	}{
		{
			name:        "not found error",
			err:         kerrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, "test-node"),
			shouldLabel: true,
			expected:    true,
		},
		{
			name:        "unprocessable entity when removing label",
			err:         &kerrors.StatusError{ErrStatus: metav1.Status{Code: 422, Reason: metav1.StatusReasonInvalid}},
			shouldLabel: false,
			expected:    true,
		},
		{
			name:        "unprocessable entity when adding label - should retry",
			err:         &kerrors.StatusError{ErrStatus: metav1.Status{Code: 422, Reason: metav1.StatusReasonInvalid}},
			shouldLabel: true,
			expected:    false,
		},
		{
			name:        "conflict error - should retry",
			err:         kerrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "test-node", nil),
			shouldLabel: true,
			expected:    false,
		},
		{
			name:        "generic error - should retry",
			err:         kerrors.NewInternalError(fmt.Errorf("internal server error")),
			shouldLabel: true,
			expected:    false,
		},
	}

	watcher := &DriverWatcher{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := watcher.isNonRetryableError(tt.err, tt.shouldLabel)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDriverWatcher_shouldSkipUpdate(t *testing.T) {
	tests := []struct {
		name        string
		nodeName    string
		shouldLabel bool
		node        *corev1.Node
		nodeExists  bool
		expected    bool
	}{
		{
			name:        "node not found - skip update",
			nodeName:    "missing-node",
			shouldLabel: true,
			nodeExists:  false,
			expected:    true,
		},
		{
			name:        "add label - node already has correct label",
			nodeName:    "test-node",
			shouldLabel: true,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{driverLabelKey: driverLabelValue},
				},
			},
			nodeExists: true,
			expected:   true,
		},
		{
			name:        "add label - node has different label value",
			nodeName:    "test-node",
			shouldLabel: true,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{driverLabelKey: "false"},
				},
			},
			nodeExists: true,
			expected:   false,
		},
		{
			name:        "add label - node has no label",
			nodeName:    "test-node",
			shouldLabel: true,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			nodeExists: true,
			expected:   false,
		},
		{
			name:        "remove label - node has no label",
			nodeName:    "test-node",
			shouldLabel: false,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{},
				},
			},
			nodeExists: true,
			expected:   true,
		},
		{
			name:        "remove label - node has the label",
			nodeName:    "test-node",
			shouldLabel: false,
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: map[string]string{driverLabelKey: driverLabelValue},
				},
			},
			nodeExists: true,
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset()
			if tt.nodeExists {
				clientset = fake.NewSimpleClientset(tt.node)
			}

			watcher := &DriverWatcher{
				clientset: clientset,
			}

			ctx := context.Background()
			result := watcher.shouldSkipUpdate(ctx, tt.nodeName, tt.shouldLabel)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDriverWatcher_handlePodEvent(t *testing.T) {
	tests := []struct {
		name          string
		pod           *corev1.Pod
		eventType     EventType
		initialState  map[string]bool
		expectedState map[string]bool
		expectUpdate  bool
		expectedLabel bool
	}{
		{
			name: "add ready pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
				Spec:       corev1.PodSpec{NodeName: "test-node"},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			eventType:     EventAdded,
			initialState:  map[string]bool{},
			expectedState: map[string]bool{"test-node": true},
			expectUpdate:  true,
			expectedLabel: true,
		},
		{
			name: "add not ready pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
				Spec:       corev1.PodSpec{NodeName: "test-node"},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionFalse},
					},
				},
			},
			eventType:     EventAdded,
			initialState:  map[string]bool{},
			expectedState: map[string]bool{"test-node": false},
			expectUpdate:  false, // No patch needed since label shouldn't be added for unready pod
			expectedLabel: false,
		},
		{
			name: "delete pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
				Spec:       corev1.PodSpec{NodeName: "test-node"},
			},
			eventType:     EventDeleted,
			initialState:  map[string]bool{"test-node": true},
			expectedState: map[string]bool{},
			expectUpdate:  true,
			expectedLabel: false,
		},
		{
			name: "pod with no node assignment",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
				Spec:       corev1.PodSpec{NodeName: ""},
			},
			eventType:     EventAdded,
			initialState:  map[string]bool{},
			expectedState: map[string]bool{},
			expectUpdate:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a node for testing
			nodeLabels := map[string]string{}
			if tt.eventType == "DELETED" && len(tt.initialState) > 0 {
				// For delete tests, start with the label present
				nodeLabels[driverLabelKey] = driverLabelValue
			}

			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: nodeLabels,
				},
			}
			clientset := fake.NewSimpleClientset(node)

			watcher := &DriverWatcher{
				clientset:  clientset,
				nodePodMap: make(map[string]bool),
			}

			// Set initial state
			for k, v := range tt.initialState {
				watcher.nodePodMap[k] = v
			}

			ctx := context.Background()
			watcher.handlePodEvent(ctx, tt.pod, tt.eventType)

			// Check final state
			assert.Equal(t, tt.expectedState, watcher.nodePodMap)

			// If we expect an update, verify the patch was called
			if tt.expectUpdate && tt.pod.Spec.NodeName != "" {
				actions := clientset.Actions()
				patchFound := false
				for _, action := range actions {
					if action.GetVerb() == "patch" && action.GetResource().Resource == "nodes" {
						patchFound = true
						break
					}
				}
				assert.True(t, patchFound, "Expected node patch to be called")
			}
		})
	}
}

// Integration test for updateNodeLabel with retry logic
func TestDriverWatcher_updateNodeLabel_RetryLogic(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node", Labels: map[string]string{}},
	}
	clientset := fake.NewSimpleClientset(node)

	// Setup client to fail first few attempts, then succeed
	callCount := 0
	clientset.PrependReactor("patch", "nodes", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		callCount++
		if callCount < 3 {
			return true, nil, kerrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "test-node", nil)
		}
		return false, nil, nil // Let the original handler run
	})

	watcher := &DriverWatcher{clientset: clientset}
	ctx := context.Background()

	err := watcher.updateNodeLabel(ctx, "test-node", true)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, callCount, 3, "Should have retried at least 3 times")
}
