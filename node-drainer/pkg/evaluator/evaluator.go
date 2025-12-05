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

package evaluator

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/customdrain"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/queue"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
)

const (
	customDrainPollInterval = 30 * time.Second
)

func NewNodeDrainEvaluator(
	cfg config.TomlConfig,
	informers InformersInterface,
	customDrainClient CustomDrainClientInterface,
) DrainEvaluator {
	return &NodeDrainEvaluator{
		config:            cfg,
		informers:         informers,
		customDrainClient: customDrainClient,
	}
}

// EvaluateEvent method has been removed - use EvaluateEventWithDatabase instead

// EvaluateEventWithDatabase evaluates using the new database-agnostic interface
func (e *NodeDrainEvaluator) EvaluateEventWithDatabase(ctx context.Context, healthEvent model.HealthEventWithStatus,
	database queue.DataStore) (*DrainActionResult, error) {
	nodeName := healthEvent.HealthEvent.NodeName

	statusPtr := healthEvent.HealthEventStatus.NodeQuarantined

	if statusPtr != nil && *statusPtr == model.UnQuarantined {
		return &DrainActionResult{Action: ActionSkip}, nil
	}

	if isTerminalStatus(healthEvent.HealthEventStatus.UserPodsEvictionStatus.Status) {
		slog.Info("Event for node is in terminal state, skipping", "node", nodeName)

		return &DrainActionResult{
			Action: ActionSkip,
		}, nil
	}

	if statusPtr != nil && *statusPtr == model.AlreadyQuarantined {
		isDrained, err := isNodeAlreadyDrained(ctx, database, nodeName)
		if err != nil {
			slog.Error("Failed to check if node is already drained",
				"node", nodeName,
				"error", err)

			return &DrainActionResult{
				Action:    ActionWait,
				WaitDelay: time.Minute,
			}, nil
		}

		if isDrained {
			return &DrainActionResult{
				Action: ActionMarkAlreadyDrained,
				Status: "AlreadyDrained",
			}, nil
		}
	}

	if e.config.CustomDrain.Enabled && e.customDrainClient != nil {
		return e.evaluateCustomDrain(ctx, healthEvent, database)
	}

	return e.evaluateUserNamespaceActions(ctx, healthEvent)
}

func (e *NodeDrainEvaluator) evaluateUserNamespaceActions(ctx context.Context,
	healthEvent model.HealthEventWithStatus) (*DrainActionResult, error) {
	nodeName := healthEvent.HealthEvent.NodeName
	systemNamespaces := e.config.SystemNamespaces

	ns := namespaces{
		immediateEvictionNamespaces:  make([]string, 0),
		allowCompletionNamespaces:    make([]string, 0),
		deleteAfterTimeoutNamespaces: make([]string, 0),
	}
	forceImmediateEviction := healthEvent.HealthEvent.DrainOverrides != nil &&
		healthEvent.HealthEvent.DrainOverrides.Force

	if forceImmediateEviction {
		slog.Info("DrainOverrides.Force is true, forcing immediate eviction for all namespaces on node",
			"node", nodeName)
	}

	for _, userNamespace := range e.config.UserNamespaces {
		matchedNamespaces, err := e.informers.GetNamespacesMatchingPattern(ctx,
			userNamespace.Name, systemNamespaces, nodeName)
		if err != nil {
			slog.Error("Failed to get namespaces for pattern",
				"pattern", userNamespace.Name,
				"error", err)

			return &DrainActionResult{
				Action:    ActionWait,
				WaitDelay: time.Minute,
			}, nil
		}

		switch {
		case forceImmediateEviction || userNamespace.Mode == config.ModeImmediateEvict:
			ns.immediateEvictionNamespaces = append(ns.immediateEvictionNamespaces, matchedNamespaces...)
		case userNamespace.Mode == config.ModeAllowCompletion:
			ns.allowCompletionNamespaces = append(ns.allowCompletionNamespaces, matchedNamespaces...)
		case userNamespace.Mode == config.ModeDeleteAfterTimeout:
			ns.deleteAfterTimeoutNamespaces = append(ns.deleteAfterTimeoutNamespaces, matchedNamespaces...)
		default:
			slog.Error("unsupported mode", "mode", userNamespace.Mode)
		}
	}

	return e.getAction(ctx, ns, nodeName), nil
}

