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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func withNow(now func() time.Time) trackerOption {
	return func(t *tracker) {
		if now != nil {
			t.now = now
		}
	}
}

func TestKey(t *testing.T) {
	tests := []struct {
		name  string
		left  *pb.HealthEvent
		right *pb.HealthEvent
		equal bool
	}{
		{
			name: "canonicalizes entity and error code order",
			left: &pb.HealthEvent{
				NodeName:  "node-a",
				CheckName: "SysLogsXIDError",
				ErrorCode: []string{"79", "31"},
				EntitiesImpacted: []*pb.Entity{
					{EntityType: "gpu", EntityValue: "GPU-1"},
					{EntityType: "pci", EntityValue: "0000:b3:00.0"},
				},
			},
			right: &pb.HealthEvent{
				NodeName:  "node-a",
				CheckName: "SysLogsXIDError",
				ErrorCode: []string{"31", "79"},
				EntitiesImpacted: []*pb.Entity{
					{EntityType: "pci", EntityValue: "0000:b3:00.0"},
					{EntityType: "gpu", EntityValue: "GPU-1"},
				},
			},
			equal: true,
		},
		{
			name: "includes check name",
			left: &pb.HealthEvent{
				NodeName:         "node-a",
				CheckName:        "InfiniBandStateCheck",
				EntitiesImpacted: []*pb.Entity{{EntityType: "port", EntityValue: "mlx5_0/1"}},
			},
			right: &pb.HealthEvent{
				NodeName:         "node-a",
				CheckName:        "InfiniBandDegradationCheck",
				EntitiesImpacted: []*pb.Entity{{EntityType: "port", EntityValue: "mlx5_0/1"}},
			},
			equal: false,
		},
		{
			name: "includes health state",
			left: &pb.HealthEvent{
				NodeName:         "node-a",
				CheckName:        "InfiniBandStateCheck",
				EntitiesImpacted: []*pb.Entity{{EntityType: "port", EntityValue: "mlx5_0/1"}},
			},
			right: &pb.HealthEvent{
				NodeName:         "node-a",
				CheckName:        "InfiniBandStateCheck",
				IsHealthy:        true,
				EntitiesImpacted: []*pb.Entity{{EntityType: "port", EntityValue: "mlx5_0/1"}},
			},
			equal: false,
		},
		{
			name: "includes processing strategy",
			left: &pb.HealthEvent{
				NodeName:           "node-a",
				CheckName:          "SysLogsXIDError",
				ErrorCode:          []string{"79"},
				ProcessingStrategy: pb.ProcessingStrategy_STORE_ONLY,
				EntitiesImpacted:   []*pb.Entity{{EntityType: "gpu", EntityValue: "GPU-1"}},
			},
			right: &pb.HealthEvent{
				NodeName:           "node-a",
				CheckName:          "SysLogsXIDError",
				ErrorCode:          []string{"79"},
				ProcessingStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
				EntitiesImpacted:   []*pb.Entity{{EntityType: "gpu", EntityValue: "GPU-1"}},
			},
			equal: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.equal {
				assert.Equal(t,
					keyWithHealthState(tt.left, tt.left.GetIsHealthy()),
					keyWithHealthState(tt.right, tt.right.GetIsHealthy()))
			} else {
				assert.NotEqual(t,
					keyWithHealthState(tt.left, tt.left.GetIsHealthy()),
					keyWithHealthState(tt.right, tt.right.GetIsHealthy()))
			}
		})
	}
}

func TestTrackerDeduplicatesWithinTTLAndReemitsAfterExpiry(t *testing.T) {
	now := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	tracker := newTracker(3*time.Minute, withNow(func() time.Time { return now }))
	event := &pb.HealthEvent{NodeName: "node-a", CheckName: "SysLogsXIDError", ErrorCode: []string{"79"}}

	require.False(t, tracker.checkAndMark(event))
	assert.True(t, tracker.checkAndMark(event))

	now = now.Add(3 * time.Minute)

	assert.False(t, tracker.checkAndMark(event))
	assert.Len(t, tracker.seen, 1)
}

