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

package crstatus

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
)

func TestCheckCondition(t *testing.T) {
	testResource := config.MaintenanceResource{
		CompleteConditionType: "Completed",
	}
	cfg := &config.TomlConfig{
		RemediationActions: map[string]config.MaintenanceResource{
			"test": testResource,
		},
	}

	checker := NewCRStatusChecker(nil, cfg, false)

	tests := []struct {
		name     string
		cr       *unstructured.Unstructured
		expected bool
	}{
		{
			name: "no status returns skip - in progress",
			cr: &unstructured.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{"name": "test-cr"},
				},
			},
			expected: true,
		},
		{
			name: "condition true returns allow create - success",
			cr: &unstructured.Unstructured{
				Object: map[string]any{
					"status": map[string]any{
						"conditions": []any{
							map[string]any{
								"type":   "Completed",
								"status": "True",
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "condition false returns allow create - failed",
			cr: &unstructured.Unstructured{
				Object: map[string]any{
					"status": map[string]any{
						"conditions": []any{
							map[string]any{
								"type":   "Completed",
								"status": "False",
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "condition unknown returns skip - in progress",
			cr: &unstructured.Unstructured{
				Object: map[string]any{
					"status": map[string]any{
						"conditions": []any{
							map[string]any{
								"type":   "Completed",
								"status": "Unknown",
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "condition not found returns skip - in progress",
			cr: &unstructured.Unstructured{
				Object: map[string]any{
					"status": map[string]any{
						"conditions": []any{
							map[string]any{
								"type":   "SomeOtherCondition",
								"status": "True",
							},
						},
					},
				},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := checker.checkCondition(tt.cr, testResource)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestShouldSkipCRCreation_DoesNotGuessAcrossOtherComponentOverrides(t *testing.T) {
	scheme := runtime.NewScheme()

	existingCR := &unstructured.Unstructured{}
	existingCR.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "janitor.dgxc.nvidia.com",
		Version: "v1alpha1",
		Kind:    "GPUReset",
	})
	existingCR.SetName("maintenance-node-1-event-1")
	existingCR.Object["status"] = map[string]any{
		"conditions": []any{
			map[string]any{
				"type":   "Complete",
				"status": "Unknown",
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingCR).Build()
	cfg := &config.TomlConfig{
		ComponentRemediationActions: config.ComponentRemediationActions{
			"GPU": {
				"COMPONENT_RESET": {
					ApiGroup:              "janitor.dgxc.nvidia.com",
					Version:               "v1alpha1",
					Kind:                  "GPUReset",
					CompleteConditionType: "Complete",
				},
			},
		},
	}

	checker := NewCRStatusChecker(client, cfg, false)

	shouldSkip := checker.ShouldSkipCRCreation(context.Background(), "LPU", "COMPONENT_RESET", "maintenance-node-1-event-1")

	assert.False(t, shouldSkip, "expected checker not to guess across other component overrides")
}

func TestShouldSkipCRCreation_FallsBackToSharedAction(t *testing.T) {
	scheme := runtime.NewScheme()

	existingCR := &unstructured.Unstructured{}
	existingCR.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "janitor.dgxc.nvidia.com",
		Version: "v1alpha1",
		Kind:    "RebootNode",
	})
	existingCR.SetName("maintenance-node-2-event-1")
	existingCR.Object["status"] = map[string]any{
		"conditions": []any{
			map[string]any{
				"type":   "NodeReady",
				"status": "Unknown",
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingCR).Build()
	cfg := &config.TomlConfig{
		RemediationActions: map[string]config.MaintenanceResource{
			"COMPONENT_RESET": {
				ApiGroup:              "janitor.dgxc.nvidia.com",
				Version:               "v1alpha1",
				Kind:                  "RebootNode",
				CompleteConditionType: "NodeReady",
			},
		},
		ComponentRemediationActions: config.ComponentRemediationActions{
			"GPU": {
				"COMPONENT_RESET": {
					ApiGroup:              "janitor.dgxc.nvidia.com",
					Version:               "v1alpha1",
					Kind:                  "GPUReset",
					CompleteConditionType: "Complete",
				},
			},
		},
	}

	checker := NewCRStatusChecker(client, cfg, false)

	shouldSkip := checker.ShouldSkipCRCreation(context.Background(), "LPU", "COMPONENT_RESET", "maintenance-node-2-event-1")

	assert.True(t, shouldSkip, "expected checker to fall back to the shared action mapping")
}

func TestShouldSkipCRCreation_EmptyStoredComponentClassTreatsAmbiguousActionAsStale(t *testing.T) {
	scheme := runtime.NewScheme()

	existingCR := &unstructured.Unstructured{}
	existingCR.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "janitor.dgxc.nvidia.com",
		Version: "v1alpha1",
		Kind:    "GPUReset",
	})
	existingCR.SetName("maintenance-node-3-event-1")
	existingCR.Object["status"] = map[string]any{
		"conditions": []any{
			map[string]any{
				"type":   "Complete",
				"status": "Unknown",
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingCR).Build()
	cfg := &config.TomlConfig{
		ComponentRemediationActions: config.ComponentRemediationActions{
			"GPU": {
				"COMPONENT_RESET": {
					ApiGroup:              "janitor.dgxc.nvidia.com",
					Version:               "v1alpha1",
					Kind:                  "GPUReset",
					CompleteConditionType: "Complete",
				},
			},
			"LPU": {
				"COMPONENT_RESET": {
					ApiGroup:              "janitor.dgxc.nvidia.com",
					Version:               "v1alpha1",
					Kind:                  "LPURemediation",
					CompleteConditionType: "Complete",
				},
			},
		},
	}

	checker := NewCRStatusChecker(client, cfg, false)

	shouldSkip := checker.ShouldSkipCRCreation(context.Background(), "", "COMPONENT_RESET", "maintenance-node-3-event-1")

	assert.False(t, shouldSkip, "expected empty stored component class to be treated as stale when action is ambiguous")
}
