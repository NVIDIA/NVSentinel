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
	"log/slog"
	"os"

	"github.com/BurntSushi/toml"
)

const (
	DefaultMaintenanceEventPollIntervalSeconds       = 60
	DefaultTriggerQuarantineWorkflowTimeLimitMinutes = 30
	DefaultPostMaintenanceHealthyDelayMinutes        = 15
	DefaultNodeReadinessTimeoutMinutes               = 60

	MinMaintenanceEventPollIntervalSeconds       = 10
	MinTriggerQuarantineWorkflowTimeLimitMinutes = 1
	MinPostMaintenanceHealthyDelayMinutes        = 1
	MinNodeReadinessTimeoutMinutes               = 1

	minCSPSpecificPollingIntervalSeconds = 30
)

type Config struct {
	MaintenanceEventPollIntervalSeconds       int       `toml:"maintenanceEventPollIntervalSeconds"`
	TriggerQuarantineWorkflowTimeLimitMinutes int       `toml:"triggerQuarantineWorkflowTimeLimitMinutes"`
	PostMaintenanceHealthyDelayMinutes        int       `toml:"postMaintenanceHealthyDelayMinutes"`
	NodeReadinessTimeoutMinutes               int       `toml:"nodeReadinessTimeoutMinutes"`
	ClusterName                               string    `toml:"clusterName"`
	GCP                                       GCPConfig `toml:"gcp"`
	AWS                                       AWSConfig `toml:"aws"`
}

// GCPConfig holds GCP specific configuration.
type GCPConfig struct {
	Enabled                   bool   `toml:"enabled"`
	TargetProjectID           string `toml:"targetProjectId"`
	APIPollingIntervalSeconds int    `toml:"apiPollingIntervalSeconds"`
	LogFilter                 string `toml:"logFilter"`
}

// AWSConfig holds AWS specific configuration.
type AWSConfig struct {
	Enabled                bool   `toml:"enabled"`
	AccountID              string `toml:"accountId"`
	PollingIntervalSeconds int    `toml:"pollingIntervalSeconds"`
	Region                 string `toml:"region"`
}

// LoadConfig reads the configuration from a TOML file.
func LoadConfig(filePath string) (*Config, error) {
	var cfg Config
	// Read the file content
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", filePath, err)
	}

	// Decode the TOML content
	if _, err := toml.Decode(string(content), &cfg); err != nil {
		return nil, fmt.Errorf("failed to decode config file %s: %w", filePath, err)
	}

	// Apply defaults for global settings if not provided
	applyDefaults(&cfg)

	// Validate general configuration (loglevel, clusterName, intervals)
	if err := validateGeneralConfig(&cfg); err != nil {
		return nil, fmt.Errorf("general config validation failed: %w", err)
	}

	// Validate CSP-specific configuration (polling intervals, single CSP)
	if err := validateCSPConfig(&cfg); err != nil {
		return nil, fmt.Errorf("CSP config validation failed: %w", err)
	}

	return &cfg, nil
}

// applyDefaults assigns default values to zero-value fields.
func applyDefaults(cfg *Config) {
	if cfg.MaintenanceEventPollIntervalSeconds == 0 {
		slog.Info("Configuration not set, applying default",
			"setting", "maintenanceEventPollIntervalSeconds",
			"default", DefaultMaintenanceEventPollIntervalSeconds)

		cfg.MaintenanceEventPollIntervalSeconds = DefaultMaintenanceEventPollIntervalSeconds
	}

	if cfg.TriggerQuarantineWorkflowTimeLimitMinutes == 0 {
		slog.Info("Configuration not set, applying default",
			"setting", "triggerQuarantineWorkflowTimeLimitMinutes",
			"default", DefaultTriggerQuarantineWorkflowTimeLimitMinutes)

		cfg.TriggerQuarantineWorkflowTimeLimitMinutes = DefaultTriggerQuarantineWorkflowTimeLimitMinutes
	}

	if cfg.PostMaintenanceHealthyDelayMinutes == 0 {
		slog.Info("Configuration not set, applying default",
			"setting", "postMaintenanceHealthyDelayMinutes",
			"default", DefaultPostMaintenanceHealthyDelayMinutes)

		cfg.PostMaintenanceHealthyDelayMinutes = DefaultPostMaintenanceHealthyDelayMinutes
	}

	if cfg.NodeReadinessTimeoutMinutes == 0 {
		slog.Info("Configuration not set, applying default",
			"setting", "nodeReadinessTimeoutMinutes",
			"default", DefaultNodeReadinessTimeoutMinutes)

		cfg.NodeReadinessTimeoutMinutes = DefaultNodeReadinessTimeoutMinutes
	}
}

