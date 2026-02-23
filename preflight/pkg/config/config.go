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

	// NCCLEnvPatterns are glob patterns for environment variable names to copy
	// from the pod's main containers to preflight init containers.
	// This allows the init container to inherit fabric-specific NCCL config
	// (e.g. NCCL_*, FI_*, LD_LIBRARY_PATH) from the user's training container.
	NCCLEnvPatterns []string `yaml:"ncclEnvPatterns,omitempty"`

	// VolumeMountPatterns are glob patterns for volume mount names to copy
	// from the pod's main containers to preflight init containers.
	// This allows the init container to inherit fabric-specific mounts
	// (e.g. host EFA libs, TCPXO plugin volumes) from the user's container.
	VolumeMountPatterns []string `yaml:"volumeMountPatterns,omitempty"`
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

	// NCCLTopoConfigMap is the name of the ConfigMap containing the NCCL topology file.
	// Required for Azure NDv4/v5 - without it, NCCL cannot map GPUs to IB NICs.
	// If NCCLTopoData is set, the controller auto-creates this ConfigMap in the
	// pod's namespace; otherwise it must already exist.
	NCCLTopoConfigMap string `yaml:"ncclTopoConfigMap,omitempty"`

	// NCCLTopoData is the raw NCCL topology XML content.
	// When set, the controller creates a ConfigMap with this data in the pod's
	// namespace alongside the gang ConfigMap. This avoids manual ConfigMap
	// creation per namespace for Azure IB topology files.
	NCCLTopoData string `yaml:"ncclTopoData,omitempty"`

	// ExtraHostPathMounts defines optional hostPath mounts to inject into
	// gang-aware preflight init containers. This is useful for environments
	// where NCCL/OFI/CUDA libraries must be sourced from host paths.
	ExtraHostPathMounts []HostPathMount `yaml:"extraHostPathMounts,omitempty"`

	// ExtraVolumeMounts references volumes that already exist in the pod
	// (e.g. injected by another webhook) and adds mounts to init containers.
	// Unlike ExtraHostPathMounts, this does NOT create new volumes — it only
	// adds volumeMounts for volumes that are expected to be present.
	// Primary use-case: GCP TCPXO daemon writes the FastRak NCCL plugin into
	// a shared emptyDir; this option lets preflight init containers access it.
	ExtraVolumeMounts []ExtraVolumeMount `yaml:"extraVolumeMounts,omitempty"`

	// MirrorResourceClaims controls whether pod-level DRA resource claims
	// (spec.resourceClaims) are automatically copied to preflight init
	// containers' resources.claims. This ensures init containers get the
	// same device access as the main containers (GPUs, RDMA, IMEX channels).
	// Defaults to true when gang coordination is enabled.
	// See ADR-026 §DRA Integration.
	MirrorResourceClaims *bool `yaml:"mirrorResourceClaims,omitempty"`
}

// HostPathMount defines a hostPath volume and corresponding container mount.
type HostPathMount struct {
	// Name is the Kubernetes volume name.
	Name string `yaml:"name"`

	// HostPath is the node filesystem path to mount.
	HostPath string `yaml:"hostPath"`

	// MountPath is the path inside the init container.
	MountPath string `yaml:"mountPath"`

	// ReadOnly controls whether the mount is read-only. Defaults to true.
	ReadOnly *bool `yaml:"readOnly,omitempty"`

	// HostPathType is an optional Kubernetes HostPathType string.
	// Supported values include: Directory, DirectoryOrCreate, File, FileOrCreate,
	// Socket, CharDevice, and BlockDevice.
	HostPathType string `yaml:"hostPathType,omitempty"`
}

// ExtraVolumeMount references an existing pod volume and defines where
// to mount it inside preflight init containers. The volume itself must
// already exist in the pod spec (typically injected by a platform webhook).
type ExtraVolumeMount struct {
	// Name is the volume name that already exists in the pod spec.
	Name string `yaml:"name"`

	// MountPath is the path inside the init container.
	MountPath string `yaml:"mountPath"`

	// ReadOnly controls whether the mount is read-only. Defaults to true.
	ReadOnly *bool `yaml:"readOnly,omitempty"`
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

	// Default to mirroring DRA claims when gang coordination is enabled.
	// Init containers need the same device access (GPUs, RDMA, IMEX) as
	// main containers for multi-node NCCL tests.
	if c.MirrorResourceClaims == nil {
		t := true
		c.MirrorResourceClaims = &t
	}

	trueVal := true

	for i := range c.ExtraHostPathMounts {
		if c.ExtraHostPathMounts[i].ReadOnly == nil {
			c.ExtraHostPathMounts[i].ReadOnly = &trueVal
		}
	}

	for i := range c.ExtraVolumeMounts {
		if c.ExtraVolumeMounts[i].ReadOnly == nil {
			c.ExtraVolumeMounts[i].ReadOnly = &trueVal
		}
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
