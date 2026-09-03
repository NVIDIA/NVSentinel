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

package coldstart

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
	"google.golang.org/protobuf/proto"
)

type eventIdentity struct {
	agent          string
	componentClass string
	checkName      string
	nodeName       string
	version        uint32
}

type eventPosition struct {
	createdAt time.Time
	id        string
}

type supersessionResolver struct {
	store datastore.HealthEventStore
	until time.Time
}

func newSupersessionResolver(
	store datastore.HealthEventStore,
	until time.Time,
) *supersessionResolver {
	return &supersessionResolver{
		store: store,
		until: until,
	}
}

var errSupersessionResolved = errors.New("supersession resolved")

const maxRecoveryProjectionRectangles = 256

func (r *supersessionResolver) resolve(
	ctx context.Context,
	candidate model.HealthEventWithStatus,
	createdAt time.Time,
	documentID string,
) (bool, *eventCoverage, error) {
	if createdAt.IsZero() || candidate.HealthEvent == nil {
		return false, nil, nil
	}

	coverage := newEventCoverage(candidate.HealthEvent)
	resolved := false

	err := r.scanNewerEvents(ctx, identityFor(candidate.HealthEvent), createdAt,
		func(record datastore.HealthEventWithStatus) error {
			id, _ := utils.ExtractDocumentID(record.RawEvent)
			if !eventAfter(eventPosition{createdAt: record.CreatedAt, id: id}, createdAt, documentID) {
				return nil
			}

			newer, err := parseStoredRecord(record)
			if err != nil || newer.HealthEvent == nil {
				// An unreadable newer state cannot safely permit an older healthy clear.
				// Fault candidates still replay conservatively so corruption cannot hide
				// a quarantine action.
				if candidate.HealthEvent.GetIsHealthy() {
					resolved = true

					return errSupersessionResolved
				}

				return nil //nolint:nilerr // Keep scanning for a valid covering event.
			}

			if coverage.add(newer.HealthEvent) {
				resolved = true

				return errSupersessionResolved
			}

			return nil
		})
	if err != nil && !errors.Is(err, errSupersessionResolved) {
		return false, nil, err
	}

	return resolved, coverage, nil
}