// validateGeneralConfig checks and enforces settings for logging and global timeouts.
func validateGeneralConfig(cfg *Config) error {
	// Validate ClusterName
	if cfg.ClusterName == "" {
		return fmt.Errorf("clusterName must be set in the configuration")
	}

	// Validate MaintenanceEventPollIntervalSeconds
	if cfg.MaintenanceEventPollIntervalSeconds < MinMaintenanceEventPollIntervalSeconds {
		return fmt.Errorf(
			"maintenanceEventPollIntervalSeconds must be at least %d seconds (got %d)",
			MinMaintenanceEventPollIntervalSeconds,
			cfg.MaintenanceEventPollIntervalSeconds,
		)
	}

	// Validate TriggerQuarantineWorkflowTimeLimitMinutes
	if cfg.TriggerQuarantineWorkflowTimeLimitMinutes < MinTriggerQuarantineWorkflowTimeLimitMinutes {
		return fmt.Errorf(
			"triggerQuarantineWorkflowTimeLimitMinutes must be at least %d minute(s) (got %d)",
			MinTriggerQuarantineWorkflowTimeLimitMinutes,
			cfg.TriggerQuarantineWorkflowTimeLimitMinutes,
		)
	}

	// Validate PostMaintenanceHealthyDelayMinutes
	if cfg.PostMaintenanceHealthyDelayMinutes < MinPostMaintenanceHealthyDelayMinutes {
		return fmt.Errorf(
			"postMaintenanceHealthyDelayMinutes must be at least %d minute(s) (got %d)",
			MinPostMaintenanceHealthyDelayMinutes,
			cfg.PostMaintenanceHealthyDelayMinutes,
		)
	}

	// Validate NodeReadinessTimeoutMinutes
	if cfg.NodeReadinessTimeoutMinutes < MinNodeReadinessTimeoutMinutes {
		return fmt.Errorf(
			"nodeReadinessTimeoutMinutes must be at least %d minute(s) (got %d)",
			MinNodeReadinessTimeoutMinutes,
			cfg.NodeReadinessTimeoutMinutes,
		)
	}

	return nil
}

// validateCSPConfig checks GCP/AWS polling intervals and ensures only one CSP is enabled.
func validateCSPConfig(cfg *Config) error {
	// Validate GCP polling interval
	if cfg.GCP.Enabled && cfg.GCP.APIPollingIntervalSeconds < minCSPSpecificPollingIntervalSeconds {
		return fmt.Errorf(
			"gcp.apiPollingIntervalSeconds must be at least %d seconds (got %d)",
			minCSPSpecificPollingIntervalSeconds,
			cfg.GCP.APIPollingIntervalSeconds,
		)
	}

	// Validate AWS polling interval
	if cfg.AWS.Enabled && cfg.AWS.PollingIntervalSeconds < minCSPSpecificPollingIntervalSeconds {
		return fmt.Errorf(
			"aws.pollingIntervalSeconds must be at least %d seconds (got %d)",
			minCSPSpecificPollingIntervalSeconds,
			cfg.AWS.PollingIntervalSeconds,
		)
	}

	// Ensure only one CSP is enabled
	if cfg.GCP.Enabled && cfg.AWS.Enabled {
		return fmt.Errorf("multiple CSPs enabled: only one of GCP or AWS can be enabled at a time in the configuration")
	}

	return nil
}
