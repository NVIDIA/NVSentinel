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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
)

func TestCheckCondition(t *testing.T) {
	testResource := config.MaintenanceResource{
		CompleteConditionType: "Completed",
	}
	cfg := map[string]config.MaintenanceResource{
		"test": testResource,
	}

	checker := NewCRStatusChecker(nil, cfg, false)

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
			result := checker.checkCondition(tt.cr, testResource)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetCRState_GetError(t *testing.T) {
	cfg := map[string]config.MaintenanceResource{
		"reboot": {
			ApiGroup:              "janitor.dgxc.nvidia.com",
			Version:               "v1alpha1",
			Kind:                  "RebootNode",
			CompleteConditionType: "NodeReady",
		},
	}

	tests := []struct {
		name       string
		getErr     error
		expected   CRState
		expectSkip bool
	}{
		{
			name:     "not found allows create",
			getErr:   apierrors.NewNotFound(schema.GroupResource{Group: "janitor.dgxc.nvidia.com", Resource: "rebootnodes"}, "maintenance-node-1"),
			expected: CRStateNotFound,
		},
		{
			name:       "timeout keeps the CR in progress",
			getErr:     apierrors.NewTimeoutError("etcdserver: request timed out", 1),
			expected:   CRStateInProgress,
			expectSkip: true,
		},
		{
			name:       "forbidden keeps the CR in progress",
			getErr:     apierrors.NewForbidden(schema.GroupResource{Group: "janitor.dgxc.nvidia.com", Resource: "rebootnodes"}, "maintenance-node-1", errors.New("denied")),
			expected:   CRStateInProgress,
			expectSkip: true,
		},
		{
			name:       "connection error keeps the CR in progress",
			getErr:     errors.New("dial tcp 10.0.0.1:443: connect: connection refused"),
			expected:   CRStateInProgress,
			expectSkip: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return tt.getErr
				},
			}).Build()
			checker := NewCRStatusChecker(c, cfg, false)

			assert.Equal(t, tt.expected, checker.GetCRState(context.Background(), "reboot", "maintenance-node-1"))
			assert.Equal(t, tt.expectSkip, checker.ShouldSkipCRCreation(context.Background(), "reboot", "maintenance-node-1"))
		})
	}
}
