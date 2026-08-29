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

package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type healthEventCursor struct {
	events    []datamodels.HealthEventWithStatus
	pos       int
	err       error
	decodeErr error
}

func newHealthEventCursor(events ...datamodels.HealthEventWithStatus) *healthEventCursor {
	return &healthEventCursor{events: events, pos: -1}
}

func (c *healthEventCursor) Next(context.Context) bool {
	c.pos++
	return c.pos < len(c.events)
}

func (c *healthEventCursor) Decode(value any) error {
	if c.decodeErr != nil {
		return c.decodeErr
	}

	if c.pos < 0 || c.pos >= len(c.events) {
		return nil
	}

	target := value.(*datamodels.HealthEventWithStatus)
	*target = c.events[c.pos]
	return nil
}

func (c *healthEventCursor) Close(context.Context) error    { return nil }
func (c *healthEventCursor) All(context.Context, any) error { return nil }
func (c *healthEventCursor) Err() error                     { return c.err }

func recoveryRule(scope config.RecoveryScope) config.HealthEventsAnalyzerRule {
	rule := config.HealthEventsAnalyzerRule{
		Name:              "RepeatedXID94OnSameGPU",
		EvaluateRule:      true,
		RecommendedAction: "CONTACT_SUPPORT",
		Message:           "Repeated XID 94",
		Stage:             []string{`{"$count":"count"}`},
		Recovery: &config.RecoveryMapping{
			SourceAgent:      "syslog-health-monitor",
			SourceCheckName:  "SysLogsXIDError",
			SourceErrorCodes: []string{"94"},
			Scope:            scope,
		},
	}

	if scope == config.RecoveryScopeEntity {
		rule.Recovery.EntityTypes = []string{"GPU_UUID"}
	}

	return rule
}

func storedEvent(createdAt time.Time, event *protos.HealthEvent) datamodels.HealthEventWithStatus {
	return datamodels.HealthEventWithStatus{
		CreatedAt:         createdAt,
		HealthEvent:       event,
		HealthEventStatus: &protos.HealthEventStatus{},
	}
}

func recoverySource(createdAt time.Time, gpuUUID string) datamodels.HealthEventWithStatus {
	return storedEvent(createdAt, &protos.HealthEvent{
		Agent:              "syslog-health-monitor",
		ComponentClass:     "GPU",
		CheckName:          "SysLogsXIDError",
		IsHealthy:          true,
		ErrorCode:          []string{"94"},
		NodeName:           "node-a",
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: gpuUUID}},
		GeneratedTimestamp: timestamppb.New(createdAt.Add(-time.Second)),
	})
}

func derivedEvent(createdAt time.Time, isHealthy bool, gpuUUID string) datamodels.HealthEventWithStatus {
	return storedEvent(createdAt, &protos.HealthEvent{
		Agent:              agentName,
		ComponentClass:     "GPU",
		CheckName:          "RepeatedXID94OnSameGPU",
		IsHealthy:          isHealthy,
		IsFatal:            !isHealthy,
		ErrorCode:          []string{"94"},
		NodeName:           "node-a",
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: gpuUUID}},
		GeneratedTimestamp: timestamppb.New(createdAt.Add(-time.Second)),
	})
}

func persistedRecovery(
	createdAt time.Time,
	source datamodels.HealthEventWithStatus,
	rule config.HealthEventsAnalyzerRule,
) datamodels.HealthEventWithStatus {
	event := proto.Clone(source.HealthEvent).(*protos.HealthEvent)
	event.Agent = agentName
	event.CheckName = rule.Name
	event.IsHealthy = true
	event.IsFatal = false
	event.ErrorCode = nil
	event.RecommendedAction = protos.RecommendedAction_NONE
	if rule.Recovery.Scope == config.RecoveryScopeNode {
		event.EntitiesImpacted = nil
	}

	return storedEvent(createdAt, event)
}

func persistedFault(
	createdAt time.Time,
	source datamodels.HealthEventWithStatus,
	rule config.HealthEventsAnalyzerRule,
	entities []*protos.Entity,
) datamodels.HealthEventWithStatus {
	event := proto.Clone(source.HealthEvent).(*protos.HealthEvent)
	event.Agent = agentName
	event.CheckName = rule.Name
	event.IsHealthy = false
	event.IsFatal = true
	event.EntitiesImpacted = entities

	return storedEvent(createdAt, event)
}

func newRecoveryReconciler(
	rule config.HealthEventsAnalyzerRule,
	database *mockDatabaseClient,
	platform *mockPublisher,
) *Reconciler {
	return &Reconciler{
		config: HealthEventsAnalyzerReconcilerConfig{
			HealthEventsAnalyzerRules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{rule}},
			Publisher:                 publisher.NewPublisher(platform, protos.ProcessingStrategy_EXECUTE_REMEDIATION),
		},
		databaseClient: database,
	}
}

