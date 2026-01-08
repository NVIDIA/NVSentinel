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

// Package config provides configuration structures and validation for fault remediation.
// It defines the TOML configuration schema for multi-template support, maintenance resources,
// and action-specific remediation settings.
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// MaintenanceResource holds configuration for maintenance custom resources
type MaintenanceResource struct {
	Namespace             string `toml:"namespace"`
	Version               string `toml:"version"`
	ApiGroup              string `toml:"apiGroup"`
	Kind                  string `toml:"kind"`
	CompleteConditionType string `toml:"completeConditionType"`

	// Scope determines if the resource is cluster-scoped or namespaced
	Scope string `toml:"scope"` // "Cluster" or "Namespaced"

	// Template file path relative to the mount path
	TemplateFileName string `toml:"templateFileName"`

	// EquivalenceGroup defines which actions are considered equivalent for deduplication
	// Actions in the same group will deduplicate against each other regardless of CRD type
	// Example: "restart" group may contain RESTART_BM (Atlas) and COMPONENT_RESET (NVIDIA)
	EquivalenceGroup string `toml:"equivalenceGroup"`
}

// Template holds configuration for template files mount
type Template struct {
	MountPath string `toml:"mountPath"`
	FileName  string `toml:"fileName"`
}

// UpdateRetry holds configuration for update retry behavior
type UpdateRetry struct {
	MaxRetries        int `toml:"maxRetries"`
	RetryDelaySeconds int `toml:"retryDelaySeconds"`
}

// TomlConfig holds the complete TOML configuration for fault remediation
type TomlConfig struct {
	// Template mount configuration
	Template Template `toml:"template"`

	// Multi-template configuration - map from RecommendedAction string to MaintenanceResource
	RemediationActions map[string]MaintenanceResource `toml:"remediationActions"`

	// Templates contains the actual template content keyed by filename
	Templates map[string]string `toml:"templates"`

	// Common configuration
	UpdateRetry UpdateRetry `toml:"updateRetry"`
}

// Validate checks the configuration for consistency and completeness
func (c *TomlConfig) Validate() error {
	if err := c.validateTemplate(); err != nil {
		return err
	}

	for actionName, resource := range c.RemediationActions {
		if err := c.validateRemediationAction(actionName, resource); err != nil {
			return err
		}
	}

	return nil
}

func (c *TomlConfig) validateTemplate() error {
	if c.Template.MountPath == "" {
		return fmt.Errorf("template mountPath must be non-empty")
	}

	return nil
}

func (c *TomlConfig) validateRemediationAction(actionName string, resource MaintenanceResource) error {
	if resource.TemplateFileName == "" {
		return fmt.Errorf("action '%s' must have a non-empty templateFileName", actionName)
	}

	if err := c.validateTemplateFileExists(actionName, resource.TemplateFileName); err != nil {
		return err
	}

	if err := validateScope(actionName, resource.Scope, resource.Namespace); err != nil {
		return err
	}

	return nil
}

func (c *TomlConfig) validateTemplateFileExists(actionName, templateFileName string) error {
	templatePath := filepath.Join(c.Template.MountPath, templateFileName)

	_, err := os.Stat(templatePath)
	if err == nil {
		return nil
	}

	if os.IsNotExist(err) {
		return fmt.Errorf(
			"action '%s' references template file '%s' which does not exist at path '%s'",
			actionName, templateFileName, templatePath,
		)
	}

	return fmt.Errorf(
		"action '%s' cannot access template file '%s' at path '%s': %w",
		actionName, templateFileName, templatePath, err,
	)
}

func validateScope(actionName, scope, namespace string) error {
	switch scope {
	case "", "Cluster":
		return nil
	case "Namespaced":
		if namespace == "" {
			return fmt.Errorf("action '%s' is Namespaced but no namespace is specified", actionName)
		}

		return nil
	default:
		return fmt.Errorf(
			"action '%s' has invalid scope '%s', must be 'Cluster' or 'Namespaced'",
			actionName, scope,
		)
	}
}
