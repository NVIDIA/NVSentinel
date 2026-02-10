// Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gpuservices

import (
	"fmt"
	"time"
)

// Manager represents a specific GPU service manager configuration,
// combining its resolved name with the detailed specification used for teardown.
type Manager struct {
	Name string      `mapstructure:"name" json:"name"`
	Spec ManagerSpec `mapstructure:"spec" json:"spec"`
}

// ManagerSpec allows for the inline definition of a custom component and its applications.
type ManagerSpec struct {
	// A map of labels common to all applications managed by this component.
	// Typically this would be 'app.kubernetes.io/managed-by': 'some-operator'.
	ManagerSelector map[string]string `mapstructure:"managerSelector" json:"managerSelector"`

	// The namespace where the managed applications are running.
	Namespace string `mapstructure:"namespace" json:"namespace"`

	// The list of specific applications to tear down and restore.
	Apps []AppSpec `mapstructure:"apps" json:"apps"`

	// TeardownTimeout defines the maximum time to wait for managed service pods to terminate.
	TeardownTimeout time.Duration `mapstructure:"teardownTimeout" json:"teardownTimeout"`

	// RestoreTimeout defines the maximum time to wait for managed service pods to become ready.
	RestoreTimeout time.Duration `mapstructure:"restoreTimeout" json:"restoreTimeout"`
}

// AppSpec defines how to find and manage a single application.
type AppSpec struct {
	// A map of labels to select the pods for this specific application.
	// This is combined with the manager's selector to form the final pod selector.
	AppSelector map[string]string `mapstructure:"appSelector" json:"appSelector"`

	// The node label used to control the deployment of this application.
	NodeLabel string `mapstructure:"nodeLabel" json:"nodeLabel"`

	// The value to set the NodeLabel to for enabling/deploying the application.
	// Defaults to "true" if not specified.
	EnabledValue string `mapstructure:"enabledValue" json:"enabledValue"`

	// The value to set the NodeLabel to for disabling/undeploying the application.
	// Defaults to "false" if not specified.
	DisabledValue string `mapstructure:"disabledValue" json:"disabledValue"`
}

// Registry holds all known, pre-defined Manager configurations.
var Registry = map[string]ManagerSpec{
	"gpu-operator": {
		ManagerSelector: map[string]string{"app.kubernetes.io/managed-by": "gpu-operator"},
		Namespace:       "gpu-operator",
		Apps: []AppSpec{
			{
				AppSelector:   map[string]string{"app": "nvidia-device-plugin-daemonset"},
				NodeLabel:     "nvidia.com/gpu.deploy.device-plugin",
				EnabledValue:  "true",
				DisabledValue: "false",
			},
			{
				AppSelector:   map[string]string{"app": "nvidia-dcgm"},
				NodeLabel:     "nvidia.com/gpu.deploy.dcgm",
				EnabledValue:  "true",
				DisabledValue: "false",
			},
			{
				AppSelector:   map[string]string{"app": "nvidia-dcgm-exporter"},
				NodeLabel:     "nvidia.com/gpu.deploy.dcgm-exporter",
				EnabledValue:  "true",
				DisabledValue: "false",
			},
		},
		TeardownTimeout: 5 * time.Minute,
		RestoreTimeout:  10 * time.Minute,
	},
	// add additional GPU services managers here
}

// NewManager constructs and resolves a Manager.
// If only 'name' is provided (and the spec is empty), it attempts to load the full configuration from the registry.
// If both 'name' and a non-empty 'spec' are provided, the custom spec is prioritized and validated.
func NewManager(name string, spec ManagerSpec) (Manager, error) {
	if name == "" {
		return Manager{Name: ""}, nil
	}

	isSpecEmpty := spec.Namespace == "" && len(spec.Apps) == 0
	if isSpecEmpty {
		registrySpec, found := Registry[name]
		if !found {
			return Manager{}, fmt.Errorf("GPU services manager '%s' not found in registry", name)
		}

		return Manager{Name: name, Spec: registrySpec}, nil
	}

	if spec.Namespace == "" {
		return Manager{}, fmt.Errorf("GPU services manager Namespace must be set when providing a custom spec")
	}

	if len(spec.Apps) == 0 {
		return Manager{}, fmt.Errorf("GPU services manager Apps list must not be empty when providing a custom spec")
	}

	if spec.TeardownTimeout < 0 {
		return Manager{}, fmt.Errorf("GPU services manager TeardownTimeout must be a non-negative value")
	}

	if spec.RestoreTimeout < 0 {
		return Manager{}, fmt.Errorf("GPU services manager RestoreTimeout must be a non-negative value")
	}

	return Manager{Name: name, Spec: spec}, nil
}
