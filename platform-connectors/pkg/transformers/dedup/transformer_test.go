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

package dedup

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestDeduplicatorMarksDuplicateEventsStoreOnly(t *testing.T) {
	now := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	tracker := newTracker(3*time.Minute, withNow(func() time.Time { return now }))
	transformer := NewDeduplicator(tracker, nil)
	event := &pb.HealthEvent{
		NodeName:           "node-a",
		CheckName:          "SysLogsXIDError",
		ErrorCode:          []string{"79"},
		ProcessingStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		EntitiesImpacted:   []*pb.Entity{{EntityType: "gpu", EntityValue: "GPU-1"}},
	}

	err := transformer.Transform(context.Background(), event)
	require.NoError(t, err)
	assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, event.ProcessingStrategy)

	err = transformer.Transform(context.Background(), event)
	require.NoError(t, err)
	assert.Equal(t, pb.ProcessingStrategy_STORE_AND_ANALYSE, event.ProcessingStrategy)
}

func TestDeduplicatorOnlyDeduplicatesIncludedChecks(t *testing.T) {
	now := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	tracker := newTracker(3*time.Minute, withNow(func() time.Time { return now }))
	transformer := NewDeduplicator(tracker, []string{"SysLogsXIDError"})
	event := &pb.HealthEvent{
		NodeName:           "node-a",
		CheckName:          "SysLogsGPUFallenOff",
		ProcessingStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
	}

	for range 2 {
		err := transformer.Transform(context.Background(), event)
		require.NoError(t, err)
		assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, event.ProcessingStrategy)
	}

	included := &pb.HealthEvent{
		NodeName:           "node-a",
		CheckName:          "SysLogsXIDError",
		ProcessingStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
	}
	err := transformer.Transform(context.Background(), included)
	require.NoError(t, err)
	assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, included.ProcessingStrategy)

	err = transformer.Transform(context.Background(), included)
	require.NoError(t, err)
	assert.Equal(t, pb.ProcessingStrategy_STORE_AND_ANALYSE, included.ProcessingStrategy)
}

func TestDeduplicatorAlwaysKeepsHealthyEvents(t *testing.T) {
	now := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	tracker := newTracker(3*time.Minute, withNow(func() time.Time { return now }))
	transformer := NewDeduplicator(tracker, nil)
	event := &pb.HealthEvent{
		NodeName:           "node-a",
		CheckName:          "InfiniBandStateCheck",
		IsHealthy:          true,
		ProcessingStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
	}

	for range 2 {
		err := transformer.Transform(context.Background(), event)
		require.NoError(t, err)
		assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, event.ProcessingStrategy)
	}
}

func TestDeduplicatorHealthyClearsUnhealthyCounterpart(t *testing.T) {
	now := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	tracker := newTracker(3*time.Minute, withNow(func() time.Time { return now }))
	transformer := NewDeduplicator(tracker, nil)
	unhealthy := &pb.HealthEvent{
		NodeName:           "node-a",
		CheckName:          "SysLogsXIDError",
		ErrorCode:          []string{"79"},
		ProcessingStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		EntitiesImpacted:   []*pb.Entity{{EntityType: "gpu", EntityValue: "GPU-1"}},
	}
	healthy := &pb.HealthEvent{
		NodeName:           unhealthy.NodeName,
		CheckName:          unhealthy.CheckName,
		ErrorCode:          append([]string(nil), unhealthy.ErrorCode...),
		IsHealthy:          true,
		ProcessingStrategy: unhealthy.ProcessingStrategy,
		EntitiesImpacted:   []*pb.Entity{{EntityType: "gpu", EntityValue: "GPU-1"}},
	}

	err := transformer.Transform(context.Background(), unhealthy)
	require.NoError(t, err)
	require.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, unhealthy.ProcessingStrategy)

	err = transformer.Transform(context.Background(), healthy)
	require.NoError(t, err)
	require.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, healthy.ProcessingStrategy)

	err = transformer.Transform(context.Background(), unhealthy)
	require.NoError(t, err)
	assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, unhealthy.ProcessingStrategy)

	err = transformer.Transform(context.Background(), healthy)
	require.NoError(t, err)
	assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, healthy.ProcessingStrategy)
}

func TestDeduplicatorKeepsHealthyEventThatClearsUnhealthyCounterpart(t *testing.T) {
	now := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	tracker := newTracker(3*time.Minute, withNow(func() time.Time { return now }))
	transformer := NewDeduplicator(tracker, nil)
	unhealthy := &pb.HealthEvent{
		NodeName:           "node-a",
		CheckName:          "gpu-operator-pod-health",
		ErrorCode:          []string{"GPU_OPERATOR_POD_UNHEALTHY"},
		ProcessingStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		EntitiesImpacted:   []*pb.Entity{{EntityType: "v1/Pod", EntityValue: "gpu-operator/test-pod"}},
	}
	healthy := &pb.HealthEvent{
		NodeName:           unhealthy.NodeName,
		CheckName:          unhealthy.CheckName,
		ErrorCode:          append([]string(nil), unhealthy.ErrorCode...),
		IsHealthy:          true,
		ProcessingStrategy: unhealthy.ProcessingStrategy,
		EntitiesImpacted:   []*pb.Entity{{EntityType: "v1/Pod", EntityValue: "gpu-operator/test-pod"}},
	}

	err := transformer.Transform(context.Background(), unhealthy)
	require.NoError(t, err)
	require.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, unhealthy.ProcessingStrategy)

	err = transformer.Transform(context.Background(), healthy)
	require.NoError(t, err)
	require.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, healthy.ProcessingStrategy)

	err = transformer.Transform(context.Background(), unhealthy)
	require.NoError(t, err)
	require.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, unhealthy.ProcessingStrategy)

	err = transformer.Transform(context.Background(), healthy)
	require.NoError(t, err)
	assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, healthy.ProcessingStrategy)
}

func TestDeduplicatorKeepsCheckLevelHealthyEventThatClearsUnhealthyCounterpart(t *testing.T) {
	now := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	tracker := newTracker(3*time.Minute, withNow(func() time.Time { return now }))
	transformer := NewDeduplicator(tracker, nil)
	unhealthy := &pb.HealthEvent{
		NodeName:           "node-a",
		CheckName:          "GpuDcgmConnectivityFailure",
		ErrorCode:          []string{"DCGM_CONNECTIVITY_ERROR"},
		ProcessingStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
	}
	healthy := &pb.HealthEvent{
		NodeName:           unhealthy.NodeName,
		CheckName:          unhealthy.CheckName,
		IsHealthy:          true,
		ProcessingStrategy: unhealthy.ProcessingStrategy,
	}

	err := transformer.Transform(context.Background(), healthy)
	require.NoError(t, err)
	require.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, healthy.ProcessingStrategy)

	err = transformer.Transform(context.Background(), unhealthy)
	require.NoError(t, err)
	require.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, unhealthy.ProcessingStrategy)

	err = transformer.Transform(context.Background(), healthy)
	require.NoError(t, err)
	assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, healthy.ProcessingStrategy)
}

func TestErrCodeLabelCanonicalizesErrorCodes(t *testing.T) {
	event := &pb.HealthEvent{ErrorCode: []string{"95", "79"}}

	assert.Equal(t, "79,95", errCodeLabel(event))
	assert.Equal(t, noErrorCodeLabel, errCodeLabel(&pb.HealthEvent{}))
}
