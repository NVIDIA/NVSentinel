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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/breaker"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/eventwatcher"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type cursorModeBreakerStub struct {
	mode    breaker.CursorMode
	actions *[]string
}

func (*cursorModeBreakerStub) AddCordonEvent(string) {}

func (*cursorModeBreakerStub) IsTripped(context.Context) (bool, error) {
	return false, nil
}

func (*cursorModeBreakerStub) ForceState(context.Context, breaker.State) error {
	return nil
}

func (*cursorModeBreakerStub) CurrentState() breaker.State {
	return breaker.StateClosed
}

func (s *cursorModeBreakerStub) GetCursorMode(context.Context) (breaker.CursorMode, error) {
	return s.mode, nil
}

func (s *cursorModeBreakerStub) SetCursorMode(_ context.Context, mode breaker.CursorMode) error {
	*s.actions = append(*s.actions, "reset-breaker")
	s.mode = mode

	return nil
}

type resumeTokenClientStub struct {
	client.DatabaseClient
}

type coldStartCallbackWatcherStub struct {
	eventwatcher.EventWatcherInterface
	callback func(context.Context) error
}

func (s *coldStartCallbackWatcherStub) SetColdStartCallback(callback func(context.Context) error) {
	s.callback = callback
}

type emptyHealthEventStoreStub struct {
	datastore.HealthEventStore
	builder datastore.QueryBuilder
	findErr error
}

func (s *emptyHealthEventStoreStub) FindHealthEventsByQueryBatched(
	_ context.Context,
	builder datastore.QueryBuilder,
	_ int,
	_ func([]datastore.HealthEventWithStatus) error,
) error {
	s.builder = builder

	return s.findErr
}

func TestHandleCircuitBreakerCreate_Success_PersistsCutoffBeforeDeletingToken(t *testing.T) {
	var actions []string
	tokenConfig := client.TokenConfig{
		ClientName:      "fault-quarantine",
		TokenDatabase:   "HealthEventsDatabase",
		TokenCollection: "ResumeTokens",
	}
	cb := &cursorModeBreakerStub{mode: breaker.CursorModeCreate, actions: &actions}
	dbClient := &resumeTokenClientStub{}
	r := NewReconciler(ReconcilerConfig{
		CircuitBreakerEnabled: true,
		TokenConfig:           tokenConfig,
	}, nil, cb)
	r.resetResumeTokenForCreate = func(
		_ context.Context,
		gotDBClient client.DatabaseClient,
		gotTokenConfig client.TokenConfig,
		onTokenDeleted func() error,
	) (client.ResumeControlDecision, error) {
		actions = append(actions, "persist-cutoff", "delete-token")
		assert.Same(t, dbClient, gotDBClient)
		assert.Equal(t, tokenConfig, gotTokenConfig)
		require.NoError(t, onTokenDeleted())

		return client.ResumeControlDecision{StartFresh: true, ColdStartCutoff: time.Now()}, nil
	}

	startFresh, err := r.handleCircuitBreakerCursorMode(context.Background(), dbClient)
	require.NoError(t, err)
	assert.True(t, startFresh)
	assert.Equal(t, []string{"persist-cutoff", "delete-token", "reset-breaker"}, actions)
}

func TestHandleCircuitBreakerCreate_CutoffPersistenceFailure_KeepsToken(t *testing.T) {
	var actions []string
	persistErr := errors.New("config map unavailable")
	cb := &cursorModeBreakerStub{mode: breaker.CursorModeCreate, actions: &actions}
	dbClient := &resumeTokenClientStub{}
	r := NewReconciler(ReconcilerConfig{CircuitBreakerEnabled: true}, nil, cb)
	r.resetResumeTokenForCreate = func(
		context.Context, client.DatabaseClient, client.TokenConfig, func() error,
	) (client.ResumeControlDecision, error) {
		actions = append(actions, "persist-cutoff")

		return client.ResumeControlDecision{}, persistErr
	}

	startFresh, err := r.handleCircuitBreakerCursorMode(context.Background(), dbClient)
	require.ErrorIs(t, err, persistErr)
	assert.False(t, startFresh)
	assert.Equal(t, []string{"persist-cutoff"}, actions)
}

func TestConfigureColdStart_MissingCutoff_SeedsAndPersistsBoundedWindow(t *testing.T) {
	watcher := &coldStartCallbackWatcherStub{}
	store := &emptyHealthEventStoreStub{}
	r := NewReconciler(ReconcilerConfig{TokenConfig: client.TokenConfig{
		ClientName: "fault-quarantine",
	}}, nil, nil)
	r.eventWatcher = watcher
	var persistedCutoff time.Time
	r.setColdStartCutoff = func(_ context.Context, clientName string, cutoff time.Time) error {
		assert.Equal(t, "fault-quarantine", clientName)
		persistedCutoff = cutoff

		return nil
	}

	r.configureColdStart(
		context.Background(), false, false, time.Time{}, store)
	require.NotNil(t, watcher.callback)
	configuredAt := time.Now().UTC()
	require.NoError(t, watcher.callback(context.Background()))

	_, args := store.builder.ToSQL()
	require.NotEmpty(t, args)
	lowerBoundary, ok := args[0].(time.Time)
	require.True(t, ok)
	upperBoundary, ok := args[len(args)-1].(time.Time)
	require.True(t, ok)
	assert.True(t, lowerBoundary.Before(upperBoundary), "seeded recovery window must be non-empty")
	assert.False(t, lowerBoundary.After(configuredAt), "lower watermark must be captured before recovery starts")
	assert.Equal(t, upperBoundary, persistedCutoff)
}

func TestConfigureColdStart_FailedSweep_DoesNotAdvanceCutoff(t *testing.T) {
	sweepErr := errors.New("database unavailable")
	watcher := &coldStartCallbackWatcherStub{}
	r := NewReconciler(ReconcilerConfig{TokenConfig: client.TokenConfig{
		ClientName: "fault-quarantine",
	}}, nil, nil)
	r.eventWatcher = watcher
	persistCalls := 0
	r.setColdStartCutoff = func(context.Context, string, time.Time) error {
		persistCalls++

		return nil
	}

	r.configureColdStart(context.Background(), false, false, time.Time{},
		&emptyHealthEventStoreStub{findErr: sweepErr})
	require.NotNil(t, watcher.callback)
	require.ErrorIs(t, watcher.callback(context.Background()), sweepErr)
	assert.Zero(t, persistCalls)
}

func TestConfigureColdStart_ExistingCutoff_DoesNotAdvanceWatermark(t *testing.T) {
	watcher := &coldStartCallbackWatcherStub{}
	store := &emptyHealthEventStoreStub{}
	r := NewReconciler(ReconcilerConfig{TokenConfig: client.TokenConfig{
		ClientName: "fault-quarantine",
	}}, nil, nil)
	r.eventWatcher = watcher
	persistCalls := 0
	r.setColdStartCutoff = func(context.Context, string, time.Time) error {
		persistCalls++

		return nil
	}
	existingCutoff := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)

	r.configureColdStart(context.Background(), false, false, existingCutoff, store)
	require.NotNil(t, watcher.callback)
	require.NoError(t, watcher.callback(context.Background()))

	_, args := store.builder.ToSQL()
	require.NotEmpty(t, args)
	assert.Equal(t, existingCutoff, args[0])
	assert.Zero(t, persistCalls)
}
