// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package mongodb

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

// fakeClientEvent implements client.Event for adapter tests.
type fakeClientEvent struct {
	doc   map[string]any
	token []byte
}

func (f *fakeClientEvent) GetDocumentID() (string, error) { return "doc-id", nil }
func (f *fakeClientEvent) GetRecordUUID() (string, error) { return "doc-id", nil }
func (f *fakeClientEvent) GetNodeName() (string, error)   { return "node", nil }
func (f *fakeClientEvent) GetResumeToken() []byte         { return f.token }

func (f *fakeClientEvent) UnmarshalDocument(v any) error {
	target, ok := v.(*map[string]any)
	if !ok {
		return fmt.Errorf("unsupported target type %T", v)
	}

	*target = f.doc

	return nil
}

// fakeClientWatcher implements client.ChangeStreamWatcher for adapter tests.
type fakeClientWatcher struct {
	events chan client.Event
	marked [][]byte
}

func (f *fakeClientWatcher) Start(ctx context.Context)       {}
func (f *fakeClientWatcher) Events() <-chan client.Event     { return f.events }
func (f *fakeClientWatcher) Close(ctx context.Context) error { return nil }

func (f *fakeClientWatcher) MarkProcessed(ctx context.Context, token []byte) error {
	f.marked = append(f.marked, token)
	return nil
}

func TestAdaptedChangeStreamWatcher_EventsCarryResumeToken(t *testing.T) {
	token := []byte("per-event-resume-token")
	doc := map[string]any{"_id": "abc123", "healthevent": map[string]any{"nodename": "node-1"}}

	watcher := &fakeClientWatcher{events: make(chan client.Event, 1)}
	watcher.events <- &fakeClientEvent{doc: doc, token: token}
	close(watcher.events)

	adapted := NewAdaptedChangeStreamWatcher(watcher)

	select {
	case eventWithToken, ok := <-adapted.Events():
		require.True(t, ok, "expected an adapted event before channel close")
		assert.Equal(t, token, eventWithToken.ResumeToken,
			"adapter must propagate the per-event resume token so consumers can checkpoint")
		assert.Equal(t, doc, map[string]any(eventWithToken.Event))
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for adapted event")
	}
}

func TestAdaptedChangeStreamWatcher_MarkProcessedDelegatesToken(t *testing.T) {
	token := []byte("token-to-persist")
	watcher := &fakeClientWatcher{events: make(chan client.Event)}

	adapted := NewAdaptedChangeStreamWatcher(watcher)

	require.NoError(t, adapted.MarkProcessed(context.Background(), token))
	require.Len(t, watcher.marked, 1)
	assert.Equal(t, token, watcher.marked[0])
}
