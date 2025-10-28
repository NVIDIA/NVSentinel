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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	janitordgxcnvidiacomv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
)

// nolint:unused
// log is for logging in this package.
var janitorWebhookLog = logf.Log.WithName("janitor-webhook")

const (
	controllerTypeRebootNode    = "RebootNode"
	controllerTypeTerminateNode = "TerminateNode"
)

// SetupJanitorWebhookWithManager registers the webhook for CRs managed by Janitor.
func SetupJanitorWebhookWithManager(mgr ctrl.Manager, cfg *config.Config) error {
	validator := &JanitorCustomValidator{
		Config: cfg,
		Client: mgr.GetClient(),
	}

	// Register webhook for RebootNode
	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&janitordgxcnvidiacomv1alpha1.RebootNode{}).
		WithValidator(validator).
		Complete(); err != nil {
		return err
	}

	// Register webhook for TerminateNode
	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&janitordgxcnvidiacomv1alpha1.TerminateNode{}).
		WithValidator(validator).
		Complete(); err != nil {
		return err
	}

	return nil
}

// nolint:lll
// +kubebuilder:webhook:path=/validate-janitor-dgxc-nvidia-com-v1alpha1-rebootnode,mutating=false,failurePolicy=fail,sideEffects=None,groups=janitor.dgxc.nvidia.com,resources=rebootnodes,verbs=create;update;delete,versions=v1alpha1,name=vrebootnode-v1alpha1.kb.io,admissionReviewVersions=v1

// nolint:lll
// +kubebuilder:webhook:path=/validate-janitor-dgxc-nvidia-com-v1alpha1-terminatenode,mutating=false,failurePolicy=fail,sideEffects=None,groups=janitor.dgxc.nvidia.com,resources=terminatenodes,verbs=create;update;delete,versions=v1alpha1,name=vterminatenode-v1alpha1.kb.io,admissionReviewVersions=v1

// JanitorCustomValidator struct is responsible for validating all Janitor resources
// when they are created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
//
// +kubebuilder:object:generate=false
type JanitorCustomValidator struct {
	Config *config.Config
	Client client.Client
}

var _ webhook.CustomValidator = &JanitorCustomValidator{}

// validateNodeExists checks if the specified node exists in the cluster
func (v *JanitorCustomValidator) validateNodeExists(ctx context.Context, nodeName string) error {
	if v.Client == nil {
		return fmt.Errorf("kubernetes client not available for node validation")
	}

	var node corev1.Node
	if err := v.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return fmt.Errorf("node '%s' does not exist in the cluster: %w", nodeName, err)
	}

	return nil
}

func (v *JanitorCustomValidator) validateNodeNotInExclusions(ctx context.Context, nodeName string) error {
	if v.Client == nil {
		return fmt.Errorf("kubernetes client not available for node validation")
	}

	var node corev1.Node
	if err := v.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return fmt.Errorf("node '%s' does not exist in the cluster: %w", nodeName, err)
	}

	for _, exclusion := range v.Config.Global.Nodes.Exclusions {
		selector, err := metav1.LabelSelectorAsSelector(&exclusion)
		if err != nil {
			return fmt.Errorf("invalid exclusion label selector '%s': %w", exclusion.String(), err)
		}

		if selector.Matches(labels.Set(node.Labels)) {
			return fmt.Errorf(
				"node '%s' is excluded from janitor operations due to a label on the node matching the label exclusion '%s' from config value global.nodes.exclusions", // nolint:lll
				nodeName,
				selector.String(),
			)
		}
	}

	return nil
}

// validateNoActiveReboot checks if there's already an active reboot for the node
func (v *JanitorCustomValidator) validateNoActiveReboot(ctx context.Context, nodeName string) error {
	if v.Client == nil {
		return fmt.Errorf("kubernetes client not available for reboot validation")
	}

	var rebootNodeList janitordgxcnvidiacomv1alpha1.RebootNodeList
	if err := v.Client.List(ctx, &rebootNodeList); err != nil {
		return fmt.Errorf("failed to list RebootNode resources: %w", err)
	}

	for _, rebootNode := range rebootNodeList.Items {
		// Skip if it's for a different node
		if rebootNode.Spec.NodeName != nodeName {
			continue
		}

		// Check if this reboot is still active (not completed)
		if rebootNode.Status.CompletionTime == nil {
			return fmt.Errorf("node '%s' already has an active reboot in progress (RebootNode: %s)", nodeName, rebootNode.Name)
		}
	}

	return nil
}

