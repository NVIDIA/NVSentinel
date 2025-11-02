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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLoadConfig_FileNotFound(t *testing.T) {
	// Test that loading a non-existent config file returns error
	config, err := LoadConfig("/nonexistent/path/config.yaml")
	assert.Error(t, err, "Should error when config file path is invalid")
	assert.Nil(t, config, "Should return nil config on error")
}

func TestLoadConfig_ValidYAML(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "janitor-config.yaml")

	configContent := `
global:
  timeout: 30m
  manualMode: true
  nodes:
    exclusions:
      - matchLabels:
          environment: production
          critical: "true"

rebootNodeController:
  enabled: true
  manualMode: false
  timeout: 20m

terminateNodeController:
  enabled: false
  manualMode: true
  timeout: 15m
`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	// Load the config
	config, err := LoadConfig(configPath)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Verify global config
	assert.Equal(t, 30*time.Minute, config.Global.Timeout)
	assert.True(t, config.Global.ManualMode)
	assert.Len(t, config.Global.Nodes.Exclusions, 1)
	assert.Equal(t, "production", config.Global.Nodes.Exclusions[0].MatchLabels["environment"])
	assert.Equal(t, "true", config.Global.Nodes.Exclusions[0].MatchLabels["critical"])

	// Verify RebootNode config
	assert.True(t, config.RebootNode.Enabled)
	assert.False(t, config.RebootNode.ManualMode)
	assert.Equal(t, 20*time.Minute, config.RebootNode.Timeout)

	// Verify TerminateNode config
	assert.False(t, config.TerminateNode.Enabled)
	assert.True(t, config.TerminateNode.ManualMode)
	assert.Equal(t, 15*time.Minute, config.TerminateNode.Timeout)

	// Verify that node exclusions are propagated to controller configs
	assert.Equal(t, config.Global.Nodes.Exclusions, config.RebootNode.NodeExclusions)
	assert.Equal(t, config.Global.Nodes.Exclusions, config.TerminateNode.NodeExclusions)
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	// Create a temporary invalid config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid-config.yaml")

	invalidContent := `
global:
  timeout: invalid-duration
  manualMode: not-a-boolean
`

	err := os.WriteFile(configPath, []byte(invalidContent), 0644)
	require.NoError(t, err)

	// Load the config - should error on unmarshal
	config, err := LoadConfig(configPath)
	assert.Error(t, err)
	assert.Nil(t, config)
	assert.Contains(t, err.Error(), "failed to unmarshal config")
}

func TestLoadConfig_EmptyFile(t *testing.T) {
	// Create an empty config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "empty-config.yaml")

	err := os.WriteFile(configPath, []byte(""), 0644)
	require.NoError(t, err)

	// Load the config - should succeed with defaults
	config, err := LoadConfig(configPath)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Verify defaults (zero values)
	assert.Equal(t, time.Duration(0), config.Global.Timeout)
	assert.False(t, config.Global.ManualMode)
	assert.Len(t, config.Global.Nodes.Exclusions, 0)
}

func TestLoadConfig_PartialConfig(t *testing.T) {
	// Create a config file with only some fields set
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "partial-config.yaml")

	configContent := `
global:
  timeout: 45m

rebootNodeController:
  enabled: true
`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	// Load the config
	config, err := LoadConfig(configPath)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Verify set values
	assert.Equal(t, 45*time.Minute, config.Global.Timeout)
	assert.True(t, config.RebootNode.Enabled)

	// Verify unset values use defaults
	assert.False(t, config.Global.ManualMode)
	assert.False(t, config.TerminateNode.Enabled)
	assert.Equal(t, time.Duration(0), config.TerminateNode.Timeout)
}

func TestLoadConfig_NodeExclusionsPropagation(t *testing.T) {
	// Test that node exclusions from global config are properly propagated
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "exclusions-config.yaml")

	configContent := `
global:
  nodes:
    exclusions:
      - matchLabels:
          tier: system
      - matchExpressions:
          - key: node-role.kubernetes.io/control-plane
            operator: Exists
`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	// Load the config
	config, err := LoadConfig(configPath)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Verify exclusions are set in global config
	assert.Len(t, config.Global.Nodes.Exclusions, 2)

	// Verify exclusions are propagated to both controller configs
	assert.Equal(t, config.Global.Nodes.Exclusions, config.RebootNode.NodeExclusions)
	assert.Equal(t, config.Global.Nodes.Exclusions, config.TerminateNode.NodeExclusions)

	// Verify first exclusion (matchLabels)
	assert.Equal(t, "system", config.RebootNode.NodeExclusions[0].MatchLabels["tier"])

	// Verify second exclusion (matchExpressions)
	require.Len(t, config.RebootNode.NodeExclusions[1].MatchExpressions, 1)
	assert.Equal(t, "node-role.kubernetes.io/control-plane", config.RebootNode.NodeExclusions[1].MatchExpressions[0].Key)
	assert.Equal(t, metav1.LabelSelectorOpExists, config.RebootNode.NodeExclusions[1].MatchExpressions[0].Operator)
}

func TestConfig_ZeroValues(t *testing.T) {
	t.Run("zero value config has expected defaults", func(t *testing.T) {
		config := &Config{}

		// Global config zero values
		assert.Equal(t, time.Duration(0), config.Global.Timeout)
		assert.False(t, config.Global.ManualMode)
		assert.Empty(t, config.Global.Nodes.Exclusions)

		// RebootNode config zero values
		assert.False(t, config.RebootNode.Enabled)
		assert.False(t, config.RebootNode.ManualMode)
		assert.Equal(t, time.Duration(0), config.RebootNode.Timeout)
		assert.Empty(t, config.RebootNode.NodeExclusions)

		// TerminateNode config zero values
		assert.False(t, config.TerminateNode.Enabled)
		assert.False(t, config.TerminateNode.ManualMode)
		assert.Equal(t, time.Duration(0), config.TerminateNode.Timeout)
		assert.Empty(t, config.TerminateNode.NodeExclusions)
	})

	t.Run("controller configs are independent", func(t *testing.T) {
		config := &Config{
			RebootNode: RebootNodeControllerConfig{
				Enabled:    true,
				ManualMode: false,
				Timeout:    5 * time.Minute,
			},
			TerminateNode: TerminateNodeControllerConfig{
				Enabled:    false,
				ManualMode: true,
				Timeout:    10 * time.Minute,
			},
		}

		// Verify each controller can have different settings
		assert.True(t, config.RebootNode.Enabled)
		assert.False(t, config.RebootNode.ManualMode)
		assert.Equal(t, 5*time.Minute, config.RebootNode.Timeout)

		assert.False(t, config.TerminateNode.Enabled)
		assert.True(t, config.TerminateNode.ManualMode)
		assert.Equal(t, 10*time.Minute, config.TerminateNode.Timeout)

		// Modifying one doesn't affect the other
		config.RebootNode.Timeout = 15 * time.Minute
		assert.Equal(t, 15*time.Minute, config.RebootNode.Timeout)
		assert.Equal(t, 10*time.Minute, config.TerminateNode.Timeout)
	})
}
