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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTomlConfig_Validate(t *testing.T) {
	// Create temp directory for template files
	tempDir, err := os.MkdirTemp("", "config-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	defer os.RemoveAll(tempDir)

	// Create template files for valid test cases
	templateFiles := []string{"template-a.yaml", "template-b.yaml", "template.yaml"}
	for _, f := range templateFiles {
		if err := os.WriteFile(filepath.Join(tempDir, f), []byte("apiVersion: v1"), 0644); err != nil {
			t.Fatalf("Failed to create template file %s: %v", f, err)
		}
	}

	tests := []struct {
		name        string
		config      TomlConfig
		expectError bool
		errorSubstr string
	}{
		{
			name: "valid config with matching templates",
			config: TomlConfig{
				Template: Template{MountPath: tempDir},
				RemediationActions: map[string]MaintenanceResource{
					"ACTION_A": {
						TemplateFileName: "template-a.yaml",
						Scope:            "Cluster",
						EquivalenceGroup: "restart",
					},
					"COMPONENT_RESET": {
						TemplateFileName:             "template-b.yaml",
						Scope:                        "Namespaced",
						Namespace:                    "test-namespace",
						EquivalenceGroup:             "reset",
						SupersedingEquivalenceGroups: []string{"restart"},
						ImpactedEntityScope:          "GPU_UUID",
					},
				},
			},
			expectError: false,
		},
		{
			name: "missing template file reference",
			config: TomlConfig{
				Template: Template{MountPath: tempDir},
				RemediationActions: map[string]MaintenanceResource{
					"ACTION_A": {
						TemplateFileName: "missing-template.yaml",
						Scope:            "Cluster",
						EquivalenceGroup: "restart",
					},
				},
			},
			expectError: true,
			errorSubstr: "references template file 'missing-template.yaml' which does not exist",
		},
		{
			name: "invalid scope value",
			config: TomlConfig{
				Template: Template{MountPath: tempDir},
				RemediationActions: map[string]MaintenanceResource{
					"ACTION_A": {
						TemplateFileName: "template.yaml",
						Scope:            "Invalid",
						EquivalenceGroup: "restart",
					},
				},
			},
			expectError: true,
			errorSubstr: "invalid scope 'Invalid'",
		},
		{
			name: "namespaced scope without namespace",
			config: TomlConfig{
				Template: Template{MountPath: tempDir},
				RemediationActions: map[string]MaintenanceResource{
					"ACTION_A": {
						TemplateFileName: "template.yaml",
						Scope:            "Namespaced",
						Namespace:        "", // Missing namespace
						EquivalenceGroup: "restart",
					},
				},
			},
			expectError: true,
			errorSubstr: "is Namespaced but no namespace is specified",
		},
		{
			name: "empty template file reference should be rejected",
			config: TomlConfig{
				Template: Template{MountPath: tempDir},
				RemediationActions: map[string]MaintenanceResource{
					"ACTION_A": {
						TemplateFileName: "", // Empty should fail
						Scope:            "Cluster",
						EquivalenceGroup: "restart",
					},
				},
			},
			expectError: true,
			errorSubstr: "must have a non-empty templateFileName",
		},
		{
			name: "empty EquivalenceGroup should be rejected",
			config: TomlConfig{
				Template: Template{MountPath: tempDir},
				RemediationActions: map[string]MaintenanceResource{
					"ACTION_A": {
						TemplateFileName: "template-a.yaml",
						Scope:            "Cluster",
						EquivalenceGroup: "",
					},
				},
			},
			expectError: true,
		},
		{
			name: "missing SupersedingEquivalenceGroup should be rejected",
			config: TomlConfig{
				Template: Template{MountPath: tempDir},
				RemediationActions: map[string]MaintenanceResource{
					"ACTION_A": {
						TemplateFileName:             "template-a.yaml",
						Scope:                        "Cluster",
						EquivalenceGroup:             "reset",
						SupersedingEquivalenceGroups: []string{"restart"},
					},
				},
			},
			expectError: true,
		},
		{
			name: "SupersedingEquivalenceGroup cannot include the EquivalenceGroup",
			config: TomlConfig{
				Template: Template{MountPath: tempDir},
				RemediationActions: map[string]MaintenanceResource{
					"COMPONENT_RESET": {
						TemplateFileName:             "template-b.yaml",
						Scope:                        "Namespaced",
						Namespace:                    "test-namespace",
						EquivalenceGroup:             "reset",
						SupersedingEquivalenceGroups: []string{"restart", "reset"},
						ImpactedEntityScope:          "GPU_UUID",
					},
				},
			},
			expectError: true,
		},
		{
			name: "Non-supported ImpactedEntityScope should be rejected",
			config: TomlConfig{
				Template: Template{MountPath: tempDir},
				RemediationActions: map[string]MaintenanceResource{
					"COMPONENT_RESET": {
						TemplateFileName:    "template-a.yaml",
						Scope:               "Cluster",
						EquivalenceGroup:    "reset",
						ImpactedEntityScope: "PCI",
					},
				},
			},
			expectError: true,
		},
		{
			name: "SupersedingEquivalenceGroup cannot have an ImpactedEntityScope",
			config: TomlConfig{
				Template: Template{MountPath: tempDir},
				RemediationActions: map[string]MaintenanceResource{
					"ACTION_A": {
						TemplateFileName:    "template-a.yaml",
						Scope:               "Cluster",
						EquivalenceGroup:    "restart",
						ImpactedEntityScope: "PCI",
					},
					"ACTION_B": {
						TemplateFileName:             "template-b.yaml",
						Scope:                        "Namespaced",
						Namespace:                    "test-namespace",
						EquivalenceGroup:             "reset",
						SupersedingEquivalenceGroups: []string{"restart"},
						ImpactedEntityScope:          "GPU_UUID",
					},
				},
			},
			expectError: true,
		},
		{
			name: "SupersedingEquivalenceGroup cannot have an ImpactedEntityScope",
			config: TomlConfig{
				Template: Template{MountPath: tempDir},
				RemediationActions: map[string]MaintenanceResource{
					"ACTION_A": {
						TemplateFileName:    "template-a.yaml",
						Scope:               "Cluster",
						EquivalenceGroup:    "restart",
						ImpactedEntityScope: "PCI",
					},
					"COMPONENT_RESET": {
						TemplateFileName:             "template-b.yaml",
						Scope:                        "Namespaced",
						Namespace:                    "test-namespace",
						EquivalenceGroup:             "reset",
						SupersedingEquivalenceGroups: []string{"restart"},
						ImpactedEntityScope:          "GPU_UUID",
					},
				},
			},
			expectError: true,
		},
		{
			name: "Only the COMPONENT_RESET action can have an ImpactedEntityScope",
			config: TomlConfig{
				Template: Template{MountPath: tempDir},
				RemediationActions: map[string]MaintenanceResource{
					"ACTION_A": {
						TemplateFileName: "template-a.yaml",
						Scope:            "Cluster",
						EquivalenceGroup: "restart",
					},
					"ACTION_B": {
						TemplateFileName:             "template-b.yaml",
						Scope:                        "Namespaced",
						Namespace:                    "test-namespace",
						EquivalenceGroup:             "reset",
						SupersedingEquivalenceGroups: []string{"restart"},
						ImpactedEntityScope:          "GPU_UUID",
					},
				},
			},
			expectError: true,
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