// validateNoActiveTermination checks if there's already an active termination for the node
func (v *JanitorCustomValidator) validateNoActiveTermination(ctx context.Context, nodeName string) error {
	if v.Client == nil {
		return fmt.Errorf("kubernetes client not available for termination validation")
	}

	var terminateNodeList janitordgxcnvidiacomv1alpha1.TerminateNodeList
	if err := v.Client.List(ctx, &terminateNodeList); err != nil {
		return fmt.Errorf("failed to list TerminateNode resources: %w", err)
	}

	for _, terminateNode := range terminateNodeList.Items {
		// Skip if it's for a different node
		if terminateNode.Spec.NodeName != nodeName {
			continue
		}

		// Check if this termination is still active (not completed)
		if terminateNode.Status.CompletionTime == nil {
			return fmt.Errorf(
				"node '%s' already has an active termination in progress (TerminateNode: %s)", // nolint:lll
				nodeName,
				terminateNode.Name,
			)
		}
	}

	return nil
}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for all Janitor CRD types.
// nolint:cyclop
func (v *JanitorCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	var (
		objName        string
		controllerType string
		nodeName       string
	)

	switch typedObj := obj.(type) {
	case *janitordgxcnvidiacomv1alpha1.RebootNode:
		objName = typedObj.GetName()
		controllerType = controllerTypeRebootNode
		nodeName = typedObj.Spec.NodeName

		if v.Config == nil || !v.Config.RebootNode.Enabled {
			janitorWebhookLog.Info("RebootNode controller is disabled, rejecting creation", "name", objName)
			return nil, fmt.Errorf("RebootNode controller is disabled in configuration")
		}

		// Check for active reboots
		if err := v.validateNoActiveReboot(ctx, nodeName); err != nil {
			janitorWebhookLog.Info(
				"Active reboot validation failed", // nolint:lll
				"type", controllerType,
				"name", objName,
				"nodeName", nodeName,
				"error", err.Error(),
			)

			return nil, err
		}

	case *janitordgxcnvidiacomv1alpha1.TerminateNode:
		objName = typedObj.GetName()
		controllerType = controllerTypeTerminateNode
		nodeName = typedObj.Spec.NodeName

		if v.Config == nil || !v.Config.TerminateNode.Enabled {
			janitorWebhookLog.Info("TerminateNode controller is disabled, rejecting creation", "name", objName)
			return nil, fmt.Errorf("TerminateNode controller is disabled in configuration")
		}

		// Check for active terminations
		if err := v.validateNoActiveTermination(ctx, nodeName); err != nil {
			janitorWebhookLog.Info(
				"Active termination validation failed", // nolint:lll
				"type", controllerType,
				"name", objName,
				"nodeName", nodeName,
				"error", err.Error(),
			)

			return nil, err
		}

	default:
		return nil, fmt.Errorf("expected a Janitor CR object but got %T", obj)
	}

	// Validate node existence for CRs that reference nodes
	if nodeName != "" {
		if err := v.validateNodeExists(ctx, nodeName); err != nil {
			janitorWebhookLog.Info(
				"Node validation failed", // nolint:lll
				"type", controllerType,
				"name", objName,
				"nodeName", nodeName,
				"error", err.Error(),
			)

			return nil, err
		}

		if err := v.validateNodeNotInExclusions(ctx, nodeName); err != nil {
			janitorWebhookLog.Info(
				"Node exclusion list validation failed", // nolint:lll
				"type", controllerType,
				"name", objName,
				"nodeName", nodeName,
				"error", err.Error(),
			)

			return nil, err
		}
	}

	janitorWebhookLog.Info("Validation for Janitor CR upon creation", "type", controllerType, "name", objName)

	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for all Janitor CRD types.
// nolint:cyclop,lll
func (v *JanitorCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	var (
		objName        string
		controllerType string
		nodeName       string
		oldNodeName    string
	)

	switch typedObj := newObj.(type) {
	case *janitordgxcnvidiacomv1alpha1.RebootNode:
		objName = typedObj.GetName()
		controllerType = controllerTypeRebootNode
		nodeName = typedObj.Spec.NodeName

		if v.Config == nil || !v.Config.RebootNode.Enabled {
			janitorWebhookLog.Info("RebootNode controller is disabled, rejecting update", "name", objName)
			return nil, fmt.Errorf("RebootNode controller is disabled in configuration")
		}

		// Prevent changes to nodeName
		if oldRebootNode, ok := oldObj.(*janitordgxcnvidiacomv1alpha1.RebootNode); ok {
			oldNodeName = oldRebootNode.Spec.NodeName
			if oldNodeName != nodeName {
				return nil, fmt.Errorf("nodeName cannot be changed after creation")
			}
		}

	case *janitordgxcnvidiacomv1alpha1.TerminateNode:
		objName = typedObj.GetName()
		controllerType = controllerTypeTerminateNode
		nodeName = typedObj.Spec.NodeName

		if v.Config == nil || !v.Config.TerminateNode.Enabled {
			janitorWebhookLog.Info("TerminateNode controller is disabled, rejecting update", "name", objName)
			return nil, fmt.Errorf("TerminateNode controller is disabled in configuration")
		}

		// Prevent changes to nodeName
		if oldTerminateNode, ok := oldObj.(*janitordgxcnvidiacomv1alpha1.TerminateNode); ok {
			oldNodeName = oldTerminateNode.Spec.NodeName
			if oldNodeName != nodeName {
				return nil, fmt.Errorf("nodeName cannot be changed after creation")
			}
		}

	default:
		return nil, fmt.Errorf("expected a Janitor CR object but got %T", newObj)
	}

	// Validate node existence for CRs that reference nodes
	if nodeName != "" {
		if err := v.validateNodeExists(ctx, nodeName); err != nil {
			janitorWebhookLog.Info(
				"Node validation failed", // nolint:lll
				"type", controllerType,
				"name", objName,
				"nodeName", nodeName,
				"error", err.Error(),
			)

			return nil, err
		}

		if err := v.validateNodeNotInExclusions(ctx, nodeName); err != nil {
			janitorWebhookLog.Info(
				"Node exclusion list validation failed", // nolint:lll
				"type", controllerType,
				"name", objName,
				"nodeName", nodeName,
				"error", err.Error(),
			)

			return nil, err
		}
	}

	janitorWebhookLog.Info("Validation for Janitor CR upon update", "type", controllerType, "name", objName)

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for all Janitor CRD types.
func (v *JanitorCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	var (
		objName        string
		controllerType string
	)

	switch typedObj := obj.(type) {
	case *janitordgxcnvidiacomv1alpha1.RebootNode:
		objName = typedObj.GetName()
		controllerType = controllerTypeRebootNode

		if v.Config == nil || !v.Config.RebootNode.Enabled {
			janitorWebhookLog.Info("RebootNode controller is disabled, rejecting deletion", "name", objName)
			return nil, fmt.Errorf("RebootNode controller is disabled in configuration")
		}

	case *janitordgxcnvidiacomv1alpha1.TerminateNode:
		objName = typedObj.GetName()
		controllerType = controllerTypeTerminateNode

		if v.Config == nil || !v.Config.TerminateNode.Enabled {
			janitorWebhookLog.Info("TerminateNode controller is disabled, rejecting deletion", "name", objName)
			return nil, fmt.Errorf("TerminateNode controller is disabled in configuration")
		}

	default:
		return nil, fmt.Errorf("expected a Janitor CR object but got %T", obj)
	}

	// Note: We don't validate node existence for deletions since the node might have been deleted already
	janitorWebhookLog.Info("Validation for Janitor CR upon deletion", "type", controllerType, "name", objName)

	return nil, nil
}
