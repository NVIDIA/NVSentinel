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

package kubernetes

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// gatherFamily returns the named metric family from the default registry, or nil when
// the family is absent. Absent and present-but-empty are different outcomes here: a
// CounterVec with no children exports no family at all, which is the bug under test.
func gatherFamily(t *testing.T, name string) *dto.MetricFamily {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}

	return nil
}

// resetAndInit puts the shared counters back to the state a freshly started process is
// in. Other tests in this package call Reset() on these vectors for isolation, which
// deletes the children init() created, so these tests would otherwise depend on order.
//
// Note that Reset() undoing the pre-initialisation is exactly the failure mode being
// guarded against: nothing in production calls it, but anything that did would make the
// families disappear again.
func resetAndInit() {
	droppedWritesCounter.Reset()
	droppedBatchesCounter.Reset()
	nodeConditionUpdateCounter.Reset()
	nodeEventOperationsCounter.Reset()
	initMetrics()
}

func TestInitMetrics_BeforeAnyEvent_CountersArePresentAtZero(t *testing.T) {
	resetAndInit()

	for name, wantSeries := range map[string]int{
		"k8s_platform_connector_dropped_writes_total":        16, // 2 operations x 4 reasons x 2 health
		"k8s_platform_connector_dropped_batches_total":       4,  // 4 reasons
		"k8s_platform_connector_node_condition_update_total": 3,  // 3 statuses
		"k8s_platform_connector_node_event_operations_total": 5,  // create is never skipped
	} {
		family := gatherFamily(t, name)
		require.NotNilf(t, family, "%s is absent from the registry, so an empty query cannot "+
			"distinguish no occurrences from the exporter being down", name)
		require.Lenf(t, family.GetMetric(), wantSeries, "%s series count", name)

		for _, m := range family.GetMetric() {
			require.Zerof(t, m.GetCounter().GetValue(), "%s %v should start at zero", name, m.GetLabel())
		}
	}
}

// The reasons initMetrics pre-creates must be exactly what writeDropReason can return.
// Without this, adding a branch there silently leaves that series uninitialised again.
func TestWriteDropReason_EveryReturnValue_IsPreInitialised(t *testing.T) {
	resetAndInit()

	family := gatherFamily(t, "k8s_platform_connector_dropped_batches_total")
	require.NotNil(t, family)

	got := make(map[string]bool, len(family.GetMetric()))

	for _, m := range family.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "reason" {
				got[l.GetValue()] = true
			}
		}
	}

	for _, reason := range dropReasons {
		require.Truef(t, got[reason], "reason %q is returned by writeDropReason but not pre-initialised", reason)
	}

	require.Len(t, got, len(dropReasons), "pre-initialised reasons that writeDropReason never returns")
}

// Control for the test above: a CounterVec really does export nothing until it has a
// child, so the assertions are not passing for some unrelated reason.
func TestCounterVec_WithNoChildren_ExportsNoFamily(t *testing.T) {
	registry := prometheus.NewRegistry()
	vec := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_uninitialised_total",
		Help: "control",
	}, []string{"label"})
	registry.MustRegister(vec)

	families, err := registry.Gather()
	require.NoError(t, err)
	require.Empty(t, families, "a CounterVec with no children should export no family")

	vec.WithLabelValues("a")

	families, err = registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1, "one child should make the family appear")
	require.Zero(t, families[0].GetMetric()[0].GetCounter().GetValue())
}
