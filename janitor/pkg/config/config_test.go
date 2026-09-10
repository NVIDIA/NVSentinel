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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/nvidia/nvsentinel/janitor/pkg/gpuservices"
)

const (
	testNamespace = "nvsentinel"
)

func TestLoadConfig_FileNotFound(t *testing.T) {
	// Test that loading a non-existent config file returns error
	config, err := LoadConfig("/nonexistent/path/config.yaml", testNamespace)
	assert.Error(t, err, "Should error when config file path is invalid")
	assert.Nil(t, config, "Should return nil config on error")
}

func TestLoadConfig_GlobalDefaults(t *testing.T) {
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
  cspProviderHost: janitor-provider.nvsentinel.svc.cluster.local:50051

rebootNodeController:
  enabled: true

terminateNodeController:
  enabled: true

gpuResetController:
  enabled: false
`
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	// Load the config
	config, err := LoadConfig(configPath, testNamespace)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Verify global config
	assert.Equal(t, 30*time.Minute, config.Global.Timeout)
	assert.True(t, *config.Global.ManualMode)
	assert.Len(t, config.Global.Nodes.Exclusions, 1)
	assert.Equal(t, "production", config.Global.Nodes.Exclusions[0].MatchLabels["environment"])
	assert.Equal(t, "true", config.Global.Nodes.Exclusions[0].MatchLabels["critical"])
	assert.Equal(t, "janitor-provider.nvsentinel.svc.cluster.local:50051", config.Global.CSPProviderHost)

	// Verify RebootNode config
	assert.True(t, config.RebootNode.Enabled)
	assert.True(t, *config.RebootNode.ManualMode)
	assert.Equal(t, config.Global.Timeout, config.RebootNode.Timeout)
	assert.Equal(t, config.Global.Nodes.Exclusions, config.RebootNode.Exclusions)
	assert.Equal(t, config.Global.CSPProviderHost, config.RebootNode.CSPProviderHost)

	// Verify TerminateNode config
	assert.True(t, config.TerminateNode.Enabled)
	assert.True(t, *config.TerminateNode.ManualMode)
	assert.Equal(t, config.Global.Timeout, config.TerminateNode.Timeout)
	assert.Equal(t, config.Global.Nodes.Exclusions, config.TerminateNode.Exclusions)
	assert.Equal(t, config.Global.CSPProviderHost, config.TerminateNode.CSPProviderHost)

	// Verify GPUReset config
	assert.False(t, config.GPUReset.Enabled)
	assert.True(t, *config.GPUReset.ManualMode)
	assert.Equal(t, config.Global.Timeout, config.GPUReset.Timeout)
	assert.Equal(t, config.Global.Nodes.Exclusions, config.GPUReset.Exclusions)
	assert.Equal(t, config.Global.CSPProviderHost, config.GPUReset.CSPProviderHost)
}

func TestLoadConfig_ControllerOverrides(t *testing.T) {
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
  cspProviderHost: janitor-provider.nvsentinel.svc.cluster.local:50051

rebootNodeController:
  enabled: true
  timeout: 25m
  manualMode: false
  exclusions:
  - matchLabels:
      environment: dev
      critical: "true"
  cspProviderHost: janitor-provider.nvsentinel.svc.cluster.local:50052

terminateNodeController:
  enabled: true
  timeout: 26m
  manualMode: false
  exclusions:
  - matchLabels:
      environment: staging
      critical: "true"
  cspProviderHost: janitor-provider.nvsentinel.svc.cluster.local:50053

gpuResetController:
  enabled: true
  timeout: 27m
  manualMode: false
  exclusions:
  - matchLabels:
      environment: qa
      critical: "true"
  cspProviderHost: janitor-provider.nvsentinel.svc.cluster.local:50054
  resetJob:
    writeSysLogEvent: true
    runtimeClassName: "nvidia"
    uploadURL: "http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local/upload"
    imageConfig:
      image: "alpine:latest"
      imagePullSecrets:
      - name: pull-secret
    resources:
      limits:
        cpu: 100m
        memory: 128Mi
      requests:
        cpu: 50m
        memory: 64Mi
  serviceManager:
    name: gpu-operator
    spec:
      apps:
      - appSelector:
          app: nvidia-dcgm
        disabledValue: "false"
        enabledValue: "true"
        nodeLabel: nvidia.com/gpu.deploy.dcgm
      managerSelector:
        app.kubernetes.io/managed-by: tilt
      namespace: gpu-operator
      restoreTimeout: 10m
      teardownTimeout: 5m
`
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	// Load the config
	config, err := LoadConfig(configPath, testNamespace)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Verify global config
	assert.Equal(t, 30*time.Minute, config.Global.Timeout)
	assert.True(t, *config.Global.ManualMode)
	assert.Len(t, config.Global.Nodes.Exclusions, 1)
	assert.Equal(t, "production", config.Global.Nodes.Exclusions[0].MatchLabels["environment"])
	assert.Equal(t, "true", config.Global.Nodes.Exclusions[0].MatchLabels["critical"])
	assert.Equal(t, "janitor-provider.nvsentinel.svc.cluster.local:50051", config.Global.CSPProviderHost)

	// Verify RebootNode config
	assert.True(t, config.RebootNode.Enabled)
	assert.Equal(t, 25*time.Minute, config.RebootNode.Timeout)
	assert.False(t, *config.RebootNode.ManualMode)
	assert.Len(t, config.RebootNode.Exclusions, 1)
	assert.Equal(t, "dev", config.RebootNode.Exclusions[0].MatchLabels["environment"])
	assert.Equal(t, "true", config.RebootNode.Exclusions[0].MatchLabels["critical"])
	assert.Equal(t, "janitor-provider.nvsentinel.svc.cluster.local:50052", config.RebootNode.CSPProviderHost)

	// Verify TerminateNode config
	assert.True(t, config.TerminateNode.Enabled)
	assert.Equal(t, 26*time.Minute, config.TerminateNode.Timeout)
	assert.False(t, *config.TerminateNode.ManualMode)
	assert.Len(t, config.TerminateNode.Exclusions, 1)
	assert.Equal(t, "staging", config.TerminateNode.Exclusions[0].MatchLabels["environment"])
	assert.Equal(t, "true", config.TerminateNode.Exclusions[0].MatchLabels["critical"])
	assert.Equal(t, "janitor-provider.nvsentinel.svc.cluster.local:50053", config.TerminateNode.CSPProviderHost)

	// Verify GPUReset config
	assert.True(t, config.GPUReset.Enabled)
	assert.Equal(t, 27*time.Minute, config.GPUReset.Timeout)
	assert.False(t, *config.GPUReset.ManualMode)
	assert.Equal(t, "http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local/upload",
		config.GPUReset.ResetJob.UploadURL)
	assert.Len(t, config.GPUReset.Exclusions, 1)
	assert.Equal(t, "qa", config.GPUReset.Exclusions[0].MatchLabels["environment"])
	assert.Equal(t, "true", config.GPUReset.Exclusions[0].MatchLabels["critical"])
	assert.Equal(t, "janitor-provider.nvsentinel.svc.cluster.local:50054", config.GPUReset.CSPProviderHost)

	imagePullSecrets := []ImagePullSecret{
		{
			Name: "pull-secret",
		},
	}
	resourceRequirements := ResourceRequirements{
		Limits: map[string]string{
			"memory": "128Mi",
			"cpu":    "100m",
		},
		Requests: map[string]string{
			"memory": "64Mi",
			"cpu":    "50m",
		},
	}
	expectedJobTemplate, err := getDefaultGPUResetJobTemplate(testNamespace, "alpine:latest", imagePullSecrets,
		resourceRequirements, "nvidia", true, "http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local/upload")
	assert.NoError(t, err)
	assert.Equal(t, expectedJobTemplate, config.GPUReset.ResolvedJobTemplate)

	expectedServiceManager := gpuservices.Manager{
		Name: "gpu-operator",
		Spec: gpuservices.ManagerSpec{
			ManagerSelector: map[string]string{"app.kubernetes.io/managed-by": "tilt"},
			Namespace:       "gpu-operator",
			Apps: []gpuservices.AppSpec{
				{
					AppSelector:   map[string]string{"app": "nvidia-dcgm"},
					NodeLabel:     "nvidia.com/gpu.deploy.dcgm",
					EnabledValue:  "true",
					DisabledValue: "false",
				},
			},
			TeardownTimeout: 5 * time.Minute,
			RestoreTimeout:  10 * time.Minute,
		},
	}
	assert.Equal(t, expectedServiceManager, config.GPUReset.ServiceManager)
}

