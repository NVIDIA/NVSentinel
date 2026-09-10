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
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedMapper returns the given results in order, then blocks the poll loop from making
// progress by reporting success forever.
type scriptedMapper struct {
	results []error
	calls   int
}

func (m *scriptedMapper) UpdatePodDevicesAnnotations() (int, error) {
	m.calls++

	if m.calls <= len(m.results) {
		return 0, m.results[m.calls-1]
	}

	return 0, nil
}

// tick drives the loop by hand so these tests do not wait on the real 30s period.
func tick(t *testing.T, ticks chan time.Time, n int) {
	t.Helper()

	for range n {
		ticks <- time.Time{}
	}
}

func TestPollPodDevices_FailuresBelowThreshold_KeepsPolling(t *testing.T) {
	poll := errors.New("kubelet said no")
	mapper := &scriptedMapper{results: []error{poll, poll}}
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- pollPodDevices(ctx, mapper, ticks, 3, newPodMapperMetrics(prometheus.NewRegistry())) }()

	tick(t, ticks, 2)
	// A third tick that succeeds proves the loop was still running rather than having returned.
	tick(t, ticks, 1)
	cancel()

	require.NoError(t, <-done)
	assert.Equal(t, 3, mapper.calls)
}

func TestPollPodDevices_ConsecutiveFailuresReachThreshold_ReturnsError(t *testing.T) {
	poll := errors.New("kubelet said no")
	mapper := &scriptedMapper{results: []error{poll, poll, poll}}
	ticks := make(chan time.Time)

	done := make(chan error, 1)
	go func() { done <- pollPodDevices(context.Background(), mapper, ticks, 3, newPodMapperMetrics(prometheus.NewRegistry())) }()

	tick(t, ticks, 3)

	err := <-done
	require.Error(t, err)
	assert.ErrorIs(t, err, poll)
	assert.Contains(t, err.Error(), "3 consecutive failures")
}

// The streak has to reset, or a collector that fails once every few hours eventually exits for
// no good reason. Two runs of threshold-minus-one failures separated by a success must survive.
func TestPollPodDevices_SuccessBetweenFailures_ResetsTheStreak(t *testing.T) {
	poll := errors.New("kubelet said no")
	mapper := &scriptedMapper{results: []error{poll, poll, nil, poll, poll}}
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- pollPodDevices(ctx, mapper, ticks, 3, newPodMapperMetrics(prometheus.NewRegistry())) }()

	tick(t, ticks, 5)
	cancel()

	require.NoError(t, <-done)
	assert.Equal(t, 5, mapper.calls)
}

func TestPollPodDevices_ContextCancelled_ReturnsNil(t *testing.T) {
	mapper := &scriptedMapper{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, pollPodDevices(ctx, mapper, make(chan time.Time), 10, newPodMapperMetrics(prometheus.NewRegistry())))
	assert.Equal(t, 0, mapper.calls)
}

// A threshold of 0 would mean "exit before the first poll", and a negative one is meaningless.
// Refusing both is better than reinterpreting them as 1.
func TestPollPodDevices_ThresholdBelowOne_ReturnsErrorWithoutPolling(t *testing.T) {
	for _, threshold := range []int{0, -1} {
		mapper := &scriptedMapper{}

		err := pollPodDevices(context.Background(), mapper, make(chan time.Time), threshold, newPodMapperMetrics(prometheus.NewRegistry()))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be at least 1")
		assert.Equal(t, 0, mapper.calls)
	}
}

