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
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
)

type latestEventStoreStub struct {
	datastore.HealthEventStore
	events     []datastore.HealthEventWithStatus
	batches    [][]datastore.HealthEventWithStatus
	findCalls  int
	batchCalls int
	builder    datastore.QueryBuilder
}

func (s *latestEventStoreStub) FindHealthEventsByQueryBatched(
	_ context.Context,
	builder datastore.QueryBuilder,
	_ int,
	fn func([]datastore.HealthEventWithStatus) error,
) error {
	s.findCalls++
	s.builder = builder
	if s.batches != nil {
		for _, batch := range s.batches {
			s.batchCalls++
			if err := fn(batch); err != nil {
				return err
			}
		}

		return nil
	}

	s.batchCalls++
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

func withErrorCodes(record datastore.HealthEventWithStatus, codes ...any) datastore.HealthEventWithStatus {
	record.RawEvent["healthevent"].(map[string]any)["errorCode"] = codes

	return record
}

func resolveSupersession(
	t *testing.T,
	resolver *supersessionResolver,
	record datastore.HealthEventWithStatus,
) (bool, error) {
	t.Helper()

	parsed, err := parseStoredRecord(record)
	require.NoError(t, err)
	documentID, err := utils.ExtractDocumentID(record.RawEvent)
	require.NoError(t, err)

	return resolver.superseded(context.Background(), parsed, record.CreatedAt, documentID)
}

func TestSupersessionResolver_FullyClearedFailure_SkipsEvent(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	failure := recoveryRecord(base, false, []any{impactedEntity("0"), impactedEntity("1")})
	recovery := recoveryRecord(base.Add(time.Minute), true, nil)
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{failure, recovery}}

	superseded, err := resolveSupersession(t, newSupersessionResolver(store, base.Add(time.Hour)), failure)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolver_PartiallyClearedCompoundFailure_KeepsEvent(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	failure := recoveryRecord(base, false, []any{impactedEntity("0"), impactedEntity("1")})
	recovery := recoveryRecord(base.Add(time.Minute), true, []any{impactedEntity("0")})
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{failure, recovery}}

	superseded, err := resolveSupersession(t, newSupersessionResolver(store, base.Add(time.Hour)), failure)
	require.NoError(t, err)
	assert.False(t, superseded)
}

func TestSupersessionResolver_FullyClearedCompoundFailure_SkipsEvent(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	failure := recoveryRecord(base, false, []any{impactedEntity("0"), impactedEntity("1")})
	recovery0 := recoveryRecord(base.Add(time.Minute), true, []any{impactedEntity("0")})
	recovery1 := recoveryRecord(base.Add(2*time.Minute), true, []any{impactedEntity("1")})
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{failure, recovery0, recovery1}}

	superseded, err := resolveSupersession(t, newSupersessionResolver(store, base.Add(time.Hour)), failure)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolver_LaterFailure_ReplacesEarlierFailure(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	oldFailure := recoveryRecord(base, false, []any{impactedEntity("0")})
	currentFailure := recoveryRecord(base.Add(time.Minute), false, []any{impactedEntity("0")})
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{oldFailure, currentFailure}}

	superseded, err := resolveSupersession(t, newSupersessionResolver(store, base.Add(time.Hour)), oldFailure)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolver_CompleteCoverage_StopsReading(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	failure := recoveryRecord(base, false, []any{impactedEntity("0")})
	recovery := recoveryRecord(base.Add(time.Minute), true, []any{impactedEntity("0")})
	unreachable := recoveryRecord(base.Add(2*time.Minute), false, []any{impactedEntity("1")})
	store := &latestEventStoreStub{batches: [][]datastore.HealthEventWithStatus{{failure, recovery}, {unreachable}}}

	superseded, err := resolveSupersession(t, newSupersessionResolver(store, base.Add(2*time.Hour)), failure)
	require.NoError(t, err)
	assert.True(t, superseded)
	assert.Equal(t, 1, store.findCalls)
	assert.Equal(t, 1, store.batchCalls)
}

