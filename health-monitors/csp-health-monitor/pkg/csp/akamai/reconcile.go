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

package akamai

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"time"

	akamaiapi "github.com/nvidia/nvsentinel/commons/pkg/akamai"
	eventpkg "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/event"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

const (
	// cancelMissThreshold is the number of consecutive complete polls in which a
	// pre-start event must be absent before it is treated as cancelled. Releasing
	// a node that is about to be maintained is the unsafe direction, so one
	// transient absence, such as a page shifting while the list is read, must not
	// release it.
	cancelMissThreshold = 2

	// emptyListMissThreshold replaces cancelMissThreshold when the whole list is
	// empty. An empty list that lasts is more likely a fault on the way to the
	// API, such as a permission or scoping change, than every maintenance of the
	// account being cancelled at once, so it must last much longer before it
	// releases anything.
	emptyListMissThreshold = 10

	// cancellationReasonKey is the metadata key that records why the monitor
	// cancelled an event.
	cancellationReasonKey = "cancellationReason"

	cancellationReason = "maintenance entry is no longer listed by the Akamai maintenance API"
)

// activeEventFinder is the subset of the datastore this package needs to find
// the events it stored earlier.
type activeEventFinder interface {
	FindActiveEventsByStatuses(ctx context.Context, csp model.CSP, statuses []string) ([]model.MaintenanceEvent, error)
}

var (
	// preStartStatuses are the only provider statuses that are cancelled. A
	// maintenance that has started must not be released because it vanished.
	preStartStatuses = []string{eventpkg.AkamaiStatusPending, eventpkg.AkamaiStatusScheduled}

	// activeStatuses are every provider status that keeps a node quarantined for
	// maintenance. The started statuses are read only to know that another event
	// still holds a node, and are never cancelled.
	activeStatuses = []string{
		eventpkg.AkamaiStatusPending,
		eventpkg.AkamaiStatusScheduled,
		eventpkg.AkamaiStatusStarted,
		eventpkg.AkamaiStatusInProgress,
	}
)

// reconcileVanished emits a CANCELLED event for each stored pre-start event of
// this cluster that the API stopped listing. The monitor does not rely on the
// API to return a canceled status, so an entry that is no longer listed counts
// as cancelled. It must only run after a complete, successful list, because an
// incomplete list would make every missing entry look cancelled. A store failure
// is logged and skipped, since the next poll retries it; only a cancelled
// context is returned.
func (c *Client) reconcileVanished(
	ctx context.Context,
	listed []akamaiapi.Maintenance,
	eventChan chan<- model.MaintenanceEvent,
) error {
	if c.store == nil {
		c.warnNoStoreOnce.Do(func() {
			slog.Warn("Akamai: no datastore, so cancelled maintenance is not detected and nodes stay quarantined")
		})

		return nil
	}

	stored, err := c.store.FindActiveEventsByStatuses(ctx, model.CSPAkamai, activeStatuses)
	if err != nil {
		slog.Error("Akamai: failed to query stored events for cancellation check", "error", err)

		return nil
	}

	stored = c.ofThisCluster(stored)
	candidates := c.trackAbsence(stored, listedEventIDs(listed), len(listed) == 0)

	for _, vanished := range releasable(candidates, stored) {
		cancelled := cancelledEvent(vanished, time.Now().UTC())

		select {
		case <-ctx.Done():
			return ctx.Err()
		case eventChan <- cancelled:
			delete(c.misses, cancelled.EventID)
			slog.Info("Akamai: maintenance no longer listed, emitting cancellation",
				"eventID", cancelled.EventID, "node", cancelled.NodeName)
		}
	}

	return nil
}

// ofThisCluster keeps the stored events that belong to this monitor's cluster.
// The datastore query is not scoped to a cluster.
func (c *Client) ofThisCluster(stored []model.MaintenanceEvent) []model.MaintenanceEvent {
	var own []model.MaintenanceEvent

	for _, event := range stored {
		if event.ClusterName == c.clusterName {
			own = append(own, event)
		}
	}

	return own
}