func (e *NodeDrainEvaluator) getAction(ctx context.Context, ns namespaces, nodeName string) *DrainActionResult {
	if len(ns.immediateEvictionNamespaces) > 0 {
		timeout := e.config.EvictionTimeoutInSeconds.Duration
		if !e.informers.CheckIfAllPodsAreEvictedInImmediateMode(ctx, ns.immediateEvictionNamespaces, nodeName, timeout) {
			slog.Info("Performing immediate eviction for node", "node", nodeName)

			return &DrainActionResult{
				Action:     ActionEvictImmediate,
				Namespaces: ns.immediateEvictionNamespaces,
				Timeout:    timeout,
			}
		}
	}

	if len(ns.allowCompletionNamespaces) > 0 {
		action := e.handleAllowCompletionNamespaces(ns, nodeName)
		if action != nil {
			return action
		}
	}

	if len(ns.deleteAfterTimeoutNamespaces) > 0 {
		action := e.handleDeleteAfterTimeoutNamespaces(ns, nodeName)
		if action != nil {
			return action
		}
	}

	slog.Info("All pods evicted successfully on node", "node", nodeName)

	return &DrainActionResult{
		Action: ActionUpdateStatus,
		Status: "StatusCompleted",
	}
}

func (e *NodeDrainEvaluator) handleAllowCompletionNamespaces(ns namespaces, nodeName string) *DrainActionResult {
	hasRemainingPods := false

	for _, namespace := range ns.allowCompletionNamespaces {
		pods, err := e.informers.FindEvictablePodsInNamespaceAndNode(namespace, nodeName)
		if err != nil {
			slog.Error("Failed to check pods in namespace on node",
				"namespace", namespace,
				"node", nodeName,
				"error", err)

			hasRemainingPods = true

			break
		}

		if len(pods) > 0 {
			hasRemainingPods = true
			break
		}
	}

	if hasRemainingPods {
		slog.Info("Checking pod completion status for AllowCompletion namespaces on node",
			"node", nodeName)

		return &DrainActionResult{
			Action:     ActionCheckCompletion,
			Namespaces: ns.allowCompletionNamespaces,
		}
	}

	return nil
}

func (e *NodeDrainEvaluator) handleDeleteAfterTimeoutNamespaces(ns namespaces, nodeName string) *DrainActionResult {
	hasRemainingPods := false

	for _, namespace := range ns.deleteAfterTimeoutNamespaces {
		pods, err := e.informers.FindEvictablePodsInNamespaceAndNode(namespace, nodeName)
		if err != nil {
			slog.Error("Failed to check pods in namespace on node",
				"namespace", namespace,
				"node", nodeName,
				"error", err)

			hasRemainingPods = true

			break
		}

		if len(pods) > 0 {
			hasRemainingPods = true
			break
		}
	}

	if hasRemainingPods {
		slog.Info("Deleting pods after timeout for DeleteAfterTimeout namespaces on node",
			"node", nodeName)

		return &DrainActionResult{
			Action:     ActionEvictWithTimeout,
			Namespaces: ns.deleteAfterTimeoutNamespaces,
			Timeout:    time.Duration(e.config.DeleteAfterTimeoutMinutes) * time.Minute,
		}
	}

	return nil
}

func isTerminalStatus(status model.Status) bool {
	return status == model.StatusSucceeded ||
		status == model.StatusFailed ||
		status == model.Cancelled ||
		status == model.AlreadyDrained
}

func (e *NodeDrainEvaluator) evaluateCustomDrain(
	ctx context.Context,
	healthEvent model.HealthEventWithStatus,
	database queue.DataStore,
) (*DrainActionResult, error) {
	nodeName := healthEvent.HealthEvent.NodeName

	eventID, err := getEventID(ctx, database, nodeName)
	if err != nil {
		slog.Error("Failed to get event ID for custom drain",
			"node", nodeName,
			"error", err)

		return &DrainActionResult{
			Action:    ActionWait,
			WaitDelay: customDrainPollInterval,
		}, nil
	}

	crName := customdrain.GenerateCRName(nodeName, eventID)

	exists, err := e.customDrainClient.Exists(ctx, crName)
	if err != nil {
		slog.Error("Failed to check if drain CR exists",
			"node", nodeName,
			"crName", crName,
			"error", err)

		return &DrainActionResult{
			Action:    ActionWait,
			WaitDelay: customDrainPollInterval,
		}, nil
	}

	if !exists {
		systemNamespaces := e.config.SystemNamespaces

		namespaces, err := e.informers.GetNamespacesMatchingPattern(ctx, "*", systemNamespaces, nodeName)
		if err != nil {
			return nil, fmt.Errorf("failed to get user namespaces: %w", err)
		}

		slog.Info("Creating custom drain CR",
			"node", nodeName,
			"crName", crName)

		return &DrainActionResult{
			Action:     ActionCreateCR,
			Namespaces: namespaces,
		}, nil
	}

	isComplete, err := e.customDrainClient.GetCRStatus(ctx, crName)
	if err != nil {
		slog.Error("Failed to get drain CR status",
			"node", nodeName,
			"crName", crName,
			"error", err)

		return &DrainActionResult{
			Action:    ActionWait,
			WaitDelay: customDrainPollInterval,
		}, nil
	}

	if !isComplete {
		slog.Debug("Drain CR in progress",
			"node", nodeName,
			"crName", crName)

		return &DrainActionResult{
			Action:    ActionWait,
			WaitDelay: customDrainPollInterval,
		}, nil
	}

	slog.Info("Drain CR completed",
		"node", nodeName,
		"crName", crName)

	return &DrainActionResult{
		Action: ActionMarkAlreadyDrained,
		Status: "AlreadyDrained",
	}, nil
}