func TestRecoveryIdentityUsesConfiguredEntitySet(t *testing.T) {
	rule := recoveryRule(config.RecoveryScopeEntity)
	event := &protos.HealthEvent{
		NodeName: "node-a",
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
			{EntityType: "GPU_UUID", EntityValue: "GPU-b"},
			{EntityType: "GPU_UUID", EntityValue: "GPU-a"},
		},
	}

	identity, ok := recoveryIdentityForEvent(rule, event)
	require.True(t, ok)
	require.Equal(t, "node-a|8:GPU_UUID=5:GPU-a|8:GPU_UUID=5:GPU-b", identity.key)
	require.Len(t, identity.entities, 2)
	require.Equal(t, "GPU_UUID", identity.entities[0].EntityType)
	require.Equal(t, "GPU-a", identity.entities[0].EntityValue)
	require.Equal(t, "GPU_UUID", identity.entities[1].EntityType)
	require.Equal(t, "GPU-b", identity.entities[1].EntityValue)

	event.EntitiesImpacted = []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}}
	_, ok = recoveryIdentityForEvent(rule, event)
	require.False(t, ok)

	rule.Recovery.EntityTypes = []string{"GPU_UUID", "PCI"}
	event.EntitiesImpacted = []*protos.Entity{
		nil,
		{EntityType: "GPU_UUID"},
		{EntityType: "GPU_UUID", EntityValue: "GPU-a"},
		{EntityType: "PCI", EntityValue: "0000:b4:00.0"},
	}
	identity, ok = recoveryIdentityForEvent(rule, event)
	require.True(t, ok)
	require.Equal(t, "GPU_UUID", identity.entities[0].EntityType)
	require.Equal(t, "PCI", identity.entities[1].EntityType)
}

func TestHandleRecoveryEventsRejectsNonRecoveryInput(t *testing.T) {
	reconciler := &Reconciler{}

	for name, event := range map[string]*datamodels.HealthEventWithStatus{
		"nil":           nil,
		"missing event": {},
		"unhealthy":     {HealthEvent: &protos.HealthEvent{IsHealthy: false}},
	} {
		t.Run(name, func(t *testing.T) {
			published, err := reconciler.handleRecoveryEvents(context.Background(), event)
			require.NoError(t, err)
			require.False(t, published)
		})
	}
}

func TestRecoverySourceMatchesConfiguredSource(t *testing.T) {
	mapping := recoveryRule(config.RecoveryScopeEntity).Recovery
	event := recoverySource(time.Now(), "GPU-a").HealthEvent
	require.True(t, recoverySourceMatches(mapping, event))

	withoutCodeFilter := *mapping
	withoutCodeFilter.SourceErrorCodes = nil
	healthyReset := proto.Clone(event).(*protos.HealthEvent)
	healthyReset.ErrorCode = nil
	require.True(t, recoverySourceMatches(&withoutCodeFilter, healthyReset))

	for name, mutate := range map[string]func(*protos.HealthEvent){
		"unhealthy":    func(event *protos.HealthEvent) { event.IsHealthy = false },
		"wrong agent":  func(event *protos.HealthEvent) { event.Agent = "other-monitor" },
		"wrong check":  func(event *protos.HealthEvent) { event.CheckName = "OtherCheck" },
		"wrong code":   func(event *protos.HealthEvent) { event.ErrorCode = []string{"13"} },
		"missing code": func(event *protos.HealthEvent) { event.ErrorCode = nil },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := proto.Clone(event).(*protos.HealthEvent)
			mutate(candidate)
			require.False(t, recoverySourceMatches(mapping, candidate))
		})
	}
}

func TestRecoveryLookupFilterUsesProviderSchema(t *testing.T) {
	mongo := (&Reconciler{}).recoveryLookupFilter("monitor", "check", "node")
	require.Equal(t, map[string]any{
		"healthevent.agent":     "monitor",
		"healthevent.checkname": "check",
		"healthevent.nodename":  "node",
	}, mongo)

	postgres := (&Reconciler{provider: datastore.ProviderPostgreSQL}).
		recoveryLookupFilter("monitor", "check", "node")
	require.Equal(t, map[string]any{
		"healthevent.agent": "monitor",
		"event_type":        "check",
		"node_name":         "node",
	}, postgres)
}