func TestTrackerCheckAndMarkIsAtomic(t *testing.T) {
	now := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	tracker := newTracker(3*time.Minute, withNow(func() time.Time { return now }))
	event := &pb.HealthEvent{NodeName: "node-a", CheckName: "SysLogsXIDError", ErrorCode: []string{"79"}}

	const workers = 32

	var uniqueCount atomic.Int32
	var wg sync.WaitGroup

	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			if !tracker.checkAndMark(event) {
				uniqueCount.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), uniqueCount.Load())
}

func TestTrackerEvictExpiredRemovesStaleEntries(t *testing.T) {
	now := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	tracker := newTracker(3*time.Minute, withNow(func() time.Time { return now }))
	stale := &pb.HealthEvent{NodeName: "node-a", CheckName: "SysLogsXIDError", ErrorCode: []string{"79"}}
	fresh := &pb.HealthEvent{NodeName: "node-a", CheckName: "SysLogsSXIDError", ErrorCode: []string{"95"}}

	require.False(t, tracker.checkAndMark(stale))
	now = now.Add(2 * time.Minute)
	require.False(t, tracker.checkAndMark(fresh))
	now = now.Add(90 * time.Second)

	tracker.evictExpired()

	assert.False(t, tracker.checkAndMark(stale))
	assert.True(t, tracker.checkAndMark(fresh))
}

func TestClearUnhealthyCounterpart(t *testing.T) {
	now := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	tracker := newTracker(3*time.Minute, withNow(func() time.Time { return now }))
	unhealthy := &pb.HealthEvent{
		NodeName:  "node-a",
		CheckName: "SysLogsXIDError",
		ErrorCode: []string{"79"},
		EntitiesImpacted: []*pb.Entity{
			{EntityType: "gpu", EntityValue: "GPU-1"},
		},
	}
	healthy := &pb.HealthEvent{
		NodeName:  unhealthy.NodeName,
		CheckName: unhealthy.CheckName,
		ErrorCode: append([]string(nil), unhealthy.ErrorCode...),
		IsHealthy: true,
		EntitiesImpacted: []*pb.Entity{
			{EntityType: "gpu", EntityValue: "GPU-1"},
		},
	}

	require.False(t, tracker.checkAndMark(unhealthy))
	require.False(t, tracker.checkAndMark(healthy))

	require.True(t, tracker.checkAndMark(unhealthy))
	require.True(t, tracker.checkAndMark(healthy))

	cleared := tracker.clearUnhealthyCounterpart(healthy)

	assert.True(t, cleared)
	assert.False(t, tracker.checkAndMark(unhealthy))
	assert.True(t, tracker.checkAndMark(healthy))
}

func TestClearUnhealthyCounterpartWithCheckLevelHealthyEvent(t *testing.T) {
	now := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	tracker := newTracker(3*time.Minute, withNow(func() time.Time { return now }))
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

	require.False(t, tracker.checkAndMark(unhealthy))
	require.False(t, tracker.checkAndMark(healthy))

	require.True(t, tracker.checkAndMark(unhealthy))
	require.True(t, tracker.checkAndMark(healthy))

	cleared := tracker.clearUnhealthyCounterpart(healthy)

	assert.True(t, cleared)
	assert.False(t, tracker.checkAndMark(unhealthy))
	assert.True(t, tracker.checkAndMark(healthy))
}

func TestClearUnhealthyCounterpartNoopForUnhealthyEvent(t *testing.T) {
	now := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	tracker := newTracker(3*time.Minute, withNow(func() time.Time { return now }))
	event := &pb.HealthEvent{NodeName: "node-a", CheckName: "SysLogsXIDError"}

	tracker.checkAndMark(event)
	cleared := tracker.clearUnhealthyCounterpart(event)

	assert.False(t, cleared)
	assert.True(t, tracker.checkAndMark(event))
}
