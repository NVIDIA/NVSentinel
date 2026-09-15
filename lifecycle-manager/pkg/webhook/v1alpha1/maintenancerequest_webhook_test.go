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

package v1alpha1

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

func TestValidateCreate_ValidMR_Succeeds(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	mr := validMR()

	warnings, err := v.ValidateCreate(context.Background(), mr)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_Disabled_RejectsAll(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: false}
	mr := validMR()

	_, err := v.ValidateCreate(context.Background(), mr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disabled")
}

func TestValidateCreate_NilSpec_Rejects(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	mr := &v1alpha1.MaintenanceRequest{Name: "no-spec"}

	_, err := v.ValidateCreate(context.Background(), mr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.healthEvent is required")
}

func TestValidateCreate_NilHealthEvent_Rejects(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	mr := &v1alpha1.MaintenanceRequest{
		Name: "nil-he",
		Spec: &protos.MaintenanceRequestSpec{},
	}

	_, err := v.ValidateCreate(context.Background(), mr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.healthEvent is required")
}

func TestValidateCreate_EmptyNodeName_Rejects(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	mr := validMR()
	mr.Spec.HealthEvent.NodeName = ""

	_, err := v.ValidateCreate(context.Background(), mr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nodeName is required")
}

func TestValidateCreate_ZeroVersion_Rejects(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	mr := validMR()
	mr.Spec.HealthEvent.Version = 0

	_, err := v.ValidateCreate(context.Background(), mr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.healthEvent.version is required")
}

func TestValidateCreate_IsHealthyTrue_Rejects(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	mr := validMR()
	mr.Spec.HealthEvent.IsHealthy = true

	_, err := v.ValidateCreate(context.Background(), mr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "isHealthy must be false")
}

func TestValidateUpdate_NilSpec_Rejects(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	oldMR := validMR()
	newMR := &v1alpha1.MaintenanceRequest{Name: "nil-spec-update"}

	_, err := v.ValidateUpdate(context.Background(), oldMR, newMR)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.healthEvent is required")
}

func TestValidateUpdate_EmptyNodeName_Rejects(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	oldMR := validMR()
	newMR := validMR()
	newMR.Spec.HealthEvent.NodeName = ""

	_, err := v.ValidateUpdate(context.Background(), oldMR, newMR)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nodeName is required")
}

func TestValidateUpdate_IsHealthyTrue_Rejects(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	oldMR := validMR()
	newMR := validMR()
	newMR.Spec.HealthEvent.IsHealthy = true

	_, err := v.ValidateUpdate(context.Background(), oldMR, newMR)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "isHealthy must be false")
}

func TestValidateUpdate_ImmutableSpec_Rejects(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	oldMR := validMR()
	newMR := validMR()
	newMR.Spec.HealthEvent.NodeName = "different-node"

	_, err := v.ValidateUpdate(context.Background(), oldMR, newMR)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "immutable")
}

func TestValidateCreate_PastStartTime_Rejects(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	mr := validMR()
	mr.Spec.StartTime = timestamppb.New(time.Now().Add(-1 * time.Hour))

	_, err := v.ValidateCreate(context.Background(), mr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "startTime must be in the future")
}

func TestValidateCreate_NilStartTime_Succeeds(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	mr := validMR()
	mr.Spec.StartTime = nil

	warnings, err := v.ValidateCreate(context.Background(), mr)
	assert.NoError(t, err, "nil startTime should be allowed")
	assert.Nil(t, warnings)
}

func TestValidateUpdate_StartTimeChangedToPast_Rejects(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	oldMR := validMR()
	newMR := validMR()
	newMR.Spec.StartTime = timestamppb.New(time.Now().Add(-1 * time.Hour))

	_, err := v.ValidateUpdate(context.Background(), oldMR, newMR)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "startTime must be in the future")
}

func TestValidateUpdate_StartTimeChangedToFuture_Succeeds(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	oldMR := validMR()
	newMR := validMR()
	newMR.Spec.StartTime = timestamppb.New(time.Now().Add(2 * time.Hour))

	warnings, err := v.ValidateUpdate(context.Background(), oldMR, newMR)
	assert.NoError(t, err, "rescheduling startTime to a future value must be allowed")
	assert.Nil(t, warnings)
}

func TestValidateUpdate_StartTimeUnchanged_Succeeds(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	oldMR := validMR()
	oldMR.Spec.StartTime = timestamppb.New(time.Now().Add(-1 * time.Hour))
	newMR := oldMR.DeepCopy()

	warnings, err := v.ValidateUpdate(context.Background(), oldMR, newMR)
	assert.NoError(t, err, "unchanged startTime that is now in the past must not be rejected")
	assert.Nil(t, warnings)
}

func TestDefault_PopulatesMissingEventFields(t *testing.T) {
	t.Parallel()

	mr := validMR()
	mr.Spec.HealthEvent.Metadata = map[string]string{
		"existing":               "value",
		"maintenanceRequestName": "spoofed-name",
		"maintenanceRequestUID":  "spoofed-uid",
	}

	before := time.Now()
	err := (&MaintenanceRequestDefaulter{}).Default(context.Background(), mr)
	after := time.Now()

	require.NoError(t, err)
	_, err = uuid.Parse(mr.Spec.HealthEvent.Id)
	assert.NoError(t, err)
	require.NotNil(t, mr.Spec.HealthEvent.GeneratedTimestamp)
	assert.False(t, mr.Spec.HealthEvent.GeneratedTimestamp.AsTime().Before(before))
	assert.False(t, mr.Spec.HealthEvent.GeneratedTimestamp.AsTime().After(after))
	assert.Equal(t, "value", mr.Spec.HealthEvent.Metadata["existing"])
	assert.Equal(t, "test-mr",
		mr.Spec.HealthEvent.Metadata["maintenanceRequestName"])
	assert.NotContains(t, mr.Spec.HealthEvent.Metadata,
		"maintenanceRequestUID")
}

func TestDefault_PreservesExistingEventFields(t *testing.T) {
	t.Parallel()

	mr := validMR()
	timestamp := timestamppb.New(time.Date(
		2026, 1, 1, 0, 0, 0, 0, time.UTC,
	))
	mr.Spec.HealthEvent.Id = "existing-id"
	mr.Spec.HealthEvent.GeneratedTimestamp = timestamp

	err := (&MaintenanceRequestDefaulter{}).Default(context.Background(), mr)

	require.NoError(t, err)
	assert.Equal(t, "existing-id", mr.Spec.HealthEvent.Id)
	assert.Equal(t, timestamp, mr.Spec.HealthEvent.GeneratedTimestamp)
}

func TestDefault_DefaultsFieldsIndependently(t *testing.T) {
	t.Parallel()

	timestamp := timestamppb.New(time.Date(
		2026, 1, 1, 0, 0, 0, 0, time.UTC,
	))
	tests := []struct {
		name            string
		id              string
		timestamp       *timestamppb.Timestamp
		wantIDPreserved bool
		wantTSPreserved bool
	}{
		{
			name:            "missing ID only",
			timestamp:       timestamp,
			wantTSPreserved: true,
		},
		{
			name:            "missing timestamp only",
			id:              "existing-id",
			wantIDPreserved: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mr := validMR()
			mr.Spec.HealthEvent.Id = tt.id
			mr.Spec.HealthEvent.GeneratedTimestamp = tt.timestamp

			err := (&MaintenanceRequestDefaulter{}).Default(
				context.Background(), mr,
			)

			require.NoError(t, err)
			assert.NotEmpty(t, mr.Spec.HealthEvent.Id)
			assert.NotNil(t, mr.Spec.HealthEvent.GeneratedTimestamp)
			if tt.wantIDPreserved {
				assert.Equal(t, tt.id, mr.Spec.HealthEvent.Id)
			}
			if tt.wantTSPreserved {
				assert.Equal(t, tt.timestamp,
					mr.Spec.HealthEvent.GeneratedTimestamp)
			}
		})
	}
}

func TestDefault_MissingRequiredFieldsDoNotFail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mr   *v1alpha1.MaintenanceRequest
	}{
		{
			name: "nil spec",
			mr: &v1alpha1.MaintenanceRequest{
				Name: "missing-spec",
			},
		},
		{
			name: "nil health event",
			mr: &v1alpha1.MaintenanceRequest{
				Name: "missing-health-event",
				Spec: &protos.MaintenanceRequestSpec{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := (&MaintenanceRequestDefaulter{}).Default(
				context.Background(), tt.mr,
			)

			assert.NoError(t, err)
		})
	}
}

