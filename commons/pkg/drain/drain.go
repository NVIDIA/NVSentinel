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

package drain

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
)

/*
IsNodeDrained returns if the given node is drained based on the current status of HealthEvents. The HealthEvent ID
is extracted from each event prior to looking up the current drain status for each event. If any event reports a
drain status of Succeeded, or AlreadyDrained without DrainOverrides.Skip, and if that HealthEvent covers
currentPartialEntity, then we consider the node drained. An AlreadyDrained status caused by DrainOverrides.Skip does
not count as proof of a completed drain since no pods were actually evicted for that event. Note that if
currentPartialEntity is nil, we require a full drain and if currentPartialEntity is non-nil, we require either a
full drain or a partial drain for the same partialDrainEntity. This function is used in 2 contexts:

1. Determining if a new event in node-drainer can be marked as AlreadyDrained (with support for partial or full drains)
- The excludeEventID is set to the new event so that we don't unnecessarily look its drain status
- The list of HealthEvents are sourced from the quarantineHealthEvent annotation
- If the current event has a partialDrainEntity, we support skipping the drain for either full drains or partial
drains targeting the same partialDrainEntity. If the current event has no partialDrainEntity, we require a full drain.

2. Determining if any event as part of a quarantine session completed a full drain
- The excludeEventID is not set because there is no notion of a current event to ignore
- The list of HealthEvents are sourced from the quarantineValidationHealthEvent annotation which persists unhealthy
events from the quarantine session that require post-remediation validation even if they recover and are removed from
the related quarantineHealthEvent annotation.
- We will always pass no currentPartialEntity, so an event only counts as proof of a full drain if it was not itself
partially drained — a partially drained event never proves the rest of the node was evicted.

In both contexts, the scope of an earlier event comes from its recorded drain decision when it has one. A record
with no scope is a full drain. A record with a scope is a partial drain of that entity. partialDrainEnabled is used
only for an event that has no record, such as an event written before node-drainer recorded decisions. When it is
false, an event with no record is treated as fully drained.
*/
func IsNodeDrained(ctx context.Context, healthEventStore datastore.HealthEventStore, nodeName string,
	events []*protos.HealthEvent, excludeEventID string, currentPartialEntity *protos.Entity,
	partialDrainEnabled bool) (bool, error) {
	for _, event := range events {
		if event == nil || len(event.Id) == 0 || event.Id == excludeEventID {
			continue
		}

		provesDrained, err := didDrainCompleteForEvent(ctx, healthEventStore, nodeName, event.Id, currentPartialEntity,
			partialDrainEnabled)
		if err != nil {
			return false, err
		}

		if provesDrained {
			return true, nil
		}
	}

	return false, nil
}

func didDrainCompleteForEvent(ctx context.Context, healthEventStore datastore.HealthEventStore, nodeName,
	eventID string, currentPartialEntity *protos.Entity, partialDrainEnabled bool) (bool, error) {
	status, healthEvent, err := getHealthEventFromId(ctx, healthEventStore, nodeName, eventID)
	if err != nil {
		return false, fmt.Errorf("looking up health event %s for node %s: %w", eventID, nodeName, err)
	}

	entity, err := earlierDrainScope(status, healthEvent, partialDrainEnabled)
	if err != nil {
		return false, fmt.Errorf("evaluating partial drain entity for health event %s on node %s: %w",
			eventID, nodeName, err)
	}

	drainStatus := status.HealthEventStatus.UserPodsEvictionStatus.Status
	wasSkipped := healthEvent.DrainOverrides != nil && healthEvent.DrainOverrides.Skip
	drainCompletedForEvent := drainStatus == datastore.StatusSucceeded ||
		(drainStatus == datastore.AlreadyDrained && !wasSkipped)

	return doesDrainCoverPartialEntity(ctx, drainCompletedForEvent, entity, currentPartialEntity, eventID, nodeName), nil
}

// preUpgradeEntityTypes is the partial drain rule of the versions that did not record the drain scope: only
// COMPONENT_RESET got a partial drain, scoped to GPU_UUID. It works out the scope of the events that those
// versions drained, so it does not follow the configuration. It has no devices, because working out the
// scope reads only the scoped actions.
var preUpgradeEntityTypes = model.PartialRemediationEntityTypes{
	model.GPUUUIDEntityType: {ScopedActions: []string{protos.RecommendedAction_COMPONENT_RESET.String()}},
}

