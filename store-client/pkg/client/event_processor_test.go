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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

type markingWatcherStub struct {
	marked int
}

func (*markingWatcherStub) Start(context.Context) {}
func (*markingWatcherStub) Events() <-chan Event  { return nil }
func (s *markingWatcherStub) MarkProcessed(context.Context, []byte) error {
	s.marked++

	return nil
}
func (*markingWatcherStub) Close(context.Context) error { return nil }

func TestDefaultEventProcessorSkipsCompletionOnlyUpdate(t *testing.T) {
	const completionPath = "healtheventstatus.faultquarantinerecovery"
	event := &updatedFieldsTestEvent{updated: map[string]any{completionPath: "completed"}}
	watcher := &markingWatcherStub{}
	processor := &DefaultEventProcessor{
		changeStreamWatcher: watcher,
		config: EventProcessorConfig{SkipEvent: func(event Event) bool {
			return EventUpdatesOnly(event, completionPath)
		}},
	}

	require.NoError(t, processor.handleSingleEvent(context.Background(), event))
	assert.False(t, event.unmarshalCall)
	assert.Equal(t, 1, watcher.marked)
	assert.False(t, EventUpdatesOnly(&updatedFieldsTestEvent{updated: map[string]any{
		completionPath:                      "completed",
		"healtheventstatus.nodequarantined": "Quarantined",
	}}, completionPath))
}
