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

package client

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestPermanentError_WrappedAndJoinedErrors_ClassifiesOnlyFullyPermanentChains(t *testing.T) {
	permanent := PermanentError(errors.New("invalid event"))
	require.True(t, IsPermanentError(permanent))
	require.True(t, IsPermanentError(fmt.Errorf("wrapped: %w", permanent)))
	require.True(t, IsPermanentError(errors.Join(permanent, PermanentError(errors.New("bad rule")))))
	require.False(t, IsPermanentError(errors.Join(permanent, errors.New("database unavailable"))))
	require.False(t, IsPermanentError(nil))
}

func TestEventProcessor_PermanentHandlerFailure_CheckpointsAndContinues(t *testing.T) {
	events := make(chan Event, 2)
	events <- newPermanentErrorTestEvent("1")
	events <- newPermanentErrorTestEvent("2")
	close(events)

	watcher := &permanentErrorTestWatcher{events: events}
	processor := NewEventProcessor(watcher, nil, EventProcessorConfig{}).(*DefaultEventProcessor)
	var handled []string
	processor.SetEventHandler(EventHandlerFunc(func(_ context.Context, event *datamodels.HealthEventWithStatus) error {
		id := event.HealthEvent.GetId()
		handled = append(handled, id)
		if id == "1" {
			return PermanentError(errors.New("deterministic failure"))
		}

		return nil
	}))

	require.NoError(t, processor.processEvents(context.Background()))
	require.Equal(t, []string{"1", "2"}, handled)
	require.Equal(t, []string{"1", "2"}, watcher.marked)
}

func TestEventProcessor_PermanentFailureCheckpointFails_StopsAtUncheckpointedEvent(t *testing.T) {
	events := make(chan Event, 1)
	events <- newPermanentErrorTestEvent("1")
	close(events)

	checkpointErr := errors.New("checkpoint unavailable")
	watcher := &permanentErrorTestWatcher{events: events, markErr: checkpointErr}
	processor := NewEventProcessor(watcher, nil, EventProcessorConfig{}).(*DefaultEventProcessor)
	processor.SetEventHandler(EventHandlerFunc(func(context.Context, *datamodels.HealthEventWithStatus) error {
		return PermanentError(errors.New("deterministic failure"))
	}))

	err := processor.processEvents(context.Background())
	require.ErrorContains(t, err, "stopping at uncheckpointed event")
	require.ErrorIs(t, err, checkpointErr)
	require.Equal(t, []string{"1"}, watcher.marked)
}

type permanentErrorTestEvent struct {
	id string
}

func newPermanentErrorTestEvent(id string) *permanentErrorTestEvent {
	return &permanentErrorTestEvent{id: id}
}
func (e *permanentErrorTestEvent) GetDocumentID() (string, error) { return e.id, nil }
func (e *permanentErrorTestEvent) GetRecordUUID() (string, error) { return e.id, nil }
func (e *permanentErrorTestEvent) GetNodeName() (string, error)   { return "node-a", nil }
func (e *permanentErrorTestEvent) GetResumeToken() []byte         { return []byte(e.id) }
func (e *permanentErrorTestEvent) UnmarshalDocument(value any) error {
	event := value.(*datamodels.HealthEventWithStatus)
	event.HealthEvent = &protos.HealthEvent{Id: e.id}

	return nil
}

type permanentErrorTestWatcher struct {
	events  chan Event
	marked  []string
	markErr error
}

func (w *permanentErrorTestWatcher) Start(context.Context)       {}
func (w *permanentErrorTestWatcher) Events() <-chan Event        { return w.events }
func (w *permanentErrorTestWatcher) Close(context.Context) error { return nil }
func (w *permanentErrorTestWatcher) MarkProcessed(_ context.Context, token []byte) error {
	w.marked = append(w.marked, string(token))

	return w.markErr
}
