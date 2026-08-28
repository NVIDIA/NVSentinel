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
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
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

func TestHandleCircuitBreakerCreatePersistsCutoffBeforeDeletingToken(t *testing.T) {
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

func TestHandleCircuitBreakerCreateKeepsTokenWhenCutoffPersistenceFails(t *testing.T) {
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