func TestHandleEventPublishesScopedRecoveryForActiveLegacyFault(t *testing.T) {
	now := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeEntity)
	database := new(mockDatabaseClient)
	platform := new(mockPublisher)
	reconciler := newRecoveryReconciler(rule, database, platform)
	recovery := recoverySource(now, "GPU-target")

	// The latest event for another GPU must not hide the older active event for
	// the recovery scope. Neither event needs feature-specific metadata.
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).Return(
		newHealthEventCursor(
			derivedEvent(now.Add(-time.Minute), false, "GPU-other"),
			derivedEvent(now.Add(-2*time.Minute), false, "GPU-target"),
		), nil,
	).Once()
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).Return(
		newHealthEventCursor(persistedRecovery(now.Add(time.Second), recovery, rule)), nil,
	).Once()

	var published *protos.HealthEvent
	platform.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			published = proto.Clone(args.Get(1).(*protos.HealthEvents).Events[0]).(*protos.HealthEvent)
		}).
		Return(&emptypb.Empty{}, nil).
		Once()

	didPublish, err := reconciler.handleEvent(context.Background(), &recovery)
	require.NoError(t, err)
	require.True(t, didPublish)
	require.NotNil(t, published)
	require.True(t, published.IsHealthy)
	require.False(t, published.IsFatal)
	require.Equal(t, protos.RecommendedAction_NONE, published.RecommendedAction)
	require.Equal(t, rule.Name, published.CheckName)
	require.Equal(t, []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: "GPU-target"}},
		published.EntitiesImpacted)
	require.Empty(t, published.ErrorCode)
	database.AssertNotCalled(t, "Aggregate", mock.Anything, mock.Anything)
	database.AssertExpectations(t)
	platform.AssertExpectations(t)
}

func TestHandleEventPublishesNodeScopedRecovery(t *testing.T) {
	now := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeNode)
	database := new(mockDatabaseClient)
	platform := new(mockPublisher)
	reconciler := newRecoveryReconciler(rule, database, platform)
	recovery := recoverySource(now, "GPU-target")

	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(derivedEvent(now.Add(-time.Minute), false, "GPU-other")), nil).
		Once()
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(persistedRecovery(now.Add(time.Second), recovery, rule)), nil).
		Once()

	var published *protos.HealthEvent
	platform.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			published = proto.Clone(args.Get(1).(*protos.HealthEvents).Events[0]).(*protos.HealthEvent)
		}).
		Return(&emptypb.Empty{}, nil).
		Once()

	didPublish, err := reconciler.handleEvent(context.Background(), &recovery)
	require.NoError(t, err)
	require.True(t, didPublish)
	require.NotNil(t, published)
	require.Empty(t, published.EntitiesImpacted)
	database.AssertExpectations(t)
	platform.AssertExpectations(t)
}

func TestRecoveryWaitsForPersistedOutputBeforeReturning(t *testing.T) {
	now := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeEntity)
	database := new(mockDatabaseClient)
	platform := new(mockPublisher)
	reconciler := newRecoveryReconciler(rule, database, platform)
	reconciler.recoveryPoll = time.Millisecond
	recovery := recoverySource(now, "GPU-target")

	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(derivedEvent(now.Add(-time.Minute), false, "GPU-target")), nil).
		Once()
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(), nil).
		Once()
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(persistedRecovery(now.Add(time.Second), recovery, rule)), nil).
		Once()
	platform.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Return(&emptypb.Empty{}, nil).
		Once()

	didPublish, err := reconciler.handleEvent(context.Background(), &recovery)
	require.NoError(t, err)
	require.True(t, didPublish)

	database.AssertNumberOfCalls(t, "Find", 3)
	platform.AssertNumberOfCalls(t, "HealthEventOccurredV1", 1)
}

func TestRecoveryRepublishesWhenAcceptedOutputIsNotStored(t *testing.T) {
	now := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeEntity)
	database := new(mockDatabaseClient)
	platform := new(mockPublisher)
	reconciler := newRecoveryReconciler(rule, database, platform)
	reconciler.recoveryPoll = time.Millisecond
	reconciler.recoveryRepublish = time.Millisecond
	recovery := recoverySource(now, "GPU-target")

	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(derivedEvent(now.Add(-time.Minute), false, "GPU-target")), nil).
		Once()
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(), nil).
		Once()
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(persistedRecovery(now.Add(time.Second), recovery, rule)), nil).
		Once()
	platform.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Return(&emptypb.Empty{}, nil).
		Twice()

	didPublish, err := reconciler.handleEvent(context.Background(), &recovery)
	require.NoError(t, err)
	require.True(t, didPublish)
	database.AssertNumberOfCalls(t, "Find", 3)
	platform.AssertNumberOfCalls(t, "HealthEventOccurredV1", 2)
}

