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
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// ShouldSkipCRCreation returns true if an existing CR should suppress creation of a new CR. A CR that cannot be
// read suppresses creation, since it may still be running.
func (c *CRStatusChecker) ShouldSkipCRCreation(ctx context.Context, actionName string, crName string) bool {
	state, err := c.GetCRState(ctx, actionName, crName)
	if err != nil {
		return true
	}

	return state == CRStateInProgress || state == CRStateSucceeded
}

// GetCRState returns the state of a maintenance CR. A CR that does not exist is CRStateNotFound. Any other read
// error is returned, because it says nothing about the CR: treating it as missing would let a second CR be created
// for the node, and treating it as in progress would report the node as remediated.
func (c *CRStatusChecker) GetCRState(ctx context.Context, actionName string, crName string) (CRState, error) {
	resource, exists := c.remediationActions[actionName]
	if !exists {
		slog.ErrorContext(ctx, "No remediation configuration found for action", "action", actionName)
		return CRStateNotFound, nil
	}

	if c.dryRun {
		slog.InfoContext(ctx, "DRY-RUN: CR doesn't exist (dry-run mode)", "crName", crName, "action", actionName)
		return CRStateNotFound, nil
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
		if apierrors.IsNotFound(err) {
			slog.InfoContext(ctx, "CR not found, allowing create", "crName", crName, "gvk", gvk.String())
			return CRStateNotFound, nil
		}

		return "", fmt.Errorf("failed to get %s %s: %w", gvk.Kind, crName, err)
	}

	return c.checkCondition(obj, resource), nil
}

func (c *CRStatusChecker) checkCondition(obj *unstructured.Unstructured, resource config.MaintenanceResource) CRState {
	status, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil || !found {
		return CRStateInProgress
	}

	conditions, found, err := unstructured.NestedSlice(status, "conditions")
	if err != nil || !found {
		return CRStateInProgress
	}

	conditionStatus := c.findConditionStatus(conditions, resource.CompleteConditionType)

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
		condition, ok := cond.(map[string]any)
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
