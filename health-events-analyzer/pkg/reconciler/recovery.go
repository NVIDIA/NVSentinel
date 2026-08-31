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
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"time"

	multierror "github.com/hashicorp/go-multierror"
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

type recoveryTarget struct {
	identity recoveryIdentity
	state    derivedState
}

type storedDocumentDecodeError struct {
	cause error
}

func (e *storedDocumentDecodeError) Error() string {
	return e.cause.Error()
}

func (e *storedDocumentDecodeError) Unwrap() error {
	return e.cause
}

type replayableStoredDocumentError struct {
	ruleName string
	cause    error
}

func (e *replayableStoredDocumentError) Error() string {
	return fmt.Sprintf("stored state prevented recovery rule %q from completing: %v", e.ruleName, e.cause)
}

const (
	defaultRecoveryPollInterval       = 250 * time.Millisecond
	defaultRecoveryRepublishInterval  = 30 * time.Second
	defaultRecoveryPersistenceTimeout = 2 * time.Minute
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

func recoveryIdentityForSource(
	rule config.HealthEventsAnalyzerRule,
	event *protos.HealthEvent,
) (identity recoveryIdentity, nodeWide bool, ok bool) {
	if rule.Recovery == nil || event == nil || event.NodeName == "" {
		return recoveryIdentity{}, false, false
	}

	if rule.Recovery.Scope == config.RecoveryScopeNode {
		identity, ok := recoveryIdentityForEvent(rule, event)
		return identity, false, ok
	}

	identity, ok = recoveryIdentityForEvent(rule, event)
	if ok {
		return identity, false, true
	}

	if len(event.EntitiesImpacted) != 0 {
		return recoveryIdentity{}, false, false
	}

	return nodeRecoveryIdentity(event.NodeName), true, true
}

func nodeRecoveryIdentity(nodeName string) recoveryIdentity {
	return recoveryIdentity{
		nodeName: nodeName,
		key:      nodeName + "|*",
	}
}

func recoveryEntities(entities []*protos.Entity, entityTypes []string) ([]*protos.Entity, bool) {
	allowedTypes := make(map[string]struct{}, len(entityTypes))
	for _, entityType := range entityTypes {
		allowedTypes[entityType] = struct{}{}
	}

	selectedByType := make(map[string]*protos.Entity, len(entityTypes))

	for _, entity := range entities {
		if !selectRecoveryEntity(selectedByType, allowedTypes, entity) {
			return nil, false
		}
	}

	if len(selectedByType) != len(allowedTypes) {
		return nil, false
	}

	selected := make([]*protos.Entity, 0, len(selectedByType))
	for _, entity := range selectedByType {
		selected = append(selected, entity)
	}

	slices.SortFunc(selected, func(a, b *protos.Entity) int {
		if result := strings.Compare(a.EntityType, b.EntityType); result != 0 {
			return result
		}

		return strings.Compare(a.EntityValue, b.EntityValue)
	})

	return selected, true
}

func selectRecoveryEntity(
	selected map[string]*protos.Entity,
	allowed map[string]struct{},
	entity *protos.Entity,
) bool {
	if entity == nil || entity.EntityValue == "" {
		return true
	}

	if _, ok := allowed[entity.EntityType]; !ok {
		return true
	}

	existing, found := selected[entity.EntityType]
	if found {
		return existing.EntityValue == entity.EntityValue
	}

	selected[entity.EntityType] = proto.Clone(entity).(*protos.Entity)

	return true
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

	var multiErr *multierror.Error

	for _, rule := range r.config.HealthEventsAnalyzerRules.Rules {
		recovered, err := r.handleRecoveryRule(ctx, event, rule)
		if err != nil {
			published = recovered || published

			var decodeErr *storedDocumentDecodeError
			if errors.As(err, &decodeErr) {
				slog.ErrorContext(ctx, "Recovery remains incomplete after stored document decode failure",
					"rule_name", rule.Name, "error", err)
				totalEventProcessingError.WithLabelValues("recovery_stored_document_decode_error").Inc()
				// The malformed document is a dependency of this valid source event.
				// Do not expose a nested PermanentError to the event processor, which
				// would checkpoint the source and discard the recovery opportunity.
				multiErr = multierror.Append(multiErr, &replayableStoredDocumentError{
					ruleName: rule.Name,
					cause:    err,
				})

				continue
			}

			if client.IsPermanentError(err) {
				slog.ErrorContext(ctx, "Skipping recovery rule after deterministic failure",
					"rule_name", rule.Name, "error", err)
				totalEventProcessingError.WithLabelValues("permanent_recovery_rule_error").Inc()

				continue
			}

			multiErr = multierror.Append(multiErr, err)

			continue
		}

		published = recovered || published
	}

	return published, multiErr.ErrorOrNil()
}

//nolint:cyclop // Recovery keeps partial scan, publication, and boundary failures distinct.
func (r *Reconciler) handleRecoveryRule(
	ctx context.Context,
	event *datamodels.HealthEventWithStatus,
	rule config.HealthEventsAnalyzerRule,
) (bool, error) {
	if !rule.EvaluateRule || !recoverySourceMatches(rule.Recovery, event.HealthEvent) {
		return false, nil
	}

	identity, nodeWide, ok := recoveryIdentityForSource(rule, event.HealthEvent)
	if !ok {
		slog.WarnContext(ctx, "Recovery event does not contain the configured scope",
			"rule_name", rule.Name,
			"node", event.HealthEvent.NodeName,
			"entity_types", rule.Recovery.EntityTypes)

		return false, nil
	}

	sourceBoundary := boundaryFromEvent(event)

	targets, targetErr := r.recoveryTargets(ctx, rule, identity, nodeWide)
	if targetErr != nil && len(targets) == 0 {
		return false, fmt.Errorf("find current states for rule %q: %w", rule.Name, targetErr)
	}

	if !nodeWide && len(targets) == 0 {
		// A verified recovery also resets rule history when there is no derived
		// condition to clear yet.
		r.rememberRecoveryBoundary(rule.Name, identity, sourceBoundary)
	}

	published, err := r.publishRecoveryTargets(ctx, event, rule, targets, sourceBoundary, false)
	if err != nil {
		if targetErr != nil {
			return published, multierror.Append(nil, targetErr, err).ErrorOrNil()
		}

		return published, err
	}

	if targetErr != nil {
		return published, fmt.Errorf("find current states for rule %q: %w", rule.Name, targetErr)
	}

	// A node-wide boundary affects every entity on the node, so expose it only
	// after all applicable derived conditions have been durably recovered.
	if nodeWide {
		r.rememberRecoveryBoundary(rule.Name, identity, sourceBoundary)
	}

	return published, nil
}

func (r *Reconciler) publishRecoveryTargets(
	ctx context.Context,
	event *datamodels.HealthEventWithStatus,
	rule config.HealthEventsAnalyzerRule,
	targets []recoveryTarget,
	sourceBoundary recoveryBoundary,
	published bool,
) (bool, error) {
	for _, target := range targets {
		// A delayed healthy event must not clear a newer derived condition or
		// move its history boundary forward.
		if !boundaryAfter(sourceBoundary, target.state.boundary) {
			continue
		}

		if target.state.isHealthy {
			r.rememberRecoveryBoundary(rule.Name, target.identity, sourceBoundary)
			continue
		}

		persistedBoundary, didPublish, err := r.publishRecoveryUntilStored(ctx, event, rule, target.identity)
		if err != nil {
			return published, fmt.Errorf("publish recovery for rule %q: %w", rule.Name, err)
		}

		r.rememberRecoveryBoundary(rule.Name, target.identity, sourceBoundary)
		r.rememberDerivedState(rule.Name, target.identity, derivedState{
			boundary:  persistedBoundary,
			isHealthy: true,
		})

		if didPublish {
			recoveryEventsPublishedTotal.WithLabelValues(rule.Name, string(rule.Recovery.Scope)).Inc()

			published = true
		}
	}

	return published, nil
}

func (r *Reconciler) recoveryTargets(
	ctx context.Context,
	rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
	nodeWide bool,
) ([]recoveryTarget, error) {
	if nodeWide {
		return r.currentDerivedStatesForNode(ctx, rule, identity.nodeName)
	}

	state, found, err := r.currentDerivedState(ctx, rule, identity)
	if err != nil || !found {
		return nil, err
	}

	return []recoveryTarget{{identity: identity, state: state}}, nil
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

	if boundary, found := r.cachedEffectiveRecoveryBoundary(rule, identity); found {
		effective, err := r.recoveryBoundaryIsEffective(ctx, rule, identity, boundary)
		if err != nil {
			return nil, fmt.Errorf("validate cached recovery boundary: %w", err)
		}

		if !effective {
			return nil, nil
		}

		return &boundary, nil
	}

	if r.recoveryLookupLoaded(rule.Name, identity) {
		return nil, nil
	}

	latest, err := r.latestRecoverySource(ctx, rule, identity)
	if err != nil {
		return nil, fmt.Errorf("find latest recovery source: %w", err)
	}

	if latest == nil {
		r.rememberRecoveryLookup(rule.Name, identity)
		return nil, nil
	}

	boundary := boundaryFromEvent(latest)

	effective, err := r.recoveryBoundaryIsEffective(ctx, rule, identity, boundary)
	if err != nil {
		return nil, fmt.Errorf("find derived state for recovery boundary: %w", err)
	}

	if !effective {
		return nil, nil
	}

	r.rememberRecoveryBoundaryFromSource(rule, identity, latest.HealthEvent, boundary)

	return &boundary, nil
}

func (r *Reconciler) latestRecoverySource(
	ctx context.Context,
	rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
) (*datamodels.HealthEventWithStatus, error) {
	return r.findLatestMatchingEvent(ctx, rule.Name, "recovery_source", r.recoveryLookupFilter(
		rule.Recovery.SourceAgent, rule.Recovery.SourceCheckName, identity.nodeName,
	), func(candidate *datamodels.HealthEventWithStatus) bool {
		return recoverySourceMatches(rule.Recovery, candidate.HealthEvent) &&
			recoverySourceAppliesToIdentity(rule, candidate.HealthEvent, identity)
	})
}

func (r *Reconciler) recoveryBoundaryIsEffective(
	ctx context.Context,
	rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
	boundary recoveryBoundary,
) (bool, error) {
	state, found, err := r.currentDerivedState(ctx, rule, identity)
	if err != nil {
		return false, err
	}

	// A healthy source is not an effective history boundary while a preceding
	// derived condition is still unhealthy. This can happen when recovery
	// publication failed and the source event was later checkpointed as poison.
	return !found || state.isHealthy || !boundaryAfter(boundary, state.boundary), nil
}

func (r *Reconciler) rememberRecoveryBoundaryFromSource(
	rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
	source *protos.HealthEvent,
	boundary recoveryBoundary,
) {
	sourceIdentity, nodeWide, valid := recoveryIdentityForSource(rule, source)
	if valid && nodeWide {
		r.rememberRecoveryBoundary(rule.Name, sourceIdentity, boundary)
	} else {
		r.rememberRecoveryBoundary(rule.Name, identity, boundary)
	}
}

func recoverySourceAppliesToIdentity(
	rule config.HealthEventsAnalyzerRule,
	event *protos.HealthEvent,
	identity recoveryIdentity,
) bool {
	sourceIdentity, nodeWide, ok := recoveryIdentityForSource(rule, event)
	return ok && (nodeWide || sourceIdentity.key == identity.key)
}

func (r *Reconciler) currentDerivedState(
	ctx context.Context,
	rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
) (state derivedState, found bool, err error) {
	if state, found := r.cachedDerivedState(rule.Name, identity); found {
		return state, true, nil
	}

	latest, err := r.findLatestMatchingEvent(ctx, rule.Name, "derived_state", r.recoveryLookupFilter(
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

func (r *Reconciler) currentDerivedStatesForNode(
	ctx context.Context,
	rule config.HealthEventsAnalyzerRule,
	nodeName string,
) ([]recoveryTarget, error) {
	cursor, err := r.databaseClient.Find(ctx, r.recoveryLookupFilter(agentName, rule.Name, nodeName), nil)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	states := make(map[string]recoveryTarget)

	var decodeErrs *multierror.Error

	for cursor.Next(ctx) {
		var candidate datamodels.HealthEventWithStatus
		if err := cursor.Decode(&candidate); err != nil {
			decodeErr := &storedDocumentDecodeError{cause: classifyRecoveryDecodeError(ctx, err)}
			r.recordStoredDocumentDecodeError(ctx, rule.Name, "node_derived_states", decodeErr)
			decodeErrs = multierror.Append(decodeErrs, decodeErr)

			continue
		}

		identity, valid := recoveryIdentityForEvent(rule, candidate.HealthEvent)
		if !valid {
			continue
		}

		state := derivedState{
			boundary:  boundaryFromEvent(&candidate),
			isHealthy: candidate.HealthEvent.IsHealthy,
		}

		current, found := states[identity.key]
		if !found || boundaryAfter(state.boundary, current.state.boundary) {
			states[identity.key] = recoveryTarget{identity: identity, state: state}
		}
	}

	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate health events: %w", err)
	}

	targets := make([]recoveryTarget, 0, len(states))
	for _, target := range states {
		targets = append(targets, target)
	}

	slices.SortFunc(targets, func(a, b recoveryTarget) int {
		return strings.Compare(a.identity.key, b.identity.key)
	})

	return targets, decodeErrs.ErrorOrNil()
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
) (recoveryBoundary, bool, error) {
	return r.publishDerivedUntilStored(
		ctx, source, rule, identity, true, "recovery",
		func(publishCtx context.Context) error {
			return r.config.Publisher.PublishRecovery(
				publishCtx, source.HealthEvent, rule.Name, identity.entities, &rule,
			)
		},
	)
}

func (r *Reconciler) publishFaultUntilStored(
	ctx context.Context,
	source *datamodels.HealthEventWithStatus,
	rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
) (recoveryBoundary, bool, error) {
	event := source.HealthEvent
	if rule.Recovery.Scope == config.RecoveryScopeEntity {
		event = proto.Clone(source.HealthEvent).(*protos.HealthEvent)
		event.EntitiesImpacted = identity.entities
	}

	action := protos.RecommendedAction(r.getRecommendedActionValue(rule.RecommendedAction, rule.Name))

	return r.publishDerivedUntilStored(
		ctx, source, rule, identity, false, "fault",
		func(publishCtx context.Context) error {
			return r.config.Publisher.Publish(publishCtx, event, action, rule.Name, rule.Message, &rule)
		},
	)
}

func (r *Reconciler) publishDerivedUntilStored(
	ctx context.Context,
	source *datamodels.HealthEventWithStatus,
	rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
	isHealthy bool,
	stateName string,
	publish func(context.Context) error,
) (recoveryBoundary, bool, error) {
	pollInterval, republishInterval := r.recoveryIntervals()

	waitCtx, cancel := context.WithTimeout(ctx, r.recoveryPersistenceTimeout())

	defer cancel()

	nextPublish := time.Time{}
	published := false

	for {
		persisted, err := r.findPersistedDerived(waitCtx, source, rule, identity, isHealthy)
		if err == nil && persisted != nil {
			return boundaryFromEvent(persisted), published, nil
		}

		if err != nil {
			if client.IsPermanentError(err) {
				return recoveryBoundary{}, published, err
			}

			slog.WarnContext(waitCtx, "Failed to confirm persisted derived event; retrying",
				"state", stateName,
				"rule_name", rule.Name,
				"node", identity.nodeName,
				"error", err)
		}

		var didPublish bool

		nextPublish, didPublish = publishDerivedIfDue(
			waitCtx, publish, stateName, rule.Name, identity.nodeName,
			nextPublish, republishInterval,
		)
		published = didPublish || published

		if err := waitForRecoveryPoll(waitCtx, pollInterval); err != nil {
			if waitCtx.Err() == context.DeadlineExceeded {
				recoveryPersistenceTimeoutsTotal.WithLabelValues(rule.Name, stateName).Inc()

				return recoveryBoundary{}, published, fmt.Errorf(
					"timed out waiting for persisted derived %s for rule %q: %w",
					stateName, rule.Name, waitCtx.Err(),
				)
			}

			return recoveryBoundary{}, published, err
		}
	}
}

func waitForRecoveryPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *Reconciler) recoveryPersistenceTimeout() time.Duration {
	if r.recoveryTimeout > 0 {
		return r.recoveryTimeout
	}

	return defaultRecoveryPersistenceTimeout
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
	publish func(context.Context) error,
	stateName string,
	ruleName string,
	nodeName string,
	nextPublish time.Time,
	republishInterval time.Duration,
) (time.Time, bool) {
	now := time.Now()
	if nextPublish.After(now) {
		return nextPublish, false
	}

	err := publish(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Failed to enqueue derived event; retrying",
			"state", stateName,
			"rule_name", ruleName,
			"node", nodeName,
			"error", err)

		return now.Add(republishInterval), false
	}

	return now.Add(republishInterval), true
}

func (r *Reconciler) findPersistedDerived(
	ctx context.Context,
	source *datamodels.HealthEventWithStatus,
	rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
	isHealthy bool,
) (*datamodels.HealthEventWithStatus, error) {
	return r.findLatestMatchingEvent(ctx, rule.Name, "persisted_derived", r.recoveryLookupFilter(
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
	ruleName string,
	lookup string,
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

	var decodeErrs *multierror.Error

	for cursor.Next(ctx) {
		var candidate datamodels.HealthEventWithStatus
		if err := cursor.Decode(&candidate); err != nil {
			decodeErr := &storedDocumentDecodeError{cause: classifyRecoveryDecodeError(ctx, err)}
			r.recordStoredDocumentDecodeError(ctx, ruleName, lookup, decodeErr)
			decodeErrs = multierror.Append(decodeErrs, decodeErr)

			continue
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

	return latest, decodeErrs.ErrorOrNil()
}

func (r *Reconciler) recordStoredDocumentDecodeError(
	ctx context.Context,
	ruleName string,
	lookup string,
	err error,
) {
	classification := "transient"
	if client.IsPermanentError(err) {
		classification = "malformed"
	}

	slog.ErrorContext(ctx, "Skipping undecodable stored health event",
		"rule_name", ruleName, "lookup", lookup, "classification", classification, "error", err)
	recoveryStoredDocumentDecodeErrorsTotal.WithLabelValues(ruleName, lookup, classification).Inc()
}

func classifyRecoveryDecodeError(ctx context.Context, err error) error {
	wrapped := fmt.Errorf("decode health event: %w", err)

	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%w: %w", wrapped, ctxErr)
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, driver.ErrBadConn) || errors.Is(err, io.ErrUnexpectedEOF) {
		return wrapped
	}

	var networkError net.Error
	if errors.As(err, &networkError) {
		return wrapped
	}

	return client.PermanentError(wrapped)
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

	if r.recoveryLoaded == nil {
		r.recoveryLoaded = make(map[string]struct{})
	}

	key := recoveryStateKey(ruleName, identity)
	r.recoveryLoaded[key] = struct{}{}

	current, exists := r.recoveryBoundaries[key]
	if !exists || boundaryAfter(boundary, current) {
		r.recoveryBoundaries[key] = boundary
	}
}

func (r *Reconciler) rememberRecoveryLookup(ruleName string, identity recoveryIdentity) {
	r.recoveryMu.Lock()
	defer r.recoveryMu.Unlock()

	if r.recoveryLoaded == nil {
		r.recoveryLoaded = make(map[string]struct{})
	}

	r.recoveryLoaded[recoveryStateKey(ruleName, identity)] = struct{}{}
}

func (r *Reconciler) recoveryLookupLoaded(ruleName string, identity recoveryIdentity) bool {
	r.recoveryMu.RLock()
	defer r.recoveryMu.RUnlock()

	_, found := r.recoveryLoaded[recoveryStateKey(ruleName, identity)]

	return found
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

func (r *Reconciler) cachedEffectiveRecoveryBoundary(
	rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
) (recoveryBoundary, bool) {
	// Cached boundaries are candidates, not pre-approved answers. The caller
	// must run recoveryBoundaryIsEffective for the exact rule and identity
	// before serving any candidate, including a node-wide one.
	boundary, found := r.cachedRecoveryBoundary(rule.Name, identity)
	if rule.Recovery == nil || rule.Recovery.Scope != config.RecoveryScopeEntity {
		return boundary, found
	}

	nodeBoundary, nodeFound := r.cachedRecoveryBoundary(rule.Name, nodeRecoveryIdentity(identity.nodeName))
	if !nodeFound || (found && !boundaryAfter(nodeBoundary, boundary)) {
		return boundary, found
	}

	return nodeBoundary, true
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