func TestRecoveryRetriesFailedEnqueue(t *testing.T) {
	now := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeEntity)
	database := new(mockDatabaseClient)
	platform := new(mockPublisher)
	reconciler := newRecoveryReconciler(rule, database, platform)
	reconciler.recoveryPoll = time.Millisecond
	recovery := recoverySource(now, "GPU-target")

	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(derivedEvent(now.Add(-time.Minute), false, "GPU-target")), nil).
		Once()
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(), nil).
		Once()
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(persistedRecovery(now.Add(time.Second), recovery, rule)), nil).
		Once()
	platform.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Return((*emptypb.Empty)(nil), errors.New("connector unavailable")).
		Once()
	platform.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Return(&emptypb.Empty{}, nil).
		Once()

	didPublish, err := reconciler.handleEvent(context.Background(), &recovery)
	require.NoError(t, err)
	require.True(t, didPublish)
	platform.AssertNumberOfCalls(t, "HealthEventOccurredV1", 2)
	database.AssertExpectations(t)
}

func TestRecoveryCancellationBeforePersistenceReturnsError(t *testing.T) {
	now := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeEntity)
	database := new(mockDatabaseClient)
	platform := new(mockPublisher)
	reconciler := newRecoveryReconciler(rule, database, platform)
	recovery := recoverySource(now, "GPU-target")

	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(derivedEvent(now.Add(-time.Minute), false, "GPU-target")), nil).
		Once()
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(), nil).
		Once()

	ctx, cancel := context.WithCancel(context.Background())
	platform.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { cancel() }).
		Return(&emptypb.Empty{}, nil).
		Once()

	didPublish, err := reconciler.handleEvent(ctx, &recovery)
	require.False(t, didPublish)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled))
	database.AssertNumberOfCalls(t, "Find", 2)
	platform.AssertNumberOfCalls(t, "HealthEventOccurredV1", 1)
}

func TestDelayedRecoveryDoesNotClearNewerDerivedFault(t *testing.T) {
	now := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeEntity)
	database := new(mockDatabaseClient)
	platform := new(mockPublisher)
	reconciler := newRecoveryReconciler(rule, database, platform)
	recovery := recoverySource(now.Add(time.Minute), "GPU-target")
	recovery.HealthEvent.GeneratedTimestamp = timestamppb.New(now.Add(-time.Hour))

	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(derivedEvent(now, false, "GPU-target")), nil).
		Once()

	didPublish, err := reconciler.handleEvent(context.Background(), &recovery)
	require.NoError(t, err)
	require.False(t, didPublish)
	platform.AssertNotCalled(t, "HealthEventOccurredV1", mock.Anything, mock.Anything)

	identity, ok := recoveryIdentityForEvent(rule, recovery.HealthEvent)
	require.True(t, ok)
	_, found := reconciler.cachedRecoveryBoundary(rule.Name, identity)
	require.False(t, found)
}

func TestPersistedRecoveryMustBelongToCurrentSource(t *testing.T) {
	now := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeEntity)
	source := recoverySource(now, "GPU-target")
	old := persistedRecovery(now.Add(-time.Second), source, rule)
	newer := persistedRecovery(now.Add(time.Second), source, rule)

	require.False(t, sameRecoverySource(&old, &source))
	require.True(t, sameRecoverySource(&newer, &source))

	newer.HealthEvent.GeneratedTimestamp = timestamppb.New(now.Add(time.Second))
	require.False(t, sameRecoverySource(&newer, &source))
}

func TestRecoveryDoesNotPublishWithoutActiveDerivedFault(t *testing.T) {
	rule := recoveryRule(config.RecoveryScopeEntity)

	for name, events := range map[string][]datamodels.HealthEventWithStatus{
		"no derived event": nil,
		"already healthy":  {derivedEvent(time.Now(), true, "GPU-target")},
	} {
		t.Run(name, func(t *testing.T) {
			database := new(mockDatabaseClient)
			platform := new(mockPublisher)
			reconciler := newRecoveryReconciler(rule, database, platform)
			recovery := recoverySource(time.Now().UTC(), "GPU-target")

			database.On("Find", mock.Anything, mock.Anything, mock.Anything).
				Return(newHealthEventCursor(events...), nil).
				Once()

			didPublish, err := reconciler.handleEvent(context.Background(), &recovery)
			require.NoError(t, err)
			require.False(t, didPublish)
			platform.AssertNotCalled(t, "HealthEventOccurredV1", mock.Anything, mock.Anything)
		})
	}
}

func TestRecoveryBoundaryTruncatesStoredAndOutOfOrderHistory(t *testing.T) {
	rule := recoveryRule(config.RecoveryScopeEntity)
	reconciler := &Reconciler{}
	boundaryTime := time.Date(2026, 8, 29, 10, 0, 0, 250_000_000, time.UTC)
	boundary := &recoveryBoundary{
		createdAt: boundaryTime,
		generated: timestamppb.New(boundaryTime.Add(-time.Second)),
	}
	event := storedEvent(boundaryTime.Add(time.Minute), &protos.HealthEvent{
		NodeName: "node-a",
	})

	pipeline, err := reconciler.getPipelineStages(rule, event, boundary)
	require.NoError(t, err)
	match := pipeline[0]["$match"].(map[string]any)
	require.Equal(t, map[string]any{"$gt": boundaryTime}, match["createdAt"])
	require.Equal(t, generatedAfterExpression(boundary.generated), match["$expr"])
}

