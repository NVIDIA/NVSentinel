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

package client

import (
	"context"
	"errors"
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
