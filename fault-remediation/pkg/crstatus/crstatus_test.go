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
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
)

func TestCheckCondition(t *testing.T) {
	testResource := config.MaintenanceResource{
		CompleteConditionType: "Completed",
	}
	cfg := map[string]config.MaintenanceResource{
		"test": testResource,
	}
	checker := NewCRStatusChecker(nil, nil, cfg, false)

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
