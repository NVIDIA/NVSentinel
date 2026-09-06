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

package reconciler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type mockHealthEventStore struct {
	datastore.HealthEventStore
	findHealthEventsByQueryFn func(ctx context.Context, builder datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error)
}

func (m *mockHealthEventStore) FindHealthEventsByQuery(ctx context.Context,
	builder datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
	return m.findHealthEventsByQueryFn(ctx, builder)
}

func mockHealthEventStoreWithDrainStatus(t *testing.T, drained bool) *mockHealthEventStore {
	t.Helper()

	status := datastore.StatusFailed
	if drained {
		status = datastore.StatusSucceeded
	}

	return &mockHealthEventStore{
		findHealthEventsByQueryFn: func(ctx context.Context, builder datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
			_, args := builder.ToSQL()
			require.Len(t, args, 1, "expected a single-argument _id query")

			id, _ := args[0].(string)

			return []datastore.HealthEventWithStatus{
				{
					HealthEvent: &protos.HealthEvent{Id: id},
					HealthEventStatus: datastore.HealthEventStatus{
						UserPodsEvictionStatus: datastore.OperationStatus{Status: status},
					},
				},
			}, nil
		},
	}
}

func mockHealthEventStoreWithDrainedComponentResetEvent(t *testing.T, entityType, entityValue string) *mockHealthEventStore {
	t.Helper()

	return &mockHealthEventStore{
		findHealthEventsByQueryFn: func(ctx context.Context, builder datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
			_, args := builder.ToSQL()
			require.Len(t, args, 1, "expected a single-argument _id query")

			id, _ := args[0].(string)

			return []datastore.HealthEventWithStatus{
				{
					HealthEvent: &protos.HealthEvent{
						Id:                id,
						RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
						EntitiesImpacted:  []*protos.Entity{{EntityType: entityType, EntityValue: entityValue}},
					},
					HealthEventStatus: datastore.HealthEventStatus{
						UserPodsEvictionStatus: datastore.OperationStatus{Status: datastore.StatusSucceeded},
					},
				},
			}, nil
		},
	}
}
