// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
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
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	_ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"
)

const recoveryIntegrationEnv = "NVSENTINEL_RUN_RECOVERY_INTEGRATION"

type integrationStoreSink struct {
	database client.DatabaseClient
	calls    atomic.Int32
	results  chan error
	drop     map[int32]bool
}

type recoveryObservingWatcher struct {
	client.ChangeStreamWatcher
	reconciler *Reconciler
	source     *datamodels.HealthEventWithStatus
	rule       config.HealthEventsAnalyzerRule
	identity   recoveryIdentity
	started    chan struct{}
	marked     chan error
}

func (w *recoveryObservingWatcher) Start(ctx context.Context) {
	w.ChangeStreamWatcher.Start(ctx)
	close(w.started)
}

func (w *recoveryObservingWatcher) MarkProcessed(ctx context.Context, token []byte) error {
	persisted, err := w.reconciler.findPersistedDerived(ctx, w.source, w.rule, w.identity, true)
	if err == nil && persisted == nil {
		err = errors.New("resume token reached MarkProcessed before recovery was stored")
	}
	if err == nil {
		err = w.ChangeStreamWatcher.MarkProcessed(ctx, token)
	}

	w.marked <- err

	return err
}

func (s *integrationStoreSink) HealthEventOccurredV1(
	_ context.Context,
	events *protos.HealthEvents,
	_ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	call := s.calls.Add(1)
	drop := call == 1
	if s.drop != nil {
		drop = s.drop[call]
	}

	if drop {
		// Model an accepted ring-buffer item that is lost before the store sink
		// inserts it. The reconciler must detect and republish it.
		return &emptypb.Empty{}, nil
	}

	documents := make([]any, 0, len(events.Events))
	for _, event := range events.Events {
		documents = append(documents, datamodels.HealthEventWithStatus{
			CreatedAt:         time.Now().UTC(),
			HealthEvent:       proto.Clone(event).(*protos.HealthEvent),
			HealthEventStatus: &protos.HealthEventStatus{},
		})
	}

	go func() {
		time.Sleep(5 * time.Millisecond)
		_, err := s.database.InsertMany(context.Background(), documents)
		s.results <- err
	}()

	return &emptypb.Empty{}, nil
}

