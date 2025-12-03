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

package reconciler

import (
	"fmt"
	"path/filepath"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
)

// CRTypeConfig holds configuration for a specific CR type
type CRTypeConfig struct {
	TemplateFileName string
	MaintenanceResource config.MaintenanceResource
}

// CRTypeSelector selects the appropriate CR type configuration based on the recommended action
type CRTypeSelector struct {
	configs map[string]CRTypeConfig
	basePath string
}

// NewCRTypeSelector creates a new CR type selector
func NewCRTypeSelector(basePath string) *CRTypeSelector {
	return &CRTypeSelector{
		basePath: basePath,
		configs: map[string]CRTypeConfig{
			"restart": {
				TemplateFileName: "rebootnode-template.yaml",
				MaintenanceResource: config.MaintenanceResource{
					ApiGroup:              "janitor.dgxc.nvidia.com",
					Version:               "v1alpha1",
					Kind:                  "RebootNode",
					CompleteConditionType: "NodeReady",
				},
			},
			"datacenter": {
				TemplateFileName: "datacenterremediation-template.yaml",
				MaintenanceResource: config.MaintenanceResource{
					ApiGroup:              "janitor.dgxc.nvidia.com",
					Version:               "v1alpha1",
					Kind:                  "DataCenterRemediationRequest",
					CompleteConditionType: "RemediationComplete",
				},
			},
		},
	}
}

// GetCRTypeConfig returns the CR type configuration for a given recommended action
func (s *CRTypeSelector) GetCRTypeConfig(action protos.RecommendedAction) (*CRTypeConfig, error) {
	group := common.GetRemediationGroupForAction(action)
	if group == "" {
		return nil, fmt.Errorf("no remediation group found for action %s", action.String())
	}

	config, exists := s.configs[group]
	if !exists {
		return nil, fmt.Errorf("no CR type configuration found for group %s", group)
	}

	return &config, nil
}

// GetTemplatePath returns the full path to the template file for a given action
func (s *CRTypeSelector) GetTemplatePath(action protos.RecommendedAction) (string, error) {
	config, err := s.GetCRTypeConfig(action)
	if err != nil {
		return "", err
	}

	return filepath.Join(s.basePath, config.TemplateFileName), nil
}

// GetTemplateDataForAction creates template data with appropriate fields for the action
func (s *CRTypeSelector) GetTemplateDataForAction(action protos.RecommendedAction, nodeName, healthEventID string) (*TemplateData, error) {
	config, err := s.GetCRTypeConfig(action)
	if err != nil {
		return nil, err
	}

	templateData := &TemplateData{
		NodeName:            nodeName,
		HealthEventID:       healthEventID,
		RecommendedAction:   action,
		TemplateMountPath:   s.basePath,
		TemplateFileName:    config.TemplateFileName,
		MaintenanceResource: config.MaintenanceResource,
	}

	// Add specific fields for datacenter remediation
	if common.GetRemediationGroupForAction(action) == "datacenter" {
		templateData.RemediationReason = s.getRemediationReason(action)
	}

	return templateData, nil
}

// getRemediationReason maps action to remediation reason
func (s *CRTypeSelector) getRemediationReason(action protos.RecommendedAction) string {
	switch action {
	case protos.RecommendedAction_REPLACE_VM:
		return "VM needs replacement"
	case protos.RecommendedAction_CONTACT_SUPPORT:
		return "Manual intervention required"
	default:
		return "Automated remediation failed"
	}
}