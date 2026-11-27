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
	"flag"
	"io"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An older pod spec or host setup still passes the flags of the pod device mapper. metadata-collector must parse
// them, including a bare boolean --pod-mapper-enabled, and report which ones it ignores.
func TestMovedFlags_OlderCommandLine_ParsesAndReportsThem(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.String("output-path", "", "")

	for _, name := range movedFlags {
		flags.String(name, "", "")
	}

	flags.Bool(movedBoolFlag, true, "")

	require.NoError(t, flags.Parse([]string{
		"--output-path=/var/lib/nvsentinel/gpu_metadata.json",
		"--pod-mapper-enabled",
		"--pod-mapper-max-consecutive-failures=10",
		"--config=/etc/config/config.toml",
		"--kubeconfig=/etc/kubeconfig",
		"--kubelet-kubeconfig=/etc/kubelet.kubeconfig",
	}))
	assert.ElementsMatch(t, []string{
		"pod-mapper-enabled", "pod-mapper-max-consecutive-failures", "config", "kubeconfig", "kubelet-kubeconfig",
	}, setMovedFlags(flags))
}

func TestMovedFlags_CurrentCommandLine_ReportsNone(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.String("output-path", "", "")

	for _, name := range movedFlags {
		flags.String(name, "", "")
	}

	require.NoError(t, flags.Parse([]string{"--output-path=/var/lib/nvsentinel/gpu_metadata.json"}))
	assert.Empty(t, setMovedFlags(flags))
}

// Alerts on a stalled collection depend on this metric being scrapeable at 0 before collection
// completes, so its name and starting value are part of the contract.
func TestCollectionCompletedGauge_StartsAtZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	gauge := newCollectionCompletedGauge(reg)

	count, err := testutil.GatherAndCount(reg, "metadata_collector_collection_completed_timestamp_seconds")
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Zero(t, testutil.ToFloat64(gauge))

	gauge.SetToCurrentTime()
	assert.InDelta(t, float64(time.Now().Unix()), testutil.ToFloat64(gauge), 5)
}