func TestRecoveryBoundaryLoadsAndCachesLatestMatchingSource(t *testing.T) {
	base := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeEntity)
	database := new(mockDatabaseClient)
	reconciler := &Reconciler{databaseClient: database}
	incoming := &protos.HealthEvent{
		NodeName:         "node-a",
		EntitiesImpacted: []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: "GPU-target"}},
	}
	wrongEntity := recoverySource(base.Add(2*time.Hour), "GPU-other")
	matching := recoverySource(base.Add(time.Hour), "GPU-target")
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(wrongEntity, matching), nil).
		Once()

	boundary, err := reconciler.recoveryBoundaryForEvent(context.Background(), rule, incoming)
	require.NoError(t, err)
	require.NotNil(t, boundary)
	require.Equal(t, matching.CreatedAt, boundary.createdAt)
	require.True(t, proto.Equal(matching.HealthEvent.GeneratedTimestamp, boundary.generated))

	// The second lookup uses the in-memory boundary established by the first.
	cached, err := reconciler.recoveryBoundaryForEvent(context.Background(), rule, incoming)
	require.NoError(t, err)
	require.Equal(t, boundary, cached)
	database.AssertNumberOfCalls(t, "Find", 1)
}

func TestRecoveryBoundaryCacheUsesGenerationOrder(t *testing.T) {
	rule := recoveryRule(config.RecoveryScopeEntity)
	reconciler := &Reconciler{}
	identity := recoveryIdentity{key: "node-a|GPU-a", nodeName: "node-a"}
	base := time.Now().UTC()

	reconciler.rememberRecoveryBoundary(rule.Name, identity, recoveryBoundary{
		createdAt: base.Add(time.Hour),
		generated: timestamppb.New(base),
	})
	reconciler.rememberRecoveryBoundary(rule.Name, identity, recoveryBoundary{
		createdAt: base,
		generated: timestamppb.New(base.Add(2 * time.Hour)),
	})

	cached, found := reconciler.cachedRecoveryBoundary(rule.Name, identity)
	require.True(t, found)
	require.Equal(t, base.Add(2*time.Hour), cached.generated.AsTime())

	// A delayed older recovery must not replace the newer generation merely
	// because it was stored later.
	reconciler.rememberRecoveryBoundary(rule.Name, identity, recoveryBoundary{
		createdAt: base.Add(3 * time.Hour),
		generated: timestamppb.New(base.Add(-time.Hour)),
	})
	cached, found = reconciler.cachedRecoveryBoundary(rule.Name, identity)
	require.True(t, found)
	require.Equal(t, base.Add(2*time.Hour), cached.generated.AsTime())
}

func TestFindLatestMatchingEventUsesGenerationOrder(t *testing.T) {
	base := time.Now().UTC()
	staleButStoredLater := derivedEvent(base.Add(2*time.Hour), false, "GPU-target")
	staleButStoredLater.HealthEvent.GeneratedTimestamp = timestamppb.New(base)
	newerButStoredEarlier := derivedEvent(base.Add(time.Hour), true, "GPU-target")
	newerButStoredEarlier.HealthEvent.GeneratedTimestamp = timestamppb.New(base.Add(3 * time.Hour))

	database := new(mockDatabaseClient)
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(staleButStoredLater, newerButStoredEarlier), nil).
		Once()
	reconciler := &Reconciler{databaseClient: database}

	latest, err := reconciler.findLatestMatchingEvent(
		context.Background(),
		map[string]any{"event_type": "RepeatedXID94OnSameGPU"},
		func(*datamodels.HealthEventWithStatus) bool { return true },
	)
	require.NoError(t, err)
	require.NotNil(t, latest)
	require.True(t, latest.HealthEvent.IsHealthy)
	require.Equal(t, base.Add(3*time.Hour), latest.HealthEvent.GeneratedTimestamp.AsTime())
}

