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

package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// DatastoreConnectedMetricName is the Prometheus metric reporting datastore readiness.
const DatastoreConnectedMetricName = "datastore_connected"

// LagStateProvider reports change-stream lag timestamps. It matches lagstate.Provider
// from store-client without introducing a cross-module package dependency.
type LagStateProvider interface {
	LagState() (lastEmptyBatch, lastEventRead time.Time)
}

// DatastoreReadinessChecker reports readiness based on change stream watcher lag state.
// It implements ReadinessChecker for server.WithReadinessCheck, controller-runtime healthz.Checker
// for mgr.AddReadyzCheck, and prometheus.Collector for exporting the datastore_connected gauge.
type DatastoreReadinessChecker struct {
	mu       sync.RWMutex
	provider LagStateProvider
	desc     *prometheus.Desc
}

// NewDatastoreReadinessChecker creates a DatastoreReadinessChecker and registers the
// datastore_connected metric on reg. If reg is nil, prometheus.DefaultRegisterer is used.
func NewDatastoreReadinessChecker(reg prometheus.Registerer) *DatastoreReadinessChecker {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	desc := prometheus.NewDesc(
		DatastoreConnectedMetricName,
		"Reports 1 if the datastore watcher has connected, 0 otherwise.",
		nil,
		nil,
	)

	checker := &DatastoreReadinessChecker{
		desc: desc,
	}

	if err := reg.Register(checker); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if errors.As(err, &alreadyRegistered) {
			if existing, ok := alreadyRegistered.ExistingCollector.(*DatastoreReadinessChecker); ok {
				return existing
			}
		}

		slog.Warn("Failed to register datastore_connected metric", "error", err)
	}

	return checker
}

// SetLagProvider sets the LagStateProvider used to determine datastore connectivity.
func (c *DatastoreReadinessChecker) SetLagProvider(provider LagStateProvider) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.provider = provider
}

// SetWatcher sets the watcher, extracting its LagStateProvider.
func (c *DatastoreReadinessChecker) SetWatcher(watcher any) {
	if watcher == nil {
		return
	}

	if provider, ok := watcher.(LagStateProvider); ok {
		c.SetLagProvider(provider)
	}
}

// Ready implements ReadinessChecker. It reports ready once the datastore watcher
// has established connection.
func (c *DatastoreReadinessChecker) Ready(_ context.Context) error {
	c.mu.RLock()
	provider := c.provider
	c.mu.RUnlock()

	if provider == nil {
		return errors.New("datastore watcher initializing")
	}

	return nil
}

// Check implements controller-runtime healthz.Checker for mgr.AddReadyzCheck.
func (c *DatastoreReadinessChecker) Check(req *http.Request) error {
	if req == nil {
		return c.Ready(context.Background())
	}

	return c.Ready(req.Context())
}

// Describe implements prometheus.Collector.
func (c *DatastoreReadinessChecker) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect implements prometheus.Collector.
func (c *DatastoreReadinessChecker) Collect(ch chan<- prometheus.Metric) {
	val := 0.0
	if err := c.Ready(context.Background()); err == nil {
		val = 1.0
	}

	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, val)
}