func TestLoadConfig_MinimalGPUResetControllerConfig(t *testing.T) {
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
  cspProviderHost: janitor-provider.nvsentinel.svc.cluster.local:50051

rebootNodeController:
  enabled: true

terminateNodeController:
  enabled: true

gpuResetController:
  enabled: true
  resetJob:
    imageConfig:
      image: "alpine:latest"
  serviceManager:
    name: "gpu-operator"
`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	// Load the config
	config, err := LoadConfig(configPath, testNamespace)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Verify GPUReset config
	assert.True(t, config.GPUReset.Enabled)

	expectedJobTemplate, err := getDefaultGPUResetJobTemplate(testNamespace, "alpine:latest", nil,
		ResourceRequirements{}, "", true, "")
	assert.NoError(t, err)
	assert.Equal(t, expectedJobTemplate, config.GPUReset.ResolvedJobTemplate)

	assert.Equal(t, gpuservices.Manager{Name: "gpu-operator"}, config.GPUReset.ServiceManager)
}

func TestLoadConfig_GPUResetControllerConfigEmptyRuntimeClass(t *testing.T) {
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
  cspProviderHost: janitor-provider.nvsentinel.svc.cluster.local:50051

rebootNodeController:
  enabled: true

terminateNodeController:
  enabled: true

gpuResetController:
  enabled: true
  resetJob:
    runtimeClassName: ""
    imageConfig:
      image: "alpine:latest"
  serviceManager:
    name: "gpu-operator"
`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	// Load the config
	config, err := LoadConfig(configPath, testNamespace)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Verify GPUReset config
	assert.True(t, config.GPUReset.Enabled)

	expectedJobTemplate, err := getDefaultGPUResetJobTemplate(testNamespace, "alpine:latest", nil,
		ResourceRequirements{}, "", true, "")
	assert.NoError(t, err)
	assert.Equal(t, expectedJobTemplate, config.GPUReset.ResolvedJobTemplate)

	assert.Equal(t, gpuservices.Manager{Name: "gpu-operator"}, config.GPUReset.ServiceManager)
}

