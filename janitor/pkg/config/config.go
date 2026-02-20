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
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/janitor/pkg/gpuservices"
)

// Config represents the janitor configuration structure
type Config struct {
	Global        GlobalConfig                  `mapstructure:"global" json:"global"`
	RebootNode    RebootNodeControllerConfig    `mapstructure:"rebootNodeController" json:"rebootNodeController"`
	TerminateNode TerminateNodeControllerConfig `mapstructure:"terminateNodeController" json:"terminateNodeController"`
	GPUReset      GPUResetControllerConfig      `mapstructure:"gpuResetController" json:"gpuResetController"`
}

// GlobalConfig contains global janitor settings
type GlobalConfig struct {
	Timeout         time.Duration `mapstructure:"timeout" json:"timeout"`
	ManualMode      *bool         `mapstructure:"manualMode" json:"manualMode"`
	Nodes           NodeConfig    `mapstructure:"nodes" json:"nodes"`
	CSPProviderHost string        `mapstructure:"cspProviderHost" json:"cspProviderHost"`
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
	ManualMode *bool
	// Timeout for reboot operations
	Timeout time.Duration
	// Exclusions defines label selectors for nodes that should be excluded from reboot operations
	// Nodes matching any of these label selectors will be rejected by the admission webhook
	Exclusions []metav1.LabelSelector
	// CSPProviderHost is the host of the CSP provider
	CSPProviderHost string
}

// TerminateNodeControllerConfig contains configuration for terminate node controller
type TerminateNodeControllerConfig struct {
	// Enabled indicates if the controller is enabled
	Enabled bool
	// ManualMode indicates if the controller should skip sending terminate signals
	ManualMode *bool
	// Timeout for terminate operations
	Timeout time.Duration
	// NodeExclusions defines label selectors for nodes that should be excluded from terminate operations
	// Nodes matching any of these label selectors will be rejected by the admission webhook
	Exclusions []metav1.LabelSelector
	// CSPProviderHost is the host of the CSP provider
	CSPProviderHost string
}

// GPUResetControllerConfig contains configuration for gpu reset controller
type GPUResetControllerConfig struct {
	Enabled         bool                   `mapstructure:"enabled" json:"enabled"`
	ManualMode      *bool                  `mapstructure:"manualMode" json:"manualMode"`
	Timeout         time.Duration          `mapstructure:"timeout" json:"timeout"`
	Mock            bool                   `mapstructure:"mock" json:"mock"`
	Exclusions      []metav1.LabelSelector `mapstructure:"exclusions" json:"exclusions"`
	CSPProviderHost string                 `mapstructure:"cspProviderHost" json:"cspProviderHost"`
	ServiceManager  gpuservices.Manager    `mapstructure:"serviceManager" json:"serviceManager"`
	// reset ResetJob will be used to construct the ResolvedJobTemplate from the default Job template
	ResetJob            ResetJobConfig `mapstructure:"resetJob" json:"resetJob"`
	ResolvedJobTemplate *batchv1.JobTemplateSpec
}

type ResetJobConfig struct {
	ImageConfig      ImageConfig          `mapstructure:"imageConfig" json:"imageConfig"`
	Resources        ResourceRequirements `mapstructure:"resources" json:"resources"`
	RuntimeClassName string               `mapstructure:"runtimeClassName" json:"runtimeClassName"`
}

type ResourceRequirements struct {
	Limits   map[string]string `mapstructure:"limits" json:"limits"`
	Requests map[string]string `mapstructure:"requests" json:"requests"`
}

type ImageConfig struct {
	Image            string            `mapstructure:"image" json:"image"`
	ImagePullSecrets []ImagePullSecret `mapstructure:"imagePullSecrets" json:"imagePullSecrets"`
}

type ImagePullSecret struct {
	Name string `mapstructure:"name" json:"name"`
}

// LoadConfig loads configuration from a YAML file using Viper
func LoadConfig(configPath string, namespace string) (*Config, error) {
	// Using "::" as the key delimiter instead of the default "." to prevent
	// Viper from splitting map keys (e.g., k8s labels/annotations) into nested objects.
	v := viper.NewWithOptions(viper.KeyDelimiter("::"))

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
	if err := v.Unmarshal(&config, func(dc *mapstructure.DecoderConfig) {
		dc.DecodeHook = mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		)
		dc.ErrorUnused = true
		dc.Metadata = nil
		dc.WeaklyTypedInput = true
		dc.TagName = "mapstructure"
	}); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	applyConfigDefaults(&config)

	if config.GPUReset.Enabled {
		if len(config.GPUReset.ResetJob.ImageConfig.Image) == 0 {
			return nil, fmt.Errorf("ResetJob.ImageConfig.Image is required but not set")
		}

		jobTemplate, err := getDefaultGPUResetJobTemplate(namespace,
			config.GPUReset.ResetJob.ImageConfig.Image, config.GPUReset.ResetJob.ImageConfig.ImagePullSecrets,
			config.GPUReset.ResetJob.Resources, config.GPUReset.ResetJob.RuntimeClassName)
		if err != nil {
			return nil, err
		}

		config.GPUReset.ResolvedJobTemplate = jobTemplate
	}

	cfgJSON, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config: %w", err)
	}

	slog.Info("Loaded configuration", "config", string(cfgJSON))

	return &config, nil
}
