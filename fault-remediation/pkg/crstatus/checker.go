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

package crstatus

import (
	"context"
	"log/slog"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
)

type CRStatusChecker struct {
	client             client.Client
	remediationActions map[string]config.MaintenanceResource
	dryRun             bool
}

func NewCRStatusChecker(
	client client.Client,
	remediationActions map[string]config.MaintenanceResource,
	dryRun bool,
) *CRStatusChecker {
	return &CRStatusChecker{
		client:             client,
		remediationActions: remediationActions,
		dryRun:             dryRun,
	}
}

// ShouldSkipCRCreation returns true if the CR exists and is not in a terminal state otherwise returns false.
func (c *CRStatusChecker) ShouldSkipCRCreation(ctx context.Context, actionName string, crName string) bool {
	resource, exists := c.remediationActions[actionName]
	if !exists {
		slog.Error("No remediation configuration found for action", "action", actionName)
		return false
	}

	if c.dryRun {
		slog.Info("DRY-RUN: CR doesn't exist (dry-run mode)", "crName", crName, "action", actionName)
		return false
	}

	gvk := schema.GroupVersionKind{
		Group:   resource.ApiGroup,
		Version: resource.Version,
		Kind:    resource.Kind,
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)

	key := client.ObjectKey{Name: crName, Namespace: resource.Namespace}

	if err := c.client.Get(ctx, key, obj); err != nil {
		slog.Warn("Failed to get CR, allowing create", "crName", crName, "gvk", gvk.String(), "error", err)
		return false
	}

	return c.checkCondition(obj, resource)
}

func (c *CRStatusChecker) checkCondition(obj *unstructured.Unstructured, resource config.MaintenanceResource) bool {
	status, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil || !found {
		return true
	}

	conditions, found, err := unstructured.NestedSlice(status, "conditions")
	if err != nil || !found {
		return true
	}

	conditionStatus := c.findConditionStatus(conditions, resource.CompleteConditionType)

	return !c.isTerminal(conditionStatus)
}

func (c *CRStatusChecker) findConditionStatus(conditions []any, completeConditionType string) string {
	for _, cond := range conditions {
		condition, ok := cond.(map[string]interface{})
		if !ok {
			continue
		}

		condType, _ := condition["type"].(string)
		if condType == completeConditionType {
			condStatus, _ := condition["status"].(string)
			return condStatus
		}
	}

	return ""
}

func (c *CRStatusChecker) isTerminal(conditionStatus string) bool {
	return conditionStatus == "True" || conditionStatus == "False"
}
