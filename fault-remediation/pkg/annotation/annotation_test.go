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
	"fmt"
	"sync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

func TestGetRemediationState(t *testing.T) {
	ctx := context.Background()
	nodeName := "test-node"
	now := time.Now()

	tests := []struct {
		name          string
		node          *corev1.Node
		expectError   bool
		expectedState RemediationStateAnnotation
	}{
		{
			name:        "Node not found",
			expectError: true,
		},
		{
			name: "Node has no annotation, returns node and empty state",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{},
				},
			},
		},
		{
			name: "Node has bad annotation, returns node and empty state",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Annotations: map[string]string{
						AnnotationKey: "asdfasdf",
					},
				},
			},
		},
		{
			name: "Node has annotation, returns node and correct state",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Annotations: map[string]string{
						AnnotationKey: fmt.Sprintf(`{
						  "equivalenceGroups": {
							"gpu-timeout": {
							  "maintenanceCR": "gpu-maintenance-abc123",
							  "createdAt": "%s",
                              "actionName": "testAction1"
							},
							"node-reboot": {
							  "maintenanceCR": "node-reboot-cr-789",
							  "createdAt": "%s",
                              "actionName": "testAction2"
							}
						  }
						}`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)),
					},
				},
			},
			expectedState: RemediationStateAnnotation{
				EquivalenceGroups: map[string]EquivalenceGroupState{
					"gpu-timeout": {
						MaintenanceCR: "gpu-maintenance-abc123",
						CreatedAt:     now,
						ActionName:    "testAction1",
					},
					"node-reboot": {
						MaintenanceCR: "node-reboot-cr-789",
						CreatedAt:     now,
						ActionName:    "testAction2",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder()

			if tt.node != nil {
				builder.WithObjects(tt.node)
			}

			client := builder.Build()

			annotationManager := NodeAnnotationManager{
				client: client,
			}

			resultState, node, err := annotationManager.GetRemediationState(ctx, nodeName)
			if tt.expectError {
				assert.Error(t, err)
				return
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.node.Name, node.Name)
				assert.Equal(t, len(tt.expectedState.EquivalenceGroups), len(resultState.EquivalenceGroups))
				for expectedKey, expectedValue := range tt.expectedState.EquivalenceGroups {
					assert.Equal(t, expectedValue.MaintenanceCR, resultState.EquivalenceGroups[expectedKey].MaintenanceCR)
					assert.Equal(t, expectedValue.ActionName, resultState.EquivalenceGroups[expectedKey].ActionName)
					assert.Equal(t, expectedValue.CreatedAt.Unix(), resultState.EquivalenceGroups[expectedKey].CreatedAt.Unix())
				}
			}
		})
	}
}

func TestUpdateRemediationState(t *testing.T) {
	group := "test"
	crName := "reboot"
	nodeName := "node"
	actionName := "reboot-action"
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: map[string]string{},
		},
	}
	client := fake.NewClientBuilder().WithObjects(node).Build()
	annotationManager := NodeAnnotationManager{
		client: client,
	}
	err := annotationManager.UpdateRemediationState(context.TODO(), nodeName, group, crName, actionName)
	assert.NoError(t, err)

	state, _, err := annotationManager.GetRemediationState(context.TODO(), nodeName)
	assert.NoError(t, err)

	assert.Contains(t, state.EquivalenceGroups, group)
	assert.Equal(t, crName, state.EquivalenceGroups[group].MaintenanceCR)
	assert.Equal(t, actionName, state.EquivalenceGroups[group].ActionName)
}

func TestClearRemediationState(t *testing.T) {
	nodeName := "node"
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				AnnotationKey: "test",
			},
		},
	}
	client := fake.NewClientBuilder().WithObjects(node).Build()
	annotationManager := NodeAnnotationManager{
		client: client,
	}
	err := annotationManager.ClearRemediationState(context.TODO(), nodeName)
	assert.NoError(t, err)

	err = client.Get(context.TODO(), types.NamespacedName{
		Name: nodeName,
	}, node)
	assert.NoError(t, err)

	assert.NotContains(t, node.Annotations, AnnotationKey)
}

