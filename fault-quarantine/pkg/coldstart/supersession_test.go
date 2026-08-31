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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
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

	resolved, _, err := resolver.resolve(context.Background(), parsed, record.CreatedAt, documentID)

	return resolved, err
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

func TestSupersessionResolver_HealthyWildcardPreservesOtherErrorCodeRecovery(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	healthy := recoveryRecord(base, true, []any{impactedEntity("0")})
	failure := withErrorCodes(
		recoveryRecord(base.Add(time.Minute), false, []any{impactedEntity("0")}), "79")
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{healthy, failure}}

	parsed, err := parseStoredRecord(healthy)
	require.NoError(t, err)
	resolved, effects, err := newSupersessionResolver(store, base.Add(time.Hour)).resolve(
		context.Background(), parsed, healthy.CreatedAt, "event-id")
	require.NoError(t, err)
	assert.False(t, resolved,
		"a newer scoped fault must not discard recovery of unrelated stored error codes")
	ctx := withRecoveryEffects(context.Background(), effects)
	assert.False(t, ShouldRecoverEffect(ctx, "GPU", "0", "79"))
	assert.True(t, ShouldRecoverEffect(ctx, "GPU", "0", "48"))
	assert.True(t, ShouldRecoverEffect(ctx, "GPU", "0", ""))
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

func TestSupersessionResolver_UncodedFailureSurvivesScopedFailureAndRecovery(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	uncodedFailure := recoveryRecord(base, false, []any{impactedEntity("0")})
	scopedFailure := withErrorCodes(
		recoveryRecord(base.Add(time.Minute), false, []any{impactedEntity("0")}), "79")
	scopedRecovery := withErrorCodes(
		recoveryRecord(base.Add(2*time.Minute), true, []any{impactedEntity("0")}), "79")
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{
		uncodedFailure, scopedFailure, scopedRecovery,
	}}

	superseded, err := resolveSupersession(
		t, newSupersessionResolver(store, base.Add(time.Hour)), uncodedFailure)
	require.NoError(t, err)
	assert.False(t, superseded,
		"a scoped failure and recovery must not erase an earlier uncoded fault")
}

func TestSupersessionResolver_UnhealthyUncodedFaultDoesNotCoverScopedFault(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	scopedFailure := withErrorCodes(
		recoveryRecord(base, false, []any{impactedEntity("0")}), "79")
	uncodedFailure := recoveryRecord(base.Add(time.Minute), false, []any{impactedEntity("0")})
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{scopedFailure, uncodedFailure}}

	superseded, err := resolveSupersession(
		t, newSupersessionResolver(store, base.Add(time.Hour)), scopedFailure)
	require.NoError(t, err)
	assert.False(t, superseded,
		"an unhealthy no-code event is a distinct fault, not a wildcard over scoped faults")
}

func TestSupersessionResolver_ExplicitEmptyCodeRecoveryIsNotWildcard(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	scopedFailure := withErrorCodes(
		recoveryRecord(base, false, []any{impactedEntity("0")}), "79")
	explicitUncodedRecovery := withErrorCodes(
		recoveryRecord(base.Add(time.Minute), true, []any{impactedEntity("0")}), "")
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{
		scopedFailure, explicitUncodedRecovery,
	}}

	superseded, err := resolveSupersession(
		t, newSupersessionResolver(store, base.Add(time.Hour)), scopedFailure)
	require.NoError(t, err)
	assert.False(t, superseded,
		"only an absent healthy errorCode is a wildcard; an explicit empty code clears the uncoded key")
}

func TestSupersessionResolver_UnhealthyCheckWideFaultDoesNotCoverEntityFault(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	entityFailure := withErrorCodes(
		recoveryRecord(base, false, []any{impactedEntity("0")}), "79")
	checkWideFailure := withErrorCodes(recoveryRecord(base.Add(time.Minute), false, nil), "79")
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{
		entityFailure, checkWideFailure,
	}}

	superseded, err := resolveSupersession(
		t, newSupersessionResolver(store, base.Add(time.Hour)), entityFailure)
	require.NoError(t, err)
	assert.False(t, superseded,
		"an unhealthy check-wide event is a distinct fault, not a wildcard over entity faults")
}

