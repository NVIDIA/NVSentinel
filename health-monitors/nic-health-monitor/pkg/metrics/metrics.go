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

// Label names shared across the metrics declared below.
const (
	labelNode   = "node"
	labelCheck  = "check"
	labelDevice = "device"
	labelPort   = "port"
)

var (
	// DevicesDiscovered is the number of NIC devices discovered in sysfs.
	DevicesDiscovered = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nic_health_monitor_devices_discovered",
		Help: "Number of NIC devices discovered in sysfs",
	}, []string{labelNode, labelCheck})

	// HealthEventsSent counts health events sent to the platform connector.
	HealthEventsSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nic_health_monitor_health_events_sent_total",
		Help: "Total number of health events sent to the platform connector",
	}, []string{labelNode, labelCheck, "is_fatal"})

	// PollCycleDuration measures the wall time of each poll cycle.
	PollCycleDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nic_health_monitor_poll_cycle_duration_seconds",
		Help:    "Duration of each poll cycle in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{labelNode, "category"})

	// PollCycleLastCompletedTimestamp is initialized at monitor startup and then
	// advanced whenever each poll category completes. Its age therefore remains
	// observable even when the first or a later sysfs read is blocked.
	PollCycleLastCompletedTimestamp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nic_health_monitor_poll_cycle_last_completed_timestamp_seconds",
		Help: "Unix timestamp of monitor startup or the last completed poll cycle",
	}, []string{labelNode, "category"})

	// StateCheckErrors counts per-device state-check error events (port
	// DOWN, device disappeared, etc.).
	StateCheckErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nic_health_monitor_state_check_errors_total",
		Help: "Total number of state check error events",
	}, []string{labelNode, labelCheck, labelDevice, labelPort})

	// PortStateBlips counts ports that turned unhealthy and recovered before
	// the port-state hold-down reported them.
	PortStateBlips = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nic_health_monitor_port_state_blips_total",
		Help: "Ports that turned unhealthy and recovered before the port-state hold-down reported them",
	}, []string{labelNode, labelCheck, labelDevice, labelPort})

	// CounterThresholdBreaches counts counter threshold breaches.
	CounterThresholdBreaches = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nic_health_monitor_counter_threshold_breaches_total",
		Help: "Total number of counter threshold breaches detected",
	}, []string{labelNode, "counter", labelDevice, labelPort, "is_fatal"})

	// FirstPollDeferred counts polls a check skipped before its first
	// complete evaluation because discovery reported unreadable devices.
	// A non-zero value that stops growing means the deferral limit was
	// reached and monitoring proceeded on the readable subset.
	FirstPollDeferred = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nic_health_monitor_first_poll_deferred_total",
		Help: "Polls deferred before the first evaluation because devices were unreadable",
	}, []string{labelNode, labelCheck})
)

// isFatalValues are the two values the is_fatal label can take, matching the
// strings the call site formats.
var isFatalValues = []string{"false", "true"}

// InitForChecks creates the zero-valued children of the counters whose labels
// are fully known once the enabled checks are, so each exports 0 from startup.
//
// Without this a CounterVec with no children exports nothing at all: no series,
// no HELP and no TYPE. An empty query then cannot distinguish "nothing has
// happened" from "the monitor is not running, not scraped, or renamed", and an
// alert on the counter can never fire, because there is no series for rate() to
// evaluate until the event being alerted on has already occurred.
//
// StateCheckErrors and CounterThresholdBreaches are deliberately excluded: their
// labels include the device and port discovered at runtime, so there is no
// correct set to pre-create here.
func InitForChecks(node string, checkNames []string) {
	for _, check := range checkNames {
		FirstPollDeferred.WithLabelValues(node, check)

		for _, isFatal := range isFatalValues {
			HealthEventsSent.WithLabelValues(node, check, isFatal)
		}
	}
}
