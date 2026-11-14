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

package drain

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func TestIsNodeDrained_UsesRecordedScope(t *testing.T) {
	t.Parallel()

	gpu0 := gpuEntity("GPU-0")
	gpu1 := gpuEntity("GPU-1")

	tests := []struct {
		name                string
		events              []*protos.HealthEvent
		stored              map[string]datastore.HealthEventWithStatus
		excludeEventID      string
		current             *protos.Entity
		partialDrainEnabled bool
		storeErr            error
		want                bool
		wantErr             string
	}{
		{
			name:                "recorded partial stays partial after partial drain is turned off",
			events:              []*protos.HealthEvent{{Id: "event-a"}},
			stored:              map[string]datastore.HealthEventWithStatus{"event-a": storedDrain("event-a", componentReset(gpu0), datastore.StatusSucceeded, recordedPartial("GPU-0"))},
			partialDrainEnabled: false,
			want:                false,
		},
		{
			name:                "recorded partial covers the same entity after partial drain is turned off",
			events:              []*protos.HealthEvent{{Id: "event-a"}},
			stored:              map[string]datastore.HealthEventWithStatus{"event-a": storedDrain("event-a", componentReset(gpu0), datastore.StatusSucceeded, recordedPartial("GPU-0"))},
			current:             gpu0,
			partialDrainEnabled: false,
			want:                true,
		},
		{
			name:                "recorded partial does not cover a different entity",
			events:              []*protos.HealthEvent{{Id: "event-a"}},
			stored:              map[string]datastore.HealthEventWithStatus{"event-a": storedDrain("event-a", componentReset(gpu0), datastore.StatusSucceeded, recordedPartial("GPU-0"))},
			current:             gpu1,
			partialDrainEnabled: true,
			want:                false,
		},
		{
			name:                "recorded full drain covers a later full drain",
			events:              []*protos.HealthEvent{{Id: "event-a"}},
			stored:              map[string]datastore.HealthEventWithStatus{"event-a": storedDrain("event-a", restartVM(), datastore.StatusSucceeded, recordedFull())},
			partialDrainEnabled: false,
			want:                true,
		},
		{
			name:                "recorded full drain covers a later partial drain",
			events:              []*protos.HealthEvent{{Id: "event-a"}},
			stored:              map[string]datastore.HealthEventWithStatus{"event-a": storedDrain("event-a", restartVM(), datastore.StatusSucceeded, recordedFull())},
			current:             gpu0,
			partialDrainEnabled: true,
			want:                true,
		},
		{
			name:                "no record and partial drain off derives a full drain",
			events:              []*protos.HealthEvent{{Id: "event-a"}},
			stored:              map[string]datastore.HealthEventWithStatus{"event-a": storedDrain("event-a", componentReset(gpu0), datastore.StatusSucceeded, nil)},
			partialDrainEnabled: false,
			want:                true,
		},
		{
			name:                "no record and partial drain on covers the same entity",
			events:              []*protos.HealthEvent{{Id: "event-a"}},
			stored:              map[string]datastore.HealthEventWithStatus{"event-a": storedDrain("event-a", componentReset(gpu0), datastore.StatusSucceeded, nil)},
			current:             gpu0,
			partialDrainEnabled: true,
			want:                true,
		},
		{
			name:                "no record and partial drain on does not cover a full drain",
			events:              []*protos.HealthEvent{{Id: "event-a"}},
			stored:              map[string]datastore.HealthEventWithStatus{"event-a": storedDrain("event-a", componentReset(gpu0), datastore.StatusSucceeded, nil)},
			partialDrainEnabled: true,
			want:                false,
		},
		{
			name:                "no record and partial drain on does not cover a different entity",
			events:              []*protos.HealthEvent{{Id: "event-a"}},
			stored:              map[string]datastore.HealthEventWithStatus{"event-a": storedDrain("event-a", componentReset(gpu0), datastore.StatusSucceeded, nil)},
			current:             gpu1,
			partialDrainEnabled: true,
			want:                false,
		},
		{
			name:                "already drained from drain skip does not cover a later drain",
			events:              []*protos.HealthEvent{{Id: "event-a"}},
			stored:              map[string]datastore.HealthEventWithStatus{"event-a": storedDrain("event-a", skippedEvent(), datastore.AlreadyDrained, nil)},
			partialDrainEnabled: false,
			want:                false,
		},
		{
			name:                "recorded scope wins over a different entity on the event",
			events:              []*protos.HealthEvent{{Id: "event-a"}},
			stored:              map[string]datastore.HealthEventWithStatus{"event-a": storedDrain("event-a", componentReset(gpu1), datastore.StatusSucceeded, recordedPartial("GPU-0"))},
			current:             gpu0,
			partialDrainEnabled: true,
			want:                true,
		},
		{
			name:   "a covered already-drained event keeps its partial scope in the chain",
			events: []*protos.HealthEvent{{Id: "event-a"}, {Id: "event-b"}},
			stored: map[string]datastore.HealthEventWithStatus{
				"event-a": storedDrain("event-a", componentReset(gpu0), datastore.StatusSucceeded, recordedPartial("GPU-0")),
				"event-b": storedDrain("event-b", componentReset(gpu0), datastore.AlreadyDrained, recordedPartial("GPU-0")),
			},
			partialDrainEnabled: false,
			want:                false,
		},
		{
			name:   "a covered already-drained event alone still covers the same entity",
			events: []*protos.HealthEvent{{Id: "event-b"}},
			stored: map[string]datastore.HealthEventWithStatus{
				"event-b": storedDrain("event-b", componentReset(gpu0), datastore.AlreadyDrained, recordedPartial("GPU-0")),
			},
			current:             gpu0,
			partialDrainEnabled: false,
			want:                true,
		},
		{
			name:                "the current event is not proof of its own drain",
			events:              []*protos.HealthEvent{{Id: "event-c"}},
			stored:              map[string]datastore.HealthEventWithStatus{"event-c": storedDrain("event-c", restartVM(), datastore.StatusSucceeded, recordedFull())},
			excludeEventID:      "event-c",
			partialDrainEnabled: false,
			want:                false,
		},
		{
			name:                "a store error is returned",
			events:              []*protos.HealthEvent{{Id: "event-a"}},
			storeErr:            errors.New("store down"),
			partialDrainEnabled: false,
			wantErr:             "store down",
		},
		{
			name:                "a component reset with no supported entity and no record is an error",
			events:              []*protos.HealthEvent{{Id: "event-a"}},
			stored:              map[string]datastore.HealthEventWithStatus{"event-a": storedDrain("event-a", componentReset(nil), datastore.StatusSucceeded, nil)},
			partialDrainEnabled: true,
			wantErr:             "no supported entities",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := &fakeHealthEventStore{byID: tt.stored, err: tt.storeErr}

			got, err := IsNodeDrained(context.Background(), store, "node-a", tt.events, tt.excludeEventID, tt.current,
				tt.partialDrainEnabled)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.False(t, got)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

type fakeHealthEventStore struct {
	datastore.HealthEventStore
	byID map[string]datastore.HealthEventWithStatus
	err  error
}

func (f *fakeHealthEventStore) FindHealthEventsByQuery(_ context.Context,
	builder datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
	if f.err != nil {
		return nil, f.err
	}

	id, _ := builder.ToMongo()["_id"].(string)

	event, ok := f.byID[id]
	if !ok {
		return nil, nil
	}

	return []datastore.HealthEventWithStatus{event}, nil
}

func storedDrain(id string, healthEvent *protos.HealthEvent, status datastore.Status,
	decision *datastore.DrainDecision) datastore.HealthEventWithStatus {
	healthEvent.Id = id
	healthEvent.NodeName = "node-a"

	return datastore.HealthEventWithStatus{
		HealthEvent: healthEvent,
		HealthEventStatus: datastore.HealthEventStatus{
			UserPodsEvictionStatus: datastore.OperationStatus{Status: status},
			DrainDecision:          decision,
		},
	}
}

func componentReset(entity *protos.Entity) *protos.HealthEvent {
	event := &protos.HealthEvent{RecommendedAction: protos.RecommendedAction_COMPONENT_RESET}
	if entity != nil {
		event.EntitiesImpacted = []*protos.Entity{entity}
	}

	return event
}

func restartVM() *protos.HealthEvent {
	return &protos.HealthEvent{RecommendedAction: protos.RecommendedAction_RESTART_VM}
}

func skippedEvent() *protos.HealthEvent {
	return &protos.HealthEvent{DrainOverrides: &protos.BehaviourOverrides{Skip: true}}
}

func recordedPartial(value string) *datastore.DrainDecision {
	return &datastore.DrainDecision{Scope: &datastore.Entity{EntityType: "GPU_UUID", EntityValue: value}}
}

func recordedFull() *datastore.DrainDecision {
	return &datastore.DrainDecision{Method: "BuiltIn"}
}

func gpuEntity(value string) *protos.Entity {
	return &protos.Entity{EntityType: "GPU_UUID", EntityValue: value}
}
