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
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	annotation "github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/customdrain"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/queue"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
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
//
//nolint:cyclop
func (e *NodeDrainEvaluator) EvaluateEventWithDatabase(ctx context.Context, healthEvent model.HealthEventWithStatus,
	database queue.DataStore, healthEventStore datastore.HealthEventStore) (*DrainActionResult, error) {
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

	partialDrainEntity, err := e.shouldExecutePartialDrain(healthEvent.HealthEvent)
	if err != nil {
		slog.Error("Failed to check if node should be partially drained",
			"node", nodeName,
			"error", err)

		return &DrainActionResult{
			Action: ActionUpdateStatus,
			Status: model.StatusFailed,
		}, nil
	}

	if statusPtr != nil && *statusPtr == model.AlreadyQuarantined {
		isDrained, err := e.isNodeAlreadyDrained(ctx, healthEvent.HealthEvent.Id, partialDrainEntity,
			nodeName, healthEventStore)
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
				Status: model.AlreadyDrained,
			}, nil
		}
	}

	if e.config.CustomDrain.Enabled && e.customDrainClient != nil {
		return e.evaluateCustomDrain(ctx, healthEvent, database, partialDrainEntity)
	}

	return e.evaluateUserNamespaceActions(ctx, healthEvent, partialDrainEntity)
}

func (e *NodeDrainEvaluator) evaluateUserNamespaceActions(ctx context.Context,
	healthEvent model.HealthEventWithStatus, partialDrainEntity *protos.Entity) (*DrainActionResult, error) {
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

		mapUserNamespacesToMode(&ns, forceImmediateEviction, userNamespace, matchedNamespaces)
	}

	return e.getAction(ctx, ns, nodeName, partialDrainEntity), nil
}

func mapUserNamespacesToMode(ns *namespaces, forceImmediateEviction bool, userNamespace config.UserNamespace,
	matchedNamespaces []string) {
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

func (e *NodeDrainEvaluator) getAction(ctx context.Context, ns namespaces, nodeName string,
	partialDrainEntity *protos.Entity) *DrainActionResult {
	if len(ns.immediateEvictionNamespaces) > 0 {
		timeout := e.config.EvictionTimeoutInSeconds.Duration
		if !e.informers.CheckIfAllPodsAreEvictedInImmediateMode(ctx, ns.immediateEvictionNamespaces, nodeName,
			timeout, partialDrainEntity) {
			slog.Info("Performing immediate eviction for node", "node", nodeName)

			return &DrainActionResult{
				Action:             ActionEvictImmediate,
				Namespaces:         ns.immediateEvictionNamespaces,
				Timeout:            timeout,
				PartialDrainEntity: partialDrainEntity,
			}
		}
	}

	// Priority 2: DeleteAfterTimeout - pods have a deadline and must be force-deleted after timeout
	// Process BEFORE AllowCompletion to ensure timeout-based eviction is not blocked
	if len(ns.deleteAfterTimeoutNamespaces) > 0 {
		action := e.handleDeleteAfterTimeoutNamespaces(ns, nodeName, partialDrainEntity)
		if action != nil {
			return action
		}
	}

	// Priority 3: AllowCompletion - pods wait indefinitely for natural completion
	// Checked last since they have no deadline (unlike DeleteAfterTimeout)
	if len(ns.allowCompletionNamespaces) > 0 {
		action := e.handleAllowCompletionNamespaces(ns, nodeName, partialDrainEntity)
		if action != nil {
			return action
		}
	}

	slog.Info("All pods evicted successfully on node", "node", nodeName)

	return &DrainActionResult{
		Action: ActionUpdateStatus,
		Status: model.StatusSucceeded,
	}
}

func (e *NodeDrainEvaluator) handleAllowCompletionNamespaces(ns namespaces, nodeName string,
	partialDrainEntity *protos.Entity) *DrainActionResult {
	hasRemainingPods := false

	for _, namespace := range ns.allowCompletionNamespaces {
		pods, err := e.informers.FindEvictablePodsInNamespaceAndNode(namespace, nodeName, partialDrainEntity)
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
			Action:             ActionCheckCompletion,
			Namespaces:         ns.allowCompletionNamespaces,
			PartialDrainEntity: partialDrainEntity,
		}
	}

	return nil
}