func TestRecoveryBoundaryQueryFailurePreventsRuleEvaluation(t *testing.T) {
	now := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeEntity)
	database := new(mockDatabaseClient)
	platform := new(mockPublisher)
	reconciler := newRecoveryReconciler(rule, database, platform)
	incoming := storedEvent(now, &protos.HealthEvent{
		Agent:              "syslog-health-monitor",
		CheckName:          "SysLogsXIDError",
		NodeName:           "node-a",
		ErrorCode:          []string{"94"},
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: "GPU-target"}},
		GeneratedTimestamp: timestamppb.New(now),
	})

	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return((*healthEventCursor)(nil), errors.New("query failed")).
		Once()

	didPublish, err := reconciler.handleEvent(context.Background(), &incoming)
	require.False(t, didPublish)
	require.ErrorContains(t, err, "find recovery boundary")
	database.AssertNotCalled(t, "Aggregate", mock.Anything, mock.Anything)
	platform.AssertNotCalled(t, "HealthEventOccurredV1", mock.Anything, mock.Anything)
}

func TestActiveDerivedFaultSuppressesDuplicateUnhealthyPublish(t *testing.T) {
	now := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeEntity)
	database := new(mockDatabaseClient)
	platform := new(mockPublisher)
	reconciler := newRecoveryReconciler(rule, database, platform)
	incoming := storedEvent(now, &protos.HealthEvent{
		Agent:              "syslog-health-monitor",
		CheckName:          "SysLogsXIDError",
		NodeName:           "node-a",
		ErrorCode:          []string{"94"},
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: "GPU-target"}},
		GeneratedTimestamp: timestamppb.New(now),
	})

	// No recovery boundary exists, the aggregation matches, and an active
	// derived fault already exists for this exact scope.
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(), nil).
		Once()
	aggregateCursor, _ := createMockCursor([]map[string]any{{"ruleMatched": true}})
	database.On("Aggregate", mock.Anything, mock.Anything).Return(aggregateCursor, nil).Once()
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(derivedEvent(now.Add(-time.Minute), false, "GPU-target")), nil).
		Once()

	didPublish, err := reconciler.handleEvent(context.Background(), &incoming)
	require.NoError(t, err)
	require.False(t, didPublish)
	platform.AssertNotCalled(t, "HealthEventOccurredV1", mock.Anything, mock.Anything)
}

func TestDerivedFaultWaitsForPersistenceAndUsesRecoveryScope(t *testing.T) {
	now := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeEntity)
	database := new(mockDatabaseClient)
	platform := new(mockPublisher)
	reconciler := newRecoveryReconciler(rule, database, platform)
	reconciler.recoveryPoll = time.Millisecond
	reconciler.recoveryRepublish = time.Millisecond
	incoming := storedEvent(now, &protos.HealthEvent{
		Agent:     "syslog-health-monitor",
		CheckName: "SysLogsXIDError",
		NodeName:  "node-a",
		ErrorCode: []string{"94"},
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "PCI", EntityValue: "0000:b4:00.0"},
			{EntityType: "GPU_UUID", EntityValue: "GPU-target"},
		},
		GeneratedTimestamp: timestamppb.New(now),
	})

	identity, ok := recoveryIdentityForEvent(rule, incoming.HealthEvent)
	require.True(t, ok)
	reconciler.rememberRecoveryBoundary(rule.Name, identity, recoveryBoundary{
		createdAt: now.Add(-time.Minute),
		generated: timestamppb.New(now.Add(-time.Minute)),
	})
	reconciler.rememberDerivedState(rule.Name, identity, derivedState{
		boundary:  recoveryBoundary{createdAt: now.Add(-time.Minute)},
		isHealthy: true,
	})

	aggregate, _ := createMockCursor([]map[string]any{{"ruleMatched": true}})
	database.On("Aggregate", mock.Anything, mock.Anything).Return(aggregate, nil).Once()
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(), nil).
		Once()
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(persistedFault(
			now.Add(time.Second), incoming, rule,
			[]*protos.Entity{{EntityType: "GPU_UUID", EntityValue: "GPU-target"}},
		)), nil).
		Once()

	var published *protos.HealthEvent
	platform.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			published = proto.Clone(args.Get(1).(*protos.HealthEvents).Events[0]).(*protos.HealthEvent)
		}).
		Return(&emptypb.Empty{}, nil).
		Twice()

	didPublish, err := reconciler.handleEvent(context.Background(), &incoming)
	require.NoError(t, err)
	require.True(t, didPublish)
	platform.AssertNumberOfCalls(t, "HealthEventOccurredV1", 2)
	require.Equal(t, []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: "GPU-target"}},
		published.EntitiesImpacted)
	database.AssertExpectations(t)
}

