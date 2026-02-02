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

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
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
	// Note that if an ImpactedEntityScope is defined, the EffectiveEquivalenceGroup will
	// use the EquivalenceGroup as the prefix and the suffix will be the EntityValue for
	// the EntityType referenced in ImpactedEntityScope. For example, if the EquivalenceGroup
	// is "reset" and the ImpactedEntityScope is GPU_UUID, the EffectiveEquivalenceGroup
	// will be reset-GPU-123 where GPU-123 is the EntityValue for the GPU_UUID EntityType.
	// If no ImpactedEntityScope is defined, we will use the EquivalenceGroup as the
	// EffectiveEquivalenceGroup.
	ImpactedEntityScope string `toml:"impactedEntityScope"`
	// SupersedingEquivalenceGroups allow EquivalenceGroups to have membership
	// in other EquivalenceGroups. For example, we will count GPU resets
	// EquivalenceGroups like reset-GPU-123 as being a member in restart because a
	// node reboot has the same effect as a GPU reset.
	SupersedingEquivalenceGroups []string `toml:"supersedingEquivalenceGroups"`
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
	if err := c.validateEquivalenceGroup(actionName, resource); err != nil {
		return err
	}

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

/*
EquivalenceGroup requirements:
- All MaintenanceResources must have an EquivalenceGroup defined.
- Any SupersedingEquivalenceGroup must have a corresponding MaintenanceResource which cannot include an
ImpactedEntityScope. In the future, we could update our SupersedingEquivalenceGroup logic to match the EquivalenceGroup
(rather than require an exact EffectiveEquivalenceGroup match), however we will punt on this complexity since there's
not a use-case for it.
- The SupersedingEquivalenceGroups cannot include the EquivalenceGroup for the same MaintenanceResource.
- Only the COMPONENT_RESET recommended action can have a MaintenanceResource which includes an ImpactedEntityScope.
Additionally, the ImpactedEntityScope must be included in EntityTypeToResourceNames (meaning that partial draining is
enabled for that entity).
*/
// nolint:cyclop
func (c *TomlConfig) validateEquivalenceGroup(actionName string, resource MaintenanceResource) error {
	if len(resource.EquivalenceGroup) == 0 {
		return fmt.Errorf("action '%s' must have a non-empty EquivalenceGroup", actionName)
	}

	for _, group := range resource.SupersedingEquivalenceGroups {
		foundGroup := false

		for _, maintenanceResource := range c.RemediationActions {
			if group == maintenanceResource.EquivalenceGroup {
				if len(maintenanceResource.ImpactedEntityScope) != 0 {
					return fmt.Errorf("supersedingEquivalenceGroup %s cannot have an impactedEntityScopes: %s",
						maintenanceResource.EquivalenceGroup, maintenanceResource.ImpactedEntityScope)
				}

				foundGroup = true
			}
		}

		if !foundGroup {
			return fmt.Errorf("superseding EquivalenceGroup %s must be defined in config", group)
		}

		if group == resource.EquivalenceGroup {
			return fmt.Errorf("SupersedingEquivalenceGroup cannot include the EquivalenceGroup itself: %s", group)
		}
	}

	if len(resource.ImpactedEntityScope) != 0 {
		if actionName != protos.RecommendedAction_COMPONENT_RESET.String() {
			return fmt.Errorf("action '%s' cannot have an ImpactedEntityScope defined", actionName)
		}

		if _, ok := model.EntityTypeToResourceNames[resource.ImpactedEntityScope]; !ok {
			return fmt.Errorf("impacted entity for action does not support partial draining: %s",
				resource.ImpactedEntityScope)
		}
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
