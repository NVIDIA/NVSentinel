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
	"testing"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestEquivalenceGroupConfiguration(t *testing.T) {
	// Create temp directory for template files
	tempDir, err := os.MkdirTemp("", "equivalence-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	defer os.RemoveAll(tempDir)

	// Create template files
	templateFiles := []string{"template-restart.yaml", "template-reset.yaml"}
	for _, f := range templateFiles {
		if err := os.WriteFile(filepath.Join(tempDir, f), []byte("apiVersion: v1"), 0644); err != nil {
			t.Fatalf("Failed to create template file %s: %v", f, err)
		}
	}

	// Test config with equivalence groups
	config := TomlConfig{
		Template: Template{MountPath: tempDir},
		RemediationActions: map[string]MaintenanceResource{
			protos.RecommendedAction_RESTART_BM.String(): {
				ApiGroup:         "example.io",
				Kind:             "NodeRestart",
				EquivalenceGroup: "restart",
				TemplateFileName: "template-restart.yaml",
			},
			protos.RecommendedAction_RESTART_VM.String(): {
				ApiGroup:         "example.io",
				Kind:             "ComponentReset",
				EquivalenceGroup: "restart", // Same group
				TemplateFileName: "template-reset.yaml",
			},
			protos.RecommendedAction_COMPONENT_RESET.String(): {
				ApiGroup:                     "example.io",
				Kind:                         "ComponentReset",
				TemplateFileName:             "template-reset.yaml",
				EquivalenceGroup:             "reset",
				SupersedingEquivalenceGroups: []string{"restart"},
				ImpactedEntityScope:          "GPU_UUID",
			},
		},
	}

	// Test equivalence group retrieval
	restartBMAction := config.RemediationActions[protos.RecommendedAction_RESTART_BM.String()]
	restartVMAction := config.RemediationActions[protos.RecommendedAction_RESTART_VM.String()]
	componentResetAction := config.RemediationActions[protos.RecommendedAction_COMPONENT_RESET.String()]

	// Both actions should have the same equivalence group
	if restartBMAction.EquivalenceGroup != restartVMAction.EquivalenceGroup {
		t.Errorf("Expected actions to have same equivalence group, got %s and %s",
			restartBMAction.EquivalenceGroup, restartVMAction.EquivalenceGroup)
	}

	// Both should be in "restart" group
	expectedGroup := "restart"
	if restartBMAction.EquivalenceGroup != expectedGroup {
		t.Errorf("Expected RESTART_BM to be in %s group, got %s",
			expectedGroup, restartBMAction.EquivalenceGroup)
	}

	if restartVMAction.EquivalenceGroup != expectedGroup {
		t.Errorf("Expected RESTART_VM to be in %s group, got %s",
			expectedGroup, restartVMAction.EquivalenceGroup)
	}

	// But they should use different CRD types
	if restartBMAction.Kind == restartVMAction.Kind {
		t.Errorf("Expected actions to use different CRD kinds, both used %s",
			restartBMAction.Kind)
	}

	if componentResetAction.EquivalenceGroup != "reset" {
		t.Errorf("Expected COMPONENT_RESET actions to be in reset group, got %s",
			componentResetAction.EquivalenceGroup)
	}

	// Validate the configuration
	if err := config.Validate(); err != nil {
		t.Fatalf("Configuration should be valid, got error: %v", err)
	}
}

func TestEquivalenceGroupLookup(t *testing.T) {
	config := TomlConfig{
		RemediationActions: map[string]MaintenanceResource{
			"TEST_ACTION_1": {
				EquivalenceGroup: "group1",
				TemplateFileName: "template1.yaml",
			},
			"TEST_ACTION_2": {
				EquivalenceGroup: "group1", // Same group
				TemplateFileName: "template2.yaml",
			},
			"TEST_ACTION_3": {
				EquivalenceGroup: "group2", // Different group
				TemplateFileName: "template3.yaml",
			},
		},
		Templates: map[string]string{
			"template1.yaml": "content1",
			"template2.yaml": "content2",
			"template3.yaml": "content3",
		},
	}

	// Test group lookup function
	getEquivalenceGroup := func(actionName string) string {
		if action, exists := config.RemediationActions[actionName]; exists {
			return action.EquivalenceGroup
		}
		return ""
	}

	// Test that actions in same group return same group name
	group1a := getEquivalenceGroup("TEST_ACTION_1")
	group1b := getEquivalenceGroup("TEST_ACTION_2")
	group2 := getEquivalenceGroup("TEST_ACTION_3")

	if group1a != group1b {
		t.Errorf("Expected TEST_ACTION_1 and TEST_ACTION_2 to have same group, got %s and %s",
			group1a, group1b)
	}

	if group1a == group2 {
		t.Errorf("Expected TEST_ACTION_1 and TEST_ACTION_3 to have different groups, both got %s",
			group1a)
	}

	// Test non-existent action
	nonExistentGroup := getEquivalenceGroup("NON_EXISTENT")
	if nonExistentGroup != "" {
		t.Errorf("Expected empty group for non-existent action, got %s", nonExistentGroup)
	}
}
