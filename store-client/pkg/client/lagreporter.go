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

package client

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nvidia/nvsentinel/store-client/pkg/lagstate"
)

// Change stream lag metrics, per ADR-054. The providers own the timestamps; this file owns the
// metrics that read them.
const (
	lagSecondsName = "change_stream_lag_seconds"
	lagKnownName   = "change_stream_lag_known"
)

// lagCollector reports the lag of every watcher registered against one registry.
//
// It is a Collector rather than a gauge because lag has to be computed at scrape time: a gauge
// set on each read would freeze at its last update, which is exactly the case that needs to be
// visible.
//
// One collector serves every client, rather than one collector per client. Because `client` is a
// variable label, per-client collectors would all carry identical descriptors, and Prometheus
// identifies a collector by its descriptors: registering the second one returns
// AlreadyRegisteredError and that client's lag is never exported.
type lagCollector struct {
	now func() time.Time

	lagDesc   *prometheus.Desc
	knownDesc *prometheus.Desc

	mu        sync.Mutex
	providers map[string]lagstate.Provider
}

func newLagCollector() *lagCollector {
	return &lagCollector{
		now:       time.Now,
		providers: map[string]lagstate.Provider{},
		lagDesc: prometheus.NewDesc(
			lagSecondsName,
			"Seconds since this consumer last had evidence it was caught up with its own change "+
				"stream, whether from an empty batch or from the server-side timestamp of the last "+
				"event it read. Not reported until one of those has been observed.",
			[]string{"client"},
			nil,
		),
		knownDesc: prometheus.NewDesc(
			lagKnownName,
			"1 once "+lagSecondsName+" can be computed for this consumer, 0 before then. A watcher "+
				"that never starts, or is wedged before its first read, stays at 0.",
			[]string{"client"},
			nil,
		),
	}
}

// add registers a client's provider, reporting whether it was newly added.
func (c *lagCollector) add(clientName string, provider lagstate.Provider) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.providers[clientName]; exists {
		return false
	}

	c.providers[clientName] = provider

	return true
}

func (c *lagCollector) snapshot() map[string]lagstate.Provider {
	c.mu.Lock()
	defer c.mu.Unlock()

	providers := make(map[string]lagstate.Provider, len(c.providers))
	for clientName, provider := range c.providers {
		providers[clientName] = provider
	}

	return providers
}

func (c *lagCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.lagDesc

	ch <- c.knownDesc
}

func (c *lagCollector) Collect(ch chan<- prometheus.Metric) {
	for clientName, provider := range c.snapshot() {
		lastEmptyBatch, lastEventRead := provider.LagState()

		observed := lastEmptyBatch
		if lastEventRead.After(observed) {
			observed = lastEventRead
		}

		if observed.IsZero() {
			ch <- prometheus.MustNewConstMetric(c.knownDesc, prometheus.GaugeValue, 0, clientName)

			continue
		}

		ch <- prometheus.MustNewConstMetric(c.knownDesc, prometheus.GaugeValue, 1, clientName)

		// lastEventRead is a database server timestamp compared against the local clock, so skew
		// can make a caught-up consumer look slightly ahead of itself. Report that as zero lag.
		lag := c.now().Sub(observed).Seconds()
		if lag < 0 {
			lag = 0
		}

		ch <- prometheus.MustNewConstMetric(c.lagDesc, prometheus.GaugeValue, lag, clientName)
	}
}

// collectors holds the one collector per registry, so a second client joins the existing
// collector instead of trying to register a duplicate.
var (
	collectorsMu sync.Mutex
	collectors   = map[prometheus.Registerer]*lagCollector{}
)

// RegisterChangeStreamLag exports the lag metrics for watcher, if it reports lag state. Watchers
// that do not are left alone, so this is safe to call on any watcher.
//
// reg may be nil, in which case the default registry is used. Callers that serve a different
// registry must pass it: a consumer serving only controller-runtime's registry would otherwise
// never see these metrics.
func RegisterChangeStreamLag(reg prometheus.Registerer, clientName string, watcher any) {
	provider, ok := watcher.(lagstate.Provider)
	if !ok {
		slog.Debug("Change stream watcher does not report lag state; skipping lag metrics",
			"client", clientName, "watcherType", fmt.Sprintf("%T", watcher))

		return
	}

	if clientName == "" {
		slog.Warn("Skipping change stream lag metrics: client name is empty")

		return
	}

	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	collector, usable := collectorFor(reg)
	if !usable {
		return
	}

	if !collector.add(clientName, provider) {
		return
	}

	slog.Info("Registered change stream lag metrics", "client", clientName)
}

// collectorFor returns the collector serving reg, creating and registering it on first use. The
// second return value reports whether the collector is usable; a failed registration is logged
// once and leaves the metrics off rather than failing the caller's startup.
func collectorFor(reg prometheus.Registerer) (*lagCollector, bool) {
	collectorsMu.Lock()
	defer collectorsMu.Unlock()

	if collector, exists := collectors[reg]; exists {
		return collector, true
	}

	collector := newLagCollector()

	if err := reg.Register(collector); err != nil {
		slog.Warn("Failed to register change stream lag metrics", "error", err)

		return collector, false
	}

	collectors[reg] = collector

	return collector, true
}
