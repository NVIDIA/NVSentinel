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
	"github.com/stretchr/testify/assert"
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

func TestRemoveGroupFromState(t *testing.T) {
	removedGroup := "gpu-timeout"
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
					  "maintenanceCR": "node-reboot-cr-789"
					}
				  }
				}`, removedGroup, notRemovedGroup),
			},
		},
	}

	client := fake.NewClientBuilder().WithObjects(node).Build()
	annotationManager := NodeAnnotationManager{
		client: client,
	}
	err := annotationManager.RemoveGroupFromState(context.TODO(), nodeName, removedGroup)
	assert.NoError(t, err)

	state, _, err := annotationManager.GetRemediationState(context.TODO(), nodeName)
	assert.NoError(t, err)

	assert.NotContains(t, state.EquivalenceGroups, removedGroup)
	assert.Contains(t, state.EquivalenceGroups, notRemovedGroup)
}
