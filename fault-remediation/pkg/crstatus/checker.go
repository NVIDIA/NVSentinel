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

	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
)

type CRStatusChecker struct {
	dynamicClient dynamic.Interface
	restMapper    *restmapper.DeferredDiscoveryRESTMapper
	config        *config.MaintenanceResource
	dryRun        bool
}

func NewCRStatusChecker(
	dynamicClient dynamic.Interface,
	restMapper *restmapper.DeferredDiscoveryRESTMapper,
	cfg *config.MaintenanceResource,
	dryRun bool,
) *CRStatusChecker {
	return &CRStatusChecker{
		dynamicClient: dynamicClient,
		restMapper:    restMapper,
		config:        cfg,
		dryRun:        dryRun,
	}
}

func (c *CRStatusChecker) ShouldSkipCRCreation(ctx context.Context, crName string) bool {
	if c.dryRun {
		slog.Info("DRY-RUN: CR doesn't exist (dry-run mode)", "crName", crName)
		return false
	}

	gvk := schema.GroupVersionKind{
		Group:   c.config.ApiGroup,
		Version: c.config.Version,
		Kind:    c.config.Kind,
	}

	mapping, err := c.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		slog.Error("Failed to get REST mapping", "gvk", gvk.String(), "error", err)
		return false
	}

	resource, err := c.dynamicClient.Resource(mapping.Resource).Get(ctx, crName, metav1.GetOptions{})
	if err != nil {
		slog.Warn("Failed to get CR, allowing create", "crName", crName, "error", err)
		return false
	}

	return c.checkCondition(resource)
}

func (c *CRStatusChecker) checkCondition(obj *unstructured.Unstructured) bool {
	status, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil || !found {
		return true
	}

	conditions, found, err := unstructured.NestedSlice(status, "conditions")
	if err != nil || !found {
		return true
	}

	conditionStatus := c.findConditionStatus(conditions)

	return !c.isTerminal(conditionStatus)
}

func (c *CRStatusChecker) findConditionStatus(conditions []any) string {
	for _, cond := range conditions {
		condition, ok := cond.(map[string]interface{})
		if !ok {
			continue
		}

		condType, _ := condition["type"].(string)
		if condType == c.config.CompleteConditionType {
			condStatus, _ := condition["status"].(string)
			return condStatus
		}
	}

	return ""
}

func (c *CRStatusChecker) isTerminal(conditionStatus string) bool {
	return conditionStatus == "True" || conditionStatus == "False"
}
