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
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type recoveryIdentity struct {
	key      string
	nodeName string
	entities []*protos.Entity
}

type recoveryBoundary struct {
	createdAt time.Time
	generated *timestamppb.Timestamp
}

type derivedState struct {
	boundary  recoveryBoundary
	isHealthy bool
}

const (
	defaultRecoveryPollInterval      = 250 * time.Millisecond
	defaultRecoveryRepublishInterval = 30 * time.Second
)

func recoveryIdentityForEvent(
	rule config.HealthEventsAnalyzerRule,
	event *protos.HealthEvent,
) (recoveryIdentity, bool) {
	if rule.Recovery == nil || event == nil || event.NodeName == "" {
		return recoveryIdentity{}, false
	}

	identity := recoveryIdentity{
		nodeName: event.NodeName,
		key:      event.NodeName,
	}

	if rule.Recovery.Scope == config.RecoveryScopeNode {
		return identity, true
	}

	entities, foundAllTypes := recoveryEntities(event.EntitiesImpacted, rule.Recovery.EntityTypes)
	if !foundAllTypes {
		return recoveryIdentity{}, false
	}

	identity.entities = entities
	identity.key = recoveryEntityKey(event.NodeName, entities)

	return identity, true
}

func recoveryEntities(entities []*protos.Entity, entityTypes []string) ([]*protos.Entity, bool) {
	allowedTypes := make(map[string]struct{}, len(entityTypes))
	for _, entityType := range entityTypes {
		allowedTypes[entityType] = struct{}{}
	}

	selected := make([]*protos.Entity, 0, len(entityTypes))
	foundTypes := make(map[string]struct{}, len(allowedTypes))

	for _, entity := range entities {
		if entity == nil || entity.EntityValue == "" {
			continue
		}

		if _, ok := allowedTypes[entity.EntityType]; !ok {
			continue
		}

		selected = append(selected, proto.Clone(entity).(*protos.Entity))
		foundTypes[entity.EntityType] = struct{}{}
	}

	if len(foundTypes) != len(allowedTypes) {
		return nil, false
	}

	slices.SortFunc(selected, func(a, b *protos.Entity) int {
		if result := strings.Compare(a.EntityType, b.EntityType); result != 0 {
			return result
		}

		return strings.Compare(a.EntityValue, b.EntityValue)
	})

	return selected, true
}

func recoveryEntityKey(nodeName string, entities []*protos.Entity) string {
	var key strings.Builder
	key.WriteString(nodeName)

	for _, entity := range entities {
		fmt.Fprintf(&key, "|%d:%s=%d:%s", len(entity.EntityType), entity.EntityType,
			len(entity.EntityValue), entity.EntityValue)
	}

	return key.String()
}

func recoverySourceMatches(mapping *config.RecoveryMapping, event *protos.HealthEvent) bool {
	if mapping == nil || event == nil || !event.IsHealthy || event.CheckName != mapping.SourceCheckName {
		return false
	}

	if mapping.SourceAgent != "" && event.Agent != mapping.SourceAgent {
		return false
	}

	if len(mapping.SourceErrorCodes) == 0 {
		return true
	}

	for _, eventCode := range event.ErrorCode {
		if slices.Contains(mapping.SourceErrorCodes, eventCode) {
			return true
		}
	}

	return false
}

func recoveryStateKey(ruleName string, identity recoveryIdentity) string {
	return ruleName + "\x00" + identity.key
}

func (r *Reconciler) handleRecoveryEvents(
	ctx context.Context,
	event *datamodels.HealthEventWithStatus,
) (bool, error) {
	if event == nil || event.HealthEvent == nil || !event.HealthEvent.IsHealthy {
		return false, nil
	}

	published := false

	for _, rule := range r.config.HealthEventsAnalyzerRules.Rules {
		recovered, err := r.handleRecoveryRule(ctx, event, rule)
		if err != nil {
			return published, err
		}

		published = recovered || published
	}

	return published, nil
}

