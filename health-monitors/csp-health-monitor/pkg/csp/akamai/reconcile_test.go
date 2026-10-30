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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	akamaiapi "github.com/nvidia/nvsentinel/commons/pkg/akamai"
	eventpkg "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/event"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

// fakeStore returns a fixed list of stored events, like the datastore does for
// the statuses it is asked for.
type fakeStore struct {
	events []model.MaintenanceEvent
	err    error
	asked  []string
}

func (f *fakeStore) FindActiveEventsByStatuses(
	_ context.Context, csp model.CSP, statuses []string,
) ([]model.MaintenanceEvent, error) {
	f.asked = statuses

	if f.err != nil {
		return nil, f.err
	}

	var out []model.MaintenanceEvent

	for _, e := range f.events {
		if e.CSP != csp {
			continue
		}

		for _, s := range statuses {
			if string(e.CSPStatus) == s {
				out = append(out, e)
			}
		}
	}

	return out, nil
}

// countingNodes counts node lookups, to show a poll skipped them.
type countingNodes struct {
	calls int
	nodes fakeNodes
}

func (c *countingNodes) InstanceNodeNames(ctx context.Context) (map[int]string, error) {
	c.calls++

	return c.nodes.InstanceNodeNames(ctx)
}

func storedEvent(id, cspStatus string) model.MaintenanceEvent {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	return model.MaintenanceEvent{
		EventID:            id,
		CSP:                model.CSPAkamai,
		ClusterName:        "cluster-a",
		ResourceType:       "akamai_instance",
		ResourceID:         "11111111",
		NodeName:           "node-a",
		MaintenanceType:    model.TypeScheduled,
		Status:             model.StatusQuarantineTriggered,
		CSPStatus:          model.ProviderStatus(cspStatus),
		ScheduledStartTime: &start,
		Metadata: map[string]string{
			model.ProviderLastUpdatedKey: cspStatus + "|power_off_on",
			"maintenanceType":            "power_off_on",
		},
	}
}

// unrelatedListing is a listing that is not empty and does not contain the
// stored events, so an absence is judged by the normal threshold.
var unrelatedListing = []akamaiapi.Maintenance{event(99, 999, akamaiapi.MaintenanceEntityLinode)}

func reconcileClient(api maintenanceAPI, nodes nodeResolver, store activeEventFinder) *Client {
	c := newTestClient(api, nodes)
	c.store = store
	c.misses = map[string]int{}

	return c
}

// cancelledOnly returns the events that cancel a maintenance.
func cancelledOnly(events []model.MaintenanceEvent) []model.MaintenanceEvent {
	var out []model.MaintenanceEvent

	for _, e := range events {
		if e.Status == model.StatusCancelled {
			out = append(out, e)
		}
	}

	return out
}

// pollN runs n polls and returns everything they emitted.
func pollN(t *testing.T, c *Client, n int) []model.MaintenanceEvent {
	t.Helper()

	ch := make(chan model.MaintenanceEvent, 16)

	for i := 0; i < n; i++ {
		require.NoError(t, c.pollEvents(context.Background(), ch))
	}

	close(ch)

	var got []model.MaintenanceEvent
	for e := range ch {
		got = append(got, e)
	}

	return got
}

func TestPollEvents_PreStartEventVanishedTwice_EmitsOneCancelledEventWithPreservedFields(t *testing.T) {
	store := &fakeStore{events: []model.MaintenanceEvent{storedEvent("akamai-7", "scheduled")}}
	c := reconcileClient(&fakeAPI{events: unrelatedListing}, fakeNodes{}, store)

	assert.Empty(t, pollN(t, c, 1), "one miss must not release the node")

	got := pollN(t, c, 1)
	require.Len(t, got, 1)

	e := got[0]
	assert.Equal(t, "akamai-7", e.EventID)
	assert.Equal(t, "node-a", e.NodeName)
	assert.Equal(t, "11111111", e.ResourceID)
	assert.Equal(t, "cluster-a", e.ClusterName)
	assert.Equal(t, model.TypeScheduled, e.MaintenanceType)
	assert.Equal(t, model.StatusCancelled, e.Status)
	assert.Equal(t, model.ProviderStatus(eventpkg.AkamaiStatusCanceled), e.CSPStatus)
	require.NotNil(t, e.ScheduledStartTime)
	assert.True(t, e.ScheduledStartTime.Equal(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)))
	assert.Contains(t, e.Metadata[cancellationReasonKey], "no longer listed")
	assert.NotEqual(t, "scheduled|power_off_on", e.Metadata[model.ProviderLastUpdatedKey])
	assert.Equal(t, "power_off_on", e.Metadata["maintenanceType"])
	assert.ElementsMatch(t, activeStatuses, store.asked)
	assert.Empty(t, c.misses, "an emitted event must leave the counters")
}