func TestRecoveryLifecycleWithRealProvider(t *testing.T) {
	if os.Getenv(recoveryIntegrationEnv) != "1" {
		t.Skipf("set %s=1 with a real provider configuration", recoveryIntegrationEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dsConfig, err := datastore.LoadDatastoreConfig()
	require.NoError(t, err)
	ds, err := datastore.NewDataStore(ctx, *dsConfig)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ds.Close(context.Background())) })

	adapter, ok := ds.(interface {
		GetDatabaseClient() client.DatabaseClient
	})
	require.True(t, ok)
	database := adapter.GetDatabaseClient()
	require.NoError(t, database.Ping(ctx))

	runID := time.Now().UTC().UnixNano()
	nodeName := fmt.Sprintf("recovery-e2e-node-%d", runID)
	gpuUUID := fmt.Sprintf("GPU-%d", runID)
	baseTime := time.Now().UTC().Add(-time.Minute)
	rule := recoveryRule(config.RecoveryScopeEntity)
	// A successful GPU-reset event identifies the recovered GPU by entity and
	// does not carry an error code.
	rule.Recovery.SourceErrorCodes = nil
	rule.Stage = []string{
		`{"$match":{"healthevent.checkname":"SysLogsXIDError","healthevent.ishealthy":false}}`,
		`{"$count":"count"}`,
		`{"$match":{"count":{"$gte":2}}}`,
	}

	oldEvent := func(offset time.Duration) datamodels.HealthEventWithStatus {
		createdAt := time.Now().UTC()
		return storedEvent(createdAt, &protos.HealthEvent{
			Agent:              "syslog-health-monitor",
			CheckName:          "SysLogsXIDError",
			IsHealthy:          false,
			ErrorCode:          []string{"94"},
			NodeName:           nodeName,
			EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: gpuUUID}},
			GeneratedTimestamp: timestamppb.New(baseTime.Add(offset)),
			ProcessingStrategy: protos.ProcessingStrategy_EXECUTE_REMEDIATION,
		})
	}

	oldOne := oldEvent(0)
	insertHealthEvents(t, ctx, database, oldOne)
	oldTwo := oldEvent(10 * time.Second)
	insertHealthEvents(t, ctx, database, oldTwo)

	active := derivedEvent(time.Now().UTC(), false, gpuUUID)
	active.HealthEvent.NodeName = nodeName
	active.HealthEvent.GeneratedTimestamp = timestamppb.New(baseTime.Add(20 * time.Second))
	insertHealthEvents(t, ctx, database, active)

	recovery := recoverySource(time.Now().UTC(), gpuUUID)
	recovery.HealthEvent.ErrorCode = nil
	recovery.HealthEvent.NodeName = nodeName
	recovery.HealthEvent.GeneratedTimestamp = timestamppb.New(baseTime.Add(30 * time.Second))
	insertHealthEvents(t, ctx, database, recovery)

	sink := &integrationStoreSink{
		database: database,
		results:  make(chan error, 4),
		drop:     map[int32]bool{1: true, 3: true},
	}
	reconciler := &Reconciler{
		config: HealthEventsAnalyzerReconcilerConfig{
			HealthEventsAnalyzerRules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{rule}},
			Publisher: publisher.NewPublisher(
				sink,
				protos.ProcessingStrategy_EXECUTE_REMEDIATION,
			),
		},
		databaseClient:    database,
		provider:          ds.Provider(),
		recoveryPoll:      2 * time.Millisecond,
		recoveryRepublish: 100 * time.Millisecond,
	}

	published, err := reconciler.handleEvent(ctx, &recovery)
	require.NoError(t, err)
	require.True(t, published)
	require.EqualValues(t, 2, sink.calls.Load(), "lost accepted recovery must be republished")
	require.NoError(t, <-sink.results)
	requireStoredDerivedState(t, ctx, reconciler, rule, nodeName, gpuUUID, true)

	postRecoveryOne := oldEvent(40 * time.Second)
	insertHealthEvents(t, ctx, database, postRecoveryOne)
	published, err = reconciler.handleEvent(ctx, &postRecoveryOne)
	require.NoError(t, err)
	require.False(t, published, "pre-recovery history must not satisfy the threshold")

	postRecoveryTwo := oldEvent(50 * time.Second)
	insertHealthEvents(t, ctx, database, postRecoveryTwo)
	published, err = reconciler.handleEvent(ctx, &postRecoveryTwo)
	require.NoError(t, err)
	require.True(t, published, "two post-recovery events must reactivate the rule")
	require.NoError(t, <-sink.results)
	requireStoredDerivedState(t, ctx, reconciler, rule, nodeName, gpuUUID, false)

	published, err = reconciler.handleEvent(ctx, &postRecoveryTwo)
	require.NoError(t, err)
	require.False(t, published, "replayed source must reuse the persisted derived event")
	require.EqualValues(t, 4, sink.calls.Load(), "replay must not enqueue a duplicate")
}

