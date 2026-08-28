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
	latest    *datastore.HealthEventWithStatus
	findCalls int
}

func (s *latestEventStoreStub) FindLatestHealthEventByQuery(
	context.Context,
	datastore.QueryBuilder,
) (*datastore.HealthEventWithStatus, error) {
	s.findCalls++

	return s.latest, nil
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
	store := &latestEventStoreStub{latest: &recovery}

	superseded, err := newSupersessionResolver(store, base.Add(time.Hour)).superseded(
		context.Background(), failure)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolverReplaysFailureAfterPartialRecovery(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	failure := recoveryRecord(base, false, []any{impactedEntity("0"), impactedEntity("1")})
	recovery := recoveryRecord(base.Add(time.Minute), true, []any{impactedEntity("0")})
	store := &latestEventStoreStub{latest: &recovery}

	superseded, err := newSupersessionResolver(store, base.Add(time.Hour)).superseded(
		context.Background(), failure)
	require.NoError(t, err)
	assert.False(t, superseded)
}

func TestSupersessionResolverSkipsFailureReplacedByLaterFailure(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	oldFailure := recoveryRecord(base, false, []any{impactedEntity("0")})
	currentFailure := recoveryRecord(base.Add(time.Minute), false, []any{impactedEntity("0")})
	store := &latestEventStoreStub{latest: &currentFailure}

	superseded, err := newSupersessionResolver(store, base.Add(time.Hour)).superseded(
		context.Background(), oldFailure)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolverCachesLatestEventPerCheck(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	recovery := recoveryRecord(base.Add(time.Hour), true, nil)
	store := &latestEventStoreStub{latest: &recovery}
	resolver := newSupersessionResolver(store, base.Add(2*time.Hour))

	for offset := range 2 {
		failure := recoveryRecord(base.Add(time.Duration(offset)*time.Minute), false, nil)
		_, err := resolver.superseded(context.Background(), failure)
		require.NoError(t, err)
	}

	assert.Equal(t, 1, store.findCalls)
}
