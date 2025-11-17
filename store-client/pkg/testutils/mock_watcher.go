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

package testutils

import (
	"context"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

// MockChangeStreamWatcher provides a database-agnostic mock for testing
type MockChangeStreamWatcher struct {
	EventsChan chan client.Event
}

// NewMockChangeStreamWatcher creates a new mock watcher
func NewMockChangeStreamWatcher() *MockChangeStreamWatcher {
	return &MockChangeStreamWatcher{
		EventsChan: make(chan client.Event, 100),
	}
}

func (m *MockChangeStreamWatcher) Start(ctx context.Context) {
	// Mock implementation - no-op
}

func (m *MockChangeStreamWatcher) Events() <-chan client.Event {
	return m.EventsChan
}

func (m *MockChangeStreamWatcher) MarkProcessed(ctx context.Context, token []byte) error {
	return nil
}

// GetUnprocessedEventCount implements optional ChangeStreamMetrics interface
func (m *MockChangeStreamWatcher) GetUnprocessedEventCount(ctx context.Context, lastProcessedID string) (int64, error) {
	return 0, nil
}

func (m *MockChangeStreamWatcher) Close(ctx context.Context) error {
	close(m.EventsChan)
	return nil
}
