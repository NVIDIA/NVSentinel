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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/lagstate"
)

// fakeLagProvider stands in for a watcher, so the collector can be exercised without a datastore.
type fakeLagProvider struct {
	lastEmptyBatch time.Time
	lastEventRead  time.Time
}

func (f fakeLagProvider) LagState() (lastEmptyBatch, lastEventRead time.Time) {
	return f.lastEmptyBatch, f.lastEventRead
}

// nonProvider is a watcher-shaped value that does not report lag state.
type nonProvider struct{}

func collectorAt(now time.Time, provider fakeLagProvider) *lagCollector {
	collector := newLagCollector()
	collector.now = func() time.Time { return now }
	collector.add("test-client", provider)

	return collector
}

// scrape gathers the collector through a pedantic registry, so a malformed descriptor or a
// duplicate label set fails here rather than at scrape time in a live process. Each metric has
// one series, keyed by name; a name absent from the result was not reported at all.
func scrape(t *testing.T, collector prometheus.Collector) map[string]float64 {
	t.Helper()

	registry := prometheus.NewPedanticRegistry()
	require.NoError(t, registry.Register(collector))

	families, err := registry.Gather()
	require.NoError(t, err)

	values := map[string]float64{}

	for _, family := range families {
		for _, metric := range family.GetMetric() {
			require.Len(t, metric.GetLabel(), 1)
			assert.Equal(t, "client", metric.GetLabel()[0].GetName())

			values[family.GetName()] = metric.GetGauge().GetValue()
		}
	}

	return values
}

// Before the first read or empty batch the watcher has no evidence either way. Reporting zero
// would claim a caught-up consumer that has not been observed at all, so the lag series must be
// absent and only change_stream_lag_known is reported.
func TestCollect_NoObservationYet_OmitsLagSeriesAndReportsLagUnknown(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	reported := scrape(t, collectorAt(now, fakeLagProvider{}))

	assert.NotContains(t, reported, lagSecondsName)
	assert.Equal(t, map[string]float64{lagKnownName: 0}, reported)
}

func TestCollect_AfterFirstObservation_ReportsLagKnownAndSeconds(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	provider := fakeLagProvider{lastEmptyBatch: now.Add(-30 * time.Second)}

	reported := scrape(t, collectorAt(now, provider))

	assert.Equal(t, float64(1), reported[lagKnownName])
	assert.Equal(t, float64(30), reported[lagSecondsName])
}

// Lag is measured from the more recent of the two observations: a consumer that just read an old
// event is behind, but one that has since seen an empty batch is not.
func TestCollect_TwoObservations_MeasuresFromTheMoreRecent(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	tests := map[string]struct {
		provider fakeLagProvider
		expected float64
	}{
		"empty batch is newer": {
			provider: fakeLagProvider{
				lastEmptyBatch: now.Add(-5 * time.Second),
				lastEventRead:  now.Add(-2 * time.Hour),
			},
			expected: 5,
		},
		"event read is newer": {
			provider: fakeLagProvider{
				lastEmptyBatch: now.Add(-2 * time.Hour),
				lastEventRead:  now.Add(-5 * time.Second),
			},
			expected: 5,
		},
		"only an event read, and it is old": {
			provider: fakeLagProvider{lastEventRead: now.Add(-20 * time.Minute)},
			expected: 1200,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, test.expected, scrape(t, collectorAt(now, test.provider))[lagSecondsName])
		})
	}
}

// Event timestamps come from the database server, so skew against the local clock can put an
// observation in the future. Report that as caught up rather than as negative lag.
func TestCollect_ObservationInTheFuture_ReportsZeroLag(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	provider := fakeLagProvider{lastEventRead: now.Add(2 * time.Second)}

	reported := scrape(t, collectorAt(now, provider))

	assert.Equal(t, float64(0), reported[lagSecondsName])
}

// Lag is computed at scrape time, so a stuck consumer keeps growing rather than freezing at the
// value it had when it stopped reading.
func TestCollect_StuckConsumer_LagGrowsBetweenScrapes(t *testing.T) {
	observed := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	now := observed
	collector := newLagCollector()
	collector.now = func() time.Time { return now }
	collector.add("test-client", fakeLagProvider{lastEmptyBatch: observed})

	assert.Equal(t, float64(0), scrape(t, collector)[lagSecondsName])

	now = observed.Add(10 * time.Minute)

	assert.Equal(t, float64(600), scrape(t, collector)[lagSecondsName])
}

