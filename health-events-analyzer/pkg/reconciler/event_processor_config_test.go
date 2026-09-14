// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

// TestNewEventProcessorConfig_HandlerFailure_PreservesRecoveryReplay exercises
// the production configuration against the real processor and two ordered events.
func TestNewEventProcessorConfig_HandlerFailure_PreservesRecoveryReplay(t *testing.T) {
	tests := []struct {
		name     string
		rules    *config.TomlConfig
		wantStop bool
	}{
		{name: "nil rules"},
		{name: "empty rules", rules: &config.TomlConfig{}},
		{name: "ordinary enabled rule", rules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{
			{EvaluateRule: true},
		}}},
		{name: "disabled recovery rule", rules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{
			{Recovery: &config.RecoveryMapping{}},
		}}},
		{name: "enabled recovery shares ordered stream", wantStop: true,
			rules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{
				{EvaluateRule: true},
				{EvaluateRule: true, Recovery: &config.RecoveryMapping{}},
			}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			watcher := &processorConfigTestWatcher{events: make(chan client.Event, 2)}
			watcher.events <- processorConfigTestEvent("failed-transition")
			watcher.events <- processorConfigTestEvent("later-source")
			close(watcher.events)

			processor := client.NewEventProcessor(watcher, nil, newEventProcessorConfig(test.rules))
			transientErr := fmt.Errorf("derived transition was not persisted")

			var handled []string
			processor.SetEventHandler(client.EventHandlerFunc(func(_ context.Context, event *model.HealthEventWithStatus) error {
				id := event.HealthEvent.GetId()
				handled = append(handled, id)
				if id == "failed-transition" {
					return transientErr
				}

				return nil
			}))

			err := processor.Start(context.Background())
			if test.wantStop {
				require.ErrorIs(t, err, transientErr)
				require.Equal(t, []string{"failed-transition"}, handled)
				require.Empty(t, watcher.marked)
				require.Len(t, watcher.events, 1)
			} else {
				require.NoError(t, err)
				require.Equal(t, []string{"failed-transition", "later-source"}, handled)
				require.Equal(t, handled, watcher.marked)
			}
		})
	}
}

type processorConfigTestWatcher struct {
	events chan client.Event
	marked []string
}

func (w *processorConfigTestWatcher) Start(context.Context)       {}
func (w *processorConfigTestWatcher) Events() <-chan client.Event { return w.events }
func (w *processorConfigTestWatcher) Close(context.Context) error { return nil }
func (w *processorConfigTestWatcher) MarkProcessed(_ context.Context, token []byte) error {
	w.marked = append(w.marked, string(token))

	return nil
}

type processorConfigTestEvent string

func (e processorConfigTestEvent) GetDocumentID() (string, error) { return string(e), nil }
func (e processorConfigTestEvent) GetRecordUUID() (string, error) { return string(e), nil }
func (e processorConfigTestEvent) GetNodeName() (string, error)   { return "node-1", nil }
func (e processorConfigTestEvent) GetResumeToken() []byte         { return []byte(e) }
func (e processorConfigTestEvent) UnmarshalDocument(value any) error {
	event, ok := value.(*model.HealthEventWithStatus)
	if !ok {
		return fmt.Errorf("unexpected document type %T", value)
	}

	event.HealthEvent = &protos.HealthEvent{Id: string(e)}

	return nil
}
