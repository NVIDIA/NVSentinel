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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

func validMR() *v1alpha1.MaintenanceRequest {
	return &v1alpha1.MaintenanceRequest{
		Name: "test-mr",
		Spec: &protos.MaintenanceRequestSpec{
			HealthEvent: &protos.HealthEvent{
				NodeName:          "node-1",
				Agent:             "maintenance-controller",
				CheckName:         "planned-maintenance",
				IsFatal:           true,
				IsHealthy:         false,
				RecommendedAction: protos.RecommendedAction_NONE,
				Message:           "Planned maintenance window",
			},
			StartTime: timestamppb.New(time.Now().Add(1 * time.Hour)),
		},
	}
}

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

func TestValidateUpdate_ControllerAutoPopulatedFields_Succeeds(t *testing.T) {
	t.Parallel()

	v := &MaintenanceRequestValidator{Enabled: true}
	oldMR := validMR()
	newMR := oldMR.DeepCopy()
	newMR.Spec.HealthEvent.Id = "he-mr-auto-populated"
	newMR.Spec.HealthEvent.Version = 1
	newMR.Spec.HealthEvent.GeneratedTimestamp = timestamppb.Now()
	newMR.Spec.HealthEvent.Metadata = map[string]string{
		"maintenanceRequestName": "test-mr",
		"maintenanceRequestUID":  "some-uid",
	}

	warnings, err := v.ValidateUpdate(context.Background(), oldMR, newMR)
	assert.NoError(t, err, "controller-populated fields (id, version, generatedTimestamp, metadata) must not trigger immutability rejection")
	assert.Nil(t, warnings)
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