func (e *NodeDrainEvaluator) handleDeleteAfterTimeoutNamespaces(ns namespaces, nodeName string,
	partialDrainEntity *protos.Entity) *DrainActionResult {
	hasRemainingPods := false

	for _, namespace := range ns.deleteAfterTimeoutNamespaces {
		pods, err := e.informers.FindEvictablePodsInNamespaceAndNode(namespace, nodeName, partialDrainEntity)
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
			Action:             ActionEvictWithTimeout,
			Namespaces:         ns.deleteAfterTimeoutNamespaces,
			Timeout:            time.Duration(e.config.DeleteAfterTimeoutMinutes) * time.Minute,
			PartialDrainEntity: partialDrainEntity,
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

func (e *NodeDrainEvaluator) evaluateCustomDrain(ctx context.Context, healthEvent model.HealthEventWithStatus,
	database queue.DataStore, partialDrainEntity *protos.Entity) (*DrainActionResult, error) {
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
			Action:             ActionCreateCR,
			Namespaces:         namespaces,
			PartialDrainEntity: partialDrainEntity,
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

/*
This function determines whether we can skip draining for the current unhealthy HealthEvent. A full drain can be
skipped if a previous full drain completed against the node after it was most-recently quarantined. Additionally,
a partial drain can be skipped if either a previous full drain or a previous partial drain against the same impacted
entity completed against the node after it was most-recently quarantined.

To discover the set of unhealthy HealthEvents which may allow a drain to be skipped, we will fetch the
quarantineHealthEvent annotation for the current node. Each HealthEvent on the annotation includes the object ID
which can be used to query the backing DB for an up-to-date full HealthEventWithStatus object which includes its
current drain status.

Partial drain example:

1. Suppose the current HealthEvent received by the node-drainer is for event 70bd4fc9ffa9f5eca91c340c which has
recommended action COMPONENT_RESET and impacted entity GPU_UUID GPU-123.

2. Next, we fetch the quarantineHealthEvent annotation for the current node, and it has the following 2 events:

	   [{
	    id: '68bd4fc9ffa9f5eca91c340c',
		version: 1,
		agent: 'syslog-health-monitor',
		componentclass: 'GPU',
		checkname: 'SysLogsXIDError',
		isfatal: true,
		ishealthy: false,
		recommendedaction: 2,
		entitiesimpacted: [
		  {
			entitytype: 'GPU_UUID',
			entityvalue: 'GPU-123'
		  }
		],
		nodename: 'node-123'
	  },
	  {
	    id: '70bd4fc9ffa9f5eca91c340c',
		version: 1,
		agent: 'syslog-health-monitor',
		componentclass: 'GPU',
		checkname: 'SysLogsXIDError',
		isfatal: true,
		ishealthy: false,
		recommendedaction: 2,
		entitiesimpacted: [
		  {
			entitytype: 'GPU_UUID',
			entityvalue: 'GPU-123'
		  }
		],
		nodename: 'node-123'
	  }]

3. The second HealthEvent with ID 70bd4fc9ffa9f5eca91c340c matches the ID for our current event so we will ignore that
event. The first event has ID 68bd4fc9ffa9f5eca91c340c and corresponds to a different event so we will query our
backing database with this ID. Suppose the full HealthEventWithStatus item includes this status section:

		healtheventstatus: {
		  nodequarantined: 'Quarantined',
		  userpodsevictionstatus: {
		    status: 'Succeeded'
		  },
		  faultremediated: null
	    }

4. Since this previous drain completed and was a partial drain which matches the same GPU-123 impacted entity, we will
skip draining for the current event 70bd4fc9ffa9f5eca91c340c and update its status section to:

		healtheventstatus: {
		  nodequarantined: 'Quarantined',
		  userpodsevictionstatus: {
		    status: 'AlreadyDrained'
		  },
		  faultremediated: null
	    }
*/
func (e *NodeDrainEvaluator) isNodeAlreadyDrained(ctx context.Context, currentEventId string,
	currentPartialDrainEntity *protos.Entity, nodeName string, healthEventStore datastore.HealthEventStore) (bool, error) {
	node, err := e.informers.GetNode(nodeName)
	if err != nil {
		return false, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	quarantineHealthEventAnnotationStr, ok := node.Annotations[common.QuarantineHealthEventAnnotationKey]
	if !ok {
		slog.Info("No quarantine annotation found for node", "node", nodeName)

		return false, nil
	}

	var healthEventsMap annotation.HealthEventsAnnotationMap

	err = healthEventsMap.UnmarshalJSON([]byte(quarantineHealthEventAnnotationStr))
	if err != nil {
		return false, fmt.Errorf("failed to unmarshal quarantine annotation for node %s: %w", nodeName, err)
	}

	slog.Info("HealthEvents which are part of quarantineHealthEvent annotation", "eventCount", len(healthEventsMap.Events))

	for _, healthEventFromAnnotation := range healthEventsMap.Events {
		id := healthEventFromAnnotation.Id
		if len(id) == 0 {
			slog.Error("HealthEvent is missing ID for database lookup, expected for old events",
				"message", healthEventFromAnnotation.Message)

			continue
		}

		if id == currentEventId {
			continue
		}

		healthEventWithStatus, healthEvent, err := getHealthEventFromId(ctx, id, nodeName, healthEventStore)
		if err != nil {
			return false, err
		}
		// none of HealthEventStatus, UserPodsEvictionStatus, or Status are ptr values
		drainCompleted := healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus.Status == datastore.StatusSucceeded

		partialDrainEntity, err := e.shouldExecutePartialDrain(healthEvent)
		if err != nil {
			return false, err
		}

		skipDrain := canSkipDrain(drainCompleted, partialDrainEntity, currentPartialDrainEntity, id, nodeName)
		if skipDrain {
			return true, nil
		}
		// continue checking any other HealthEvents on quarantineHealthEvent annotation
	}

	return false, nil
}

func getHealthEventFromId(ctx context.Context, id string, nodeName string,
	healthEventStore datastore.HealthEventStore) (*datastore.HealthEventWithStatus, *protos.HealthEvent, error) {
	q := query.New().Build(
		query.Eq("_id", id),
	)

	events, err := healthEventStore.FindHealthEventsByQuery(ctx, q)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query health events for node %s and event ID %s: %w", nodeName, id, err)
	}

	if len(events) != 1 {
		return nil, nil, fmt.Errorf("unexpected number of events for node %s and event ID %s: %d", nodeName, id, len(events))
	}

	healthEventWithStatus := events[0]

	// We have custom types in datastore which aren't from model nor protos packages. For example,
	// datastore.HealthEventWithStatus.HealthEvent has type interface{}. If we check the underlying
	// type, we are returned with map[string]interface{}. To convert this to protos.HealthEvent, we will convert to
	// and from json rather than try to manually extract our fields.
	healthEventBytes, err := json.Marshal(healthEventWithStatus.HealthEvent)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal health event for node %s: %w", nodeName, err)
	}

	var healthEvent protos.HealthEvent
	if err := json.Unmarshal(healthEventBytes, &healthEvent); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal health event for node %s: %w", nodeName, err)
	}

	return &healthEventWithStatus, &healthEvent, nil
}

func canSkipDrain(drainCompleted bool, partialDrainEntity, currentPartialDrainEntity *protos.Entity,
	id, nodeName string) bool {
	if drainCompleted {
		// We previously executed a full drain and it's complete. We can skip the current drain whether it's a full
		// drain or a partial drain.
		if partialDrainEntity == nil {
			slog.Info("Full drain previously completed for node as part of old event, skipping drain",
				"node", nodeName, "id", id)

			return true
		}
		// If we previously completed a partial drain, we can skip the current drain if it's also a partial drain
		// that matches the same impacted entity
		if currentPartialDrainEntity != nil { // partialDrainEntity != nil
			// The protos.Entity struct type cannot be compared with equals operator. As a result,
			// we will check the identifying fields for EntityType and EntityValue rather than directly compare
			// the structs via *partialDrainEntity == *currentPartialDrainEntity
			partialDrainCompletedForSameEntity := partialDrainEntity.EntityType == currentPartialDrainEntity.EntityType &&
				partialDrainEntity.EntityValue == currentPartialDrainEntity.EntityValue
			if partialDrainCompletedForSameEntity {
				slog.Info("Partial drain previously completed for entity as part of old event, skipping drain",
					"node", nodeName, "id", id, "entityValue", currentPartialDrainEntity.EntityValue)

				return true
			}

			slog.Info("Partial drain previously completed for a different entity as part of old event",
				"node", nodeName, "id", id, "currentEntityValue", currentPartialDrainEntity.EntityValue,
				"oldEntityValue", partialDrainEntity.EntityValue)
		}
	}

	return false
}

/*
This function determines if the given unhealthy HealthEvent should result in a partial drain. A partial drain occurs if
the feature is enabled (from the partialDrainEnabled value in the node-drainer Helm chart), the recommended action is
COMPONENT_RESET, and the given unhealthy HealthEvent has an impacted entity which supports partial draining, which is
configured in pod_device_annotation.go. Currently, the node-drainer will execute partial drains against nodes which
have a COMPONENT_RESET recommended action and have a GPU_UUID impacted entity.

If the recommended action is COMPONENT_RESET but the given HealthEvent does not include a supported entity for partial
drain, we will return an error. For all other recommended actions, we will proceed with a full drain.
*/
func (e *NodeDrainEvaluator) shouldExecutePartialDrain(
	healthEvent *protos.HealthEvent) (*protos.Entity, error) {
	if e.config.PartialDrainEnabled && healthEvent.RecommendedAction == protos.RecommendedAction_COMPONENT_RESET {
		for _, entity := range healthEvent.GetEntitiesImpacted() {
			_, supportedEntity := model.EntityTypeToResourceNames[entity.EntityType]
			if supportedEntity && len(entity.EntityValue) != 0 {
				return entity, nil
			}
		}

		return nil, fmt.Errorf("no supported entities for a partial drain found in health event for node: %s",
			healthEvent.NodeName)
	}

	return nil, nil
}