func TestRecoveryEnabledRulePreservesFaultWithoutConfiguredEntity(t *testing.T) {
	now := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeEntity)
	database := new(mockDatabaseClient)
	platform := new(mockPublisher)
	reconciler := newRecoveryReconciler(rule, database, platform)
	incoming := storedEvent(now, &protos.HealthEvent{
		Agent:              "syslog-health-monitor",
		CheckName:          "SysLogsXIDError",
		NodeName:           "node-a",
		ErrorCode:          []string{"94"},
		EntitiesImpacted:   []*protos.Entity{{EntityType: "PCI", EntityValue: "0000:b4:00.0"}},
		GeneratedTimestamp: timestamppb.New(now),
	})

	aggregate, _ := createMockCursor([]map[string]any{{"ruleMatched": true}})
	database.On("Aggregate", mock.Anything, mock.Anything).Return(aggregate, nil).Once()
	platform.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
		Return(&emptypb.Empty{}, nil).
		Once()

	didPublish, err := reconciler.handleEvent(context.Background(), &incoming)
	require.NoError(t, err)
	require.True(t, didPublish)
	platform.AssertExpectations(t)
}

func TestRecoveryRuleSkipsDisabledAndIncompleteScope(t *testing.T) {
	rule := recoveryRule(config.RecoveryScopeEntity)
	recovery := recoverySource(time.Now().UTC(), "GPU-target")
	reconciler := newRecoveryReconciler(rule, new(mockDatabaseClient), new(mockPublisher))

	disabled := rule
	disabled.EvaluateRule = false
	published, err := reconciler.handleRecoveryRule(context.Background(), &recovery, disabled)
	require.NoError(t, err)
	require.False(t, published)

	recovery.HealthEvent.EntitiesImpacted = nil
	published, err = reconciler.handleRecoveryRule(context.Background(), &recovery, rule)
	require.NoError(t, err)
	require.False(t, published)
}

func TestRecoveryStateQueriesPropagateProviderErrors(t *testing.T) {
	rule := recoveryRule(config.RecoveryScopeEntity)
	recovery := recoverySource(time.Now().UTC(), "GPU-target")
	identity, ok := recoveryIdentityForEvent(rule, recovery.HealthEvent)
	require.True(t, ok)

	for name, call := range map[string]func(*Reconciler) error{
		"handle recovery": func(reconciler *Reconciler) error {
			_, err := reconciler.handleRecoveryRule(context.Background(), &recovery, rule)
			return err
		},
		"current derived state": func(reconciler *Reconciler) error {
			_, _, err := reconciler.currentDerivedState(context.Background(), rule, identity)
			return err
		},
		"derived fault state": func(reconciler *Reconciler) error {
			_, _, _, err := reconciler.derivedFaultState(context.Background(), rule, recovery.HealthEvent)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			database := new(mockDatabaseClient)
			database.On("Find", mock.Anything, mock.Anything, mock.Anything).
				Return((*healthEventCursor)(nil), errors.New("provider unavailable")).Once()
			reconciler := newRecoveryReconciler(rule, database, new(mockPublisher))
			require.ErrorContains(t, call(reconciler), "provider unavailable")
		})
	}
}

func TestRecoveryBoundaryRejectsInvalidIdentityAndMissingSource(t *testing.T) {
	rule := recoveryRule(config.RecoveryScopeEntity)
	database := new(mockDatabaseClient)
	reconciler := &Reconciler{databaseClient: database}

	boundary, err := reconciler.recoveryBoundaryForEvent(context.Background(), rule,
		&protos.HealthEvent{NodeName: "node-a"})
	require.NoError(t, err)
	require.Nil(t, boundary)

	incoming := &protos.HealthEvent{
		NodeName:         "node-a",
		EntitiesImpacted: []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: "GPU-target"}},
	}
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).Return(
		newHealthEventCursor(
			recoverySource(time.Now().UTC(), "GPU-other"),
			storedEvent(time.Now().UTC(), &protos.HealthEvent{
				Agent: "syslog-health-monitor", CheckName: "SysLogsXIDError", IsHealthy: true,
				NodeName: "node-a",
			}),
			storedEvent(time.Now().UTC(), nil),
		), nil,
	).Once()
	boundary, err = reconciler.recoveryBoundaryForEvent(context.Background(), rule, incoming)
	require.NoError(t, err)
	require.Nil(t, boundary)
}

func TestFindLatestMatchingEventReportsCursorFailures(t *testing.T) {
	for name, cursor := range map[string]*healthEventCursor{
		"decode": {
			events:    []datamodels.HealthEventWithStatus{derivedEvent(time.Now(), false, "GPU-a")},
			pos:       -1,
			decodeErr: errors.New("decode failed"),
		},
		"iteration": {pos: -1, err: errors.New("iteration failed")},
	} {
		t.Run(name, func(t *testing.T) {
			database := new(mockDatabaseClient)
			database.On("Find", mock.Anything, mock.Anything, mock.Anything).Return(cursor, nil).Once()
			reconciler := &Reconciler{databaseClient: database}
			_, err := reconciler.findLatestMatchingEvent(
				context.Background(), map[string]any{}, func(*datamodels.HealthEventWithStatus) bool { return true },
			)
			require.Error(t, err)
		})
	}
}