// listedEventIDs returns the IDs of every entry the API listed, for the whole
// account and not only this cluster's nodes, in the form the normalizer stores.
func listedEventIDs(listed []akamaiapi.Maintenance) map[string]struct{} {
	ids := make(map[string]struct{}, len(listed))
	for _, raw := range listed {
		ids[eventpkg.AkamaiEventIDPrefix+strconv.Itoa(raw.ID)] = struct{}{}
	}

	return ids
}

// trackAbsence updates the consecutive-miss counters from one complete poll and
// returns the stored pre-start events that reached the threshold. An event that
// reappeared, or that is no longer stored, is dropped from the counters, so the
// map cannot grow beyond the stored pre-start events. A returned event keeps its
// counter until the cancellation is sent, so an event that is held back is
// returned again by the next poll.
func (c *Client) trackAbsence(
	stored []model.MaintenanceEvent,
	listedIDs map[string]struct{},
	listEmpty bool,
) []model.MaintenanceEvent {
	threshold := cancelMissThreshold
	if listEmpty {
		threshold = emptyListMissThreshold
	}

	next := make(map[string]int, len(stored))

	var reached []model.MaintenanceEvent

	for _, event := range stored {
		if !slices.Contains(preStartStatuses, string(event.CSPStatus)) {
			continue
		}

		if _, listed := listedIDs[event.EventID]; listed {
			continue
		}

		next[event.EventID] = c.misses[event.EventID] + 1
		if next[event.EventID] >= threshold {
			reached = append(reached, event)
		}
	}

	c.misses = next

	return reached
}

// releasable returns the candidates that may be cancelled now. It leaves out an
// event whose node another stored event still holds, because the healthy event
// clears the maintenance check of the node and not one event.
func releasable(candidates, stored []model.MaintenanceEvent) []model.MaintenanceEvent {
	cancelling := make(map[string]struct{}, len(candidates))
	for _, event := range candidates {
		cancelling[event.EventID] = struct{}{}
	}

	var out []model.MaintenanceEvent

	for _, event := range candidates {
		if holder, held := otherEventOnNode(event, stored, cancelling); held {
			slog.Debug("Akamai: another maintenance event holds the node, keeping it quarantined",
				"eventID", event.EventID, "node", event.NodeName, "heldBy", holder)

			continue
		}

		out = append(out, event)
	}

	return out
}

// otherEventOnNode returns the ID of a stored event on the same node as event
// that is not being cancelled in this poll, if there is one.
func otherEventOnNode(
	event model.MaintenanceEvent,
	stored []model.MaintenanceEvent,
	cancelling map[string]struct{},
) (string, bool) {
	if event.NodeName == "" {
		return "", false
	}

	for _, other := range stored {
		if other.EventID == event.EventID || other.NodeName != event.NodeName {
			continue
		}

		if _, going := cancelling[other.EventID]; !going {
			return other.EventID, true
		}
	}

	return "", false
}

// cancelledEvent copies a stored event and marks it cancelled by the provider
// status the datastore keys on. The fingerprint changes so the datastore's
// unchanged-event check lets the write through, and the CANCELLED status makes
// the trigger engine send a healthy event for the same check and node.
func cancelledEvent(stored model.MaintenanceEvent, now time.Time) model.MaintenanceEvent {
	cancelled := stored
	cancelled.Status = model.StatusCancelled
	cancelled.CSPStatus = model.ProviderStatus(eventpkg.AkamaiStatusCanceled)
	cancelled.LastUpdatedTimestamp = now

	cancelled.Metadata = make(map[string]string, len(stored.Metadata)+2)
	for k, v := range stored.Metadata {
		cancelled.Metadata[k] = v
	}

	cancelled.Metadata[model.ProviderLastUpdatedKey] =
		eventpkg.AkamaiStatusCanceled + "|" + stored.Metadata[model.ProviderLastUpdatedKey]
	cancelled.Metadata[cancellationReasonKey] = cancellationReason

	return cancelled
}
