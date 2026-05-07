// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package metrics declares the Prometheus metrics exposed by the NIC
// Health Monitor.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// DevicesDiscovered is the number of NIC devices discovered in sysfs.
	DevicesDiscovered = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nic_health_monitor_devices_discovered",
		Help: "Number of NIC devices discovered in sysfs",
	}, []string{"node", "check"})

	// HealthEventsSent counts health events sent to the platform connector.
	HealthEventsSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nic_health_monitor_health_events_sent_total",
		Help: "Total number of health events sent to the platform connector",
	}, []string{"node", "check", "is_fatal"})

	// PollCycleDuration measures the wall time of each poll cycle.
	PollCycleDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nic_health_monitor_poll_cycle_duration_seconds",
		Help:    "Duration of each poll cycle in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"node", "category"})

	// StateCheckErrors counts per-device state-check error events (port
	// DOWN, device disappeared, etc.).
	StateCheckErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nic_health_monitor_state_check_errors_total",
		Help: "Total number of state check error events",
	}, []string{"node", "check", "device", "port"})
)