func TestLoadConfig_GPUResetEnabledMissingImage(t *testing.T) {
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
  cspProviderHost: janitor-provider.nvsentinel.svc.cluster.local:50051

rebootNodeController:
  enabled: true

terminateNodeController:
  enabled: true

gpuResetController:
  enabled: true
  serviceManager:
    name: "gpu-operator"
`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	// Load the config
	config, err := LoadConfig(configPath, testNamespace)
	require.Error(t, err)
	require.Nil(t, config)
}

func TestLoadConfig_GPUResetEnabledWriteSysLogEventFalse(t *testing.T) {
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
  cspProviderHost: janitor-provider.nvsentinel.svc.cluster.local:50051

rebootNodeController:
  enabled: true

terminateNodeController:
  enabled: true

gpuResetController:
  enabled: true
  resetJob:
    writeSysLogEvent: false
    runtimeClassName: ""
    imageConfig:
      image: "alpine:latest"
  serviceManager:
    name: "gpu-operator"
`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	// Load the config
	config, err := LoadConfig(configPath, testNamespace)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Verify GPUReset config
	assert.True(t, config.GPUReset.Enabled)

	expectedJobTemplate, err := getDefaultGPUResetJobTemplate(testNamespace, "alpine:latest", nil,
		ResourceRequirements{}, "", false, "")
	assert.NoError(t, err)
	assert.Equal(t, expectedJobTemplate, config.GPUReset.ResolvedJobTemplate)

	assert.Equal(t, gpuservices.Manager{Name: "gpu-operator"}, config.GPUReset.ServiceManager)
}