func TestRecoveryWatcherAcknowledgesAfterStorageWithRealProvider(t *testing.T) {
	if os.Getenv(recoveryIntegrationEnv) != "1" {
		t.Skipf("set %s=1 with a real provider configuration", recoveryIntegrationEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dsConfig, err := datastore.LoadDatastoreConfig()
	require.NoError(t, err)
	ds, err := datastore.NewDataStore(ctx, *dsConfig)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ds.Close(context.Background())) })

	adapter, ok := ds.(interface {
		GetDatabaseClient() client.DatabaseClient
	})
	require.True(t, ok)
	database := adapter.GetDatabaseClient()
	require.NoError(t, database.Ping(ctx))

	runID := time.Now().UTC().UnixNano()
	nodeName := fmt.Sprintf("recovery-watch-node-%d", runID)
	gpuUUID := fmt.Sprintf("GPU-%d", runID)
	rule := recoveryRule(config.RecoveryScopeEntity)
	rule.Recovery.SourceErrorCodes = nil
	active := derivedEvent(time.Now().UTC(), false, gpuUUID)
	active.HealthEvent.NodeName = nodeName
	active.HealthEvent.GeneratedTimestamp = timestamppb.New(time.Now().UTC().Add(-time.Minute))
	insertHealthEvents(t, ctx, database, active)

	source := recoverySource(time.Now().UTC(), gpuUUID)
	source.HealthEvent.ErrorCode = nil
	source.HealthEvent.NodeName = nodeName
	source.HealthEvent.GeneratedTimestamp = timestamppb.Now()
	source.HealthEvent.ProcessingStrategy = protos.ProcessingStrategy_EXECUTE_REMEDIATION
	identity, ok := recoveryIdentityForEvent(rule, source.HealthEvent)
	require.True(t, ok)

	sink := &integrationStoreSink{
		database: database,
		results:  make(chan error, 2),
	}
	reconciler := &Reconciler{
		config: HealthEventsAnalyzerReconcilerConfig{
			HealthEventsAnalyzerRules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{rule}},
			Publisher: publisher.NewPublisher(
				sink,
				protos.ProcessingStrategy_EXECUTE_REMEDIATION,
			),
		},
		databaseClient:    database,
		provider:          ds.Provider(),
		recoveryPoll:      2 * time.Millisecond,
		recoveryRepublish: 100 * time.Millisecond,
	}

	pipeline := analyzerPipelineForNode(nodeName)
	var providerPipeline any = pipeline
	if ds.Provider() == datastore.ProviderMongoDB {
		providerPipeline, err = client.ConvertAgnosticPipelineToMongo(pipeline)
		require.NoError(t, err)
	}

	watcher, err := database.NewChangeStreamWatcher(ctx, client.TokenConfig{
		ClientName:      fmt.Sprintf("recovery-e2e-%d", runID),
		TokenDatabase:   dsConfig.Connection.Database,
		TokenCollection: "ResumeTokens",
	}, providerPipeline)
	require.NoError(t, err)
	observingWatcher := &recoveryObservingWatcher{
		ChangeStreamWatcher: watcher,
		reconciler:          reconciler,
		source:              &source,
		rule:                rule,
		identity:            identity,
		started:             make(chan struct{}),
		marked:              make(chan error, 1),
	}

	processor := client.NewEventProcessor(observingWatcher, database, client.EventProcessorConfig{
		MarkProcessedOnError: false,
	})
	processor.SetEventHandler(client.EventHandlerFunc(reconciler.processHealthEvent))
	processorDone := make(chan error, 1)
	go func() { processorDone <- processor.Start(ctx) }()
	<-observingWatcher.started
	time.Sleep(100 * time.Millisecond)

	insertHealthEvents(t, ctx, database, source)

	select {
	case err := <-observingWatcher.marked:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for source resume token")
	}
	require.EqualValues(t, 2, sink.calls.Load())
	require.NoError(t, <-sink.results)
	requireStoredDerivedState(t, ctx, reconciler, rule, nodeName, gpuUUID, true)

	cancel()
	require.ErrorIs(t, <-processorDone, context.Canceled)
}

