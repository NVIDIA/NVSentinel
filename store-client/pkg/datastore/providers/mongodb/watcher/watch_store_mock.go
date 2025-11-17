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

package watcher

import (
	"context"
	"sync"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// FakeChangeStreamWatcher provides a fake implementation of the ChangeStreamWatcher
// for testing purposes. It allows customization of behavior through function fields.
type FakeChangeStreamWatcher struct {
	// EventsChan is the buffered channel (buffer size 10) that Events() returns
	EventsChan chan Event

	// Function fields allow customization of mock behavior
	StartFn                    func(ctx context.Context)
	MarkProcessedFn            func(ctx context.Context, token []byte) error
	CloseFn                    func(ctx context.Context) error
	GetUnprocessedEventCountFn func(ctx context.Context, lastProcessedID primitive.ObjectID,
		additionalFilters ...bson.M) (int64, error)

	// Call tracking fields
	StartCalled                    int
	MarkProcessedCalled            int
	CloseCalled                    int
	GetUnprocessedEventCountCalled int

	// Parameter tracking for verification in tests
	LastMarkProcessedCtx                context.Context
	LastCloseCtx                        context.Context
	LastGetUnprocessedEventCountCtx     context.Context
	LastGetUnprocessedEventCountID      primitive.ObjectID
	LastGetUnprocessedEventCountFilters []bson.M

	mu sync.Mutex
}

// NewFakeChangeStreamWatcher creates a new FakeChangeStreamWatcher with default behavior.
// The default behavior is safe and suitable for most tests.
func NewFakeChangeStreamWatcher() *FakeChangeStreamWatcher {
	return &FakeChangeStreamWatcher{
		EventsChan: make(chan Event, 10),
		StartFn: func(ctx context.Context) {
			// Default: no-op
		},
		MarkProcessedFn: func(ctx context.Context, token []byte) error {
			// Default: succeed
			return nil
		},
		CloseFn: func(ctx context.Context) error {
			// Default: succeed
			return nil
		},
		GetUnprocessedEventCountFn: func(ctx context.Context, lastProcessedID primitive.ObjectID,
			additionalFilters ...bson.M) (int64, error) {
			// Default: return 0 events
			return 0, nil
		},
	}
}

// Events returns the read-only events channel.
func (m *FakeChangeStreamWatcher) Events() <-chan Event {
	return m.EventsChan
}

// Start executes the configured Start function and tracks the call.
func (m *FakeChangeStreamWatcher) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.StartCalled++
	if m.StartFn != nil {
		m.StartFn(ctx)
	}
}

// MarkProcessed executes the configured MarkProcessed function and tracks the call.
func (m *FakeChangeStreamWatcher) MarkProcessed(ctx context.Context, token []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.MarkProcessedCalled++
	m.LastMarkProcessedCtx = ctx

	if m.MarkProcessedFn != nil {
		return m.MarkProcessedFn(ctx, token)
	}

	return nil
}

// Close executes the configured Close function and tracks the call.
func (m *FakeChangeStreamWatcher) Close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.CloseCalled++
	m.LastCloseCtx = ctx

	if m.CloseFn != nil {
		return m.CloseFn(ctx)
	}

	return nil
}

// GetUnprocessedEventCount executes the configured GetUnprocessedEventCount function and tracks the call.
func (m *FakeChangeStreamWatcher) GetUnprocessedEventCount(
	ctx context.Context,
	lastProcessedID primitive.ObjectID,
	additionalFilters ...bson.M,
) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.GetUnprocessedEventCountCalled++
	m.LastGetUnprocessedEventCountCtx = ctx
	m.LastGetUnprocessedEventCountID = lastProcessedID
	m.LastGetUnprocessedEventCountFilters = additionalFilters

	if m.GetUnprocessedEventCountFn != nil {
		return m.GetUnprocessedEventCountFn(ctx, lastProcessedID, additionalFilters...)
	}

	return 0, nil
}

// Reset clears all call counters, tracked parameters, and drains any pending events from EventsChan.
// This is useful when reusing the same fake across multiple test cases.
func (m *FakeChangeStreamWatcher) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.StartCalled = 0
	m.MarkProcessedCalled = 0
	m.CloseCalled = 0
	m.GetUnprocessedEventCountCalled = 0

	m.LastMarkProcessedCtx = nil
	m.LastCloseCtx = nil
	m.LastGetUnprocessedEventCountCtx = nil
	m.LastGetUnprocessedEventCountID = primitive.ObjectID{}
	m.LastGetUnprocessedEventCountFilters = nil

	for len(m.EventsChan) > 0 {
		<-m.EventsChan
	}
}

// GetCallCounts returns the current call counts for all methods in a thread-safe manner.
func (m *FakeChangeStreamWatcher) GetCallCounts() (start, markProcessed, close, getUnprocessed int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.StartCalled, m.MarkProcessedCalled, m.CloseCalled, m.GetUnprocessedEventCountCalled
}
