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

package prom

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
)

func event(agent, checkName string, action protos.RecommendedAction, isHealthy bool) *protos.HealthEvent {
	return &protos.HealthEvent{
		Agent:             agent,
		CheckName:         checkName,
		NodeName:          "node-1",
		RecommendedAction: action,
		IsFatal:           action != protos.RecommendedAction_NONE,
		IsHealthy:         isHealthy,
	}
}

// counter reads the current value of one label combination.
func counter(t *testing.T, node, agent, checkName, action, isFatal, isHealthy string) float64 {
	t.Helper()

	metric, err := healthEventsTotal.GetMetricWithLabelValues(node, agent, checkName, action, isFatal, isHealthy)
	require.NoError(t, err)

	return testutil.ToFloat64(metric)
}

func TestRecordEvent_FatalEvent_CountsAgainstItsActionAndAgent(t *testing.T) {
	before := counter(t, "node-1", "syslog-health-monitor", "SysLogsXIDError", "CONTACT_SUPPORT", "true", "false")

	recordEvent(event("syslog-health-monitor", "SysLogsXIDError",
		protos.RecommendedAction_CONTACT_SUPPORT, false))

	assert.Equal(t, before+1,
		counter(t, "node-1", "syslog-health-monitor", "SysLogsXIDError", "CONTACT_SUPPORT", "true", "false"))
}

func TestRecordEvent_NonActionableEvent_IsStillCounted(t *testing.T) {
	// 99% of this fleet's events are NONE. They must be counted, not dropped, because the
	// ratio of NONE to actionable is itself the thing operators need to see.
	before := counter(t, "node-1", "gpu-health-monitor", "GpuPowerWatch", "NONE", "false", "false")

	recordEvent(event("gpu-health-monitor", "GpuPowerWatch", protos.RecommendedAction_NONE, false))

	assert.Equal(t, before+1, counter(t, "node-1", "gpu-health-monitor", "GpuPowerWatch", "NONE", "false", "false"))
}

func TestRecordEvent_HealthyAndUnhealthy_AreSeparateSeries(t *testing.T) {
	unhealthyBefore := counter(t, "node-1", "gpu-health-monitor", "GpuNvlinkWatch", "NONE", "false", "false")
	healthyBefore := counter(t, "node-1", "gpu-health-monitor", "GpuNvlinkWatch", "NONE", "false", "true")

	recordEvent(event("gpu-health-monitor", "GpuNvlinkWatch", protos.RecommendedAction_NONE, false))

	assert.Equal(t, unhealthyBefore+1,
		counter(t, "node-1", "gpu-health-monitor", "GpuNvlinkWatch", "NONE", "false", "false"))
	assert.Equal(t, healthyBefore,
		counter(t, "node-1", "gpu-health-monitor", "GpuNvlinkWatch", "NONE", "false", "true"),
		"recovery series must not move when an unhealthy event is recorded")
}

func TestRecordEvent_IsFatalDisagreeingWithAction_IsVisibleRatherThanHidden(t *testing.T) {
	// The XID 45 bug in #1710 published isFatal=true alongside an action the catalogue
	// treats as non-actionable. Keeping both labels is what makes that visible.
	e := event("health-events-analyzer", "RepeatedXIDErrorOnSameGPU", protos.RecommendedAction_NONE, false)
	e.IsFatal = true

	before := counter(t, "node-1", "health-events-analyzer", "RepeatedXIDErrorOnSameGPU", "NONE", "true", "false")

	recordEvent(e)

	assert.Equal(t, before+1,
		counter(t, "node-1", "health-events-analyzer", "RepeatedXIDErrorOnSameGPU", "NONE", "true", "false"))
}

func TestRecordEvent_NilEvent_DoesNotPanic(t *testing.T) {
	assert.NotPanics(t, func() { recordEvent(nil) })
}

// startConnector runs the processing loop and stops it when the test ends. Cancellation
// alone cannot release a worker parked in Dequeue (see the cancellation test below), so the
// queue has to be shut down or the goroutine outlives the test.
func startConnector(t *testing.T, ctx context.Context, buffer *ringbuffer.RingBuffer, c *PromConnector) {
	t.Helper()

	done := make(chan struct{})

	go func() {
		c.FetchAndProcessHealthMetric(ctx)
		close(done)
	}()

	t.Cleanup(func() {
		buffer.ShutDownHealthMetricQueue()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("processing loop did not stop during cleanup")
		}
	})
}