func TestLoadConfig_GPUResetInvalidResources(t *testing.T) {
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
  cspProviderHost: janitor-provider.nvsentinel.svc.cluster.local:50051

rebootNodeController:
  enabled: true

terminateNodeController:
  enabled: true

gpuResetController:
  enabled: true
  timeout: 27m
  manualMode: false
  exclusions:
  - matchLabels:
      environment: qa
      critical: "true"
  cspProviderHost: janitor-provider.nvsentinel.svc.cluster.local:50054
  resetJob:
	runtimeClassName: "nvidia"
    imageConfig:
      image: "alpine:latest"
      imagePullSecrets:
      - name: pull-secret
    resources:
      limits:
        cpu: abc
        memory: 128Mi
      requests:
        cpu: 50m
        memory: 64Mi
  serviceManager:
    name: gpu-operator
    spec:
      apps:
      - appSelector:
          app: nvidia-dcgm
        disabledValue: "false"
        enabledValue: "true"
        nodeLabel: nvidia.com/gpu.deploy.dcgm
      managerSelector:
        app.kubernetes.io/managed-by: tilt
      namespace: gpu-operator
      restoreTimeout: 10m
      teardownTimeout: 5m
`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	// Load the config
	config, err := LoadConfig(configPath, testNamespace)
	require.Error(t, err)
	require.Nil(t, config)
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
	config, err := LoadConfig(configPath, testNamespace)
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
	config, err := LoadConfig(configPath, testNamespace)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Verify defaults (zero values)
	assert.Equal(t, 30*time.Minute, config.Global.Timeout)
	assert.False(t, *config.Global.ManualMode)
	assert.Len(t, config.Global.Nodes.Exclusions, 0)
	assert.Len(t, config.Global.CSPProviderHost, 0)
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
	config, err := LoadConfig(configPath, testNamespace)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Verify exclusions are set in global config
	assert.Len(t, config.Global.Nodes.Exclusions, 2)

	// Verify exclusions are propagated to both controller configs
	assert.Equal(t, config.Global.Nodes.Exclusions, config.RebootNode.Exclusions)
	assert.Equal(t, config.Global.Nodes.Exclusions, config.TerminateNode.Exclusions)
	assert.Equal(t, config.Global.Nodes.Exclusions, config.GPUReset.Exclusions)

	// Verify first exclusion (matchLabels)
	assert.Equal(t, "system", config.RebootNode.Exclusions[0].MatchLabels["tier"])

	// Verify second exclusion (matchExpressions)
	require.Len(t, config.RebootNode.Exclusions[1].MatchExpressions, 1)
	assert.Equal(t, "node-role.kubernetes.io/control-plane", config.RebootNode.Exclusions[1].MatchExpressions[0].Key)
	assert.Equal(t, metav1.LabelSelectorOpExists, config.RebootNode.Exclusions[1].MatchExpressions[0].Operator)
}

const gpuResetDefaultTemplateConfig = `
gpuResetController:
  enabled: true
  resetJob:
    writeSysLogEvent: true
    runtimeClassName: "nvidia"
    uploadURL: "http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local/upload"
    imageConfig:
      image: "ghcr.io/nvidia/nvsentinel/gpu-reset:1.16.0"
      imagePullSecrets:
      - name: pull-secret
    resources:
      limits:
        cpu: 100m
        memory: 128Mi
      requests:
        cpu: 50m
        memory: 64Mi
`

func loadConfigFromString(t *testing.T, configContent string) (*Config, error) {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "janitor-config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0644))

	return LoadConfig(configPath, testNamespace)
}

// jobTemplateConfig serialises jobTemplate into a block scalar, the way the chart renders it.
func jobTemplateConfig(t *testing.T, jobTemplate string) string {
	t.Helper()

	out, err := yaml.Marshal(map[string]any{
		"gpuResetController": map[string]any{
			"enabled":  true,
			"resetJob": map[string]any{"jobTemplate": jobTemplate},
		},
	})
	require.NoError(t, err)

	return string(out)
}

const gpuResetMinimalJobTemplate = `spec:
  template:
    spec:
      containers:
      - name: gpu-reset
        image: registry.example.com/gpu-reset:v1
`

// The golden file is a copy of the built-in template taken before jobTemplate existed. Comparing
// against getDefaultGPUResetJobTemplate instead would pass for any change to that function.
func TestLoadConfig_NoJobTemplate_MatchesDefaultTemplateGolden(t *testing.T) {
	config, err := loadConfigFromString(t, gpuResetDefaultTemplateConfig)
	require.NoError(t, err)
	require.NotNil(t, config.GPUReset.ResolvedJobTemplate)

	actual, err := yaml.Marshal(config.GPUReset.ResolvedJobTemplate)
	require.NoError(t, err)

	golden, err := os.ReadFile(filepath.Join("testdata", "default-gpu-reset-job-template.yaml"))
	require.NoError(t, err)

	assert.YAMLEq(t, string(golden), string(actual))
}

func TestLoadConfig_JobTemplate_ReplacesDefaultTemplate(t *testing.T) {
	jobTemplate := `metadata:
  labels:
    owner: platform
spec:
  backoffLimit: 1
  template:
    spec:
      containers:
      - name: gpu-reset
        image: registry.example.com/gpu-reset:v1
        volumeMounts:
        - name: driver-root
          mountPath: /home/kubernetes/bin/nvidia
      volumes:
      - name: driver-root
        hostPath:
          path: /home/kubernetes/bin/nvidia
`

	config, err := loadConfigFromString(t, jobTemplateConfig(t, jobTemplate))
	require.NoError(t, err)
	require.NotNil(t, config.GPUReset.ResolvedJobTemplate)

	resolved := config.GPUReset.ResolvedJobTemplate
	assert.Equal(t, testNamespace, resolved.Namespace)
	assert.Equal(t, map[string]string{"owner": "platform"}, resolved.Labels)
	assert.Equal(t, int32(1), *resolved.Spec.BackoffLimit)

	assert.Nil(t, resolved.Spec.TTLSecondsAfterFinished)
	assert.Nil(t, resolved.Spec.ActiveDeadlineSeconds)
	assert.Empty(t, resolved.Spec.Template.Spec.Tolerations)
	assert.Nil(t, resolved.Spec.Template.Spec.RuntimeClassName)

	podSpec := resolved.Spec.Template.Spec
	require.Len(t, podSpec.Containers, 1)
	assert.Equal(t, "registry.example.com/gpu-reset:v1", podSpec.Containers[0].Image)
	assert.Empty(t, podSpec.Containers[0].Env)

	require.Len(t, podSpec.Containers[0].VolumeMounts, 1)
	assert.Equal(t, "/home/kubernetes/bin/nvidia", podSpec.Containers[0].VolumeMounts[0].MountPath)
	require.Len(t, podSpec.Volumes, 1)
	require.NotNil(t, podSpec.Volumes[0].HostPath)
	assert.Equal(t, "/home/kubernetes/bin/nvidia", podSpec.Volumes[0].HostPath.Path)
}

func TestLoadConfig_JobTemplate_ParsesResourceQuantities(t *testing.T) {
	jobTemplate := `spec:
  template:
    spec:
      containers:
      - name: gpu-reset
        image: registry.example.com/gpu-reset:v1
        resources:
          limits:
            cpu: 100m
            memory: 1Gi
`

	config, err := loadConfigFromString(t, jobTemplateConfig(t, jobTemplate))
	require.NoError(t, err)

	limits := config.GPUReset.ResolvedJobTemplate.Spec.Template.Spec.Containers[0].Resources.Limits
	assert.True(t, resource.MustParse("1Gi").Equal(limits[corev1.ResourceMemory]))
	assert.True(t, resource.MustParse("100m").Equal(limits[corev1.ResourceCPU]))
}

func TestLoadConfig_JobTemplate_InvalidReturnsError(t *testing.T) {
	tests := []struct {
		name        string
		jobTemplate string
		expectedErr string
	}{
		{
			name: "unknown field",
			jobTemplate: `spec:
  template:
    spec:
      restartPolicyy: OnFailure
      containers:
      - name: gpu-reset
        image: registry.example.com/gpu-reset:v1
`,
			expectedErr: "restartPolicyy",
		},
		{
			name: "nodeName set",
			jobTemplate: `spec:
  template:
    spec:
      nodeName: gpu-node-1
      containers:
      - name: gpu-reset
        image: registry.example.com/gpu-reset:v1
`,
			expectedErr: "must not set spec.template.spec.nodeName",
		},
		{
			name: "no containers",
			jobTemplate: `spec:
  template:
    spec:
      containers: []
`,
			expectedErr: "at least one container",
		},
		{
			name:        "foreign namespace",
			jobTemplate: "metadata:\n  namespace: other\n" + gpuResetMinimalJobTemplate,
			expectedErr: `metadata.namespace "other"`,
		},
		{
			name:        "name set",
			jobTemplate: "metadata:\n  name: my-reset-job\n" + gpuResetMinimalJobTemplate,
			expectedErr: `must not set metadata.name "my-reset-job"`,
		},
		{
			name:        "generateName set",
			jobTemplate: "metadata:\n  generateName: my-reset-job-\n" + gpuResetMinimalJobTemplate,
			expectedErr: `must not set metadata.generateName "my-reset-job-"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := loadConfigFromString(t, jobTemplateConfig(t, tt.jobTemplate))
			require.Error(t, err)
			require.Nil(t, config)
			assert.Contains(t, err.Error(), tt.expectedErr)
		})
	}
}

