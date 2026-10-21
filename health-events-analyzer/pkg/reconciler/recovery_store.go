// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

const recoveryGreaterThan = "$gt"

func derivedFilter(rule config.HealthEventsAnalyzerRule, node string) map[string]any {
	return map[string]any{"healthevent.agent": agentName, "healthevent.checkname": rule.Name, fieldNodeName: node}
}

// A malformed record fails this request. The annotation remains available for
// retry and the controller reports a Warning Event; no per-record holdbacks.
func (r *Reconciler) scanDerived(ctx context.Context, filter map[string]any,
	visit func(*datamodels.HealthEventWithStatus) error) error {
	cursor, err := r.databaseClient.Find(ctx, filter, &client.FindOptions{Sort: map[string]any{"createdAt": -1}})
	if err != nil {
		return fmt.Errorf("find derived events: %w", err)
	}
	defer func() {
		if err := cursor.Close(ctx); err != nil {
			slog.WarnContext(ctx, "Close recovery cursor", "error", err)
		}
	}()

	for cursor.Next(ctx) {
		var event datamodels.HealthEventWithStatus
		if err := cursor.Decode(&event); err != nil {
			return fmt.Errorf("decode derived event: %w", err)
		}

		if event.HealthEvent == nil {
			return fmt.Errorf("derived record is missing its health event")
		}

		if err := event.HealthEvent.GeneratedTimestamp.CheckValid(); err != nil {
			return fmt.Errorf("invalid derived generation time: %w", err)
		}

		if err := visit(&event); err != nil {
			return err
		}
	}

	return cursor.Err()
}

func (r *Reconciler) derivedStatesForNode(ctx context.Context, rule config.HealthEventsAnalyzerRule,
	node string) (map[string]*datamodels.HealthEventWithStatus, error) {
	states := make(map[string]*datamodels.HealthEventWithStatus)
	err := r.scanDerived(ctx, derivedFilter(rule, node), func(candidate *datamodels.HealthEventWithStatus) error {
		identity, ok := recoveryIdentityForEvent(rule, candidate.HealthEvent)
		if !ok {
			return nil
		}

		// Downstream recovery distinguishes component class and event version.
		// Keep each active variant even when a rule uses the same entity scope.
		key := fmt.Sprintf("%s|%d:%s|%d", identity.key, len(candidate.HealthEvent.ComponentClass),
			candidate.HealthEvent.ComponentClass, candidate.HealthEvent.Version)

		current := states[key]
		if current == nil || derivedAfter(candidate, current) {
			states[key] = candidate
		}

		return nil
	})

	return states, err
}

func derivedAfter(a, b *datamodels.HealthEventWithStatus) bool {
	at, bt := a.HealthEvent.GeneratedTimestamp.AsTime(), b.HealthEvent.GeneratedTimestamp.AsTime()
	return at.After(bt) || (at.Equal(bt) && a.CreatedAt.After(b.CreatedAt))
}

func (r *Reconciler) recoveryStored(ctx context.Context, event *protos.HealthEvent,
	rule config.HealthEventsAnalyzerRule, identity recoveryIdentity) (bool, error) {
	filter := derivedFilter(rule, event.NodeName)
	filter["healthevent.ishealthy"] = true
	filter["healthevent.metadata."+annotationRequestKey] = event.Metadata[annotationRequestKey]
	found := false
	err := r.scanDerived(ctx, filter, func(candidate *datamodels.HealthEventWithStatus) error {
		other, ok := recoveryIdentityForEvent(rule, candidate.HealthEvent)
		if ok && other.key == identity.key && candidate.HealthEvent.ComponentClass == event.ComponentClass &&
			candidate.HealthEvent.Version == event.Version {
			found = true
		}

		return nil
	})

	return found, err
}

func (r *Reconciler) latestRecoveryTime(ctx context.Context, rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity) (time.Time, error) {
	if r.nodeRecovery == nil {
		return time.Time{}, fmt.Errorf("annotation recovery requires a synchronized node cache")
	}

	node, err := r.nodeRecovery.nodes.Get(identity.nodeName)
	if apierrors.IsNotFound(err) {
		return time.Time{}, nil
	}

	if err != nil {
		return time.Time{}, err
	}

	filter := derivedFilter(rule, identity.nodeName)
	filter["healthevent.ishealthy"] = true
	filter["healthevent.metadata."+annotationNodeUIDKey] = string(node.UID)
	filter["healthevent.metadata."+annotationMetadataKey] = rule.Recovery.AnnotationKey

	var latest time.Time

	err = r.scanDerived(ctx, filter, func(candidate *datamodels.HealthEventWithStatus) error {
		other, ok := recoveryIdentityForEvent(rule, candidate.HealthEvent)
		if !ok || other.key != identity.key {
			return nil
		}

		verified, err := time.Parse(time.RFC3339Nano, candidate.HealthEvent.Metadata[annotationVerifiedAtKey])
		if err != nil {
			return fmt.Errorf("invalid stored recovery verification time: %w", err)
		}

		if verified.Before(node.CreationTimestamp.Time) || verified.After(candidate.HealthEvent.GeneratedTimestamp.AsTime()) {
			return fmt.Errorf("stored recovery verification time is outside node lifetime or publication time")
		}

		if verified.After(latest) {
			latest = verified
		}

		return nil
	})

	return latest, err
}

func (r *Reconciler) applyRecoveryBoundary(ctx context.Context, rule config.HealthEventsAnalyzerRule,
	event *protos.HealthEvent, pipeline []map[string]any) error {
	identity, ok := recoveryIdentityForEvent(rule, event)
	if !ok {
		return nil
	}

	verified, err := r.latestRecoveryTime(ctx, rule, identity)
	if err != nil {
		return err
	}

	match := pipeline[0]["$match"].(map[string]any)

	if len(identity.entities) > 0 {
		entities := make([]any, 0, len(identity.entities))
		for _, entity := range identity.entities {
			entities = append(entities, map[string]any{"$elemMatch": map[string]any{
				"entitytype": entity.EntityType, "entityvalue": entity.EntityValue,
			}})
		}

		match["healthevent.entitiesimpacted"] = map[string]any{"$all": entities}
	}

	if !verified.IsZero() {
		match["createdAt"] = map[string]any{recoveryGreaterThan: verified}
		match["$and"] = []any{map[string]any{"$or": []any{
			map[string]any{"healthevent.generatedtimestamp.seconds": map[string]any{recoveryGreaterThan: verified.Unix()}},
			map[string]any{
				"healthevent.generatedtimestamp.seconds": verified.Unix(),
				"healthevent.generatedtimestamp.nanos":   map[string]any{recoveryGreaterThan: verified.Nanosecond()},
			},
		}}}
	}

	return nil
}
