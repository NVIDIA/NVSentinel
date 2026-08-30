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

type processorTestEvent struct {
	document datamodels.HealthEventWithStatus
	token    []byte
}

func (e *processorTestEvent) GetDocumentID() (string, error) { return "event-1", nil }
func (e *processorTestEvent) GetRecordUUID() (string, error) { return "event-1", nil }
func (e *processorTestEvent) GetNodeName() (string, error)   { return "node-a", nil }
func (e *processorTestEvent) GetResumeToken() []byte         { return e.token }

func (e *processorTestEvent) UnmarshalDocument(value any) error {
	target := value.(*datamodels.HealthEventWithStatus)
	*target = e.document

	return nil
}

type processorTestWatcher struct {
	marked chan []byte
}

func (w *processorTestWatcher) Start(context.Context)       {}
func (w *processorTestWatcher) Events() <-chan Event        { return nil }
func (w *processorTestWatcher) Close(context.Context) error { return nil }
func (w *processorTestWatcher) MarkProcessed(_ context.Context, token []byte) error {
	w.marked <- append([]byte(nil), token...)
	return nil
}

func TestEventProcessorMarksResumeTokenOnlyAfterHandlerCompletes(t *testing.T) {
	watcher := &processorTestWatcher{marked: make(chan []byte, 1)}
	processor := NewEventProcessor(watcher, nil, EventProcessorConfig{}).(*DefaultEventProcessor)
	entered := make(chan struct{})
	release := make(chan struct{})
	processor.SetEventHandler(EventHandlerFunc(func(context.Context, *datamodels.HealthEventWithStatus) error {
		close(entered)
		<-release

		return nil
	}))

	event := &processorTestEvent{
		document: datamodels.HealthEventWithStatus{
			HealthEvent:       &protos.HealthEvent{NodeName: "node-a"},
			HealthEventStatus: &protos.HealthEventStatus{},
		},
		token: []byte("resume-7"),
	}
	done := make(chan error, 1)
	go func() {
		done <- processor.handleSingleEvent(context.Background(), event)
	}()

	<-entered
	select {
	case token := <-watcher.marked:
		t.Fatalf("resume token marked before handler completed: %q", token)
	default:
	}

	close(release)
	require.NoError(t, <-done)
	require.Equal(t, []byte("resume-7"), <-watcher.marked)
}

func TestEventProcessorDoesNotMarkResumeTokenWhenHandlerFails(t *testing.T) {
	watcher := &processorTestWatcher{marked: make(chan []byte, 1)}
	processor := NewEventProcessor(watcher, nil, EventProcessorConfig{
		MarkProcessedOnError: false,
	}).(*DefaultEventProcessor)
	processor.SetEventHandler(EventHandlerFunc(func(context.Context, *datamodels.HealthEventWithStatus) error {
		return errors.New("not durable")
	}))

	event := &processorTestEvent{
		document: datamodels.HealthEventWithStatus{
			HealthEvent:       &protos.HealthEvent{NodeName: "node-a"},
			HealthEventStatus: &protos.HealthEventStatus{},
		},
		token: []byte("resume-8"),
	}

	require.Error(t, processor.handleSingleEvent(context.Background(), event))
	select {
	case token := <-watcher.marked:
		t.Fatalf("resume token marked after handler failure: %q", token)
	default:
	}
}

func TestEventProcessorStopsBeforeLaterUncheckpointedEvent(t *testing.T) {
	processingErr := errors.New("processing failed")
	checkpointErr := errors.New("checkpoint failed")

	for _, test := range []struct {
		name          string
		config        EventProcessorConfig
		first         *orderedProcessorEvent
		handlerErrors map[string]error
		markErrors    map[string]error
		wantErrors    []error
		wantHandled   []string
		wantMarked    []string
	}{
		{
			name:          "handler failure",
			first:         newOrderedProcessorEvent("1"),
			handlerErrors: map[string]error{"1": processingErr},
			wantErrors:    []error{processingErr},
			wantHandled:   []string{"1"},
		},
		{
			name:       "checkpoint failure",
			first:      newOrderedProcessorEvent("1"),
			markErrors: map[string]error{"1": checkpointErr},
			wantErrors: []error{checkpointErr},
			wantHandled: []string{
				"1",
			},
		},
		{
			name:          "configured skip continues after checkpoint",
			config:        EventProcessorConfig{MarkProcessedOnError: true},
			first:         newOrderedProcessorEvent("1"),
			handlerErrors: map[string]error{"1": processingErr},
			wantHandled:   []string{"1", "2"},
			wantMarked:    []string{"1", "2"},
		},
		{
			name:  "invalid document continues after checkpoint",
			first: &orderedProcessorEvent{id: "1", token: []byte("1"), unmarshalErr: processingErr},
			wantHandled: []string{
				"2",
			},
			wantMarked: []string{"1", "2"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			watcher := newOrderedProcessorWatcher(test.first, newOrderedProcessorEvent("2"))
			watcher.markErrors = test.markErrors
			processor := NewEventProcessor(watcher, nil, test.config).(*DefaultEventProcessor)
			var handled []string
			processor.SetEventHandler(EventHandlerFunc(
				func(_ context.Context, event *datamodels.HealthEventWithStatus) error {
					id := event.HealthEvent.GetId()
					handled = append(handled, id)
					return test.handlerErrors[id]
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
			require.Equal(t, test.wantHandled, handled)
			require.Equal(t, test.wantMarked, watcher.marked)
		})
	}
}

type orderedProcessorEvent struct {
	id            string
	token         []byte
	unmarshalErr  error
	documentIDErr error
}

func newOrderedProcessorEvent(id string) *orderedProcessorEvent {
	return &orderedProcessorEvent{id: id, token: []byte(id)}
}

func (e *orderedProcessorEvent) GetDocumentID() (string, error) { return e.id, e.documentIDErr }
func (e *orderedProcessorEvent) GetRecordUUID() (string, error) { return "", nil }
func (e *orderedProcessorEvent) GetNodeName() (string, error)   { return "", nil }
func (e *orderedProcessorEvent) GetResumeToken() []byte         { return e.token }
func (e *orderedProcessorEvent) UnmarshalDocument(value any) error {
	if e.unmarshalErr != nil {
		return e.unmarshalErr
	}

	event, ok := value.(*datamodels.HealthEventWithStatus)
	if !ok {
		return fmt.Errorf("unexpected document type %T", value)
	}
	event.HealthEvent = &protos.HealthEvent{Id: e.id}

	return nil
}

type orderedProcessorWatcher struct {
	events     chan Event
	markErrors map[string]error
	marked     []string
}

func newOrderedProcessorWatcher(events ...Event) *orderedProcessorWatcher {
	eventChannel := make(chan Event, len(events))
	for _, event := range events {
		eventChannel <- event
	}
	close(eventChannel)

	return &orderedProcessorWatcher{events: eventChannel, markErrors: make(map[string]error)}
}

func (w *orderedProcessorWatcher) Start(context.Context)       {}
func (w *orderedProcessorWatcher) Events() <-chan Event        { return w.events }
func (w *orderedProcessorWatcher) Close(context.Context) error { return nil }
func (w *orderedProcessorWatcher) MarkProcessed(_ context.Context, token []byte) error {
	value := string(token)
	if err := w.markErrors[value]; err != nil {
		return err
	}
	w.marked = append(w.marked, value)

	return nil
}
