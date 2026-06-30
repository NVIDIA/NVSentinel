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

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// Compile-time assertions that the production wrapper and the in-memory fake
// both satisfy the Reader interface. If either drifts, the package fails to
// build before any test runs.
var (
	_ Reader = (*DataStoreReader)(nil)
	_ Reader = (*FakeReader)(nil)
)

func TestFakeReader_EventsByNode_ReturnsSeededEvents(t *testing.T) {
	ctx := context.Background()
	r := NewFakeReader()

	seedTime := time.Date(2026, time.May, 14, 9, 0, 0, 0, time.UTC)
	r.SeedNodeEvents("gpu-node-1",
		datastore.HealthEventWithStatus{CreatedAt: seedTime, RawEvent: datastore.Event{"checkName": "xid"}},
		datastore.HealthEventWithStatus{CreatedAt: seedTime.Add(time.Minute), RawEvent: datastore.Event{"checkName": "dcgm"}},
	)

	got, err := r.EventsByNode(ctx, "gpu-node-1")
	if err != nil {
		t.Fatalf("EventsByNode unexpected error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d", len(got))
	}

	if got[0].RawEvent["checkName"] != "xid" {
		t.Errorf("first event checkName = %v, want xid", got[0].RawEvent["checkName"])
	}

	if got[1].RawEvent["checkName"] != "dcgm" {
		t.Errorf("second event checkName = %v, want dcgm", got[1].RawEvent["checkName"])
	}
}

func TestFakeReader_EventsByNode_UnknownNodeReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	r := NewFakeReader()

	got, err := r.EventsByNode(ctx, "missing-node")
	if err != nil {
		t.Fatalf("unexpected error for unknown node: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("want empty slice for missing node, got %d events", len(got))
	}
}

func TestFakeReader_LatestEventForNode_UnknownNodeReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	r := NewFakeReader()

	_, err := r.LatestEventForNode(ctx, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestFakeReader_LatestEventForNode_ReturnsByCreatedAt(t *testing.T) {
	ctx := context.Background()
	r := NewFakeReader()

	older := datastore.HealthEventWithStatus{CreatedAt: time.Date(2026, time.May, 14, 9, 0, 0, 0, time.UTC)}
	newer := datastore.HealthEventWithStatus{CreatedAt: time.Date(2026, time.May, 14, 10, 0, 0, 0, time.UTC)}
	// Seed out of order to verify the fake sorts by CreatedAt, not insertion order.
	r.SeedNodeEvents("node-1", newer, older)

	got, err := r.LatestEventForNode(ctx, "node-1")
	if err != nil {
		t.Fatalf("LatestEventForNode: %v", err)
	}

	if !got.CreatedAt.Equal(newer.CreatedAt) {
		t.Errorf("want latest event at %v, got %v", newer.CreatedAt, got.CreatedAt)
	}
}

func TestFakeReader_EventsByQuery_ReturnsSeededResultAndRecordsBuilder(t *testing.T) {
	ctx := context.Background()
	r := NewFakeReader()

	expected := []datastore.HealthEventWithStatus{
		{RawEvent: datastore.Event{"id": "abc"}},
	}
	r.SetNextQueryResult(expected...)

	builder := stubQueryBuilder{tag: "test-query"}

	got, err := r.EventsByQuery(ctx, builder)
	if err != nil {
		t.Fatalf("EventsByQuery: %v", err)
	}

	if len(got) != 1 || got[0].RawEvent["id"] != "abc" {
		t.Errorf("query response not what was seeded; got %+v", got)
	}

	received := r.ReceivedQueryBuilders()
	if len(received) != 1 {
		t.Fatalf("want 1 recorded builder, got %d", len(received))
	}

	sb, ok := received[0].(stubQueryBuilder)
	if !ok || sb.tag != "test-query" {
		t.Errorf("recorded builder wasn't the one we passed; got %T %+v", received[0], received[0])
	}
}

// stubQueryBuilder is a minimal datastore.QueryBuilder used only to verify
// that FakeReader records the builder the caller passed in.
type stubQueryBuilder struct {
	tag string
}

func (s stubQueryBuilder) ToMongo() map[string]interface{} {
	return map[string]interface{}{"tag": s.tag}
}

func (s stubQueryBuilder) ToSQL() (string, []interface{}) {
	return "tag = ?", []interface{}{s.tag}
}