func TestFetchAndProcessHealthMetric_QueuedBatch_CountsEveryEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buffer := ringbuffer.NewRingBuffer("prom-test-batch", ctx)
	connector := InitializePromConnector(buffer)

	before := counter(t, "node-1", "nic-health-monitor", "NicLinkWatch", "RESTART_BM", "true", "false")

	buffer.Enqueue(ringbuffer.NewQueuedHealthEvents(&protos.HealthEvents{
		Version: 1,
		Events: []*protos.HealthEvent{
			event("nic-health-monitor", "NicLinkWatch", protos.RecommendedAction_RESTART_BM, false),
			event("nic-health-monitor", "NicLinkWatch", protos.RecommendedAction_RESTART_BM, false),
		},
	}))

	startConnector(t, ctx, buffer, connector)

	assert.Eventually(t, func() bool {
		return counter(t, "node-1", "nic-health-monitor", "NicLinkWatch", "RESTART_BM", "true", "false") == before+2
	}, 5*time.Second, 10*time.Millisecond, "both events in the batch should be counted")
}

func TestFetchAndProcessHealthMetric_EmptyBatch_CompletesWithoutStalling(t *testing.T) {
	// An empty batch must be completed rather than left in flight, or the shared queue
	// stalls behind a metrics connector that cannot fail.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buffer := ringbuffer.NewRingBuffer("prom-test-empty", ctx)
	connector := InitializePromConnector(buffer)

	startConnector(t, ctx, buffer, connector)

	buffer.Enqueue(ringbuffer.NewQueuedHealthEvents(&protos.HealthEvents{Version: 1}))

	before := counter(t, "node-1", "csp-health-monitor", "CspMaintenance", "NONE", "false", "false")

	buffer.Enqueue(ringbuffer.NewQueuedHealthEvents(&protos.HealthEvents{
		Version: 1,
		Events: []*protos.HealthEvent{
			event("csp-health-monitor", "CspMaintenance", protos.RecommendedAction_NONE, false),
		},
	}))

	assert.Eventually(t, func() bool {
		return counter(t, "node-1", "csp-health-monitor", "CspMaintenance", "NONE", "false", "false") == before+1
	}, 5*time.Second, 10*time.Millisecond, "an empty batch must not block the next one")
}

// TestFetchAndProcessHealthMetric_QueueShutdown_Returns covers the shutdown path that
// actually releases an idle loop. Dequeue blocks in the workqueue's Get, which returns
// only on an item or on shutdown, so shutting the queue down is what unblocks it.
func TestFetchAndProcessHealthMetric_QueueShutdown_Returns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buffer := ringbuffer.NewRingBuffer("prom-test-shutdown", ctx)
	connector := InitializePromConnector(buffer)

	done := make(chan struct{})

	go func() {
		connector.FetchAndProcessHealthMetric(ctx)
		close(done)
	}()

	// Deliberately no cancel() here, so only the queue shutdown can end the loop.
	buffer.ShutDownHealthMetricQueue()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("FetchAndProcessHealthMetric did not return after queue shutdown")
	}
}

// TestFetchAndProcessHealthMetric_ContextCanceled_ReturnsOnNextItem documents the limit of
// cancellation: an idle loop parked in Dequeue does not observe it, because the workqueue's
// Get is not context-aware. Cancellation takes effect as soon as anything unblocks Get.
// This mirrors the store connector's loop, so it is a property of the shared ring buffer
// rather than of this connector.
func TestFetchAndProcessHealthMetric_ContextCanceled_ReturnsOnNextItem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	buffer := ringbuffer.NewRingBuffer("prom-test-cancel", ctx)
	connector := InitializePromConnector(buffer)

	done := make(chan struct{})

	go func() {
		connector.FetchAndProcessHealthMetric(ctx)
		close(done)
	}()

	cancel()

	// Unblocks Dequeue without shutting the queue down, so the return is attributable to
	// the cancellation rather than to shutdown.
	buffer.Enqueue(ringbuffer.NewQueuedHealthEvents(&protos.HealthEvents{
		Version: 1,
		Events: []*protos.HealthEvent{
			event("gpu-health-monitor", "GpuPowerWatch", protos.RecommendedAction_NONE, false),
		},
	}))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("FetchAndProcessHealthMetric did not return after context cancellation")
	}
}
