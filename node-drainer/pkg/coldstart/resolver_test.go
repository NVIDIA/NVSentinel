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

package coldstart

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// fakeSessionEndFinder is a test double for sessionEndFinder that returns a canned set
// of events (or an error) and records how many times it was queried.
type fakeSessionEndFinder struct {
	events    []datastore.HealthEventWithStatus
	err       error
	callCount int
}

// FindLatestHealthEventByQuery mimics the datastore's server-side sort+limit by
// returning the configured event with the newest non-zero CreatedAt (or nil).
func (f *fakeSessionEndFinder) FindLatestHealthEventByQuery(
	_ context.Context, _ datastore.QueryBuilder,
) (*datastore.HealthEventWithStatus, error) {
	f.callCount++

	if f.err != nil {
		return nil, f.err
	}

	var latest *datastore.HealthEventWithStatus

	for i := range f.events {
		event := f.events[i]
		if event.CreatedAt.IsZero() {
			continue
		}

		if latest == nil || event.CreatedAt.After(latest.CreatedAt) {
			latest = &event
		}
	}

	return latest, nil
}

func sessionEndEvent(status model.Status, createdAt time.Time) datastore.HealthEventWithStatus {
	s := datastore.Status(status)

	return datastore.HealthEventWithStatus{
		CreatedAt: createdAt,
		HealthEventStatus: datastore.HealthEventStatus{
			NodeQuarantined: &s,
		},
	}
}

func TestQuarantineSessionEnded(t *testing.T) {
	base := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	quarantinedAt := base
	laterUnquarantine := base.Add(1 * time.Hour)
	earlierUnquarantine := base.Add(-1 * time.Hour)

	tests := []struct {
		name           string
		finder         *fakeSessionEndFinder
		eventCreatedAt time.Time
		wantEnded      bool
		wantErr        bool
	}{
		{
			name:           "no session-ending event means still active",
			finder:         &fakeSessionEndFinder{},
			eventCreatedAt: quarantinedAt,
			wantEnded:      false,
		},
		{
			name: "later unquarantine ends the session",
			finder: &fakeSessionEndFinder{events: []datastore.HealthEventWithStatus{
				sessionEndEvent(model.UnQuarantined, laterUnquarantine),
			}},
			eventCreatedAt: quarantinedAt,
			wantEnded:      true,
		},
		{
			name: "later cancelled ends the session",
			finder: &fakeSessionEndFinder{events: []datastore.HealthEventWithStatus{
				sessionEndEvent(model.Cancelled, laterUnquarantine),
			}},
			eventCreatedAt: quarantinedAt,
			wantEnded:      true,
		},
		{
			name: "earlier unquarantine belongs to a previous session and does not end this one",
			finder: &fakeSessionEndFinder{events: []datastore.HealthEventWithStatus{
				sessionEndEvent(model.UnQuarantined, earlierUnquarantine),
			}},
			eventCreatedAt: quarantinedAt,
			wantEnded:      false,
		},
		{
			name: "uses the newest session end when several exist",
			finder: &fakeSessionEndFinder{events: []datastore.HealthEventWithStatus{
				sessionEndEvent(model.UnQuarantined, earlierUnquarantine),
				sessionEndEvent(model.UnQuarantined, laterUnquarantine),
			}},
			eventCreatedAt: quarantinedAt,
			wantEnded:      true,
		},
		{
			name: "zero candidate timestamp cannot be ordered, so treated as active",
			finder: &fakeSessionEndFinder{events: []datastore.HealthEventWithStatus{
				sessionEndEvent(model.UnQuarantined, laterUnquarantine),
			}},
			eventCreatedAt: time.Time{},
			wantEnded:      false,
		},
		{
			name:           "lookup error is propagated",
			finder:         &fakeSessionEndFinder{err: errors.New("db down")},
			eventCreatedAt: quarantinedAt,
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := newQuarantineSessionResolver(tt.finder)

			ended, err := resolver.quarantineSessionEnded(context.Background(), "node-a", tt.eventCreatedAt)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantEnded, ended)
		})
	}
}

func TestQuarantineSessionEndedCachesPerNode(t *testing.T) {
	base := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	finder := &fakeSessionEndFinder{events: []datastore.HealthEventWithStatus{
		sessionEndEvent(model.UnQuarantined, base.Add(time.Hour)),
	}}

	resolver := newQuarantineSessionResolver(finder)

	for i := 0; i < 3; i++ {
		ended, err := resolver.quarantineSessionEnded(context.Background(), "node-a", base)
		require.NoError(t, err)
		assert.True(t, ended)
	}

	assert.Equal(t, 1, finder.callCount, "session end lookup should be cached per node")
}

func TestQuarantineSessionEndedZeroTimestampSkipsLookup(t *testing.T) {
	finder := &fakeSessionEndFinder{}
	resolver := newQuarantineSessionResolver(finder)

	ended, err := resolver.quarantineSessionEnded(context.Background(), "node-a", time.Time{})

	require.NoError(t, err)
	assert.False(t, ended)
	assert.Equal(t, 0, finder.callCount, "zero-timestamp candidates should not trigger a lookup")
}
