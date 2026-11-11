// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
)

// seriesValue reads the node's series through Collect so the check does not create the
// series as a side effect the way WithLabelValues would.
func seriesValue(t *testing.T, nodeName string) (float64, bool) {
	t.Helper()

	ch := make(chan prometheus.Metric)
	go func() {
		NodeDrainAwaitingCompletionEventTimestamp.Collect(ch)
		close(ch)
	}()

	var (
		value float64
		found bool
	)

	for m := range ch {
		var metric dto.Metric
		if err := m.Write(&metric); err != nil {
			t.Fatalf("failed to write metric: %v", err)
		}

		for _, label := range metric.GetLabel() {
			if label.GetName() == labelNode && label.GetValue() == nodeName {
				found = true
				value = metric.GetGauge().GetValue()
			}
		}
	}

	return value, found
}

func TestRecordNodeDrainAwaitingCompletionEventTimestampKeepsEarliest(t *testing.T) {
	const nodeName = "node-keeps-earliest"
	NodeDrainAwaitingCompletionEventTimestamp.DeleteLabelValues(nodeName)

	older := time.Now().Add(-3 * time.Hour)
	newer := time.Now().Add(-1 * time.Hour)

	RecordNodeDrainAwaitingCompletionEventTimestamp(nodeName, newer)
	RecordNodeDrainAwaitingCompletionEventTimestamp(nodeName, older)

	value, ok := seriesValue(t, nodeName)
	assert.True(t, ok, "series should exist while an event is waiting")
	assert.Equal(t, float64(older.Unix()), value, "the earliest waiting event's time should win")

	// A newer event's poll must not overwrite the older wait it would hide.
	RecordNodeDrainAwaitingCompletionEventTimestamp(nodeName, newer)

	value, ok = seriesValue(t, nodeName)
	assert.True(t, ok)
	assert.Equal(t, float64(older.Unix()), value, "a newer event's poll should not hide an older wait")

	// The earliest is dropped together with the series, so a later wait starts from its own event.
	NodeDrainAwaitingCompletionEventTimestamp.DeleteLabelValues(nodeName)
	RecordNodeDrainAwaitingCompletionEventTimestamp(nodeName, newer)

	value, ok = seriesValue(t, nodeName)
	assert.True(t, ok)
	assert.Equal(t, float64(newer.Unix()), value, "after the series is dropped, the next wait reports from its own event")
}

func TestRecordNodeDrainAwaitingCompletionEventTimestampSkipsZeroTime(t *testing.T) {
	const nodeName = "node-zero-creation-time"
	NodeDrainAwaitingCompletionEventTimestamp.DeleteLabelValues(nodeName)

	RecordNodeDrainAwaitingCompletionEventTimestamp(nodeName, time.Time{})

	_, ok := seriesValue(t, nodeName)
	assert.False(t, ok, "an event without a creation time should not publish a series")
}
