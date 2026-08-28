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
	"log/slog"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
)

type eventIdentity struct {
	agent          string
	componentClass string
	checkName      string
	nodeName       string
	version        uint32
}

type latestEvent struct {
	event     *model.HealthEventWithStatus
	createdAt time.Time
}

type supersessionResolver struct {
	store datastore.HealthEventStore
	until time.Time
	cache map[eventIdentity]latestEvent
}

func newSupersessionResolver(
	store datastore.HealthEventStore,
	until time.Time,
) *supersessionResolver {
	return &supersessionResolver{
		store: store,
		until: until,
		cache: make(map[eventIdentity]latestEvent),
	}
}

// superseded reports whether a later healthy event fully clears this failure.
// Partial entity recovery is conservative: the failure is replayed unless every
// entity and error-code key is cleared by the later event.
func (r *supersessionResolver) superseded(
	ctx context.Context,
	record datastore.HealthEventWithStatus,
) (bool, error) {
	if record.CreatedAt.IsZero() {
		return false, nil
	}

	candidate, err := parseStoredRecord(record)
	if err != nil || candidate.HealthEvent.GetIsHealthy() {
		return false, nil
	}

	identity := identityFor(candidate.HealthEvent)
	latest, ok := r.cache[identity]
	if !ok {
		latest, err = r.findLatest(ctx, identity)
		if err != nil {
			return false, err
		}

		r.cache[identity] = latest
	}

	if latest.event == nil || !latest.createdAt.After(record.CreatedAt) ||
		!latest.event.HealthEvent.GetIsHealthy() {
		return false, nil
	}

	active := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	active.AddOrUpdateEvent(candidate.HealthEvent)
	active.RemoveEvent(latest.event.HealthEvent)

	return active.IsEmpty(), nil
}

func (r *supersessionResolver) findLatest(
	ctx context.Context,
	identity eventIdentity,
) (latestEvent, error) {
	condition := query.And(
		query.Eq("healthevent.agent", identity.agent),
		query.Eq("healthevent.componentclass", identity.componentClass),
		query.Eq("healthevent.checkname", identity.checkName),
		query.Eq("healthevent.nodename", identity.nodeName),
		query.Eq("healthevent.version", identity.version),
		processableCondition(),
	)

	if !r.until.IsZero() {
		condition = query.And(condition, query.Lte("createdAt", r.until))
	}

	record, err := r.store.FindLatestHealthEventByQuery(ctx, query.New().Build(condition))
	if err != nil {
		return latestEvent{}, fmt.Errorf("failed to find latest health event for %s/%s: %w",
			identity.nodeName, identity.checkName, err)
	}

	if record == nil {
		return latestEvent{}, nil
	}

	event, err := parseStoredRecord(*record)
	if err != nil {
		slog.WarnContext(ctx, "Ignoring invalid latest health event during recovery",
			"node", identity.nodeName, "checkName", identity.checkName, "error", err)

		return latestEvent{}, nil
	}

	return latestEvent{event: &event, createdAt: record.CreatedAt}, nil
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
