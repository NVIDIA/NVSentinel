// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0

package monitor

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
)

// namedCheck is a check that reports a caller-supplied name and no events, so
// a test can construct a monitor over an arbitrary set of check names.
type namedCheck struct{ name string }

func (c *namedCheck) Name() string                        { return c.name }
func (c *namedCheck) Run() ([]*pb.HealthEvent, error)     { return nil, nil }
func (c *namedCheck) Prepare() ([]*pb.HealthEvent, error) { return nil, nil }
func (c *namedCheck) Commit()                             {}
func (c *namedCheck) Discard()                            {}

// childrenFor returns the label sets and values of one metric family, limited
// to the series carrying the given node label.
func childrenFor(t *testing.T, family, node string) map[string]float64 {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	out := map[string]float64{}

	for _, mf := range families {
		if mf.GetName() != family {
			continue
		}

		for _, m := range mf.GetMetric() {
			key := ""
			matches := false

			for _, l := range m.GetLabel() {
				if l.GetName() == "node" {
					matches = l.GetValue() == node
					continue
				}

				key += l.GetName() + "=" + l.GetValue() + ";"
			}

			if matches {
				out[key] = m.GetCounter().GetValue()
			}
		}
	}

	return out
}

// TestNewNICHealthMonitor_PreInitialisesCounters: constructing the monitor must
// make the change-gated counters present at zero, because the checks only emit
// on change and a CounterVec with no children exports nothing at all. Drives
// the real constructor so the wiring is covered, not just the helper.
func TestNewNICHealthMonitor_PreInitialisesCounters(t *testing.T) {
	const node = "preinit-node"

	enabled := []checks.TransactionalCheck{
		&namedCheck{name: checks.InfiniBandStateCheckName},
		&namedCheck{name: checks.EthernetStateCheckName},
	}

	NewNICHealthMonitor(node, &publishFailOnceClient{}, "127.0.0.1:5555",
		enabled, time.Second)

	sent := childrenFor(t, "nic_health_monitor_health_events_sent_total", node)
	require.Len(t, sent, 4, "two checks x is_fatal false/true")

	for _, name := range []string{checks.InfiniBandStateCheckName, checks.EthernetStateCheckName} {
		for _, isFatal := range []string{"false", "true"} {
			key := "check=" + name + ";is_fatal=" + isFatal + ";"
			value, ok := sent[key]
			assert.True(t, ok, "missing pre-initialised series for %s", key)
			assert.Zero(t, value, "pre-initialised series must start at 0: %s", key)
		}
	}

	deferred := childrenFor(t, "nic_health_monitor_first_poll_deferred_total", node)
	require.Len(t, deferred, 2, "one per check")

	for _, name := range []string{checks.InfiniBandStateCheckName, checks.EthernetStateCheckName} {
		value, ok := deferred["check="+name+";"]
		assert.True(t, ok, "missing pre-initialised series for %s", name)
		assert.Zero(t, value)
	}
}

// TestCounterVec_WithNoChildren_ExportsNoFamily is the control for the test
// above. It pins the client-library behaviour the fix exists to work around:
// an untouched CounterVec yields no family at all, so "absent" and "zero" are
// different states and the assertions above are capable of failing.
func TestCounterVec_WithNoChildren_ExportsNoFamily(t *testing.T) {
	registry := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "control_counter_total",
		Help: "control",
	}, []string{"node", "check"})
	registry.MustRegister(counter)

	families, err := registry.Gather()
	require.NoError(t, err)
	assert.Empty(t, families, "a CounterVec with no children exports no family")

	counter.WithLabelValues("n", "c")

	families, err = registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1, "one touched child makes the family appear")
	assert.Equal(t, "control_counter_total", families[0].GetName())
	assert.Zero(t, families[0].GetMetric()[0].GetCounter().GetValue())
}
