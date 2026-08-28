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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type latestEventStoreStub struct {
	datastore.HealthEventStore
	events    []datastore.HealthEventWithStatus
	findCalls int
	builder   datastore.QueryBuilder
}

func (s *latestEventStoreStub) FindHealthEventsByQueryBatched(
	_ context.Context,
	builder datastore.QueryBuilder,
	_ int,
	fn func([]datastore.HealthEventWithStatus) error,
) error {
	s.findCalls++
	s.builder = builder

	return fn(s.events)
}

func recoveryRecord(
	createdAt time.Time,
	isHealthy bool,
	entities []any,
) datastore.HealthEventWithStatus {
	return datastore.HealthEventWithStatus{
		CreatedAt: createdAt,
		RawEvent: datastore.Event{
			"id": "event-id",
			"healthevent": map[string]any{
				"version":          1,
				"agent":            "gpu-health-monitor",
				"componentClass":   "GPU",
				"checkName":        "GpuNvlinkWatch",
				"nodeName":         "node-a",
				"isHealthy":        isHealthy,
				"isFatal":          !isHealthy,
				"entitiesImpacted": entities,
			},
			"healtheventstatus": map[string]any{},
		},
	}
}

func impactedEntity(value string) map[string]any {
	return map[string]any{"entityType": "GPU", "entityValue": value}
}

func TestSupersessionResolverSkipsFailureFullyClearedByLaterRecovery(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	failure := recoveryRecord(base, false, []any{impactedEntity("0"), impactedEntity("1")})
	recovery := recoveryRecord(base.Add(time.Minute), true, nil)
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{failure, recovery}}

	superseded, err := newSupersessionResolver(store, base.Add(time.Hour)).superseded(
		context.Background(), failure)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolverSkipsCompoundFailureAfterPartialRecovery(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	failure := recoveryRecord(base, false, []any{impactedEntity("0"), impactedEntity("1")})
	recovery := recoveryRecord(base.Add(time.Minute), true, []any{impactedEntity("0")})
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{failure, recovery}}

	superseded, err := newSupersessionResolver(store, base.Add(time.Hour)).superseded(
		context.Background(), failure)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolverSkipsFailureReplacedByLaterFailure(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	oldFailure := recoveryRecord(base, false, []any{impactedEntity("0")})
	currentFailure := recoveryRecord(base.Add(time.Minute), false, []any{impactedEntity("0")})
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{oldFailure, currentFailure}}

	superseded, err := newSupersessionResolver(store, base.Add(time.Hour)).superseded(
		context.Background(), oldFailure)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolverCachesLatestEventPerCheck(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	recovery := recoveryRecord(base.Add(time.Hour), true, nil)
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{recovery}}
	resolver := newSupersessionResolver(store, base.Add(2*time.Hour))

	for offset := range 2 {
		failure := recoveryRecord(base.Add(time.Duration(offset)*time.Minute), false, nil)
		_, err := resolver.superseded(context.Background(), failure)
		require.NoError(t, err)
	}

	assert.Equal(t, 1, store.findCalls)
}

func TestSupersessionResolverSkipsHealthyEventBeforeLaterFailure(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	healthy := recoveryRecord(base, true, []any{impactedEntity("0")})
	failure := recoveryRecord(base.Add(time.Minute), false, []any{impactedEntity("0")})
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{healthy, failure}}

	superseded, err := newSupersessionResolver(store, base.Add(time.Hour)).superseded(
		context.Background(), healthy)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolverConsidersEveryLaterEvent(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	oldFailure := recoveryRecord(base, false, []any{impactedEntity("0")})
	recovery := recoveryRecord(base.Add(time.Minute), true, []any{impactedEntity("0")})
	unrelatedFailure := recoveryRecord(base.Add(2*time.Minute), false, []any{impactedEntity("1")})
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{
		oldFailure, recovery, unrelatedFailure,
	}}

	superseded, err := newSupersessionResolver(store, base.Add(time.Hour)).superseded(
		context.Background(), oldFailure)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolverUsesDocumentIDForTimestampTies(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	oldFailure := recoveryRecord(base, false, []any{impactedEntity("0")})
	oldFailure.RawEvent["id"] = "event-1"
	recovery := recoveryRecord(base, true, []any{impactedEntity("0")})
	recovery.RawEvent["id"] = "event-2"
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{recovery, oldFailure}}

	superseded, err := newSupersessionResolver(store, base.Add(time.Hour)).superseded(
		context.Background(), oldFailure)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolverUsesStoredJSONFieldCasing(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	failure := recoveryRecord(base, false, nil)
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{failure}}

	_, err := newSupersessionResolver(store, base.Add(time.Hour)).superseded(
		context.Background(), failure)
	require.NoError(t, err)

	sql, _ := store.builder.ToSQL()
	assert.Contains(t, sql, "componentclass")
	assert.Contains(t, sql, "componentClass")
	assert.Contains(t, sql, "checkname")
	assert.Contains(t, sql, "checkName")
	assert.Contains(t, sql, "nodename")
	assert.Contains(t, sql, "nodeName")
}
