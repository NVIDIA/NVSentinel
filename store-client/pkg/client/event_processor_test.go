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

	"github.com/nvidia/nvsentinel/commons/pkg/healthstatus"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// TestProcessEvents_EventHandlingAndCheckpointOutcomes_PreserveCheckpointOrdering verifies ordered checkpoints.
func TestProcessEvents_EventHandlingAndCheckpointOutcomes_PreserveCheckpointOrdering(t *testing.T) {
	processingErr := errors.New("processing failed")
	checkpointErr := errors.New("checkpoint write failed")
	unmarshalErr := errors.New("invalid event")
	documentIDErr := errors.New("invalid document ID")

	tests := []struct {
		name          string
		config        EventProcessorConfig
		firstEvent    *eventProcessorTestEvent
		handlerErrors map[string]error
		markErrors    map[string]error
		wantErrors    []error
		wantErrorText string
		wantHandled   []string
		wantMarkCalls []string
		wantMarked    []string
	}{
		{
			name:          "successful events are checkpointed in order",
			firstEvent:    newEventProcessorTestEvent("1"),
			wantHandled:   []string{"1", "2"},
			wantMarkCalls: []string{"1", "2"},
			wantMarked:    []string{"1", "2"},
		},
		{
			name:          "handler failure keeps later event unprocessed",
			firstEvent:    newEventProcessorTestEvent("1"),
			handlerErrors: map[string]error{"1": processingErr},
			wantErrors:    []error{processingErr},
			wantHandled:   []string{"1"},
		},
		{
			name:          "checkpoint failure keeps later event unprocessed",
			firstEvent:    newEventProcessorTestEvent("1"),
			markErrors:    map[string]error{"1": checkpointErr},
			wantErrors:    []error{checkpointErr},
			wantHandled:   []string{"1"},
			wantMarkCalls: []string{"1"},
		},
		{
			name: "filtered event checkpoint failure keeps later event unprocessed",
			config: EventProcessorConfig{SkipEvent: func(event Event) bool {
				eventID, _ := event.GetDocumentID()

				return eventID == "1"
			}},
			firstEvent:    newEventProcessorTestEvent("1"),
			markErrors:    map[string]error{"1": checkpointErr},
			wantErrors:    []error{checkpointErr},
			wantMarkCalls: []string{"1"},
		},
		{
			name:          "configured handler failure skip continues after checkpoint",
			config:        EventProcessorConfig{MarkProcessedOnError: true},
			firstEvent:    newEventProcessorTestEvent("1"),
			handlerErrors: map[string]error{"1": processingErr},
			wantHandled:   []string{"1", "2"},
			wantMarkCalls: []string{"1", "2"},
			wantMarked:    []string{"1", "2"},
		},
		{
			name:          "configured skip stops when checkpoint fails",
			config:        EventProcessorConfig{MarkProcessedOnError: true},
			firstEvent:    newEventProcessorTestEvent("1"),
			handlerErrors: map[string]error{"1": processingErr},
			markErrors:    map[string]error{"1": checkpointErr},
			wantErrors:    []error{processingErr, checkpointErr},
			wantHandled:   []string{"1"},
			wantMarkCalls: []string{"1"},
		},
		{
			name: "unmarshal error stops when checkpoint fails",
			firstEvent: &eventProcessorTestEvent{
				id:           "1",
				token:        []byte("1"),
				unmarshalErr: unmarshalErr,
			},
			markErrors:    map[string]error{"1": checkpointErr},
			wantErrors:    []error{unmarshalErr, checkpointErr},
			wantMarkCalls: []string{"1"},
		},
		{
			name: "document ID error stops when checkpoint fails",
			firstEvent: &eventProcessorTestEvent{
				token:         []byte("1"),
				documentIDErr: documentIDErr,
			},
			markErrors:    map[string]error{"1": checkpointErr},
			wantErrors:    []error{documentIDErr, checkpointErr},
			wantErrorText: `stopping at uncheckpointed event "unknown"`,
			wantMarkCalls: []string{"1"},
		},
		{
			name: "invalid event continues after checkpoint",
			firstEvent: &eventProcessorTestEvent{
				id:           "1",
				token:        []byte("1"),
				unmarshalErr: errors.New("invalid event"),
			},
			wantHandled:   []string{"2"},
			wantMarkCalls: []string{"1", "2"},
			wantMarked:    []string{"1", "2"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			watcher := newEventProcessorTestWatcher(test.firstEvent, newEventProcessorTestEvent("2"))
			watcher.markErrors = test.markErrors
			processor := newDefaultEventProcessorForTest(watcher, test.config)

			var handled []string
			processor.SetEventHandler(EventHandlerFunc(
				func(_ context.Context, event *model.HealthEventWithStatus) error {
					eventID := event.HealthEvent.GetId()
					handled = append(handled, eventID)

					return test.handlerErrors[eventID]
				},
			))

			err := processor.processEvents(context.Background())
			if len(test.wantErrors) == 0 {
				require.NoError(t, err)
			} else {
				for _, wantErr := range test.wantErrors {
					require.ErrorIs(t, err, wantErr)
				}
			}
			if test.wantErrorText != "" {
				require.ErrorContains(t, err, test.wantErrorText)
			}

			require.Equal(t, test.wantHandled, handled)
			require.Equal(t, test.wantMarkCalls, watcher.markCalls)
			require.Equal(t, test.wantMarked, watcher.markedTokens)
		})
	}
}