func TestRecoveryOrderingFallbacks(t *testing.T) {
	base := time.Now().UTC()
	require.True(t, boundaryAfter(
		recoveryBoundary{createdAt: base.Add(time.Second)},
		recoveryBoundary{createdAt: base},
	))
	require.False(t, boundaryAfter(recoveryBoundary{}, recoveryBoundary{}))
	require.False(t, sameRecoverySource(nil, nil))
	require.False(t, sameRecoverySource(
		&datamodels.HealthEventWithStatus{HealthEvent: &protos.HealthEvent{}}, nil,
	))

	invalidTimestamp := &timestamppb.Timestamp{Seconds: 253402300800}
	boundary := boundaryFromEvent(&datamodels.HealthEventWithStatus{
		CreatedAt:   base,
		HealthEvent: &protos.HealthEvent{GeneratedTimestamp: invalidTimestamp},
	})
	require.Nil(t, boundary.generated)
}

func TestDerivedPersistenceRetriesProviderError(t *testing.T) {
	now := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeEntity)
	source := recoverySource(now, "GPU-target")
	identity, ok := recoveryIdentityForEvent(rule, source.HealthEvent)
	require.True(t, ok)
	database := new(mockDatabaseClient)
	reconciler := &Reconciler{
		databaseClient:    database,
		recoveryPoll:      time.Millisecond,
		recoveryRepublish: time.Hour,
	}
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return((*healthEventCursor)(nil), errors.New("temporary query failure")).Once()
	database.On("Find", mock.Anything, mock.Anything, mock.Anything).
		Return(newHealthEventCursor(persistedRecovery(now.Add(time.Second), source, rule)), nil).Once()

	publishCalls := 0
	boundary, err := reconciler.publishDerivedUntilStored(
		context.Background(), &source, rule, identity, true, "recovery",
		func() error { publishCalls++; return nil },
	)
	require.NoError(t, err)
	require.Equal(t, now.Add(time.Second), boundary.createdAt)
	require.Equal(t, 1, publishCalls)
}

func TestProcessRulePropagatesDerivedStateAndPersistenceErrors(t *testing.T) {
	now := time.Now().UTC()
	rule := recoveryRule(config.RecoveryScopeEntity)
	incoming := storedEvent(now, &protos.HealthEvent{
		Agent:              "syslog-health-monitor",
		CheckName:          "SysLogsXIDError",
		NodeName:           "node-a",
		ErrorCode:          []string{"94"},
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: "GPU-target"}},
		GeneratedTimestamp: timestamppb.New(now),
	})
	identity, ok := recoveryIdentityForEvent(rule, incoming.HealthEvent)
	require.True(t, ok)

	t.Run("state query", func(t *testing.T) {
		database := new(mockDatabaseClient)
		reconciler := newRecoveryReconciler(rule, database, new(mockPublisher))
		reconciler.rememberRecoveryBoundary(rule.Name, identity, recoveryBoundary{createdAt: now.Add(-time.Minute)})
		aggregate, _ := createMockCursor([]map[string]any{{"ruleMatched": true}})
		database.On("Aggregate", mock.Anything, mock.Anything).Return(aggregate, nil).Once()
		database.On("Find", mock.Anything, mock.Anything, mock.Anything).
			Return((*healthEventCursor)(nil), errors.New("state query failed")).Once()

		published, err := reconciler.processRule(context.Background(), rule, &incoming)
		require.False(t, published)
		require.ErrorContains(t, err, "state query failed")
	})

	t.Run("persistence wait", func(t *testing.T) {
		database := new(mockDatabaseClient)
		platform := new(mockPublisher)
		reconciler := newRecoveryReconciler(rule, database, platform)
		reconciler.recoveryPoll = time.Hour
		reconciler.rememberRecoveryBoundary(rule.Name, identity, recoveryBoundary{createdAt: now.Add(-time.Minute)})
		reconciler.rememberDerivedState(rule.Name, identity, derivedState{
			boundary: recoveryBoundary{createdAt: now.Add(-time.Minute)}, isHealthy: true,
		})
		aggregate, _ := createMockCursor([]map[string]any{{"ruleMatched": true}})
		database.On("Aggregate", mock.Anything, mock.Anything).Return(aggregate, nil).Once()
		database.On("Find", mock.Anything, mock.Anything, mock.Anything).
			Return(newHealthEventCursor(), nil).Once()
		ctx, cancel := context.WithCancel(context.Background())
		platform.On("HealthEventOccurredV1", mock.Anything, mock.Anything).
			Run(func(mock.Arguments) { cancel() }).Return(&emptypb.Empty{}, nil).Once()

		published, err := reconciler.processRule(ctx, rule, &incoming)
		require.False(t, published)
		require.ErrorIs(t, err, context.Canceled)
	})
}
