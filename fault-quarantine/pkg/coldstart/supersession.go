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
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
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

// superseded reports whether newer events cover every target this event could
// change. A compound event remains replayable until all of its entities (and
// error-code scopes) are covered; a check-wide event requires a newer
// check-wide event.
func (r *supersessionResolver) superseded(
	ctx context.Context,
	candidate model.HealthEventWithStatus,
	createdAt time.Time,
	documentID string,
) (bool, error) {
	if createdAt.IsZero() || candidate.HealthEvent == nil {
		return false, nil
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
				// A malformed newer event cannot prove that the candidate is obsolete.
				return nil //nolint:nilerr // Keep scanning for a valid covering event.
			}

			if coverage.add(newer.HealthEvent) {
				resolved = true

				return errSupersessionResolved
			}

			return nil
		})
	if err != nil && !errors.Is(err, errSupersessionResolved) {
		return false, err
	}

	return resolved, nil
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
	checkWide bool
	remaining map[eventEffect]struct{}
}

func newEventCoverage(event *protos.HealthEvent) *eventCoverage {
	coverage := &eventCoverage{
		checkWide: len(event.GetEntitiesImpacted()) == 0,
		remaining: make(map[eventEffect]struct{}),
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
	entities := event.GetEntitiesImpacted()
	if len(entities) == 0 {
		for candidate := range c.remaining {
			if errorCodeCoveredBy(candidate.errorCode, event) {
				delete(c.remaining, candidate)
			}
		}

		return len(c.remaining) == 0
	}

	if c.checkWide {
		return false
	}

	for candidate := range c.remaining {
		if effectCoveredBy(candidate, event) {
			delete(c.remaining, candidate)
		}
	}

	return len(c.remaining) == 0
}

func effectCoveredBy(
	candidate eventEffect,
	newer *protos.HealthEvent,
) bool {
	for _, entity := range newer.GetEntitiesImpacted() {
		if candidate.entityType != entity.GetEntityType() ||
			candidate.entityValue != entity.GetEntityValue() {
			continue
		}

		if errorCodeCoveredBy(candidate.errorCode, newer) {
			return true
		}
	}

	return false
}

func errorCodeCoveredBy(candidate string, newer *protos.HealthEvent) bool {
	for _, errorCode := range normalizedErrorCodes(newer.GetErrorCode()) {
		if errorCode == "" || candidate == errorCode ||
			(candidate == "" && !newer.GetIsHealthy()) {
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