func TestLoadConfig_JobTemplate_NamespaceDefaultsToJanitorNamespace(t *testing.T) {
	tests := []struct {
		name        string
		jobTemplate string
	}{
		{
			name:        "namespace omitted",
			jobTemplate: gpuResetMinimalJobTemplate,
		},
		{
			name:        "namespace matches",
			jobTemplate: "metadata:\n  namespace: " + testNamespace + "\n" + gpuResetMinimalJobTemplate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := loadConfigFromString(t, jobTemplateConfig(t, tt.jobTemplate))
			require.NoError(t, err)
			assert.Equal(t, testNamespace, config.GPUReset.ResolvedJobTemplate.Namespace)
		})
	}
}

func TestLoadConfig_JobTemplate_ImageConfigNotRequired(t *testing.T) {
	config, err := loadConfigFromString(t, jobTemplateConfig(t, gpuResetMinimalJobTemplate))
	require.NoError(t, err)
	assert.Empty(t, config.GPUReset.ResetJob.ImageConfig.Image)
}

func TestIgnoredResetJobFields_ReturnsFieldsThatAreSet(t *testing.T) {
	tests := []struct {
		name     string
		resetJob ResetJobConfig
		expected []string
	}{
		{
			name:     "only jobTemplate set",
			resetJob: ResetJobConfig{JobTemplate: gpuResetMinimalJobTemplate},
		},
		{
			name: "every other field set",
			resetJob: ResetJobConfig{
				ImageConfig: ImageConfig{
					Image:            "registry.example.com/gpu-reset:v1",
					ImagePullSecrets: []ImagePullSecret{{Name: "pull-secret"}},
				},
				Resources:        ResourceRequirements{Limits: map[string]string{"cpu": "100m"}},
				RuntimeClassName: "nvidia",
				WriteSysLogEvent: new(false),
				UploadURL:        "http://file-server/upload",
			},
			expected: []string{
				"imageConfig.image", "imageConfig.imagePullSecrets", "resources",
				"runtimeClassName", "writeSysLogEvent", "uploadURL",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, ignoredResetJobFields(&tt.resetJob))
		})
	}
}