func (r *Reconciler) handleRecoveryRule(
	ctx context.Context,
	event *datamodels.HealthEventWithStatus,
	rule config.HealthEventsAnalyzerRule,
) (bool, error) {
	if !rule.EvaluateRule || !recoverySourceMatches(rule.Recovery, event.HealthEvent) {
		return false, nil
	}

	identity, ok := recoveryIdentityForEvent(rule, event.HealthEvent)
	if !ok {
		slog.WarnContext(ctx, "Recovery event does not contain the configured scope",
			"rule_name", rule.Name,
			"node", event.HealthEvent.NodeName,
			"entity_types", rule.Recovery.EntityTypes)

		return false, nil
	}

	sourceBoundary := boundaryFromEvent(event)

	state, found, err := r.currentDerivedState(ctx, rule, identity)
	if err != nil {
		return false, fmt.Errorf("find current state for rule %q: %w", rule.Name, err)
	}

	// A delayed healthy event must not clear a newer derived condition or move
	// its history boundary forward. Generated time is authoritative when both
	// events provide it; persisted time handles legacy events and breaks ties.
	if found && !boundaryAfter(sourceBoundary, state.boundary) {
		return false, nil
	}

	r.rememberRecoveryBoundary(rule.Name, identity, sourceBoundary)

	if !found || state.isHealthy {
		return false, nil
	}

	persistedBoundary, err := r.publishRecoveryUntilStored(ctx, event, rule, identity)
	if err != nil {
		return false, fmt.Errorf("publish recovery for rule %q: %w", rule.Name, err)
	}

	r.rememberDerivedState(rule.Name, identity, derivedState{
		boundary:  persistedBoundary,
		isHealthy: true,
	})
	recoveryEventsPublishedTotal.WithLabelValues(rule.Name, string(rule.Recovery.Scope)).Inc()

	return true, nil
}

func (r *Reconciler) recoveryBoundaryForEvent(
	ctx context.Context,
	rule config.HealthEventsAnalyzerRule,
	event *protos.HealthEvent,
) (*recoveryBoundary, error) {
	identity, ok := recoveryIdentityForEvent(rule, event)
	if !ok {
		return nil, nil
	}

	if boundary, found := r.cachedRecoveryBoundary(rule.Name, identity); found {
		return &boundary, nil
	}

	latest, err := r.findLatestMatchingEvent(ctx, r.recoveryLookupFilter(
		rule.Recovery.SourceAgent, rule.Recovery.SourceCheckName, identity.nodeName,
	), func(candidate *datamodels.HealthEventWithStatus) bool {
		if !recoverySourceMatches(rule.Recovery, candidate.HealthEvent) {
			return false
		}

		candidateIdentity, valid := recoveryIdentityForEvent(rule, candidate.HealthEvent)

		return valid && candidateIdentity.key == identity.key
	})
	if err != nil {
		return nil, fmt.Errorf("find latest recovery source: %w", err)
	}

	if latest == nil {
		return nil, nil
	}

	boundary := boundaryFromEvent(latest)
	r.rememberRecoveryBoundary(rule.Name, identity, boundary)

	return &boundary, nil
}

func (r *Reconciler) currentDerivedState(
	ctx context.Context,
	rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
) (state derivedState, found bool, err error) {
	if state, found := r.cachedDerivedState(rule.Name, identity); found {
		return state, true, nil
	}

	latest, err := r.findLatestMatchingEvent(ctx, r.recoveryLookupFilter(
		agentName, rule.Name, identity.nodeName,
	), func(candidate *datamodels.HealthEventWithStatus) bool {
		candidateIdentity, valid := recoveryIdentityForEvent(rule, candidate.HealthEvent)
		return valid && candidateIdentity.key == identity.key
	})
	if err != nil {
		return derivedState{}, false, err
	}

	if latest == nil {
		return derivedState{}, false, nil
	}

	return derivedState{
		boundary:  boundaryFromEvent(latest),
		isHealthy: latest.HealthEvent.IsHealthy,
	}, true, nil
}

// publishRecoveryUntilStored keeps the source event in-flight until the store
// connector has durably inserted the corresponding derived recovery. The
// platform-connector RPC acknowledges an in-memory enqueue, so returning after
// the RPC alone would allow the source resume token to advance before the
// recovery is durable.
func (r *Reconciler) publishRecoveryUntilStored(
	ctx context.Context,
	source *datamodels.HealthEventWithStatus,
	rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
) (recoveryBoundary, error) {
	return r.publishDerivedUntilStored(ctx, source, rule, identity, true, "recovery", func() error {
		return r.config.Publisher.PublishRecovery(
			ctx, source.HealthEvent, rule.Name, identity.entities, &rule,
		)
	})
}