func TestSupersessionResolver_HealthyWildcardClearsOnlyEffectsNotShadowedByUncodedFault(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	healthy := recoveryRecord(base, true, []any{impactedEntity("0")})
	uncodedFailure := recoveryRecord(base.Add(time.Minute), false, []any{impactedEntity("0")})
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{healthy, uncodedFailure}}

	parsed, err := parseStoredRecord(healthy)
	require.NoError(t, err)
	resolved, effects, err := newSupersessionResolver(store, base.Add(time.Hour)).resolve(
		context.Background(), parsed, healthy.CreatedAt, "event-id")
	require.NoError(t, err)
	assert.False(t, resolved)
	ctx := withRecoveryEffects(context.Background(), effects)
	assert.False(t, ShouldRecoverEffect(ctx, "GPU", "0", ""),
		"the newer uncoded fault must survive the old wildcard recovery")
	assert.True(t, ShouldRecoverEffect(ctx, "GPU", "0", "48"))
	assert.True(t, ShouldRecoverEffect(ctx, "GPU", "0", "79"))
}

func TestSupersessionResolver_HealthyCheckWideClearsEntitiesButNotNewerCheckWideFault(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	healthy := withErrorCodes(recoveryRecord(base, true, nil), "79")
	checkWideFailure := withErrorCodes(recoveryRecord(base.Add(time.Minute), false, nil), "79")
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{healthy, checkWideFailure}}

	parsed, err := parseStoredRecord(healthy)
	require.NoError(t, err)
	resolved, effects, err := newSupersessionResolver(store, base.Add(time.Hour)).resolve(
		context.Background(), parsed, healthy.CreatedAt, "event-id")
	require.NoError(t, err)
	assert.False(t, resolved)
	ctx := withRecoveryEffects(context.Background(), effects)
	assert.True(t, ShouldRecoverEffect(ctx, "GPU", "A", "79"))
	assert.True(t, ShouldRecoverEffect(ctx, "GPU", "B", "79"))
	assert.False(t, ShouldRecoverEffect(ctx, "", "", "79"),
		"the newer check-wide fault must survive the old check-wide recovery")
}

func TestSupersessionResolver_MalformedNewerEventBlocksHealthyClear(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	healthy := recoveryRecord(base, true, []any{impactedEntity("0")})
	malformed := recoveryRecord(base.Add(time.Minute), false, []any{impactedEntity("0")})
	malformed.RawEvent["id"] = "malformed-event"
	malformed.RawEvent["healthevent"].(map[string]any)["isHealthy"] = "not-a-bool"
	_, parseErr := parseStoredRecord(malformed)
	require.Error(t, parseErr)
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{healthy, malformed}}

	superseded, err := resolveSupersession(t, newSupersessionResolver(store, base.Add(time.Hour)), healthy)
	require.NoError(t, err)
	assert.True(t, superseded, "an unreadable newer state must not allow an old healthy clear")
}

func TestSupersessionResolver_MalformedNewerEventDoesNotBlockFailure(t *testing.T) {
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	failure := recoveryRecord(base, false, []any{impactedEntity("0")})
	malformed := recoveryRecord(base.Add(time.Minute), false, []any{impactedEntity("0")})
	malformed.RawEvent["id"] = "malformed-event"
	malformed.RawEvent["healthevent"].(map[string]any)["isHealthy"] = "not-a-bool"
	_, parseErr := parseStoredRecord(malformed)
	require.Error(t, parseErr)
	store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{failure, malformed}}

	superseded, err := resolveSupersession(t, newSupersessionResolver(store, base.Add(time.Hour)), failure)
	require.NoError(t, err)
	assert.False(t, superseded, "an unreadable newer state must not suppress a conservative fault replay")
}

