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
	go func() { done <- pollPodDevices(ctx, mapper, ticks, 3) }()

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
	go func() { done <- pollPodDevices(context.Background(), mapper, ticks, 3) }()

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
	go func() { done <- pollPodDevices(ctx, mapper, ticks, 3) }()

	tick(t, ticks, 5)
	cancel()

	require.NoError(t, <-done)
	assert.Equal(t, 5, mapper.calls)
}

func TestPollPodDevices_ContextCancelled_ReturnsNil(t *testing.T) {
	mapper := &scriptedMapper{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, pollPodDevices(ctx, mapper, make(chan time.Time), 10))
	assert.Equal(t, 0, mapper.calls)
}

// A threshold of 0 would mean "exit before the first poll", and a negative one is meaningless.
// Refusing both is better than reinterpreting them as 1.
func TestPollPodDevices_ThresholdBelowOne_ReturnsErrorWithoutPolling(t *testing.T) {
	for _, threshold := range []int{0, -1} {
		mapper := &scriptedMapper{}

		err := pollPodDevices(context.Background(), mapper, make(chan time.Time), threshold)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be at least 1")
		assert.Equal(t, 0, mapper.calls)
	}
}

func TestPollPodDevices_DefaultThreshold_RidesOutAtLeastAMinute(t *testing.T) {
	tolerated := time.Duration(defaultMaxConsecutivePodMapperFailures-1) * defaultPodDeviceMonitorPeriod

	assert.GreaterOrEqual(t, tolerated, time.Minute,
		"the default must outlast a credential rotation, which is what #1767 was")
}
