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

package annotation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRetryCountIncrementsOnUpdate(t *testing.T) {
	ctx := context.Background()
	nodeName := "test-node"
	groupName := "test-group"

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: map[string]string{},
		},
	}

	client := fake.NewClientBuilder().WithObjects(node).Build()
	annotationManager := NodeAnnotationManager{client: client}

	// First update - retry count should be 0
	err := annotationManager.UpdateRemediationState(ctx, nodeName, groupName, "cr-1", "RESTART_BM")
	require.NoError(t, err)

	state, _, err := annotationManager.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	assert.Equal(t, 0, state.EquivalenceGroups[groupName].RetryCount, "First attempt should have retry count 0")

	// Second update - retry count should be 1
	err = annotationManager.UpdateRemediationState(ctx, nodeName, groupName, "cr-2", "RESTART_BM")
	require.NoError(t, err)

	state, _, err = annotationManager.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	assert.Equal(t, 1, state.EquivalenceGroups[groupName].RetryCount, "Second attempt should have retry count 1")

	// Third update - retry count should be 2
	err = annotationManager.UpdateRemediationState(ctx, nodeName, groupName, "cr-3", "RESTART_BM")
	require.NoError(t, err)

	state, _, err = annotationManager.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	assert.Equal(t, 2, state.EquivalenceGroups[groupName].RetryCount, "Third attempt should have retry count 2")
}

func TestRetryCountIndependentPerGroup(t *testing.T) {
	ctx := context.Background()
	nodeName := "test-node"

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: map[string]string{},
		},
	}

	client := fake.NewClientBuilder().WithObjects(node).Build()
	annotationManager := NodeAnnotationManager{client: client}

	// Create group-1 twice
	err := annotationManager.UpdateRemediationState(ctx, nodeName, "group-1", "cr-1", "RESTART_BM")
	require.NoError(t, err)
	err = annotationManager.UpdateRemediationState(ctx, nodeName, "group-1", "cr-2", "RESTART_BM")
	require.NoError(t, err)

	// Create group-2 once
	err = annotationManager.UpdateRemediationState(ctx, nodeName, "group-2", "cr-3", "COMPONENT_RESET")
	require.NoError(t, err)

	state, _, err := annotationManager.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)

	assert.Equal(t, 1, state.EquivalenceGroups["group-1"].RetryCount, "group-1 should have retry count 1")
	assert.Equal(t, 0, state.EquivalenceGroups["group-2"].RetryCount, "group-2 should have retry count 0")
}

func TestRetryCountPersistsAcrossPodRestarts(t *testing.T) {
	ctx := context.Background()
	nodeName := "test-node"
	groupName := "test-group"

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: map[string]string{},
		},
	}

	client := fake.NewClientBuilder().WithObjects(node).Build()

	// Simulate first pod session
	manager1 := NodeAnnotationManager{client: client}
	err := manager1.UpdateRemediationState(ctx, nodeName, groupName, "cr-1", "RESTART_BM")
	require.NoError(t, err)

	// Simulate pod restart - create new annotation manager instance
	manager2 := NodeAnnotationManager{client: client}
	err = manager2.UpdateRemediationState(ctx, nodeName, groupName, "cr-2", "RESTART_BM")
	require.NoError(t, err)

	// Verify retry count persisted
	state, _, err := manager2.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	assert.Equal(t, 1, state.EquivalenceGroups[groupName].RetryCount,
		"Retry count should persist across pod restarts")
}
