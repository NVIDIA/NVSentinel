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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
)

// RebootNodeCRStatusChecker implements CRStatusChecker for RebootNode CRs
type RebootNodeCRStatusChecker struct {
	dynamicClient dynamic.Interface
	restMapper    *restmapper.DeferredDiscoveryRESTMapper
	dryRun        bool
}

// NewRebootNodeCRStatusChecker creates a new RebootNodeCRStatusChecker
func NewRebootNodeCRStatusChecker(dynamicClient dynamic.Interface,
	restMapper *restmapper.DeferredDiscoveryRESTMapper,
	dryRun bool) *RebootNodeCRStatusChecker {
	return &RebootNodeCRStatusChecker{
		dynamicClient: dynamicClient,
		restMapper:    restMapper,
		dryRun:        dryRun,
	}
}

// GetCRStatus retrieves the status of a RebootNode CR
func (c *RebootNodeCRStatusChecker) GetCRStatus(ctx context.Context, crName string) (CRStatus, error) {
	if c.dryRun {
		slog.Info("DRY-RUN: CR doesn't exist (dry-run mode)", "crName", crName)
		return CRStatusNotFound, nil // In dry-run, CRs don't actually exist
	}

	// RebootNode CR type
	gvk := schema.GroupVersionKind{
		Group:   MaintenanceAPIGroup,
		Version: MaintenanceAPIVersion,
		Kind:    "RebootNode",
	}

	// Get GVR from GVK
	mapping, err := c.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		slog.Error("Failed to get REST mapping",
			"gvk", gvk.String(),
			"error", err)

		return CRStatusNotFound, nil
	}

	// Get the CR
	resource, err := c.dynamicClient.Resource(mapping.Resource).Get(ctx, crName, metav1.GetOptions{})
	if err != nil {
		if meta.IsNoMatchError(err) {
			slog.Info("CR not found", "crName", crName)
			return CRStatusNotFound, nil
		}

		slog.Error("Failed to get CR", "crName", crName, "error", err)

		return CRStatusNotFound, nil
	}

	// Extract status from the unstructured object
	return c.extractRebootNodeStatus(resource)
}

// extractRebootNodeStatus extracts the status from a RebootNode CR
func (c *RebootNodeCRStatusChecker) extractRebootNodeStatus(obj *unstructured.Unstructured) (CRStatus, error) {
	// Get status field
	status, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil {
		return CRStatusInProgress, fmt.Errorf("failed to extract status from RebootNode CR %s: %w",
			obj.GetName(), err)
	}

	if !found {
		slog.Warn("No status field found in RebootNode CR", "crName", obj.GetName())
		return CRStatusInProgress, nil // Assume in progress if no status
	}

	// Check for completion time - indicates CR is done (succeeded or failed)
	completionTime, _, _ := unstructured.NestedString(status, "completionTime")
	isComplete := completionTime != ""

	// Get conditions
	conditions, found, err := unstructured.NestedSlice(status, "conditions")
	if err != nil {
		return CRStatusInProgress, fmt.Errorf("failed to extract conditions from RebootNode CR %s status: %w",
			obj.GetName(), err)
	}

	if !found {
		slog.Warn("No conditions found in RebootNode CR status", "crName", obj.GetName())
		return CRStatusInProgress, nil
	}

	// Parse conditions and check for early failure
	signalSentStatus, nodeReadyStatus, earlyStatus := parseRebootConditions(conditions, isComplete)
	if earlyStatus != nil {
		return *earlyStatus, nil
	}

	// Determine final status based on conditions and completion state
	return determineRebootStatus(signalSentStatus, nodeReadyStatus, isComplete), nil
}

// parseRebootConditions extracts condition statuses from the conditions array
func parseRebootConditions(
	conditions []interface{},
	isComplete bool,
) (signalSentStatus, nodeReadyStatus string, earlyStatus *CRStatus) {
	for _, cond := range conditions {
		condition, ok := cond.(map[string]interface{})
		if !ok {
			continue
		}

		condType, _ := condition["type"].(string)
		condStatus, _ := condition["status"].(string)
		condReason, _ := condition["reason"].(string)

		switch condType {
		case "SignalSent":
			signalSentStatus = condStatus
			// If signal explicitly failed to send and CR is complete, it has failed
			if condStatus == "False" && condReason == "Failed" && isComplete {
				failed := CRStatusFailed
				earlyStatus = &failed

				return
			}
		case "NodeReady":
			nodeReadyStatus = condStatus
		}
	}

	return
}

// determineRebootStatus determines the final CR status based on conditions and completion state
func determineRebootStatus(signalSentStatus, nodeReadyStatus string, isComplete bool) CRStatus {
	// SignalSent not True yet - still initializing or in progress
	if signalSentStatus != "True" {
		return CRStatusInProgress
	}

	// Reboot succeeded - SignalSent=True, NodeReady=True
	if nodeReadyStatus == "True" {
		return CRStatusSucceeded
	}

	// NodeReady=False can mean either:
	// - Failed/Timeout (if CompletionTime is set)
	// - Still in progress (if CompletionTime is not set)
	if nodeReadyStatus == "False" && isComplete {
		return CRStatusFailed
	}

	// Signal sent but NodeReady status is Unknown - still in progress
	return CRStatusInProgress
}