// Assert only after <-done. A tick that has been received is always processed before the loop
// reaches select again, so once pollPodDevices has returned exactly the sent ticks have been
// fully handled, metrics included. Reading a metric between sends races with the tick still in
// flight, which is a mistake this test made in its first form.
//
// The counter is for alerting on a rising rate, so it must count every failure rather than the
// streak. The gauge is what the exit threshold acts on, so it must follow the streak.
func TestPollPodDevices_Failures_CountedIndependentlyOfTheStreak(t *testing.T) {
	poll := errors.New("kubelet said no")
	// fail, fail, succeed, fail -> 3 failures, but a streak of only 1.
	mapper := &scriptedMapper{results: []error{poll, poll, nil, poll}}
	ticks := make(chan time.Time)
	metrics := newPodMapperMetrics(prometheus.NewRegistry())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- pollPodDevices(ctx, mapper, ticks, 10, metrics) }()

	tick(t, ticks, 4)
	cancel()
	require.NoError(t, <-done)

	assert.Equal(t, 4, mapper.calls)
	assert.Equal(t, 3.0, testutil.ToFloat64(metrics.failures),
		"the counter must count every failure, not the streak")
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.consecutiveFailures),
		"the streak restarted at 1 after the success, so the gauge is 1 and not 3")
}

func TestPollPodDevices_Success_ResetsTheStreakGauge(t *testing.T) {
	poll := errors.New("kubelet said no")
	mapper := &scriptedMapper{results: []error{poll, poll, nil}}
	ticks := make(chan time.Time)
	metrics := newPodMapperMetrics(prometheus.NewRegistry())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- pollPodDevices(ctx, mapper, ticks, 10, metrics) }()

	tick(t, ticks, 3)
	cancel()
	require.NoError(t, <-done)

	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.failures))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.consecutiveFailures))
}

// The exit is loud, but the scrape that precedes it should still show why, so the final streak
// has to be recorded before the threshold returns rather than lost with the process.
func TestPollPodDevices_ThresholdReached_RecordsTheFinalStreak(t *testing.T) {
	poll := errors.New("kubelet said no")
	mapper := &scriptedMapper{results: []error{poll, poll, poll}}
	ticks := make(chan time.Time)
	metrics := newPodMapperMetrics(prometheus.NewRegistry())

	done := make(chan error, 1)
	go func() { done <- pollPodDevices(context.Background(), mapper, ticks, 3, metrics) }()

	tick(t, ticks, 3)
	require.Error(t, <-done)

	assert.Equal(t, 3.0, testutil.ToFloat64(metrics.failures))
	assert.Equal(t, 3.0, testutil.ToFloat64(metrics.consecutiveFailures))
}

// Both names are in the issue and will be alerted on, so a rename should break a test rather
// than silently break someone's alert.
func TestNewPodMapperMetrics_ExposesTheDocumentedNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := newPodMapperMetrics(reg)
	metrics.recordFailure(1)

	families, err := reg.Gather()
	require.NoError(t, err)

	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, f.GetName())
	}

	assert.ElementsMatch(t, []string{
		"metadata_collector_pod_mapper_failures_total",
		"metadata_collector_pod_mapper_consecutive_failures",
	}, names)
}

// main registers on prometheus.DefaultRegisterer, and the endpoint is
// server.WithPrometheusMetrics, which serves prometheus.DefaultGatherer. Those two have to be
// the same registry or /metrics is empty while every unit test still passes against its own
// registry — the exact mismatch that made the change-stream lag metric invisible in #1762.
//
// Registers on the process-wide default, so it must stay the only test that does.
func TestNewPodMapperMetrics_DefaultRegisterer_IsServedByTheDefaultGatherer(t *testing.T) {
	metrics := newPodMapperMetrics(prometheus.DefaultRegisterer)
	metrics.recordFailure(1)

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	var found []string

	for _, f := range families {
		switch f.GetName() {
		case "metadata_collector_pod_mapper_failures_total",
			"metadata_collector_pod_mapper_consecutive_failures":
			found = append(found, f.GetName())
		}
	}

	assert.ElementsMatch(t, []string{
		"metadata_collector_pod_mapper_failures_total",
		"metadata_collector_pod_mapper_consecutive_failures",
	}, found, "the endpoint serves the default gatherer, so the metrics must be on the default registry")
}

func TestPollPodDevices_DefaultThreshold_RidesOutAtLeastAMinute(t *testing.T) {
	tolerated := time.Duration(defaultMaxConsecutivePodMapperFailures-1) * defaultPodDeviceMonitorPeriod

	assert.GreaterOrEqual(t, tolerated, time.Minute,
		"the default must outlast a credential rotation, which is what #1767 was")
}
