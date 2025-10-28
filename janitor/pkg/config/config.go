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
	"time"

	"github.com/spf13/viper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Config represents the janitor configuration structure
type Config struct {
	Global        GlobalConfig                  `mapstructure:"global" json:"global"`
	RebootNode    RebootNodeControllerConfig    `mapstructure:"rebootNodeController" json:"rebootNodeController"`
	TerminateNode TerminateNodeControllerConfig `mapstructure:"terminateNodeController" json:"terminateNodeController"`
}

// GlobalConfig contains global janitor settings
type GlobalConfig struct {
	Timeout    time.Duration `mapstructure:"timeout" json:"timeout"`
	ManualMode bool          `mapstructure:"manualMode" json:"manualMode"`
	Nodes      NodeConfig    `mapstructure:"nodes" json:"nodes"`
}

// NodeConfig contains configuration for nodes
type NodeConfig struct {
	Exclusions []metav1.LabelSelector `mapstructure:"exclusions" json:"exclusions"`
}

// RebootNodeControllerConfig contains configuration for reboot node controller
type RebootNodeControllerConfig struct {
	// Enabled indicates if the controller is enabled
	Enabled bool
	// ManualMode indicates if the controller should skip sending reboot signals
	ManualMode bool
	// Timeout for reboot operations
	Timeout time.Duration
	// NodeExclusions defines label selectors for nodes that should be excluded from reboot operations
	// Nodes matching any of these label selectors will be rejected by the admission webhook
	NodeExclusions []metav1.LabelSelector
}

// TerminateNodeControllerConfig contains configuration for terminate node controller
type TerminateNodeControllerConfig struct {
	// Enabled indicates if the controller is enabled
	Enabled bool
	// ManualMode indicates if the controller should skip sending terminate signals
	ManualMode bool
	// Timeout for terminate operations
	Timeout time.Duration
	// NodeExclusions defines label selectors for nodes that should be excluded from terminate operations
	// Nodes matching any of these label selectors will be rejected by the admission webhook
	NodeExclusions []metav1.LabelSelector
}

// LoadConfig loads configuration from a YAML file using Viper
func LoadConfig(configPath string) (*Config, error) {
	v := viper.New()

	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.SetConfigName("janitor-config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("/etc/nvsentinel/janitor/")
		v.AddConfigPath("$HOME/.nvsentinel/janitor/")
	}

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok { //nolint:errorlint
			// File not found, using defaults
		} else {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
	}

	var config Config
	if err := v.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Apply node exclusions from global config to controller-specific configs
	config.RebootNode.NodeExclusions = config.Global.Nodes.Exclusions
	config.TerminateNode.NodeExclusions = config.Global.Nodes.Exclusions

	return &config, nil
}
