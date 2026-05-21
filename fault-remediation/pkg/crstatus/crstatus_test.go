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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/nvidia/nvsentinel/fault-remediation/pkg/annotation"
)

func TestCheckCondition(t *testing.T) {
	completeConditionType := "Completed"

	checker := NewCRStatusChecker(nil, false)

	tests := []struct {
		name     string
		cr       *unstructured.Unstructured
		expected CRState
	}{
		{
			name: "no status returns skip - in progress",
			cr: &unstructured.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{"name": "test-cr"},
				},
			},
			expected: CRStateInProgress,
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
			expected: CRStateSucceeded,
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
			expected: CRStateFailed,
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
			expected: CRStateInProgress,
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
			expected: CRStateInProgress,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := checker.checkConditionType(tt.cr, completeConditionType)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetCRStateForReferenceUsesStoredReference(t *testing.T) {
	storedCR := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "stored.example.com/v9",
			"kind":       "StoredMaintenance",
			"metadata": map[string]any{
				"name":      "stored-cr",
				"namespace": "stored-namespace",
			},
			"status": map[string]any{
				"conditions": []any{
					map[string]any{
						"type":   "NodeReady",
						"status": "True",
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithObjects(storedCR).Build()
	checker := NewCRStatusChecker(fakeClient, false)

	state := checker.GetCRStateForReference(context.Background(), "stored-cr", annotation.MaintenanceResourceReference{
		ApiGroup:  "stored.example.com",
		Version:   "v9",
		Kind:      "StoredMaintenance",
		Namespace: "stored-namespace",
	}, "NodeReady")

	assert.Equal(t, CRStateSucceeded, state)
}