func TestSupersessionResolver_HealthyBeforeLaterFailure_SkipsHealthyEvent(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	healthy := recoveryRecord(base, true, []any{impactedEntity("0")})
	failure := withErrorCodes(
		recoveryRecord(base.Add(time.Minute), false, []any{impactedEntity("0")}), "79")
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{healthy, failure}}

	superseded, err := resolveSupersession(t, newSupersessionResolver(store, base.Add(time.Hour)), healthy)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolver_UncodedFailureAfterScopedRecovery_KeepsFailure(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	failure := recoveryRecord(base, false, []any{impactedEntity("0")})
	recovery := withErrorCodes(
		recoveryRecord(base.Add(time.Minute), true, []any{impactedEntity("0")}), "79")
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{failure, recovery}}

	superseded, err := resolveSupersession(t, newSupersessionResolver(store, base.Add(time.Hour)), failure)
	require.NoError(t, err)
	assert.False(t, superseded)
}

func TestSupersessionResolver_CheckWideRecoveryAfterDifferentErrorCode_KeepsRecovery(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	recovery79 := withErrorCodes(recoveryRecord(base, true, nil), "79")
	recovery48 := withErrorCodes(recoveryRecord(base.Add(time.Minute), true, nil), "48")
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{recovery79, recovery48}}

	superseded, err := resolveSupersession(
		t, newSupersessionResolver(store, base.Add(time.Hour)), recovery79)
	require.NoError(t, err)
	assert.False(t, superseded)
}

func TestSupersessionResolver_CheckWideRecoveryAfterMatchingErrorCode_SkipsRecovery(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	oldRecovery := withErrorCodes(recoveryRecord(base, true, nil), "79")
	newRecovery := withErrorCodes(recoveryRecord(base.Add(time.Minute), true, nil), "79")
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{oldRecovery, newRecovery}}

	superseded, err := resolveSupersession(
		t, newSupersessionResolver(store, base.Add(time.Hour)), oldRecovery)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolver_MultipleLaterEvents_ConsidersAll(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	oldFailure := recoveryRecord(base, false, []any{impactedEntity("0")})
	recovery := recoveryRecord(base.Add(time.Minute), true, []any{impactedEntity("0")})
	unrelatedFailure := recoveryRecord(base.Add(2*time.Minute), false, []any{impactedEntity("1")})
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{
		oldFailure, recovery, unrelatedFailure,
	}}

	superseded, err := resolveSupersession(t, newSupersessionResolver(store, base.Add(time.Hour)), oldFailure)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolver_EqualTimestamp_UsesDocumentID(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	oldFailure := recoveryRecord(base, false, []any{impactedEntity("0")})
	oldFailure.RawEvent["id"] = "event-1"
	recovery := recoveryRecord(base, true, []any{impactedEntity("0")})
	recovery.RawEvent["id"] = "event-2"
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{recovery, oldFailure}}

	superseded, err := resolveSupersession(t, newSupersessionResolver(store, base.Add(time.Hour)), oldFailure)
	require.NoError(t, err)
	assert.True(t, superseded)
}

func TestSupersessionResolver_StoredDocument_UsesJSONFieldCasing(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	failure := recoveryRecord(base, false, nil)
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{failure}}

	_, err := resolveSupersession(t, newSupersessionResolver(store, base.Add(time.Hour)), failure)
	require.NoError(t, err)

	sql, _ := store.builder.ToSQL()
	assert.Contains(t, sql, "componentclass")
	assert.Contains(t, sql, "componentClass")
	assert.Contains(t, sql, "checkname")
	assert.Contains(t, sql, "checkName")
	assert.Contains(t, sql, "nodename")
	assert.Contains(t, sql, "nodeName")
}

func TestSupersessionResolver_CheckWideRecoveryAfterEntityUpdate_KeepsRecovery(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	checkWideRecovery := recoveryRecord(base, true, nil)
	entityFailure := recoveryRecord(base.Add(time.Minute), false, []any{impactedEntity("0")})
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{checkWideRecovery, entityFailure}}

	superseded, err := resolveSupersession(
		t, newSupersessionResolver(store, base.Add(time.Hour)), checkWideRecovery)
	require.NoError(t, err)
	assert.False(t, superseded)
}