func TestValidateUpdate_DefaultedFieldsAreImmutable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		field  string
		mutate func(*protos.HealthEvent)
	}{
		{
			name:  "id",
			field: "id",
			mutate: func(event *protos.HealthEvent) {
				event.Id = "different-id"
			},
		},
		{
			name:  "version",
			field: "version",
			mutate: func(event *protos.HealthEvent) {
				event.Version = 2
			},
		},
		{
			name:  "generated timestamp",
			field: "generatedTimestamp",
			mutate: func(event *protos.HealthEvent) {
				event.GeneratedTimestamp = timestamppb.Now()
			},
		},
		{
			name:  "metadata",
			field: "metadata",
			mutate: func(event *protos.HealthEvent) {
				event.Metadata = map[string]string{"changed": "value"}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := &MaintenanceRequestValidator{Enabled: true}
			oldMR := validMR()
			oldMR.Spec.HealthEvent.Id = "original-id"
			oldMR.Spec.HealthEvent.GeneratedTimestamp = timestamppb.New(
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			)
			oldMR.Spec.HealthEvent.Metadata = map[string]string{
				"maintenanceRequestName": "test-mr",
			}
			newMR := oldMR.DeepCopy()
			tt.mutate(newMR.Spec.HealthEvent)

			_, err := v.ValidateUpdate(
				context.Background(), oldMR, newMR,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.field)
			assert.Contains(t, err.Error(), "immutable")
		})
	}
}

func TestValidateUpdate_StatusOnlyChange_Succeeds(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	oldMR := validMR()
	newMR := oldMR.DeepCopy()
	newMR.Status = &protos.MaintenanceRequestStatus{
		Conditions: []*protos.Condition{
			{
				Type:   "HealthEventEmitted",
				Status: "True",
			},
		},
	}

	warnings, err := v.ValidateUpdate(context.Background(), oldMR, newMR)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateDelete_AlwaysAllowed(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	mr := validMR()

	warnings, err := v.ValidateDelete(context.Background(), mr)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func validMR() *v1alpha1.MaintenanceRequest {
	return &v1alpha1.MaintenanceRequest{
		Name: "test-mr",
		Spec: &protos.MaintenanceRequestSpec{
			HealthEvent: &protos.HealthEvent{
				NodeName:          "node-1",
				Agent:             "maintenance-controller",
				CheckName:         "planned-maintenance",
				Version:           1,
				IsFatal:           true,
				IsHealthy:         false,
				RecommendedAction: protos.RecommendedAction_NONE,
				Message:           "Planned maintenance window",
			},
			StartTime: timestamppb.New(time.Now().Add(1 * time.Hour)),
		},
	}
}
