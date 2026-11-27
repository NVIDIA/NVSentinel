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

package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// newCollectionCompletedGauge makes a collection that never finishes alertable: the metrics
// endpoint is up before collection starts, so a collector blocked on a wedged driver is a live
// target that keeps reporting 0 here.
func newCollectionCompletedGauge(reg prometheus.Registerer) prometheus.Gauge {
	return promauto.With(reg).NewGauge(prometheus.GaugeOpts{
		Name: "metadata_collector_collection_completed_timestamp_seconds",
		Help: "Unix time GPU metadata collection completed, or 0 while it is still running. " +
			"Collection can block on a wedged GPU driver, so 0 long after process start means it has stalled.",
	})
}