func TestRecoverStoredEvent_ProjectsResidualBeforeRuleEvaluation(t *testing.T) {
	type projectedEventExpectation struct {
		codes    []string
		entities []string
		message  string
	}

	tests := []struct {
		name             string
		failureEntities  []any
		failureCodes     []any
		recoveryEntities []any
		recoveryCodes    []any
		expected         []projectedEventExpectation
	}{
		{
			name:             "non-rectangular entity and error-code residual",
			failureEntities:  []any{impactedEntity("A"), impactedEntity("B")},
			failureCodes:     []any{"48", "79"},
			recoveryEntities: []any{impactedEntity("A")},
			recoveryCodes:    []any{"79"},
			expected: []projectedEventExpectation{
				{codes: []string{"48"}, entities: []string{"A", "B"}},
				{
					codes: []string{"48", "79"}, entities: []string{"B"},
					message: "the recovered A/79 effect must not reach rule evaluation " +
						"while B keeps both codes",
				},
			},
		},
		{
			name:          "check-wide error-code residual",
			failureCodes:  []any{"48", "79"},
			recoveryCodes: []any{"79"},
			expected: []projectedEventExpectation{
				{
					codes:   []string{"48"},
					message: "the recovered check-wide 79 effect must not reach rule evaluation",
				},
			},
		},
		{
			name:          "explicit blank error-code residual",
			failureCodes:  []any{"", "79"},
			recoveryCodes: []any{"79"},
			expected: []projectedEventExpectation{
				{
					codes:   []string{""},
					message: "an explicit blank code must remain distinct from an absent wildcard",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
			failure := withErrorCodes(recoveryRecord(base, false, tt.failureEntities), tt.failureCodes...)
			recovery := withErrorCodes(
				recoveryRecord(base.Add(time.Minute), true, tt.recoveryEntities), tt.recoveryCodes...)
			recovery.RawEvent["id"] = "recovery-id"
			store := &latestEventStoreStub{events: []datastore.HealthEventWithStatus{failure, recovery}}

			var projected []*protos.HealthEvent
			processor := &eventProcessorStub{process: func(
				ctx context.Context,
				event model.HealthEventWithStatus,
				_ string,
			) (ProcessResult, error) {
				var projectionErr error
				projected, projectionErr = ProjectHealthEvent(ctx, event.HealthEvent)
				require.NoError(t, projectionErr)

				return ProcessResultProcessed, nil
			}}
			completion, err := recoverStoredEvent(
				context.Background(), newSupersessionResolver(store, base.Add(time.Hour)), processor,
				parseStoredRecoveryEvent(failure))
			require.NoError(t, err)
			assert.Nil(t, completion)
			require.Len(t, projected, len(tt.expected))

			for i, expected := range tt.expected {
				assert.Equal(t, expected.codes, projected[i].GetErrorCode(), expected.message)
				var entities []string
				for _, entity := range projected[i].GetEntitiesImpacted() {
					entities = append(entities, entity.GetEntityValue())
				}
				assert.Equal(t, expected.entities, entities, expected.message)
			}
		})
	}
}

func TestProjectHealthEvent_PreservesAbsentCodeAsWildcard(t *testing.T) {
	event := &protos.HealthEvent{
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "A"},
			{EntityType: "GPU", EntityValue: "B"},
		},
	}
	ctx := withRecoveryEffects(context.Background(), &eventCoverage{
		remaining: map[eventEffect]struct{}{
			{entityType: "GPU", entityValue: "B", errorCode: ""}: {},
		},
		projected: true,
	})

	projected, err := ProjectHealthEvent(ctx, event)
	require.NoError(t, err)
	require.Len(t, projected, 1)
	assert.Empty(t, projected[0].GetErrorCode(), "an absent code must remain a wildcard")
}

func TestProjectHealthEvent_UsesConservativeFallbackForComplexResidual(t *testing.T) {
	const dimension = 9 // 2^9-1 intersections exceed the projection cap.
	remaining := make(map[eventEffect]struct{})
	event := &protos.HealthEvent{}
	for entityIndex := range dimension {
		entityValue := fmt.Sprintf("entity-%d", entityIndex)
		event.EntitiesImpacted = append(event.EntitiesImpacted, &protos.Entity{
			EntityType: "GPU", EntityValue: entityValue,
		})
		for codeIndex := range dimension {
			code := fmt.Sprintf("code-%d", codeIndex)
			if entityIndex == 0 {
				event.ErrorCode = append(event.ErrorCode, code)
			}
			if entityIndex != codeIndex {
				remaining[eventEffect{
					entityType: "GPU", entityValue: entityValue, errorCode: code,
				}] = struct{}{}
			}
		}
	}
	ctx := withRecoveryEffects(context.Background(), &eventCoverage{
		remaining: remaining, projected: true,
	})

	projected, err := ProjectHealthEvent(ctx, event)
	require.NoError(t, err)
	require.Len(t, projected, 1)
	assert.Same(t, event, projected[0],
		"overflow must fail closed for rule evaluation without cloning exponential output")
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