func TestPollEvents_PreStartEventListedEveryPoll_EmitsNothing(t *testing.T) {
	store := &fakeStore{events: []model.MaintenanceEvent{storedEvent("akamai-1", "scheduled")}}
	api := &fakeAPI{events: []akamaiapi.Maintenance{event(1, 100, akamaiapi.MaintenanceEntityLinode)}}
	c := reconcileClient(api, fakeNodes{100: "node-a"}, store)

	got := pollN(t, c, 4)

	for _, e := range got {
		assert.NotEqual(t, model.StatusCancelled, e.Status, "a listed maintenance must never be cancelled")
	}

	assert.Empty(t, c.misses)
}

func TestPollEvents_EventReappearsAfterOneMiss_ResetsCounterAndEmitsNothing(t *testing.T) {
	store := &fakeStore{events: []model.MaintenanceEvent{storedEvent("akamai-1", "scheduled")}}
	api := &fakeAPI{events: unrelatedListing}
	c := reconcileClient(api, fakeNodes{100: "node-a"}, store)

	assert.Empty(t, pollN(t, c, 1))
	assert.Equal(t, 1, c.misses["akamai-1"])

	api.events = []akamaiapi.Maintenance{event(1, 100, akamaiapi.MaintenanceEntityLinode)}
	pollN(t, c, 1)
	assert.Empty(t, c.misses, "reappearing must reset the counter")

	api.events = unrelatedListing

	assert.Empty(t, pollN(t, c, 1), "the count restarts from one after a reset")
}

func TestPollEvents_StartedOrFinishedEventAbsent_EmitsNothing(t *testing.T) {
	for _, status := range []string{"started", "in-progress", "completed", "canceled"} {
		t.Run(status, func(t *testing.T) {
			store := &fakeStore{events: []model.MaintenanceEvent{storedEvent("akamai-1", status)}}
			c := reconcileClient(&fakeAPI{}, fakeNodes{}, store)

			assert.Empty(t, pollN(t, c, 3))
			assert.Empty(t, c.misses)
		})
	}
}

func TestPollEvents_VanishedEventOfAnotherCluster_EmitsNothing(t *testing.T) {
	other := storedEvent("akamai-1", "scheduled")
	other.ClusterName = "cluster-b"
	c := reconcileClient(&fakeAPI{}, fakeNodes{}, &fakeStore{events: []model.MaintenanceEvent{other}})

	assert.Empty(t, pollN(t, c, 3))
	assert.Empty(t, c.misses)
}

func TestPollEvents_ListError_SkipsReconcileAndKeepsCounters(t *testing.T) {
	store := &fakeStore{events: []model.MaintenanceEvent{storedEvent("akamai-1", "scheduled")}}
	api := &fakeAPI{events: unrelatedListing}
	c := reconcileClient(api, fakeNodes{}, store)

	pollN(t, c, 1)

	api.err = errors.New("page 2 failed")
	ch := make(chan model.MaintenanceEvent, 4)

	require.Error(t, c.pollEvents(context.Background(), ch))
	assert.Empty(t, ch)
	assert.Equal(t, 1, c.misses["akamai-1"], "a failed list is not an absence")

	api.err = nil

	assert.Len(t, pollN(t, c, 1), 1, "the next complete poll reaches the threshold")
}

func TestPollEvents_StoreError_EmitsNothingAndKeepsCounters(t *testing.T) {
	store := &fakeStore{events: []model.MaintenanceEvent{storedEvent("akamai-1", "scheduled")}}
	c := reconcileClient(&fakeAPI{}, fakeNodes{}, store)

	pollN(t, c, 1)

	store.err = errors.New("db down")

	assert.Empty(t, pollN(t, c, 3))
	assert.Equal(t, 1, c.misses["akamai-1"])
}