func newDefaultEventProcessorForTest(
	watcher ChangeStreamWatcher, config EventProcessorConfig,
) *DefaultEventProcessor {
	return NewEventProcessor(watcher, nil, config).(*DefaultEventProcessor)
}

type eventProcessorTestEvent struct {
	id            string
	token         []byte
	unmarshalErr  error
	documentIDErr error
}

func newEventProcessorTestEvent(id string) *eventProcessorTestEvent {
	return &eventProcessorTestEvent{id: id, token: []byte(id)}
}

func (e *eventProcessorTestEvent) GetDocumentID() (string, error) {
	return e.id, e.documentIDErr
}

func (e *eventProcessorTestEvent) GetRecordUUID() (string, error) {
	return "", nil
}

func (e *eventProcessorTestEvent) GetNodeName() (string, error) {
	return "", nil
}

func (e *eventProcessorTestEvent) GetResumeToken() []byte {
	return e.token
}

func (e *eventProcessorTestEvent) UnmarshalDocument(value any) error {
	if e.unmarshalErr != nil {
		return e.unmarshalErr
	}

	event, ok := value.(*model.HealthEventWithStatus)
	if !ok {
		return fmt.Errorf("unexpected document type %T", value)
	}

	event.HealthEvent = &protos.HealthEvent{Id: e.id}

	return nil
}

type eventProcessorTestWatcher struct {
	events       chan Event
	markErrors   map[string]error
	markCalls    []string
	markedTokens []string
}

func newEventProcessorTestWatcher(events ...Event) *eventProcessorTestWatcher {
	eventChannel := make(chan Event, len(events))
	for _, event := range events {
		eventChannel <- event
	}
	close(eventChannel)

	return &eventProcessorTestWatcher{
		events:     eventChannel,
		markErrors: make(map[string]error),
	}
}

func (w *eventProcessorTestWatcher) Start(context.Context) {}

func (w *eventProcessorTestWatcher) Events() <-chan Event {
	return w.events
}

func (w *eventProcessorTestWatcher) MarkProcessed(_ context.Context, token []byte) error {
	tokenString := string(token)
	w.markCalls = append(w.markCalls, tokenString)
	if err := w.markErrors[tokenString]; err != nil {
		return err
	}

	w.markedTokens = append(w.markedTokens, tokenString)

	return nil
}

func (w *eventProcessorTestWatcher) Close(context.Context) error {
	return nil
}

type updatedFieldsTestEvent struct {
	updated       map[string]any
	unmarshalCall bool
}

func (*updatedFieldsTestEvent) GetDocumentID() (string, error)  { return "1", nil }
func (*updatedFieldsTestEvent) GetRecordUUID() (string, error)  { return "event-1", nil }
func (*updatedFieldsTestEvent) GetNodeName() (string, error)    { return "node-a", nil }
func (*updatedFieldsTestEvent) GetResumeToken() []byte          { return []byte("token") }
func (e *updatedFieldsTestEvent) UpdatedFields() map[string]any { return e.updated }
func (e *updatedFieldsTestEvent) UnmarshalDocument(any) error {
	e.unmarshalCall = true

	return errors.New("completion-only update must not be decoded")
}

// TestDefaultEventProcessor_HandleCompletionOnlyUpdate_SkipsDecodingAndCheckpoints
// verifies that internal completion writes do not re-enter analyzer handling.
func TestDefaultEventProcessor_HandleCompletionOnlyUpdate_SkipsDecodingAndCheckpoints(t *testing.T) {
	event := &updatedFieldsTestEvent{updated: map[string]any{
		healthstatus.FaultQuarantineRecoveryPath: "completed",
	}}
	watcher := &eventProcessorTestWatcher{markErrors: make(map[string]error)}
	processor := &DefaultEventProcessor{
		changeStreamWatcher: watcher,
		config: EventProcessorConfig{SkipEvent: func(event Event) bool {
			return EventUpdatesOnly(event, healthstatus.FaultQuarantineRecoveryPath)
		}},
	}

	require.NoError(t, processor.handleSingleEvent(context.Background(), event))
	require.False(t, event.unmarshalCall)
	require.Equal(t, []string{"token"}, watcher.markedTokens)
	require.False(t, EventUpdatesOnly(&updatedFieldsTestEvent{updated: map[string]any{
		healthstatus.FaultQuarantineRecoveryPath: "completed",
		"healtheventstatus.nodequarantined":      "Quarantined",
	}}, healthstatus.FaultQuarantineRecoveryPath))
}
