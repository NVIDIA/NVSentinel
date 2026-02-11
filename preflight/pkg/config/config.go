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
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

type Config struct {
	Port    int
	CertDir string

	FileConfig
}

type FileConfig struct {
	InitContainers       []corev1.Container     `yaml:"initContainers"`
	GPUResourceNames     []string               `yaml:"gpuResourceNames"`
	NetworkResourceNames []string               `yaml:"networkResourceNames"`
	DCGM                 DCGMConfig             `yaml:"dcgm"`
	GangDiscovery        GangDiscoveryConfig    `yaml:"gangDiscovery"`
	GangCoordination     GangCoordinationConfig `yaml:"gangCoordination"`
}

type DCGMConfig struct {
	HostengineAddr     string `yaml:"hostengineAddr"`
	DiagLevel          int    `yaml:"diagLevel"`
	ConnectorSocket    string `yaml:"connectorSocket"`
	ProcessingStrategy string `yaml:"processingStrategy"`
}

// GangDiscoveryConfig configures gang discovery for PodGroup-based schedulers.
// If empty (no Name set), defaults to native K8s 1.35+ WorkloadRef API.
type GangDiscoveryConfig struct {
	// Name is the discoverer identifier (used in gangID prefix and logging).
	Name string `yaml:"name,omitempty"`

	// AnnotationKeys are pod annotation keys to check for the PodGroup name (checked in order).
	AnnotationKeys []string `yaml:"annotationKeys,omitempty"`

	// LabelKeys are optional pod label keys to check as fallback (checked in order).
	LabelKeys []string `yaml:"labelKeys,omitempty"`

	// PodGroupGVR specifies the PodGroup CustomResource location.
	PodGroupGVR GVRConfig `yaml:"podGroupGVR,omitempty"`

	// MinCountExpr is a CEL expression to extract the minimum member count from the PodGroup.
	// The expression receives 'podGroup' as the unstructured object.
	// Examples: "podGroup.spec.minMember", "podGroup.spec.minReplicas"
	// Default: "podGroup.spec.minMember"
	MinCountExpr string `yaml:"minCountExpr,omitempty"`
}

// GVRConfig specifies a Kubernetes GroupVersionResource.
type GVRConfig struct {
	Group    string `yaml:"group"`
	Version  string `yaml:"version"`
	Resource string `yaml:"resource"`
}

// GangCoordinationConfig contains configuration for gang coordination.
type GangCoordinationConfig struct {
	// Enabled enables gang coordination for multi-node checks.
	Enabled bool `yaml:"enabled"`

	// Timeout is the maximum time to wait for all gang members to register.
	// Accepts duration strings like "10m", "5m30s", etc.
	// Default: 10m
	Timeout string `yaml:"timeout,omitempty"`

	// TimeoutDuration is the parsed Timeout value. Set by Load().
	TimeoutDuration time.Duration `yaml:"-"`

	// MasterPort is the port used for PyTorch distributed TCP bootstrap.
	// Default: 29500
	MasterPort int `yaml:"masterPort,omitempty"`

	// ConfigMapMountPath is the path where gang ConfigMap is mounted in init containers.
	// Default: /etc/preflight
	ConfigMapMountPath string `yaml:"configMapMountPath,omitempty"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var fileConfig FileConfig
	if err := yaml.Unmarshal(data, &fileConfig); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	fileConfig.setDefaults()

	if err := fileConfig.validate(); err != nil {
		return nil, fmt.Errorf("invalid file config: %w", err)
	}

	return &Config{FileConfig: fileConfig}, nil
}

func (c *FileConfig) setDefaults() {
	if len(c.GPUResourceNames) == 0 {
		c.GPUResourceNames = []string{"nvidia.com/gpu"}
	}

	c.DCGM.setDefaults()
	c.GangCoordination.setDefaults()
}

func (c *DCGMConfig) setDefaults() {
	if c.DiagLevel == 0 {
		c.DiagLevel = 1
	}

	if c.ProcessingStrategy == "" {
		c.ProcessingStrategy = "EXECUTE_REMEDIATION"
	}
}

func (c *GangCoordinationConfig) setDefaults() {
	if !c.Enabled {
		return
	}

	if c.Timeout == "" {
		c.Timeout = "10m"
	}

	if c.MasterPort == 0 {
		c.MasterPort = 29500
	}

	if c.ConfigMapMountPath == "" {
		c.ConfigMapMountPath = "/etc/preflight"
	}
}

func (c *FileConfig) validate() error {
	if c.GangCoordination.Enabled {
		timeout, err := time.ParseDuration(c.GangCoordination.Timeout)
		if err != nil {
			return fmt.Errorf("invalid gangCoordination.timeout %q: %w", c.GangCoordination.Timeout, err)
		}

		c.GangCoordination.TimeoutDuration = timeout
	}

	return nil
}
