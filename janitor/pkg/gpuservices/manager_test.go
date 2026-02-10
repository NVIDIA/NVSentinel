// Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gpuservices

import (
	"reflect"
	"testing"
	"time"
)

var customAppSpec = AppSpec{
	AppSelector:   map[string]string{"app": "custom-app"},
	NodeLabel:     "custom.com/deploy-app",
	EnabledValue:  "true",
	DisabledValue: "false",
}

var validCustomSpec = ManagerSpec{
	ManagerSelector: map[string]string{"app.kubernetes.io/managed-by": "custom-operator"},
	Namespace:       "custom-ns",
	Apps: []AppSpec{
		customAppSpec,
	},
	TeardownTimeout: 1 * time.Minute,
	RestoreTimeout:  2 * time.Minute,
}

func TestNewGPUServicesManager(t *testing.T) {
	manager, err := NewManager("gpu-operator", ManagerSpec{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	gpuOperatorSpec := manager.Spec

	testCases := []struct {
		name          string
		inputName     string
		inputSpec     ManagerSpec
		expected      Manager
		expectError   bool
		expectedError string
	}{
		{
			name:      "Should return no-op manager when Name is empty",
			inputName: "",
			inputSpec: ManagerSpec{
				Namespace: "should be ignored",
			},
			expected: Manager{
				Name: "",
				Spec: ManagerSpec{},
			},
			expectError: false,
		},
		{
			name:      "Should load full Spec from registry when Spec is empty",
			inputName: "gpu-operator",
			inputSpec: ManagerSpec{}, // Empty Spec triggers registry lookup
			expected: Manager{
				Name: "gpu-operator",
				Spec: gpuOperatorSpec,
			},
			expectError: false,
		},
		{
			name:      "Should use inline Spec when fully provided (prioritization/custom)",
			inputName: "custom-manager",
			inputSpec: validCustomSpec,
			expected: Manager{
				Name: "custom-manager",
				Spec: validCustomSpec,
			},
			expectError: false,
		},
		{
			name:          "Should return error for unknown Name when Spec is empty",
			inputName:     "non-existent-operator",
			inputSpec:     ManagerSpec{},
			expected:      Manager{},
			expectError:   true,
			expectedError: "GPU services manager 'non-existent-operator' not found in registry",
		},
		{
			name:      "Should fail custom validation if Namespace is missing but Spec is provided",
			inputName: "partial-custom",
			inputSpec: ManagerSpec{
				Apps: []AppSpec{{NodeLabel: "test"}},
				// Namespace is missing
			},
			expected:      Manager{},
			expectError:   true,
			expectedError: "GPU services manager Namespace must be set when providing a custom spec",
		},
		{
			name:      "Should fail custom validation if Apps list is empty but Spec is provided",
			inputName: "partial-custom",
			inputSpec: ManagerSpec{
				Namespace: "test-ns",
				// Apps is empty
			},
			expected:      Manager{},
			expectError:   true,
			expectedError: "GPU services manager Apps list must not be empty when providing a custom spec",
		},
		{
			name:      "Should fail custom validation if TeardownTimeout is negative",
			inputName: "partial-custom",
			inputSpec: ManagerSpec{
				Namespace:       "test-ns",
				Apps:            []AppSpec{{NodeLabel: "test"}},
				TeardownTimeout: -1 * time.Second,
				RestoreTimeout:  0,
			},
			expected:      Manager{},
			expectError:   true,
			expectedError: "GPU services manager TeardownTimeout must be a non-negative value",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := NewManager(tc.inputName, tc.inputSpec)

			if tc.expectError {
				if err == nil {
					t.Fatalf("Expected an error but got none")
				}
				if err != nil && err.Error() != tc.expectedError {
					t.Errorf("Expected error message '%s', but got '%s'", tc.expectedError, err.Error())
				}
				if !reflect.DeepEqual(result, tc.expected) {
					t.Errorf("Expected zero result %+v on error, but got %+v", tc.expected, result)
				}
				return
			}

			if err != nil {
				t.Fatalf("Did not expect an error, but got: %v", err)
			}

			if !reflect.DeepEqual(result, tc.expected) {
				t.Errorf("Result mismatch.\nExpected:\n%v\nGot:\n%v", tc.expected, result)
			}
		})
	}
}
