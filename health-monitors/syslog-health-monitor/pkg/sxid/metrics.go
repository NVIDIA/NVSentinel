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

package sxid

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// sxidCounterMetric is intentionally NOT pre-initialized at startup (unlike the
// sibling XID / GPU-fallen counters). Its label set includes both link and
// nvswitch, which together carry per-event cardinality that is not enumerable
// at startup: on any given NVSwitch-backed node the specific (err_code, link,
// nvswitch) triples that will actually fire are unknown until an event occurs.
//
// Emitting a fake combination at startup would either pollute the series space
// with entries that never match a real event (and never cancel out the Google
// Managed Prometheus "first-sample-as-baseline" bug for the real series) or,
// if we enumerated the full cartesian product, create hundreds of stale series
// per pod. Both outcomes are worse than the first-sample loss we are trying to
// fix.
//
// If we ever need to close the first-sample gap for SXID as well, the right
// approach is to drop the link/nvswitch labels on this counter (keeping them
// on the emitted health event / logs) so the remaining {node, err_code} space
// becomes enumerable.
var (
	sxidCounterMetric = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "syslog_health_monitor_sxid_errors",
			Help: "Total number of SXID found",
		},
		[]string{"node", "err_code", "link", "nvswitch"},
	)
)
