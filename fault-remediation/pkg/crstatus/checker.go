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

	"github.com/nvidia/nvsentinel/fault-remediation/pkg/annotation"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
)

type CRStatusChecker struct {
	client             client.Client
	remediationActions map[string]config.MaintenanceResource
	dryRun             bool
}

type CRState string

const (
	CRStateNotFound   CRState = "NotFound"
	CRStateInProgress CRState = "InProgress"
	CRStateSucceeded  CRState = "Succeeded"
	CRStateFailed     CRState = "Failed"
)

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

// ShouldSkipCRCreation returns true if an existing CR should suppress creation of a new CR.
func (c *CRStatusChecker) ShouldSkipCRCreation(ctx context.Context, actionName string, crName string) bool {
	state := c.GetCRState(ctx, actionName, crName)
	return state == CRStateInProgress || state == CRStateSucceeded
}

func (c *CRStatusChecker) GetCRState(ctx context.Context, actionName string, crName string) CRState {
	resource, exists := c.remediationActions[actionName]
	if !exists {
		slog.ErrorContext(ctx, "No remediation configuration found for action", "action", actionName)
		return CRStateNotFound
	}

	resourceRef := annotation.MaintenanceResourceReference{
		Namespace: resource.Namespace,
		Version:   resource.Version,
		ApiGroup:  resource.ApiGroup,
		Kind:      resource.Kind,
	}

	return c.GetCRStateForReference(ctx, crName, resourceRef, resource.CompleteConditionType)
}

func (c *CRStatusChecker) GetCRStateForReference(
	ctx context.Context,
	crName string,
	resourceRef annotation.MaintenanceResourceReference,
	completeConditionType string,
) CRState {
	if c.dryRun {
		slog.InfoContext(ctx, "DRY-RUN: CR doesn't exist (dry-run mode)", "crName", crName)
		return CRStateNotFound
	}

	gvk := schema.GroupVersionKind{
		Group:   resourceRef.ApiGroup,
		Version: resourceRef.Version,
		Kind:    resourceRef.Kind,
	}
	if gvk.Group == "" || gvk.Version == "" || gvk.Kind == "" {
		slog.WarnContext(ctx, "Stored CR reference is missing GVK, allowing create", "crName", crName)
		return CRStateNotFound
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)

	key := client.ObjectKey{Name: crName, Namespace: resourceRef.Namespace}

	if err := c.client.Get(ctx, key, obj); err != nil {
		slog.WarnContext(ctx, "Failed to get CR, allowing create", "crName", crName, "gvk", gvk.String(), "error", err)
		return CRStateNotFound
	}

	return c.checkConditionType(obj, completeConditionType)
}

func (c *CRStatusChecker) checkCondition(obj *unstructured.Unstructured, resource config.MaintenanceResource) CRState {
	return c.checkConditionType(obj, resource.CompleteConditionType)
}

func (c *CRStatusChecker) checkConditionType(obj *unstructured.Unstructured, completeConditionType string) CRState {
	status, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil || !found {
		return CRStateInProgress
	}

	conditions, found, err := unstructured.NestedSlice(status, "conditions")
	if err != nil || !found {
		return CRStateInProgress
	}

	conditionStatus := c.findConditionStatus(conditions, completeConditionType)

	switch conditionStatus {
	case "True":
		return CRStateSucceeded
	case "False":
		return CRStateFailed
	default:
		return CRStateInProgress
	}
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