func TestRecoveryQueryIgnoresNonNumericRowsWithRealPostgreSQL(t *testing.T) {
	if os.Getenv(recoveryIntegrationEnv) != "1" {
		t.Skipf("set %s=1 with a real PostgreSQL configuration", recoveryIntegrationEnv)
	}

	dsConfig, err := datastore.LoadDatastoreConfig()
	require.NoError(t, err)
	if dsConfig.Provider != datastore.ProviderPostgreSQL {
		t.Skip("PostgreSQL-specific numeric-cast regression")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ds, err := datastore.NewDataStore(ctx, *dsConfig)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ds.Close(context.Background())) })

	adapter, ok := ds.(interface {
		GetDatabaseClient() client.DatabaseClient
	})
	require.True(t, ok)
	database := adapter.GetDatabaseClient()
	require.NoError(t, database.Ping(ctx))

	runID := time.Now().UTC().UnixNano()
	nodeName := fmt.Sprintf("recovery-poison-node-%d", runID)
	event := storedEvent(time.Now().UTC(), &protos.HealthEvent{
		Agent:              "syslog-health-monitor",
		CheckName:          "SysLogsXIDError",
		IsHealthy:          false,
		NodeName:           nodeName,
		GeneratedTimestamp: timestamppb.Now(),
		ProcessingStrategy: protos.ProcessingStrategy_EXECUTE_REMEDIATION,
		RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: fmt.Sprintf("GPU-%d", runID)}},
	})
	insertHealthEvents(t, ctx, database, event)

	updated, err := database.UpdateDocument(ctx,
		map[string]any{"healthevent.nodename": nodeName},
		map[string]any{"$set": map[string]any{"healthevent.custommetric": "not-a-number"}},
	)
	require.NoError(t, err)
	require.EqualValues(t, 1, updated.ModifiedCount)

	rule := recoveryRule(config.RecoveryScopeEntity)
	rule.Stage = []string{
		`{"$match":{"healthevent.custommetric":{"$gte":1}}}`,
	}
	reconciler := &Reconciler{
		config: HealthEventsAnalyzerReconcilerConfig{
			HealthEventsAnalyzerRules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{rule}},
		},
		databaseClient: database,
		provider:       ds.Provider(),
	}

	matched, err := reconciler.handleEvent(ctx, &event)
	require.NoError(t, err, "non-numeric stored values must not crash-loop numeric comparisons")
	require.False(t, matched)
}

func analyzerPipelineForNode(nodeName string) datastore.Pipeline {
	pipeline := client.GetPipelineBuilder().BuildAnalyzerHealthEventInsertsPipeline()
	match := pipeline[0][0].Value.(datastore.Document)
	match = append(match, datastore.E("fullDocument.healthevent.nodename", nodeName))
	pipeline[0][0].Value = match

	return pipeline
}

func insertHealthEvents(
	t *testing.T,
	ctx context.Context,
	database client.DatabaseClient,
	events ...datamodels.HealthEventWithStatus,
) {
	t.Helper()

	documents := make([]any, len(events))
	for i := range events {
		documents[i] = events[i]
	}

	_, err := database.InsertMany(ctx, documents)
	require.NoError(t, err)
}

func requireStoredDerivedState(
	t *testing.T,
	ctx context.Context,
	reconciler *Reconciler,
	rule config.HealthEventsAnalyzerRule,
	nodeName string,
	gpuUUID string,
	wantHealthy bool,
) {
	t.Helper()

	identity, ok := recoveryIdentityForEvent(rule, &protos.HealthEvent{
		NodeName:         nodeName,
		EntitiesImpacted: []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: gpuUUID}},
	})
	require.True(t, ok)

	latest, err := reconciler.findLatestMatchingEvent(ctx, reconciler.recoveryLookupFilter(
		agentName, rule.Name, nodeName,
	), func(candidate *datamodels.HealthEventWithStatus) bool {
		candidateIdentity, valid := recoveryIdentityForEvent(rule, candidate.HealthEvent)
		return valid && candidateIdentity.key == identity.key
	})
	require.NoError(t, err)
	require.NotNil(t, latest)
	require.Equal(t, wantHealthy, latest.HealthEvent.IsHealthy)
}