// earlierDrainScope returns the entity an earlier drain covered, or nil for a drain of the full node.
// node-drainer records the scope on each event before it drains, so a later change to partialDrainEnabled or
// to partialRemediationEntityTypes does not rewrite it. An event without a record was drained by a version
// that did not record the scope and gave partial drains only to COMPONENT_RESET on GPU_UUID, so its scope is
// derived from preUpgradeEntityTypes.
func earlierDrainScope(status *datastore.HealthEventWithStatus, healthEvent *protos.HealthEvent,
	partialDrainEnabled bool) (*protos.Entity, error) {
	if decision := status.HealthEventStatus.DrainDecision; decision != nil {
		return scopeEntity(decision.Scope), nil
	}

	return PartialDrainEntity(healthEvent, partialDrainEnabled, preUpgradeEntityTypes)
}

// scopeEntity copies a stored scope into the proto entity the coverage check compares.
// A nil scope is a drain of the full node.
func scopeEntity(scope *datastore.Entity) *protos.Entity {
	if scope == nil {
		return nil
	}

	return &protos.Entity{EntityType: scope.EntityType, EntityValue: scope.EntityValue}
}

func getHealthEventFromId(ctx context.Context, healthEventStore datastore.HealthEventStore, nodeName,
	id string) (*datastore.HealthEventWithStatus, *protos.HealthEvent, error) {
	q := query.New().Build(query.Eq("_id", id))

	events, err := healthEventStore.FindHealthEventsByQuery(ctx, q)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query health events for node %s and event ID %s: %w", nodeName, id, err)
	}

	if len(events) != 1 {
		return nil, nil, fmt.Errorf(
			"unexpected number of events for node %s and event ID %s: %d", nodeName, id, len(events))
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

/*
PartialDrainEntity returns the entity the given HealthEvent's drain is scoped to (a partial
drain) or nil if it requires a full drain. A HealthEvent qualifies for a partial drain only if
partial drain is enabled (the partialDrainEnabled value in the node-drainer Helm chart) and its
effective action, including a custom action name, is listed in the scopedActions of an entity type.
The drain is then scoped to the first entity of the event with that type and a non-empty value.

If the action is scoped but the given HealthEvent has no such entity, we will return an error and the
drain is retried. In all other cases, we will proceed with a full drain.
*/
func PartialDrainEntity(healthEvent *protos.HealthEvent, partialDrainEnabled bool,
	entityTypes model.PartialRemediationEntityTypes) (*protos.Entity, error) {
	if !partialDrainEnabled {
		return nil, nil
	}

	entityType, scoped := entityTypes.EntityTypeForAction(model.GetEffectiveActionName(healthEvent))
	if !scoped {
		return nil, nil
	}

	selected, err := eventutil.FirstImpactedEntity(healthEvent, entityType)
	if err != nil {
		return nil, fmt.Errorf("no supported entities for a partial drain found in health event for node: %s; "+
			"action %s is scoped to entity type %s: %w", healthEvent.NodeName,
			model.GetEffectiveActionName(healthEvent), entityType, err)
	}

	return selected, nil
}

func doesDrainCoverPartialEntity(ctx context.Context, drainCompleted bool, partialEntity,
	currentPartialEntity *protos.Entity, id, nodeName string) bool {
	if drainCompleted {
		// We previously completed a full drain. We can skip the current drain whether it's a full drain or
		// a partial drain.
		if partialEntity == nil {
			slog.InfoContext(ctx, "Full drain previously completed for node as part of old event, skipping drain",
				"node", nodeName, "id", id)

			return true
		}
		// If we previously completed a partial drain, we can skip the current drain if it's also a partial drain
		// that matches the same impacted entity
		if currentPartialEntity != nil { // partialEntity != nil
			// The protos.Entity struct type cannot be compared with equals operator. As a result,
			// we will check the identifying fields for EntityType and EntityValue rather than directly compare
			// the structs via *partialEntity == *currentPartialEntity
			partialDrainCompletedForSameEntity := partialEntity.EntityType == currentPartialEntity.EntityType &&
				partialEntity.EntityValue == currentPartialEntity.EntityValue
			if partialDrainCompletedForSameEntity {
				slog.InfoContext(ctx, "Partial drain previously completed for entity as part of old event, skipping drain",
					"node", nodeName, "id", id, "entityValue", currentPartialEntity.EntityValue)

				return true
			}

			slog.InfoContext(ctx, "Partial drain previously completed for a different entity as part of old event",
				"node", nodeName, "id", id, "currentEntityValue", currentPartialEntity.EntityValue,
				"oldEntityValue", partialEntity.EntityValue)
		}
	}

	return false
}
