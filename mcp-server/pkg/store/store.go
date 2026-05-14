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

// Package store exposes a narrow read-only view of NVSentinel's HealthEventStore.
// MCP tools depend on the Reader interface; production wires it to
// DataStoreReader (a thin wrapper around store-client) and tests wire it to
// FakeReader. Tools never depend on store-client directly.
package store

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// ErrNotFound is returned by Reader implementations when a lookup has no match.
// Callers should detect this with errors.Is(err, ErrNotFound) and translate
// it to whatever tool-level error envelope is appropriate.
var ErrNotFound = errors.New("store: not found")

// Reader is the read-only view of NVSentinel's HealthEventStore used by MCP tools.
//
// The interface deliberately omits every write method on store-client's
// HealthEventStore so that compile-time visibility constrains tools to safe
// operations even if someone were to type-assert to the underlying store.
type Reader interface {
	// EventsByNode returns all health events for the given node, oldest first
	// (the underlying store guarantees insertion order; callers that need a
	// different order should sort the returned slice). The returned slice is
	// safe for the caller to retain and mutate.
	EventsByNode(ctx context.Context, nodeName string) ([]datastore.HealthEventWithStatus, error)

	// LatestEventForNode returns the most recent event for the given node, or
	// ErrNotFound when the node has no events in the store.
	LatestEventForNode(ctx context.Context, nodeName string) (*datastore.HealthEventWithStatus, error)

	// EventsByQuery executes the database-agnostic QueryBuilder and returns
	// the matching events. Use this for entity-, error-code-, or time-range
	// filtered reads.
	EventsByQuery(ctx context.Context, builder datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error)
}

// DataStoreReader implements Reader by delegating to a concrete
// datastore.HealthEventStore (Mongo or Postgres, chosen at startup via
// store-client's provider registry).
type DataStoreReader struct {
	store datastore.HealthEventStore
}

// NewDataStoreReader wraps the given HealthEventStore as a Reader.
func NewDataStoreReader(store datastore.HealthEventStore) *DataStoreReader {
	return &DataStoreReader{store: store}
}

// EventsByNode implements Reader.EventsByNode.
func (d *DataStoreReader) EventsByNode(ctx context.Context, nodeName string) ([]datastore.HealthEventWithStatus, error) {
	events, err := d.store.FindHealthEventsByNode(ctx, nodeName)
	if err != nil {
		return nil, fmt.Errorf("store: events by node %q: %w", nodeName, err)
	}

	return events, nil
}

// LatestEventForNode implements Reader.LatestEventForNode. A nil result from
// the underlying store is translated to ErrNotFound.
func (d *DataStoreReader) LatestEventForNode(ctx context.Context, nodeName string) (*datastore.HealthEventWithStatus, error) {
	event, err := d.store.FindLatestEventForNode(ctx, nodeName)
	if err != nil {
		return nil, fmt.Errorf("store: latest event for node %q: %w", nodeName, err)
	}

	if event == nil {
		return nil, ErrNotFound
	}

	return event, nil
}

// EventsByQuery implements Reader.EventsByQuery.
func (d *DataStoreReader) EventsByQuery(ctx context.Context, builder datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
	events, err := d.store.FindHealthEventsByQuery(ctx, builder)
	if err != nil {
		return nil, fmt.Errorf("store: events by query: %w", err)
	}

	return events, nil
}

// FakeReader is an in-memory Reader for unit tests. It is intentionally
// retained in the production package (rather than a separate testutils
// subpackage) so MCP tool tests can import it through one short path. It is
// not safe to use in production: it has no persistence and no real query
// execution.
type FakeReader struct {
	mu               sync.Mutex
	byNode           map[string][]datastore.HealthEventWithStatus
	nextQueryResult  []datastore.HealthEventWithStatus
	receivedBuilders []datastore.QueryBuilder
}

// NewFakeReader returns an empty FakeReader. Use SeedNodeEvents and
// SetNextQueryResult to prime it for a test case.
func NewFakeReader() *FakeReader {
	return &FakeReader{
		byNode: make(map[string][]datastore.HealthEventWithStatus),
	}
}

// SeedNodeEvents appends events to the given node's in-memory event list.
// Insertion order is preserved by EventsByNode; LatestEventForNode sorts by
// CreatedAt so callers may seed out of order.
func (f *FakeReader) SeedNodeEvents(node string, events ...datastore.HealthEventWithStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byNode[node] = append(f.byNode[node], events...)
}

// SetNextQueryResult primes the response that EventsByQuery returns on the
// next (and every subsequent) call until SetNextQueryResult is called again.
func (f *FakeReader) SetNextQueryResult(events ...datastore.HealthEventWithStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextQueryResult = append([]datastore.HealthEventWithStatus{}, events...)
}

// ReceivedQueryBuilders returns the QueryBuilders that EventsByQuery has been
// called with, in call order. Tests use this to assert the tool constructed
// the right query.
func (f *FakeReader) ReceivedQueryBuilders() []datastore.QueryBuilder {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]datastore.QueryBuilder{}, f.receivedBuilders...)
}

// EventsByNode implements Reader.EventsByNode against the in-memory map.
func (f *FakeReader) EventsByNode(_ context.Context, node string) ([]datastore.HealthEventWithStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]datastore.HealthEventWithStatus{}, f.byNode[node]...), nil
}

// LatestEventForNode implements Reader.LatestEventForNode. Returns ErrNotFound
// when the node has no seeded events.
func (f *FakeReader) LatestEventForNode(_ context.Context, node string) (*datastore.HealthEventWithStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	events := f.byNode[node]
	if len(events) == 0 {
		return nil, ErrNotFound
	}

	latest := events[0]
	for i := 1; i < len(events); i++ {
		if events[i].CreatedAt.After(latest.CreatedAt) {
			latest = events[i]
		}
	}

	return &latest, nil
}

// EventsByQuery implements Reader.EventsByQuery. It records the received
// builder for test assertions and returns the result primed by
// SetNextQueryResult.
func (f *FakeReader) EventsByQuery(_ context.Context, builder datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.receivedBuilders = append(f.receivedBuilders, builder)

	return append([]datastore.HealthEventWithStatus{}, f.nextQueryResult...), nil
}
