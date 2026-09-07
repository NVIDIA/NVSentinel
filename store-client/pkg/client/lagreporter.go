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
	"errors"
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

// lagCollector reports one watcher's lag. It is a Collector rather than a gauge because lag has
// to be computed at scrape time: a gauge set on each read would freeze at its last update, which
// is exactly the case that needs to be visible.
type lagCollector struct {
	clientName string
	provider   lagstate.Provider
	now        func() time.Time

	lagDesc   *prometheus.Desc
	knownDesc *prometheus.Desc
}

func newLagCollector(clientName string, provider lagstate.Provider) *lagCollector {
	return &lagCollector{
		clientName: clientName,
		provider:   provider,
		now:        time.Now,
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

func (c *lagCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.lagDesc

	ch <- c.knownDesc
}

func (c *lagCollector) Collect(ch chan<- prometheus.Metric) {
	lastEmptyBatch, lastEventRead := c.provider.LagState()

	observed := lastEmptyBatch
	if lastEventRead.After(observed) {
		observed = lastEventRead
	}

	if observed.IsZero() {
		ch <- prometheus.MustNewConstMetric(c.knownDesc, prometheus.GaugeValue, 0, c.clientName)

		return
	}

	ch <- prometheus.MustNewConstMetric(c.knownDesc, prometheus.GaugeValue, 1, c.clientName)

	// lastEventRead is a database server timestamp compared against the local clock, so skew can
	// make a caught-up consumer look slightly ahead of itself. Report that as zero lag.
	lag := c.now().Sub(observed).Seconds()
	if lag < 0 {
		lag = 0
	}

	ch <- prometheus.MustNewConstMetric(c.lagDesc, prometheus.GaugeValue, lag, c.clientName)
}

// registeredLag tracks which (registerer, client) pairs already have a collector, because a
// second collector for the same client would emit a duplicate label set and fail the whole
// scrape rather than just its own metric.
var (
	registeredLagMu sync.Mutex
	registeredLag   = map[lagKey]struct{}{}
)

type lagKey struct {
	registerer prometheus.Registerer
	client     string
}

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

	registeredLagMu.Lock()
	defer registeredLagMu.Unlock()

	key := lagKey{registerer: reg, client: clientName}
	if _, exists := registeredLag[key]; exists {
		return
	}

	if err := reg.Register(newLagCollector(clientName, provider)); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if errors.As(err, &alreadyRegistered) {
			registeredLag[key] = struct{}{}

			return
		}

		slog.Warn("Failed to register change stream lag metrics",
			"client", clientName, "error", err)

		return
	}

	registeredLag[key] = struct{}{}

	slog.Info("Registered change stream lag metrics", "client", clientName)
}