func (r *Reconciler) publishFaultUntilStored(
	ctx context.Context,
	source *datamodels.HealthEventWithStatus,
	rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
) (recoveryBoundary, error) {
	event := source.HealthEvent
	if rule.Recovery.Scope == config.RecoveryScopeEntity {
		event = proto.Clone(source.HealthEvent).(*protos.HealthEvent)
		event.EntitiesImpacted = identity.entities
	}

	action := protos.RecommendedAction(r.getRecommendedActionValue(rule.RecommendedAction, rule.Name))

	return r.publishDerivedUntilStored(ctx, source, rule, identity, false, "fault", func() error {
		return r.config.Publisher.Publish(ctx, event, action, rule.Name, rule.Message, &rule)
	})
}

func (r *Reconciler) publishDerivedUntilStored(
	ctx context.Context,
	source *datamodels.HealthEventWithStatus,
	rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
	isHealthy bool,
	stateName string,
	publish func() error,
) (recoveryBoundary, error) {
	pollInterval, republishInterval := r.recoveryIntervals()

	nextPublish := time.Time{}

	for {
		nextPublish = publishDerivedIfDue(
			ctx, publish, stateName, rule.Name, identity.nodeName,
			nextPublish, pollInterval, republishInterval,
		)

		persisted, err := r.findPersistedDerived(ctx, source, rule, identity, isHealthy)
		if err == nil && persisted != nil {
			return boundaryFromEvent(persisted), nil
		}

		if err != nil {
			slog.WarnContext(ctx, "Failed to confirm persisted derived event; retrying",
				"state", stateName,
				"rule_name", rule.Name,
				"node", identity.nodeName,
				"error", err)
		}

		select {
		case <-ctx.Done():
			return recoveryBoundary{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (r *Reconciler) recoveryIntervals() (time.Duration, time.Duration) {
	pollInterval := r.recoveryPoll
	if pollInterval <= 0 {
		pollInterval = defaultRecoveryPollInterval
	}

	republishInterval := r.recoveryRepublish
	if republishInterval <= 0 {
		republishInterval = defaultRecoveryRepublishInterval
	}

	return pollInterval, republishInterval
}

func publishDerivedIfDue(
	ctx context.Context,
	publish func() error,
	stateName string,
	ruleName string,
	nodeName string,
	nextPublish time.Time,
	pollInterval time.Duration,
	republishInterval time.Duration,
) time.Time {
	now := time.Now()
	if nextPublish.After(now) {
		return nextPublish
	}

	err := publish()
	if err != nil {
		slog.WarnContext(ctx, "Failed to enqueue derived event; retrying",
			"state", stateName,
			"rule_name", ruleName,
			"node", nodeName,
			"error", err)

		return now.Add(pollInterval)
	}

	return now.Add(republishInterval)
}

func (r *Reconciler) findPersistedDerived(
	ctx context.Context,
	source *datamodels.HealthEventWithStatus,
	rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
	isHealthy bool,
) (*datamodels.HealthEventWithStatus, error) {
	return r.findLatestMatchingEvent(ctx, r.recoveryLookupFilter(
		agentName, rule.Name, identity.nodeName,
	), func(candidate *datamodels.HealthEventWithStatus) bool {
		if candidate.HealthEvent.IsHealthy != isHealthy ||
			!sameRecoverySource(candidate, source) {
			return false
		}

		candidateIdentity, valid := recoveryIdentityForEvent(rule, candidate.HealthEvent)

		return valid && candidateIdentity.key == identity.key
	})
}

func sameRecoverySource(
	candidate *datamodels.HealthEventWithStatus,
	source *datamodels.HealthEventWithStatus,
) bool {
	if candidate == nil || candidate.HealthEvent == nil || source == nil || source.HealthEvent == nil {
		return false
	}

	if !proto.Equal(candidate.HealthEvent.GeneratedTimestamp, source.HealthEvent.GeneratedTimestamp) {
		return false
	}

	return source.CreatedAt.IsZero() || !candidate.CreatedAt.Before(source.CreatedAt)
}

func boundaryAfter(candidate, current recoveryBoundary) bool {
	if candidate.generated != nil && current.generated != nil {
		candidateTime := candidate.generated.AsTime()
		currentTime := current.generated.AsTime()

		if !candidateTime.Equal(currentTime) {
			return candidateTime.After(currentTime)
		}
	}

	if !candidate.createdAt.IsZero() && !current.createdAt.IsZero() {
		return candidate.createdAt.After(current.createdAt)
	}

	return false
}

func (r *Reconciler) recoveryLookupFilter(agent, checkName, nodeName string) map[string]any {
	checkNameField := "healthevent.checkname"
	nodeNameField := "healthevent.nodename"

	if r.provider == datastore.ProviderPostgreSQL {
		checkNameField = "event_type"
		nodeNameField = "node_name"
	}

	filter := map[string]any{
		checkNameField: checkName,
		nodeNameField:  nodeName,
	}
	if agent != "" {
		filter["healthevent.agent"] = agent
	}

	return filter
}

func (r *Reconciler) findLatestMatchingEvent(
	ctx context.Context,
	filter map[string]any,
	matches func(*datamodels.HealthEventWithStatus) bool,
) (*datamodels.HealthEventWithStatus, error) {
	cursor, err := r.databaseClient.Find(ctx, filter, &client.FindOptions{
		Sort: map[string]any{"createdAt": -1},
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var latest *datamodels.HealthEventWithStatus

	for cursor.Next(ctx) {
		var candidate datamodels.HealthEventWithStatus
		if err := cursor.Decode(&candidate); err != nil {
			return nil, fmt.Errorf("decode health event: %w", err)
		}

		if candidate.HealthEvent == nil || !matches(&candidate) {
			continue
		}

		if latest == nil || boundaryAfter(boundaryFromEvent(&candidate), boundaryFromEvent(latest)) {
			matched := candidate
			latest = &matched
		}
	}

	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate health events: %w", err)
	}

	return latest, nil
}

func boundaryFromEvent(event *datamodels.HealthEventWithStatus) recoveryBoundary {
	boundary := recoveryBoundary{createdAt: event.CreatedAt}

	if event.HealthEvent != nil && event.HealthEvent.GeneratedTimestamp != nil &&
		event.HealthEvent.GeneratedTimestamp.CheckValid() == nil {
		boundary.generated = proto.Clone(event.HealthEvent.GeneratedTimestamp).(*timestamppb.Timestamp)
	}

	return boundary
}

func (r *Reconciler) rememberRecoveryBoundary(
	ruleName string,
	identity recoveryIdentity,
	boundary recoveryBoundary,
) {
	r.recoveryMu.Lock()
	defer r.recoveryMu.Unlock()

	if r.recoveryBoundaries == nil {
		r.recoveryBoundaries = make(map[string]recoveryBoundary)
	}

	key := recoveryStateKey(ruleName, identity)

	current, exists := r.recoveryBoundaries[key]
	if !exists || boundaryAfter(boundary, current) {
		r.recoveryBoundaries[key] = boundary
	}
}

func (r *Reconciler) cachedRecoveryBoundary(
	ruleName string,
	identity recoveryIdentity,
) (recoveryBoundary, bool) {
	r.recoveryMu.RLock()
	defer r.recoveryMu.RUnlock()

	boundary, found := r.recoveryBoundaries[recoveryStateKey(ruleName, identity)]

	return boundary, found
}

func (r *Reconciler) rememberDerivedState(
	ruleName string,
	identity recoveryIdentity,
	state derivedState,
) {
	r.recoveryMu.Lock()
	defer r.recoveryMu.Unlock()

	if r.derivedStates == nil {
		r.derivedStates = make(map[string]derivedState)
	}

	r.derivedStates[recoveryStateKey(ruleName, identity)] = state
}

func (r *Reconciler) cachedDerivedState(
	ruleName string,
	identity recoveryIdentity,
) (derivedState, bool) {
	r.recoveryMu.RLock()
	defer r.recoveryMu.RUnlock()

	state, found := r.derivedStates[recoveryStateKey(ruleName, identity)]

	return state, found
}