func TestRemoveGroupsFromState(t *testing.T) {
	removedGroup1 := "gpu-timeout"
	removedGroup2 := "component-reset"
	notRemovedGroup := "node-reboot"
	nodeName := "node"
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				AnnotationKey: fmt.Sprintf(`{
				  "equivalenceGroups": {
					"%s": {
					  "maintenanceCR": "gpu-maintenance-abc123"
					},
					"%s": {
					  "maintenanceCR": "component-reset-cr-456"
					},
					"%s": {
					  "maintenanceCR": "node-reboot-cr-789"
					}
				  }
				}`, removedGroup1, removedGroup2, notRemovedGroup),
			},
		},
	}

	client := fake.NewClientBuilder().WithObjects(node).Build()
	annotationManager := NodeAnnotationManager{
		client: client,
	}
	err := annotationManager.RemoveGroupsFromState(context.TODO(), nodeName, []string{removedGroup1, removedGroup2})
	assert.NoError(t, err)

	state, _, err := annotationManager.GetRemediationState(context.TODO(), nodeName)
	assert.NoError(t, err)

	assert.NotContains(t, state.EquivalenceGroups, removedGroup1)
	assert.NotContains(t, state.EquivalenceGroups, removedGroup2)
	assert.Contains(t, state.EquivalenceGroups, notRemovedGroup)
}

func TestConcurrentRemoveGroupsFromState(t *testing.T) {
	nodeName := "test-node"
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				AnnotationKey: fmt.Sprintf(`{
				  "equivalenceGroups": {
					"%s": {"maintenanceCR": "cr-0", "actionName": "action-0"},
					"%s": {"maintenanceCR": "cr-1", "actionName": "action-1"},
					"%s": {"maintenanceCR": "cr-2", "actionName": "action-2"},
					"%s": {"maintenanceCR": "cr-3", "actionName": "action-3"},
					"%s": {"maintenanceCR": "cr-4", "actionName": "action-4"},
					"%s": {"maintenanceCR": "cr-5", "actionName": "action-5"}
				  }
				}`, "group-0", "group-1", "group-2", "group-3", "group-4", "group-5"),
			},
		},
	}

	client := fake.NewClientBuilder().WithObjects(node).Build()
	annotationManager := NodeAnnotationManager{client: client}

	var wg sync.WaitGroup
	for i := 0; i < 6; i += 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_ = annotationManager.RemoveGroupsFromState(context.TODO(), nodeName, []string{fmt.Sprintf("group-%d", idx), fmt.Sprintf("group-%d", idx+1)})
		}(i)
	}
	wg.Wait()

	state, _, err := annotationManager.GetRemediationState(context.TODO(), nodeName)
	require.NoError(t, err)

	assert.Empty(t, state.EquivalenceGroups,
		"all groups should be removed, but %d survived due to concurrent read-modify-write race",
		len(state.EquivalenceGroups))
}

func TestConcurrentUpdateAndRemoveGroupsFromState(t *testing.T) {
	nodeName := "test-node"
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				AnnotationKey: `{
				  "equivalenceGroups": {
					"existing-group-1": {"maintenanceCR": "old-cr-1", "actionName": "RESTART_BM"},
					"existing-group-2": {"maintenanceCR": "old-cr-2", "actionName": "COMPONENT_RESET"}
				  }
				}`,
			},
		},
	}

	client := fake.NewClientBuilder().WithObjects(node).Build()
	annotationManager := NodeAnnotationManager{client: client}

	const iterations = 100
	for i := 0; i < iterations; i++ {
		// Reset state each iteration
		err := annotationManager.UpdateRemediationState(context.TODO(), nodeName, "existing-group-1", "old-cr-1", "RESTART_BM")
		require.NoError(t, err)
		err = annotationManager.UpdateRemediationState(context.TODO(), nodeName, "existing-group-2", "old-cr-2", "COMPONENT_RESET")
		require.NoError(t, err)

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			_ = annotationManager.RemoveGroupsFromState(context.TODO(), nodeName, []string{"existing-group-1", "existing-group-2"})
		}()

		go func() {
			defer wg.Done()
			_ = annotationManager.UpdateRemediationState(context.TODO(), nodeName, "new-group", "new-cr", "COMPONENT_RESET")
		}()

		wg.Wait()

		state, _, err := annotationManager.GetRemediationState(context.TODO(), nodeName)
		require.NoError(t, err)

		_, existing1Present := state.EquivalenceGroups["existing-group-1"]
		_, existing2Present := state.EquivalenceGroups["existing-group-2"]
		_, newPresent := state.EquivalenceGroups["new-group"]

		if existing1Present || existing2Present || !newPresent {
			t.Errorf("race condition on iteration %d: existing-group-1 present=%v, existing-group-2 present=%v, new-group present=%v",
				i, existing1Present, existing2Present, newPresent)
			return
		}

		// Cleanup for next iteration
		_ = annotationManager.RemoveGroupsFromState(context.TODO(), nodeName, []string{"existing-group-1", "existing-group-2", "new-group"})
	}
}