// A consumer that serves its own registry rather than the default one must still get the
// metrics. A test that only checked the default registry would pass while that endpoint stayed
// empty, which is the failure this guards.
func TestRegisterChangeStreamLag_SuppliedRegistry_ExportsThereNotOnDefault(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()

	RegisterChangeStreamLag(registry, t.Name(), fakeLagProvider{lastEmptyBatch: time.Now()})

	assert.ElementsMatch(t, []string{lagSecondsName, lagKnownName}, lagMetricNames(t, registry),
		"both lag metrics should be on the supplied registry")

	assert.Empty(t, lagMetricNames(t, prometheus.DefaultGatherer),
		"nothing should have reached the default registry")
}

func TestRegisterChangeStreamLag_WatcherWithoutLagState_RegistersNothing(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()

	RegisterChangeStreamLag(registry, t.Name(), nonProvider{})

	assert.Empty(t, lagMetricNames(t, registry))
}

// A second collector for the same client would emit a duplicate label set and fail the whole
// scrape rather than just its own metric, so registration has to be idempotent.
func TestRegisterChangeStreamLag_SameClientTwice_ExportsOneSeries(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	provider := fakeLagProvider{lastEmptyBatch: time.Now()}

	RegisterChangeStreamLag(registry, t.Name(), provider)
	RegisterChangeStreamLag(registry, t.Name(), provider)

	families, err := registry.Gather()
	require.NoError(t, err, "a duplicate label set would fail the gather")

	for _, family := range families {
		assert.Len(t, family.GetMetric(), 1, family.GetName())
	}
}

// lagMetricNames returns which of the two lag metrics the gatherer reports.
func lagMetricNames(t *testing.T, gatherer prometheus.Gatherer) []string {
	t.Helper()

	families, err := gatherer.Gather()
	require.NoError(t, err)

	var names []string

	for _, family := range families {
		if name := family.GetName(); name == lagSecondsName || name == lagKnownName {
			names = append(names, name)
		}
	}

	return names
}

// Two watchers in one process must both be visible. Per-client collectors would carry identical
// descriptors, so the registry would treat the second as already registered and silently drop
// it, leaving that consumer looking like it had never been observed.
func TestRegisterChangeStreamLag_TwoClients_ExportsBoth(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	observed := time.Now()

	RegisterChangeStreamLag(registry, "client-a", fakeLagProvider{lastEmptyBatch: observed})
	RegisterChangeStreamLag(registry, "client-b", fakeLagProvider{lastEventRead: observed})

	families, err := registry.Gather()
	require.NoError(t, err)

	seen := map[string][]string{}

	for _, family := range families {
		for _, metric := range family.GetMetric() {
			seen[family.GetName()] = append(seen[family.GetName()], metric.GetLabel()[0].GetValue())
		}
	}

	assert.ElementsMatch(t, []string{"client-a", "client-b"}, seen[lagKnownName])
	assert.ElementsMatch(t, []string{"client-a", "client-b"}, seen[lagSecondsName])
}

// wrappedLagProvider is a ChangeStreamWatcher that reports lag state, so the resume-control
// wrapper can be built over something real rather than a bare stub.
type wrappedLagProvider struct {
	ChangeStreamWatcher

	observed time.Time
}

func (w *wrappedLagProvider) LagState() (lastEmptyBatch, lastEventRead time.Time) {
	return w.observed, time.Time{}
}

// Every consumer's watcher reaches RegisterChangeStreamLag through the resume-control wrapper,
// because that is what the client factory returns. Before this pass-through existed the
// assertion inside RegisterChangeStreamLag answered for the wrapper, so no MongoDB consumer
// exported lag at all while the tests passed against unwrapped stubs.
func TestRegisterChangeStreamLag_ResumeControlWrapper_StillExports(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	inner := &wrappedLagProvider{observed: time.Now()}
	wrapped := NewChangeStreamWatcherWithResumeControl(inner, ResumeControlDecision{})

	require.Implements(t, (*lagstate.Provider)(nil), wrapped)

	RegisterChangeStreamLag(registry, t.Name(), wrapped)

	assert.ElementsMatch(t, []string{lagSecondsName, lagKnownName}, lagMetricNames(t, registry))
}

// A wrapper over a watcher that does not report lag must yield "unknown", never a spurious zero.
func TestLagState_ResumeControlWrapperOverPlainWatcher_ReportsUnknown(t *testing.T) {
	wrapped := NewChangeStreamWatcherWithResumeControl(nonProviderWatcher{}, ResumeControlDecision{})

	provider, ok := wrapped.(lagstate.Provider)
	require.True(t, ok)

	lastEmptyBatch, lastEventRead := provider.LagState()
	assert.True(t, lastEmptyBatch.IsZero())
	assert.True(t, lastEventRead.IsZero())
}

type nonProviderWatcher struct{ ChangeStreamWatcher }
