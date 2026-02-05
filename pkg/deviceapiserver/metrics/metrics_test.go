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

package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNew(t *testing.T) {
	m := New()

	if m == nil {
		t.Fatal("New() returned nil")
	}

	if m.Registry() == nil {
		t.Error("Registry() returned nil")
	}
}

func TestSetServerInfo(t *testing.T) {
	m := New()
	m.SetServerInfo("v0.1.0", "go1.25.0", "node-1")

	// Verify metric was set
	count := testutil.CollectAndCount(m.ServerInfo)
	if count != 1 {
		t.Errorf("Expected 1 server info metric, got %d", count)
	}
}

func TestUpdateCacheStats(t *testing.T) {
	m := New()
	m.UpdateCacheStats(10, 8, 1, 1, 100)

	// Verify total
	total := testutil.ToFloat64(m.CacheGpusTotal)
	if total != 10 {
		t.Errorf("CacheGpusTotal = %f, want 10", total)
	}

	// Verify healthy
	healthy := testutil.ToFloat64(m.CacheGpusHealthy)
	if healthy != 8 {
		t.Errorf("CacheGpusHealthy = %f, want 8", healthy)
	}

	// Verify unhealthy
	unhealthy := testutil.ToFloat64(m.CacheGpusUnhealthy)
	if unhealthy != 1 {
		t.Errorf("CacheGpusUnhealthy = %f, want 1", unhealthy)
	}

	// Verify unknown
	unknown := testutil.ToFloat64(m.CacheGpusUnknown)
	if unknown != 1 {
		t.Errorf("CacheGpusUnknown = %f, want 1", unknown)
	}

	// Verify resource version
	rv := testutil.ToFloat64(m.CacheResourceVersion)
	if rv != 100 {
		t.Errorf("CacheResourceVersion = %f, want 100", rv)
	}
}

func TestRecordCacheOperation(t *testing.T) {
	m := New()

	// Record some operations
	m.RecordCacheOperation(OpRegister)
	m.RecordCacheOperation(OpRegister)
	m.RecordCacheOperation(OpUnregister)
	m.RecordCacheOperation(OpUpdateStatus)
	m.RecordCacheOperation(OpUpdateCondition)

	// Verify register count
	expected := `
# HELP device_api_server_cache_updates_total Total number of cache update operations
# TYPE device_api_server_cache_updates_total counter
device_api_server_cache_updates_total{operation="register"} 2
device_api_server_cache_updates_total{operation="unregister"} 1
device_api_server_cache_updates_total{operation="update_condition"} 1
device_api_server_cache_updates_total{operation="update_status"} 1
`

	err := testutil.CollectAndCompare(m.CacheUpdatesTotal, strings.NewReader(expected))
	if err != nil {
		t.Errorf("CacheUpdatesTotal mismatch: %v", err)
	}
}

func TestUpdateWatchStreams(t *testing.T) {
	m := New()
	m.UpdateWatchStreams(5)

	value := testutil.ToFloat64(m.WatchStreamsActive)
	if value != 5 {
		t.Errorf("WatchStreamsActive = %f, want 5", value)
	}
}

func TestRecordWatchEvent(t *testing.T) {
	m := New()

	m.RecordWatchEvent(EventAdded)
	m.RecordWatchEvent(EventAdded)
	m.RecordWatchEvent(EventModified)
	m.RecordWatchEvent(EventDeleted)

	expected := `
# HELP device_api_server_watch_events_total Total number of watch events sent
# TYPE device_api_server_watch_events_total counter
device_api_server_watch_events_total{type="ADDED"} 2
device_api_server_watch_events_total{type="DELETED"} 1
device_api_server_watch_events_total{type="MODIFIED"} 1
`

	err := testutil.CollectAndCompare(m.WatchEventsTotal, strings.NewReader(expected))
	if err != nil {
		t.Errorf("WatchEventsTotal mismatch: %v", err)
	}
}

func TestUpdateNVMLStatus(t *testing.T) {
	m := New()

	// Test enabled
	m.UpdateNVMLStatus(true, 4, true)

	if testutil.ToFloat64(m.NVMLProviderEnabled) != 1 {
		t.Error("NVMLProviderEnabled should be 1 when enabled")
	}

	if testutil.ToFloat64(m.NVMLGpuCount) != 4 {
		t.Error("NVMLGpuCount should be 4")
	}

	if testutil.ToFloat64(m.NVMLHealthMonitorRunning) != 1 {
		t.Error("NVMLHealthMonitorRunning should be 1 when running")
	}

	// Test disabled
	m.UpdateNVMLStatus(false, 0, false)

	if testutil.ToFloat64(m.NVMLProviderEnabled) != 0 {
		t.Error("NVMLProviderEnabled should be 0 when disabled")
	}

	if testutil.ToFloat64(m.NVMLGpuCount) != 0 {
		t.Error("NVMLGpuCount should be 0 when disabled")
	}

	if testutil.ToFloat64(m.NVMLHealthMonitorRunning) != 0 {
		t.Error("NVMLHealthMonitorRunning should be 0 when not running")
	}
}
