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
	"fmt"
	"sort"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
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

type orderedEvent struct {
	event     model.HealthEventWithStatus
	createdAt time.Time
	id        string
}

type supersessionResolver struct {
	store datastore.HealthEventStore
	until time.Time
	cache map[eventIdentity][]orderedEvent
}

func newSupersessionResolver(
	store datastore.HealthEventStore,
	until time.Time,
) *supersessionResolver {
	return &supersessionResolver{
		store: store,
		until: until,
		cache: make(map[eventIdentity][]orderedEvent),
	}
}

// superseded reports whether replaying this event could overwrite newer state.
// Compound events are skipped as a unit when any affected key changed later;
// replaying only part would make the status update expose the original full
// event to downstream consumers.
func (r *supersessionResolver) superseded(
	ctx context.Context,
	record datastore.HealthEventWithStatus,
) (bool, error) {
	if record.CreatedAt.IsZero() {
		return false, nil
	}

	candidate, err := parseStoredRecord(record)
	if err != nil {
		return false, nil //nolint:nilerr // The event processor classifies and skips invalid records.
	}

	identity := identityFor(candidate.HealthEvent)

	timeline, ok := r.cache[identity]
	if !ok {
		timeline, err = r.findTimeline(ctx, identity, record.CreatedAt)
		if err != nil {
			return false, err
		}

		r.cache[identity] = timeline
	}

	candidateID, _ := utils.ExtractDocumentID(record.RawEvent)
	affected := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	affected.AddOrUpdateEvent(candidate.HealthEvent)

	for i := range timeline {
		if !eventAfter(timeline[i], record.CreatedAt, candidateID) {
			continue
		}

		if candidate.HealthEvent.GetIsHealthy() && len(candidate.HealthEvent.GetEntitiesImpacted()) == 0 {
			return true, nil
		}

		if affected.RemoveEvent(timeline[i].event.HealthEvent) > 0 {
			return true, nil
		}
	}

	return false, nil
}

func (r *supersessionResolver) findTimeline(
	ctx context.Context,
	identity eventIdentity,
	from time.Time,
) ([]orderedEvent, error) {
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

	records := make([]datastore.HealthEventWithStatus, 0)

	err := r.store.FindHealthEventsByQueryBatched(
		ctx,
		query.New().Build(condition),
		batchSize,
		func(batch []datastore.HealthEventWithStatus) error {
			records = append(records, batch...)

			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to find health-event history for %s/%s: %w",
			identity.nodeName, identity.checkName, err)
	}

	timeline := make([]orderedEvent, 0, len(records))
	for i := range records {
		event, err := parseStoredRecord(records[i])
		if err != nil {
			continue
		}

		id, _ := utils.ExtractDocumentID(records[i].RawEvent)
		timeline = append(timeline, orderedEvent{
			event: event, createdAt: records[i].CreatedAt, id: id,
		})
	}

	sort.Slice(timeline, func(i, j int) bool {
		if timeline[i].createdAt.Equal(timeline[j].createdAt) {
			return timeline[i].id < timeline[j].id
		}

		return timeline[i].createdAt.Before(timeline[j].createdAt)
	})

	return timeline, nil
}

func eventAfter(event orderedEvent, createdAt time.Time, id string) bool {
	if event.createdAt.Equal(createdAt) {
		return event.id > id
	}

	return event.createdAt.After(createdAt)
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
