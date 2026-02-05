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

// Package metrics provides Prometheus metrics for the Device API Server.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

const (
	namespace = "device_api_server"
)

// Cache operation labels.
const (
	OpRegister        = "register"
	OpUnregister      = "unregister"
	OpUpdateStatus    = "update_status"
	OpUpdateCondition = "update_condition"
	OpSet             = "set"
)

// Watch event type labels.
const (
	EventAdded    = "ADDED"
	EventModified = "MODIFIED"
	EventDeleted  = "DELETED"
)

// Metrics holds all Prometheus metrics for the Device API Server.
type Metrics struct {
	// Server info
	ServerInfo *prometheus.GaugeVec

	// Cache metrics
	CacheGpusTotal       prometheus.Gauge
	CacheGpusHealthy     prometheus.Gauge
	CacheGpusUnhealthy   prometheus.Gauge
	CacheGpusUnknown     prometheus.Gauge
	CacheUpdatesTotal    *prometheus.CounterVec
	CacheResourceVersion prometheus.Gauge

	// Watch metrics
	WatchStreamsActive prometheus.Gauge
	WatchEventsTotal   *prometheus.CounterVec
	WatchEventsDropped prometheus.Counter

	// NVML provider metrics
	NVMLProviderEnabled      prometheus.Gauge
	NVMLGpuCount             prometheus.Gauge
	NVMLHealthMonitorRunning prometheus.Gauge

	// Provider metrics (for containerized providers via heartbeat)
	ProviderConnectionState    *prometheus.GaugeVec
	ProviderLastHeartbeat      *prometheus.GaugeVec
	ProviderHeartbeatTotal     *prometheus.CounterVec
	ProviderHeartbeatTimeouts  *prometheus.CounterVec
	ProviderGpusManaged        *prometheus.GaugeVec

	// gRPC metrics (optional, for go-grpc-prometheus integration)
	GRPCRequestsTotal   *prometheus.CounterVec
	GRPCRequestDuration *prometheus.HistogramVec

	// Registry for custom metrics
	registry *prometheus.Registry
}

// New creates and registers all metrics with a new registry.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
	}

	// Server info
	m.ServerInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "info",
			Help:      "Server information with labels for version and node",
		},
		[]string{"version", "go_version", "node"},
	)

	// Cache metrics
	m.CacheGpusTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "cache",
			Name:      "gpus_total",
			Help:      "Total number of GPUs in cache",
		},
	)

	m.CacheGpusHealthy = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "cache",
			Name:      "gpus_healthy",
			Help:      "Number of healthy GPUs",
		},
	)

	m.CacheGpusUnhealthy = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "cache",
			Name:      "gpus_unhealthy",
			Help:      "Number of unhealthy GPUs",
		},
	)

	m.CacheGpusUnknown = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "cache",
			Name:      "gpus_unknown",
			Help:      "Number of GPUs with unknown health state",
		},
	)

	m.CacheUpdatesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "cache",
			Name:      "updates_total",
			Help:      "Total number of cache update operations",
		},
		[]string{"operation"},
	)

	m.CacheResourceVersion = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "cache",
			Name:      "resource_version",
			Help:      "Current cache resource version",
		},
	)

	// Watch metrics
	m.WatchStreamsActive = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "watch",
			Name:      "streams_active",
			Help:      "Number of active watch streams",
		},
	)

	m.WatchEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "watch",
			Name:      "events_total",
			Help:      "Total number of watch events sent",
		},
		[]string{"type"},
	)

	m.WatchEventsDropped = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "watch",
			Name:      "events_dropped_total",
			Help:      "Total number of watch events dropped due to full subscriber buffers",
		},
	)

	// NVML provider metrics
	m.NVMLProviderEnabled = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "nvml",
			Name:      "provider_enabled",
			Help:      "Whether the NVML provider is enabled (1) or disabled (0)",
		},
	)

	m.NVMLGpuCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "nvml",
			Name:      "gpu_count",
			Help:      "Number of GPUs discovered by NVML",
		},
	)

	m.NVMLHealthMonitorRunning = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "nvml",
			Name:      "health_monitor_running",
			Help:      "Whether the NVML health monitor is running (1) or not (0)",
		},
	)

	// Provider metrics (for containerized providers via heartbeat)
	m.ProviderConnectionState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "provider",
			Name:      "connection_state",
			Help:      "Provider connection state (1=connected, 0=disconnected)",
		},
		[]string{"provider_id"},
	)

	m.ProviderLastHeartbeat = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "provider",
			Name:      "last_heartbeat_timestamp_seconds",
			Help:      "Unix timestamp of last heartbeat from provider",
		},
		[]string{"provider_id"},
	)

	m.ProviderHeartbeatTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "provider",
			Name:      "heartbeat_total",
			Help:      "Total number of heartbeats received from provider",
		},
		[]string{"provider_id"},
	)

	m.ProviderHeartbeatTimeouts = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "provider",
			Name:      "heartbeat_timeout_total",
			Help:      "Total number of heartbeat timeouts per provider",
		},
		[]string{"provider_id"},
	)

	m.ProviderGpusManaged = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "provider",
			Name:      "gpus_managed",
			Help:      "Number of GPUs managed by provider",
		},
		[]string{"provider_id"},
	)

	// gRPC metrics
	m.GRPCRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "grpc",
			Name:      "requests_total",
			Help:      "Total number of gRPC requests",
		},
		[]string{"method", "code"},
	)

	m.GRPCRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: "grpc",
			Name:      "request_duration_seconds",
			Help:      "Duration of gRPC requests in seconds",
			Buckets:   []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"method"},
	)

	// Register all metrics
	m.registry.MustRegister(
		// Standard Go collectors
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),

		// Server info
		m.ServerInfo,

		// Cache metrics
		m.CacheGpusTotal,
		m.CacheGpusHealthy,
		m.CacheGpusUnhealthy,
		m.CacheGpusUnknown,
		m.CacheUpdatesTotal,
		m.CacheResourceVersion,

		// Watch metrics
		m.WatchStreamsActive,
		m.WatchEventsTotal,
		m.WatchEventsDropped,

		// NVML metrics
		m.NVMLProviderEnabled,
		m.NVMLGpuCount,
		m.NVMLHealthMonitorRunning,

		// Provider metrics
		m.ProviderConnectionState,
		m.ProviderLastHeartbeat,
		m.ProviderHeartbeatTotal,
		m.ProviderHeartbeatTimeouts,
		m.ProviderGpusManaged,

		// gRPC metrics
		m.GRPCRequestsTotal,
		m.GRPCRequestDuration,
	)

	return m
}