func TestPollEvents_EmptyListWithStoredPendingEvent_CancelsOnlyAfterLongerThresholdWithoutNodeLookup(t *testing.T) {
	store := &fakeStore{events: []model.MaintenanceEvent{storedEvent("akamai-1", "pending")}}
	nodes := &countingNodes{nodes: fakeNodes{100: "node-a"}}
	c := reconcileClient(&fakeAPI{}, nodes, store)

	assert.Empty(t, pollN(t, c, emptyListMissThreshold-1), "a list that is empty for a short time must not release the node")

	got := pollN(t, c, 1)

	require.Len(t, got, 1)
	assert.Equal(t, model.StatusCancelled, got[0].Status)
	assert.Zero(t, nodes.calls, "an empty list must not list nodes")
}

func TestPollEvents_SecondEventOnSameNodeStillListed_DoesNotCancelTheFirst(t *testing.T) {
	first := storedEvent("akamai-7", "scheduled")
	second := storedEvent("akamai-8", "scheduled")
	store := &fakeStore{events: []model.MaintenanceEvent{first, second}}
	// Entry 8 is still listed for the same node; entry 7 is not.
	api := &fakeAPI{events: []akamaiapi.Maintenance{event(8, 100, akamaiapi.MaintenanceEntityLinode)}}
	c := reconcileClient(api, fakeNodes{100: "node-a"}, store)

	assert.Empty(t, cancelledOnly(pollN(t, c, cancelMissThreshold+2)),
		"the healthy event of a cancellation would clear the quarantine the second event holds")

	store.events = []model.MaintenanceEvent{first}
	api.events = unrelatedListing

	assert.Len(t, cancelledOnly(pollN(t, c, 1)), 1, "the first event is cancelled once nothing else holds the node")
}

func TestPollEvents_StartedEventOnSameNode_DoesNotLetAVanishedEventReleaseIt(t *testing.T) {
	vanished := storedEvent("akamai-7", "scheduled")
	holder := storedEvent("akamai-9", "started")
	store := &fakeStore{events: []model.MaintenanceEvent{vanished, holder}}
	c := reconcileClient(&fakeAPI{events: unrelatedListing}, fakeNodes{}, store)

	assert.Empty(t, pollN(t, c, cancelMissThreshold+2))
}

func TestPollEvents_TwoEventsOnSameNodeBothVanish_CancelsBoth(t *testing.T) {
	store := &fakeStore{events: []model.MaintenanceEvent{
		storedEvent("akamai-7", "scheduled"), storedEvent("akamai-8", "pending"),
	}}
	c := reconcileClient(&fakeAPI{events: unrelatedListing}, fakeNodes{}, store)

	got := pollN(t, c, cancelMissThreshold)

	require.Len(t, got, 2)
	assert.ElementsMatch(t, []string{"akamai-7", "akamai-8"}, []string{got[0].EventID, got[1].EventID})
}

func TestPollEvents_NilStore_DoesNotPanicAndStillEmitsEvents(t *testing.T) {
	api := &fakeAPI{events: []akamaiapi.Maintenance{event(1, 100, akamaiapi.MaintenanceEntityLinode)}}
	c := reconcileClient(api, fakeNodes{100: "node-a"}, nil)

	var got []model.MaintenanceEvent

	require.NotPanics(t, func() { got = pollN(t, c, 3) })
	require.Len(t, got, 3)
	assert.Equal(t, "akamai-1", got[0].EventID)
	assert.Equal(t, model.StatusDetected, got[0].Status)
}

func TestPollEvents_StoredEventOfOtherAccountInstanceListed_IsNotTreatedAsVanished(t *testing.T) {
	store := &fakeStore{events: []model.MaintenanceEvent{storedEvent("akamai-2", "scheduled")}}
	// Entry 2 is listed for an instance that is not a node of this cluster.
	api := &fakeAPI{events: []akamaiapi.Maintenance{event(2, 999, akamaiapi.MaintenanceEntityLinode)}}
	c := reconcileClient(api, fakeNodes{100: "node-a"}, store)

	assert.Empty(t, pollN(t, c, 4))
	assert.Empty(t, c.misses)
}

func TestCancelledEvent_StoredMetadata_IsNotMutated(t *testing.T) {
	stored := storedEvent("akamai-1", "scheduled")

	_ = cancelledEvent(stored, time.Now())

	assert.Equal(t, "scheduled|power_off_on", stored.Metadata[model.ProviderLastUpdatedKey])
	assert.NotContains(t, stored.Metadata, cancellationReasonKey)
}