func getEventID(ctx context.Context, database queue.DataStore, nodeName string) (string, error) {
	opts := &client.FindOneOptions{
		Sort: map[string]any{"_id": -1},
	}

	filter := map[string]any{
		"healthevent.nodename": nodeName,
	}

	result, err := database.FindDocument(ctx, filter, opts)
	if err != nil {
		return "", fmt.Errorf("failed to query database for node %s: %w", nodeName, err)
	}

	var document map[string]any
	if err := result.Decode(&document); err != nil {
		return "", fmt.Errorf("failed to decode health event for node %s: %w", nodeName, err)
	}

	eventID, err := utils.ExtractDocumentID(document)
	if err != nil {
		return "", fmt.Errorf("failed to extract document ID for node %s: %w", nodeName, err)
	}

	return eventID, nil
}

// isNodeAlreadyDrained checks if a node has already been drained using the database-agnostic interface
//
//nolint:cyclop // Complexity acceptable for dual-case field name lookups (MongoDB vs PostgreSQL)
func isNodeAlreadyDrained(ctx context.Context, database queue.DataStore, nodeName string) (bool, error) {
	// Use database-agnostic filter building
	filter := client.NewFilterBuilder().
		Eq("healthevent.nodename", nodeName).
		In("healtheventstatus.nodequarantined", []string{string(model.Quarantined), string(model.UnQuarantined)}).
		Build()

	// ObjectID contains timestamp, sort descending to get latest
	opts := &client.FindOneOptions{
		Sort: map[string]any{"_id": -1},
	}

	// Use database-agnostic method and semantic error handling
	result, err := database.FindDocument(ctx, filter, opts)
	if err != nil {
		return false, fmt.Errorf("failed to query latest event for node %s: %w", nodeName, err)
	}

	var document map[string]any
	if err := result.Decode(&document); err != nil {
		// Use centralized error checking to eliminate string comparisons
		if client.IsNoDocumentsError(err) {
			return false, nil
		}

		return false, fmt.Errorf("failed to decode result for node %s: %w", nodeName, err)
	}

	// Try lowercase first (MongoDB compatibility)
	healthEventStatus, ok := document["healtheventstatus"].(map[string]interface{})
	if !ok {
		// Try camelCase (PostgreSQL JSON)
		healthEventStatus, ok = document["healthEventStatus"].(map[string]interface{})
		if !ok {
			return false, fmt.Errorf("invalid healtheventstatus format for node %s", nodeName)
		}
	}

	// Try lowercase first (MongoDB compatibility)
	nodeQuarantined, ok := healthEventStatus["nodequarantined"].(string)
	if !ok {
		// Try camelCase (PostgreSQL JSON)
		nodeQuarantined, ok = healthEventStatus["nodeQuarantined"].(string)
		if !ok {
			return false, fmt.Errorf("invalid nodequarantined format for node %s", nodeName)
		}
	}

	if nodeQuarantined == string(model.UnQuarantined) {
		return false, nil
	}

	userPodsEvictionStatus, ok := healthEventStatus["userpodsevictionstatus"].(map[string]any)
	if !ok {
		return false, nil
	}

	drainStatus, ok := userPodsEvictionStatus["status"].(string)
	if !ok {
		return false, nil
	}

	return drainStatus == string(model.StatusSucceeded), nil
}
