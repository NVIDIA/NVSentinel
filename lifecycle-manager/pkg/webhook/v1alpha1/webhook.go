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

package v1alpha1

import (
	"context"
	"fmt"
	"slices"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

var webhookLog = logf.Log.WithName("validationrequest-webhook")

func SetupWebhookWithManager(mgr ctrl.Manager, cfg *v1alpha1.ValidationConfiguration, enabled bool) error {
	uncachedClient, err := client.New(mgr.GetConfig(), client.Options{
		Scheme: mgr.GetScheme(),
	})
	if err != nil {
		return fmt.Errorf("failed to create uncached client: %w", err)
	}

	validator := &ValidationRequestValidator{
		Enabled: enabled,
		Config:  cfg,
		Client:  uncachedClient,
	}

	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.ValidationRequest{}).
		WithValidator(validator).
		Complete(); err != nil {
		return err
	}

	return nil
}

// +kubebuilder:object:generate=false
type ValidationRequestValidator struct {
	Enabled bool
	Config  *v1alpha1.ValidationConfiguration
	Client  client.Client
}

func (v *ValidationRequestValidator) ValidateCreate(_ context.Context,
	obj *v1alpha1.ValidationRequest) (admission.Warnings, error) {
	webhookLog.Info("Validating ValidationRequest on create", "name", obj.Name)

	if !v.Enabled {
		return nil, fmt.Errorf("ValidationRequest controller is disabled")
	}

	if err := v.validateSpec(obj); err != nil {
		return nil, err
	}

	return nil, nil
}

func (v *ValidationRequestValidator) ValidateUpdate(_ context.Context,
	oldObj, newObj *v1alpha1.ValidationRequest) (admission.Warnings, error) {
	webhookLog.Info("Validating ValidationRequest on update", "name", newObj.Name)

	if !v.Enabled {
		return nil, fmt.Errorf("ValidationRequest controller is disabled")
	}

	nodesChanged := !slices.EqualFunc(oldObj.Spec.Nodes, newObj.Spec.Nodes,
		func(a, b v1alpha1.NodeSpec) bool { return a.Name == b.Name })

	if nodesChanged {
		return nil, fmt.Errorf("spec.nodes is immutable after creation")
	}

	if !slices.Equal(oldObj.Spec.Tests, newObj.Spec.Tests) {
		return nil, fmt.Errorf("spec.tests is immutable after creation")
	}

	return nil, nil
}

func (v *ValidationRequestValidator) ValidateDelete(_ context.Context,
	obj *v1alpha1.ValidationRequest) (admission.Warnings, error) {
	webhookLog.Info("Validating ValidationRequest on delete", "name", obj.Name)

	return nil, nil
}

func (v *ValidationRequestValidator) validateSpec(obj *v1alpha1.ValidationRequest) error {
	if len(obj.Spec.Nodes) == 0 {
		return fmt.Errorf("spec.nodes must contain at least one node")
	}

	seenNodes := make(map[string]bool, len(obj.Spec.Nodes))

	for _, node := range obj.Spec.Nodes {
		if len(node.Name) == 0 {
			return fmt.Errorf("spec.nodes contains an entry with an empty name")
		}

		if _, ok := seenNodes[node.Name]; ok {
			return fmt.Errorf("spec.nodes contains duplicate node %q", node.Name)
		}

		seenNodes[node.Name] = true
	}

	return v.validateTestNames(obj)
}

func (v *ValidationRequestValidator) validateTestNames(obj *v1alpha1.ValidationRequest) error {
	tests := obj.Spec.Tests
	if len(tests) == 0 {
		tests = v.Config.Spec.DefaultTests
	}

	if len(tests) == 0 {
		return fmt.Errorf("spec.tests must be set, or defaultTests must be configured in ValidationConfiguration")
	}

	seenTests := make(map[string]bool, len(tests))
	for _, test := range tests {
		if _, ok := v.Config.Spec.Tests[test]; !ok {
			return fmt.Errorf("test %q is not defined in the ValidationConfiguration", test)
		}

		if seenTests[test] {
			return fmt.Errorf("spec.tests contains duplicate test %q", test)
		}

		seenTests[test] = true
	}

	return nil
}