func (r *supersessionResolver) scanNewerEvents(
	ctx context.Context,
	identity eventIdentity,
	from time.Time,
	visit func(datastore.HealthEventWithStatus) error,
) error {
	versionCondition := query.Condition(query.Eq("healthevent.version", identity.version))
	if identity.version == 0 {
		versionCondition = query.Or(
			versionCondition,
			query.Eq("healthevent.version", nil),
		)
	}

	condition := query.And(
		query.Eq("healthevent.agent", identity.agent),
		query.Eq("healthevent.componentclass", identity.componentClass),
		query.Eq("healthevent.checkname", identity.checkName),
		query.Eq("healthevent.nodename", identity.nodeName),
		versionCondition,
		query.Gte("createdAt", from),
		processableCondition(),
	)

	if !r.until.IsZero() {
		condition = query.And(condition, query.Lte("createdAt", r.until))
	}

	err := r.store.FindHealthEventsByQueryBatched(
		ctx,
		query.New().Build(condition),
		batchSize,
		func(batch []datastore.HealthEventWithStatus) error {
			for i := range batch {
				if err := visit(batch[i]); err != nil {
					return err
				}
			}

			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("failed to find health-event history for %s/%s: %w",
			identity.nodeName, identity.checkName, err)
	}

	return nil
}

func eventAfter(event eventPosition, createdAt time.Time, id string) bool {
	if event.createdAt.Equal(createdAt) {
		return event.id > id
	}

	return event.createdAt.After(createdAt)
}

type eventEffect struct {
	entityType  string
	entityValue string
	errorCode   string
}

type eventCoverage struct {
	checkWide             bool
	candidateHealthy      bool
	candidateCodeWildcard bool
	remaining             map[eventEffect]struct{}
	shadowing             []*protos.HealthEvent
	projected             bool
}

type recoveryEffectsContextKey struct{}

func withRecoveryEffects(ctx context.Context, coverage *eventCoverage) context.Context {
	if coverage == nil {
		return ctx
	}

	return context.WithValue(ctx, recoveryEffectsContextKey{}, coverage)
}

// ShouldRecoverEffect reports whether a stored annotation effect is still
// current for the event being replayed. Outside a projected cold start every
// effect is accepted.
func ShouldRecoverEffect(
	ctx context.Context,
	entityType, entityValue, errorCode string,
) bool {
	coverage, ok := ctx.Value(recoveryEffectsContextKey{}).(*eventCoverage)
	if !ok {
		return true
	}

	return coverage.allows(eventEffect{
		entityType: entityType, entityValue: entityValue, errorCode: errorCode,
	})
}

// ProjectHealthEvent returns rule-evaluation-safe residual events. Projected
// events are the maximal entity/error-code rectangles contained in the exact
// residual effects. This avoids recreating an outdated pair while preserving
// compound rule predicates that are still true for a coherent residual group.
func ProjectHealthEvent(ctx context.Context, event *protos.HealthEvent) ([]*protos.HealthEvent, error) {
	coverage, ok := ctx.Value(recoveryEffectsContextKey{}).(*eventCoverage)
	if !ok || !coverage.projected || len(coverage.remaining) == 0 {
		return []*protos.HealthEvent{event}, nil
	}

	if coverage.checkWide {
		return projectCheckWideHealthEvent(event, coverage.remaining), nil
	}

	rectangles, overflow := maximalResidualRectangles(coverage.remaining)
	if overflow {
		// Exact rule evaluation over an arbitrary entity/code relation requires
		// every maximal rectangle and can have exponential output. At the hard
		// limit, evaluate the original event conservatively instead of silently
		// missing a compound fault or trapping startup in a deterministic retry.
		// Annotation mutations remain filtered by the exact residual coverage.
		slog.WarnContext(ctx, "Recovery projection limit reached; evaluating original event conservatively",
			"remaining_effects", len(coverage.remaining),
			"projection_limit", maxRecoveryProjectionRectangles)

		return []*protos.HealthEvent{event}, nil
	}

	return projectHealthEventRectangles(event, rectangles), nil
}

func projectCheckWideHealthEvent(
	event *protos.HealthEvent,
	remaining map[eventEffect]struct{},
) []*protos.HealthEvent {
	codes := make([]string, 0, len(remaining))
	for effect := range remaining {
		codes = append(codes, effect.errorCode)
	}

	sort.Strings(codes)

	projected := proto.Clone(event).(*protos.HealthEvent)
	projected.EntitiesImpacted = nil
	projected.ErrorCode = projectedErrorCodes(event, codes)

	return []*protos.HealthEvent{projected}
}

func projectHealthEventRectangles(
	event *protos.HealthEvent,
	rectangles []residualRectangle,
) []*protos.HealthEvent {
	projected := make([]*protos.HealthEvent, 0, len(rectangles))
	for _, rectangle := range rectangles {
		candidate := proto.Clone(event).(*protos.HealthEvent)

		candidate.EntitiesImpacted = make([]*protos.Entity, 0, len(rectangle.entities))
		for _, entity := range rectangle.entities {
			candidate.EntitiesImpacted = append(candidate.EntitiesImpacted, &protos.Entity{
				EntityType: entity.entityType, EntityValue: entity.entityValue,
			})
		}

		candidate.ErrorCode = projectedErrorCodes(event, rectangle.errorCodes)
		projected = append(projected, candidate)
	}

	return projected
}

func projectedErrorCodes(event *protos.HealthEvent, codes []string) []string {
	if len(codes) == 1 && codes[0] == "" && !slices.Contains(event.GetErrorCode(), "") {
		return nil
	}

	return codes
}

type residualEntity struct {
	entityType  string
	entityValue string
}

type residualRectangle struct {
	entities   []residualEntity
	errorCodes []string
}

// maximalResidualRectangles enumerates the formal concepts of the residual
// entity/error-code relation. Formal-concept output can be exponential, so an
// oversized relation is reported before projected protobufs are cloned so the
// caller can use its documented conservative fallback.
//
//nolint:cyclop,gocognit // Formal-concept closure has several bounded graph branches.
func maximalResidualRectangles(remaining map[eventEffect]struct{}) ([]residualRectangle, bool) {
	neighbors := make(map[residualEntity]map[string]struct{})

	for effect := range remaining {
		entity := residualEntity{entityType: effect.entityType, entityValue: effect.entityValue}
		if neighbors[entity] == nil {
			neighbors[entity] = make(map[string]struct{})
		}

		neighbors[entity][effect.errorCode] = struct{}{}
	}

	entities := make([]residualEntity, 0, len(neighbors))
	for entity := range neighbors {
		entities = append(entities, entity)
	}

	sort.Slice(entities, func(i, j int) bool {
		if entities[i].entityType == entities[j].entityType {
			return entities[i].entityValue < entities[j].entityValue
		}

		return entities[i].entityType < entities[j].entityType
	})

	// Every closed code set is an intersection of entity neighborhoods. Build
	// the intersection closure, then derive its maximal entity set.
	codeSets := make([]map[string]struct{}, 0, len(entities))
	seenCodeSets := make(map[string]struct{})
	overflow := false

	addCodeSet := func(codes map[string]struct{}) {
		if len(codes) == 0 {
			return
		}

		key := sortedStringSetKey(codes)
		if _, exists := seenCodeSets[key]; exists {
			return
		}

		if len(codeSets) >= maxRecoveryProjectionRectangles {
			overflow = true

			return
		}

		seenCodeSets[key] = struct{}{}

		codeSets = append(codeSets, codes)
	}

	for _, entity := range entities {
		addCodeSet(cloneStringSet(neighbors[entity]))
	}

	for i := 0; i < len(codeSets); i++ {
		for j := 0; j < i; j++ {
			addCodeSet(intersectStringSets(codeSets[i], codeSets[j]))

			if overflow {
				return nil, true
			}
		}
	}

	concepts := make(map[string]residualRectangle)
	for _, codes := range codeSets {
		conceptEntities := make([]residualEntity, 0, len(entities))
		for _, entity := range entities {
			if stringSetContainsAll(neighbors[entity], codes) {
				conceptEntities = append(conceptEntities, entity)
			}
		}

		if len(conceptEntities) == 0 {
			continue
		}

		closedCodes := cloneStringSet(neighbors[conceptEntities[0]])
		for _, entity := range conceptEntities[1:] {
			closedCodes = intersectStringSets(closedCodes, neighbors[entity])
		}

		key := sortedStringSetKey(closedCodes)
		concepts[key] = residualRectangle{
			entities: conceptEntities, errorCodes: sortedStringSet(closedCodes),
		}
	}

	result := make([]residualRectangle, 0, len(concepts))
	for _, concept := range concepts {
		result = append(result, concept)
	}

	sortResidualRectangles(result)

	return result, false
}

func sortResidualRectangles(rectangles []residualRectangle) {
	sort.Slice(rectangles, func(i, j int) bool {
		leftCodes := stringSliceKey(rectangles[i].errorCodes)
		rightCodes := stringSliceKey(rectangles[j].errorCodes)

		if leftCodes == rightCodes {
			return residualEntitySliceKey(rectangles[i].entities) < residualEntitySliceKey(rectangles[j].entities)
		}

		return leftCodes < rightCodes
	})
}

func cloneStringSet(source map[string]struct{}) map[string]struct{} {
	cloned := make(map[string]struct{}, len(source))
	for value := range source {
		cloned[value] = struct{}{}
	}

	return cloned
}

func intersectStringSets(left, right map[string]struct{}) map[string]struct{} {
	if len(left) > len(right) {
		left, right = right, left
	}

	intersection := make(map[string]struct{})

	for value := range left {
		if _, exists := right[value]; exists {
			intersection[value] = struct{}{}
		}
	}

	return intersection
}

func stringSetContainsAll(container, values map[string]struct{}) bool {
	for value := range values {
		if _, exists := container[value]; !exists {
			return false
		}
	}

	return true
}

func sortedStringSet(values map[string]struct{}) []string {
	sorted := make([]string, 0, len(values))
	for value := range values {
		sorted = append(sorted, value)
	}

	sort.Strings(sorted)

	return sorted
}

func sortedStringSetKey(values map[string]struct{}) string {
	return stringSliceKey(sortedStringSet(values))
}

func stringSliceKey(values []string) string {
	var key strings.Builder
	for _, value := range values {
		fmt.Fprintf(&key, "%d:", len(value))
		key.WriteString(value)
	}

	return key.String()
}

func residualEntitySliceKey(entities []residualEntity) string {
	values := make([]string, 0, len(entities)*2)
	for _, entity := range entities {
		values = append(values, entity.entityType, entity.entityValue)
	}

	return stringSliceKey(values)
}

func newEventCoverage(event *protos.HealthEvent) *eventCoverage {
	coverage := &eventCoverage{
		checkWide:             len(event.GetEntitiesImpacted()) == 0,
		candidateHealthy:      event.GetIsHealthy(),
		candidateCodeWildcard: event.GetIsHealthy() && len(event.GetErrorCode()) == 0,
		remaining:             make(map[eventEffect]struct{}),
	}
	errorCodes := normalizedErrorCodes(event.GetErrorCode())

	if coverage.checkWide {
		for _, errorCode := range errorCodes {
			coverage.remaining[eventEffect{errorCode: errorCode}] = struct{}{}
		}

		return coverage
	}

	for _, entity := range event.GetEntitiesImpacted() {
		for _, errorCode := range errorCodes {
			coverage.remaining[eventEffect{
				entityType: entity.GetEntityType(), entityValue: entity.GetEntityValue(), errorCode: errorCode,
			}] = struct{}{}
		}
	}

	return coverage
}

func (c *eventCoverage) add(event *protos.HealthEvent) bool {
	if c.candidateHealthy && c.newerCanShadowCandidate(event) {
		c.shadowing = append(c.shadowing, event)
		c.projected = true
	}

	for candidate := range c.remaining {
		if c.identityFullyCoveredBy(candidate, event) && c.errorCodeFullyCoveredBy(candidate.errorCode, event) {
			delete(c.remaining, candidate)
			c.projected = true
		}
	}

	return len(c.remaining) == 0
}

func (c *eventCoverage) newerCanShadowCandidate(newer *protos.HealthEvent) bool {
	for candidate := range c.remaining {
		if !c.newerIdentityCanShadow(candidate, newer) {
			continue
		}

		if c.candidateCodeWildcard || c.errorCodeFullyCoveredBy(candidate.errorCode, newer) {
			return true
		}
	}

	return false
}

func (c *eventCoverage) allows(effect eventEffect) bool {
	if !c.hasRemainingEffect(effect) {
		return false
	}

	return !c.candidateHealthy || !c.isShadowed(effect)
}

func (c *eventCoverage) hasRemainingEffect(effect eventEffect) bool {
	for remaining := range c.remaining {
		identityMatches := (c.checkWide && c.candidateHealthy) ||
			(remaining.entityType == effect.entityType && remaining.entityValue == effect.entityValue)

		codeMatches := remaining.errorCode == effect.errorCode ||
			(c.candidateCodeWildcard && remaining.errorCode == "")
		if identityMatches && codeMatches {
			return true
		}
	}

	return false
}

func (c *eventCoverage) isShadowed(effect eventEffect) bool {
	for _, newer := range c.shadowing {
		if newerShadowsStoredEffect(effect, newer) {
			return true
		}
	}

	return false
}

func (c *eventCoverage) identityFullyCoveredBy(candidate eventEffect, newer *protos.HealthEvent) bool {
	if c.checkWide {
		if c.candidateHealthy {
			return newer.GetIsHealthy() && len(newer.GetEntitiesImpacted()) == 0
		}

		return len(newer.GetEntitiesImpacted()) == 0
	}

	if len(newer.GetEntitiesImpacted()) == 0 {
		return newer.GetIsHealthy()
	}

	for _, entity := range newer.GetEntitiesImpacted() {
		if candidate.entityType == entity.GetEntityType() &&
			candidate.entityValue == entity.GetEntityValue() {
			return true
		}
	}

	return false
}

func (c *eventCoverage) newerIdentityCanShadow(candidate eventEffect, newer *protos.HealthEvent) bool {
	if c.checkWide {
		return true
	}

	if len(newer.GetEntitiesImpacted()) == 0 {
		return newer.GetIsHealthy()
	}

	for _, entity := range newer.GetEntitiesImpacted() {
		if candidate.entityType == entity.GetEntityType() &&
			candidate.entityValue == entity.GetEntityValue() {
			return true
		}
	}

	return false
}

func (c *eventCoverage) errorCodeFullyCoveredBy(candidate string, newer *protos.HealthEvent) bool {
	if c.candidateCodeWildcard && candidate == "" {
		return newer.GetIsHealthy() && len(newer.GetErrorCode()) == 0
	}

	for _, errorCode := range normalizedErrorCodes(newer.GetErrorCode()) {
		if candidate == errorCode || (newer.GetIsHealthy() && len(newer.GetErrorCode()) == 0) {
			return true
		}
	}

	return false
}

func newerShadowsStoredEffect(effect eventEffect, newer *protos.HealthEvent) bool {
	return newerIdentityShadowsStoredEffect(effect, newer) &&
		newerCodeShadowsStoredEffect(effect.errorCode, newer)
}

func newerIdentityShadowsStoredEffect(effect eventEffect, newer *protos.HealthEvent) bool {
	entities := newer.GetEntitiesImpacted()
	if len(entities) == 0 {
		return newer.GetIsHealthy() || (effect.entityType == "" && effect.entityValue == "")
	}

	for _, entity := range entities {
		if effect.entityType == entity.GetEntityType() && effect.entityValue == entity.GetEntityValue() {
			return true
		}
	}

	return false
}

func newerCodeShadowsStoredEffect(candidateCode string, newer *protos.HealthEvent) bool {
	for _, newerCode := range normalizedErrorCodes(newer.GetErrorCode()) {
		if newerCode == candidateCode || (newer.GetIsHealthy() && len(newer.GetErrorCode()) == 0) {
			return true
		}
	}

	return false
}

func normalizedErrorCodes(errorCodes []string) []string {
	if len(errorCodes) == 0 {
		return []string{""}
	}

	return errorCodes
}

func parseStoredRecord(record datastore.HealthEventWithStatus) (model.HealthEventWithStatus, error) {
	if record.RawEvent != nil {
		return eventutil.ParseHealthEventFromEvent(record.RawEvent)
	}

	return eventutil.ParseHealthEventFromEvent(datastore.Event{
		"healthevent":       record.HealthEvent,
		"healtheventstatus": record.HealthEventStatus,
	})
}

func identityFor(event *protos.HealthEvent) eventIdentity {
	return eventIdentity{
		agent:          event.GetAgent(),
		componentClass: event.GetComponentClass(),
		checkName:      event.GetCheckName(),
		nodeName:       event.GetNodeName(),
		version:        event.GetVersion(),
	}
}

func processableCondition() query.Condition {
	strategy := int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION)

	return query.Or(
		query.Eq("healthevent.processingstrategy", strategy),
		query.Eq("healthevent.processingStrategy", strategy),
		query.And(
			query.Eq("healthevent.processingstrategy", nil),
			query.Eq("healthevent.processingStrategy", nil),
		),
	)
}
