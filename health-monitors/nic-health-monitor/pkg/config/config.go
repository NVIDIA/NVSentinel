// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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

	"gopkg.in/yaml.v3"
)

// Config represents the NIC Health Monitor configuration loaded from YAML.
type Config struct {
	// NicExclusionRegex contains comma-separated regex patterns for NICs to exclude
	NicExclusionRegex string `yaml:"nicExclusionRegex"`

	// NicInclusionRegexOverride, when non-empty, bypasses automatic device discovery
	// and monitors only NIC devices whose names match these comma-separated regex patterns.
	NicInclusionRegexOverride string `yaml:"nicInclusionRegexOverride"`

	// SysClassNetPath is the sysfs path for network interfaces (container mount point)
	SysClassNetPath string `yaml:"sysClassNetPath"`

	// SysClassInfinibandPath is the sysfs path for InfiniBand devices (container mount point)
	SysClassInfinibandPath string `yaml:"sysClassInfinibandPath"`

	// CounterDetection contains counter monitoring configuration
	CounterDetection CounterDetectionConfig `yaml:"counterDetection"`
}

// CounterDetectionConfig contains the configuration for counter-based monitoring.
type CounterDetectionConfig struct {
	// Enabled controls whether counter monitoring is active
	Enabled bool `yaml:"enabled"`
	// Counters is the list of counter definitions to monitor
	Counters []CounterConfig `yaml:"counters"`
}

// CounterConfig defines a single counter to monitor.
type CounterConfig struct {
	// Name is the counter identifier (e.g., "link_downed")
	Name string `yaml:"name"`
	// Path is the sysfs path relative to the port directory (e.g., "counters/link_downed")
	Path string `yaml:"path"`
	// Enabled controls whether this counter is monitored
	Enabled bool `yaml:"enabled"`
	// IsFatal indicates whether threshold breach triggers a Fatal event
	IsFatal bool `yaml:"isFatal"`
	// ThresholdType is either "delta" (absolute change) or "velocity" (rate per time unit)
	ThresholdType string `yaml:"thresholdType"`
	// Threshold is the numeric threshold value
	Threshold float64 `yaml:"threshold"`
	// VelocityUnit is the time unit for velocity thresholds: "second", "minute", "hour"
	VelocityUnit string `yaml:"velocityUnit,omitempty"`
	// Description is a human-readable description for event messages
	Description string `yaml:"description"`
	// RecommendedAction is the action for events (e.g., "REPLACE_VM", "NONE")
	RecommendedAction string `yaml:"recommendedAction"`
}

// LoadConfig reads and parses the YAML configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}

	if cfg.SysClassNetPath == "" {
		cfg.SysClassNetPath = "/nvsentinel/sys/class/net"
	}

	if cfg.SysClassInfinibandPath == "" {
		cfg.SysClassInfinibandPath = "/nvsentinel/sys/class/infiniband"
	}

	return cfg, nil
}
