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

package config

import (
	"strings"
	"testing"
)

func TestTomlConfig_Validate(t *testing.T) {
	tests := []struct {
		name        string
		config      TomlConfig
		expectError bool
		errorSubstr string
	}{
		{
			name: "valid config with matching templates",
			config: TomlConfig{
				RemediationActions: map[string]MaintenanceResource{
					"RESTART_BM": {
						TemplateFile: "atlas-reboot.yaml",
						Scope:        "Cluster",
					},
					"COMPONENT_RESET": {
						TemplateFile: "gpu-reset.yaml",
						Scope:        "Namespaced",
						Namespace:    "nvidia-gpu-operator",
					},
				},
				Templates: map[string]string{
					"atlas-reboot.yaml": "apiVersion: ...",
					"gpu-reset.yaml":    "apiVersion: ...",
				},
			},
			expectError: false,
		},
		{
			name: "missing template file reference",
			config: TomlConfig{
				RemediationActions: map[string]MaintenanceResource{
					"RESTART_BM": {
						TemplateFile: "missing-template.yaml",
						Scope:        "Cluster",
					},
				},
				Templates: map[string]string{
					"existing-template.yaml": "apiVersion: ...",
				},
			},
			expectError: true,
			errorSubstr: "references template file 'missing-template.yaml' which does not exist",
		},
		{
			name: "invalid scope value",
			config: TomlConfig{
				RemediationActions: map[string]MaintenanceResource{
					"RESTART_BM": {
						TemplateFile: "template.yaml",
						Scope:        "Invalid",
					},
				},
				Templates: map[string]string{
					"template.yaml": "apiVersion: ...",
				},
			},
			expectError: true,
			errorSubstr: "invalid scope 'Invalid'",
		},
		{
			name: "namespaced scope without namespace",
			config: TomlConfig{
				RemediationActions: map[string]MaintenanceResource{
					"COMPONENT_RESET": {
						TemplateFile: "template.yaml",
						Scope:        "Namespaced",
						Namespace:    "", // Missing namespace
					},
				},
				Templates: map[string]string{
					"template.yaml": "apiVersion: ...",
				},
			},
			expectError: true,
			errorSubstr: "is Namespaced but no namespace is specified",
		},
		{
			name: "empty template file reference should be allowed",
			config: TomlConfig{
				RemediationActions: map[string]MaintenanceResource{
					"RESTART_BM": {
						TemplateFile: "", // Empty is OK
						Scope:        "Cluster",
					},
				},
				Templates: map[string]string{
					"template.yaml": "apiVersion: ...",
				},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected validation error but got none")
					return
				}
				if !strings.Contains(err.Error(), tt.errorSubstr) {
					t.Errorf("Expected error to contain '%s' but got: %v", tt.errorSubstr, err)
				}
			} else {
				if err != nil {
					t.Errorf("Expected no validation error but got: %v", err)
				}
			}
		})
	}
}