// Registry returns the Prometheus registry for this metrics instance.
func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}

// RecordCacheOperation increments the cache operations counter.
func (m *Metrics) RecordCacheOperation(operation string) {
	m.CacheUpdatesTotal.WithLabelValues(operation).Inc()
}

// RecordWatchEvent increments the watch events counter.
func (m *Metrics) RecordWatchEvent(eventType string) {
	m.WatchEventsTotal.WithLabelValues(eventType).Inc()
}

// RecordWatchEventDropped increments the dropped watch events counter.
func (m *Metrics) RecordWatchEventDropped() {
	m.WatchEventsDropped.Inc()
}

// UpdateCacheStats updates cache-related gauge metrics.
func (m *Metrics) UpdateCacheStats(total, healthy, unhealthy, unknown int, resourceVersion int64) {
	m.CacheGpusTotal.Set(float64(total))
	m.CacheGpusHealthy.Set(float64(healthy))
	m.CacheGpusUnhealthy.Set(float64(unhealthy))
	m.CacheGpusUnknown.Set(float64(unknown))
	m.CacheResourceVersion.Set(float64(resourceVersion))
}

// UpdateWatchStreams updates the active watch streams gauge.
func (m *Metrics) UpdateWatchStreams(count int) {
	m.WatchStreamsActive.Set(float64(count))
}

// UpdateNVMLStatus updates NVML provider metrics.
func (m *Metrics) UpdateNVMLStatus(enabled bool, gpuCount int, healthMonitorRunning bool) {
	if enabled {
		m.NVMLProviderEnabled.Set(1)
	} else {
		m.NVMLProviderEnabled.Set(0)
	}

	m.NVMLGpuCount.Set(float64(gpuCount))

	if healthMonitorRunning {
		m.NVMLHealthMonitorRunning.Set(1)
	} else {
		m.NVMLHealthMonitorRunning.Set(0)
	}
}

// SetServerInfo sets the server info gauge.
func (m *Metrics) SetServerInfo(version, goVersion, node string) {
	m.ServerInfo.WithLabelValues(version, goVersion, node).Set(1)
}

// RecordProviderHeartbeat records a heartbeat from a provider.
func (m *Metrics) RecordProviderHeartbeat(providerID string, gpuCount int, timestampSeconds float64) {
	m.ProviderConnectionState.WithLabelValues(providerID).Set(1)
	m.ProviderLastHeartbeat.WithLabelValues(providerID).Set(timestampSeconds)
	m.ProviderHeartbeatTotal.WithLabelValues(providerID).Inc()
	m.ProviderGpusManaged.WithLabelValues(providerID).Set(float64(gpuCount))
}

// RecordProviderDisconnected marks a provider as disconnected.
func (m *Metrics) RecordProviderDisconnected(providerID string) {
	m.ProviderConnectionState.WithLabelValues(providerID).Set(0)
	m.ProviderHeartbeatTimeouts.WithLabelValues(providerID).Inc()
}

// DeleteProviderMetrics removes metrics for a provider that has been cleaned up.
// This should be called after a provider has been disconnected for an extended period.
func (m *Metrics) DeleteProviderMetrics(providerID string) {
	m.ProviderConnectionState.DeleteLabelValues(providerID)
	m.ProviderLastHeartbeat.DeleteLabelValues(providerID)
	m.ProviderGpusManaged.DeleteLabelValues(providerID)
	// Note: Counter metrics (HeartbeatTotal, HeartbeatTimeouts) are not deleted
	// to preserve historical data
}
