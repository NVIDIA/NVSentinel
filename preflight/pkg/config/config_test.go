// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// TestLoad covers YAML parsing, default population (GPU resources,
// gang coordination), validation errors (bad timeout), file errors,
// and extraHostPathMounts readOnly defaulting.
func TestLoad(t *testing.T) {
	t.Run("minimal config with defaults", func(t *testing.T) {
		path := writeYAML(t, `
initContainers:
  - name: preflight-dcgm-diag
    image: dcgm:latest
`)
		cfg, err := Load(path)
		require.NoError(t, err)

		assert.Equal(t, []string{"nvidia.com/gpu"}, cfg.GPUResourceNames)
		assert.Equal(t, "EXECUTE_REMEDIATION", cfg.ProcessingStrategy)
		assert.Len(t, cfg.InitContainers, 1)
		assert.Equal(t, "preflight-dcgm-diag", cfg.InitContainers[0].Name)
	})

	t.Run("gang enabled defaults", func(t *testing.T) {
		path := writeYAML(t, `
initContainers:
  - name: preflight-dcgm-diag
    image: dcgm:latest
gangCoordination:
  enabled: true
`)
		cfg, err := Load(path)
		require.NoError(t, err)

		assert.True(t, cfg.GangCoordination.Enabled)
		assert.Equal(t, 10*time.Minute, cfg.GangCoordination.TimeoutDuration)
		assert.Equal(t, 29500, cfg.GangCoordination.MasterPort)
		assert.Equal(t, "/etc/preflight", cfg.GangCoordination.ConfigMapMountPath)
		require.NotNil(t, cfg.GangCoordination.MirrorResourceClaims)
		assert.True(t, *cfg.GangCoordination.MirrorResourceClaims)
	})

	t.Run("gang custom values", func(t *testing.T) {
		path := writeYAML(t, `
initContainers:
  - name: preflight-dcgm-diag
    image: dcgm:latest
gangCoordination:
  enabled: true
  timeout: "5m30s"
  masterPort: 29501
  configMapMountPath: "/custom/path"
`)
		cfg, err := Load(path)
		require.NoError(t, err)

		assert.Equal(t, 5*time.Minute+30*time.Second, cfg.GangCoordination.TimeoutDuration)
		assert.Equal(t, 29501, cfg.GangCoordination.MasterPort)
		assert.Equal(t, "/custom/path", cfg.GangCoordination.ConfigMapMountPath)
	})

	t.Run("gang invalid timeout", func(t *testing.T) {
		path := writeYAML(t, `
initContainers:
  - name: preflight-dcgm-diag
    image: dcgm:latest
gangCoordination:
  enabled: true
  timeout: "not-a-duration"
`)
		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timeout")
	})

	t.Run("invalid YAML", func(t *testing.T) {
		path := writeYAML(t, `{invalid yaml: [`)
		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse")
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := Load("/nonexistent/path/config.yaml")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read")
	})

	t.Run("extra hostPath readOnly defaults to true", func(t *testing.T) {
		path := writeYAML(t, `
initContainers:
  - name: preflight-dcgm-diag
    image: dcgm:latest
gangCoordination:
  enabled: true
  extraHostPathMounts:
    - name: host-libs
      hostPath: /opt/libs
      mountPath: /opt/libs
`)
		cfg, err := Load(path)
		require.NoError(t, err)

		require.Len(t, cfg.GangCoordination.ExtraHostPathMounts, 1)
		require.NotNil(t, cfg.GangCoordination.ExtraHostPathMounts[0].ReadOnly)
		assert.True(t, *cfg.GangCoordination.ExtraHostPathMounts[0].ReadOnly)
	})

	t.Run("initContainerPlacement defaults to append", func(t *testing.T) {
		path := writeYAML(t, `
initContainers:
  - name: preflight-dcgm-diag
    image: dcgm:latest
`)
		cfg, err := Load(path)
		require.NoError(t, err)

		assert.Equal(t, PlacementAppend, cfg.InitContainerPlacement)
	})

	t.Run("initContainerPlacement prepend", func(t *testing.T) {
		path := writeYAML(t, `
initContainers:
  - name: preflight-dcgm-diag
    image: dcgm:latest
initContainerPlacement: "prepend"
`)
		cfg, err := Load(path)
		require.NoError(t, err)

		assert.Equal(t, PlacementPrepend, cfg.InitContainerPlacement)
	})

	t.Run("initContainerPlacement invalid value", func(t *testing.T) {
		path := writeYAML(t, `
initContainers:
  - name: preflight-dcgm-diag
    image: dcgm:latest
initContainerPlacement: "middle"
`)
		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "initContainerPlacement")
	})

	t.Run("empty init container name rejected", func(t *testing.T) {
		path := writeYAML(t, `
initContainers:
  - name: ""
    image: dcgm:latest
`)
		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "name must be set")
	})

	t.Run("duplicate init container names rejected", func(t *testing.T) {
		path := writeYAML(t, `
initContainers:
  - name: preflight-dcgm-diag
    image: dcgm:latest
  - name: preflight-dcgm-diag
    image: dcgm:v2
`)
		_, err := Load(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate")
		assert.Contains(t, err.Error(), "preflight-dcgm-diag")
	})

	t.Run("defaultEnabled parsed from YAML", func(t *testing.T) {
		path := writeYAML(t, `
initContainers:
  - name: preflight-dcgm-diag
    image: dcgm:latest
  - name: preflight-nccl-allreduce
    image: nccl:latest
    defaultEnabled: false
`)
		cfg, err := Load(path)
		require.NoError(t, err)
		require.Len(t, cfg.InitContainers, 2)

		assert.True(t, cfg.InitContainers[0].IsDefaultEnabled(), "nil DefaultEnabled should be true")
		assert.False(t, cfg.InitContainers[1].IsDefaultEnabled(), "explicit false should be false")
	})

	t.Run("extra hostPath readOnly explicit false", func(t *testing.T) {
		path := writeYAML(t, `
initContainers:
  - name: preflight-dcgm-diag
    image: dcgm:latest
gangCoordination:
  enabled: true
  extraHostPathMounts:
    - name: host-libs
      hostPath: /opt/libs
      mountPath: /opt/libs
      readOnly: false
`)
		cfg, err := Load(path)
		require.NoError(t, err)

		require.Len(t, cfg.GangCoordination.ExtraHostPathMounts, 1)
		require.NotNil(t, cfg.GangCoordination.ExtraHostPathMounts[0].ReadOnly)
		assert.False(t, *cfg.GangCoordination.ExtraHostPathMounts[0].ReadOnly)
	})
}
