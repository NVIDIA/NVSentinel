// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package monitor

import (
	"context"
	"time"
)

// defaultPublishDrain is the window every shutdown publish shares, armed
// once at SIGTERM. The events nearest shutdown are the ones nothing
// else can report: the poll-stall event is the only record that the node
// was blind while liveness restarted this container, and a poll that
// completes during shutdown carries the recoveries that ended the stall.
// On the cancelled context the publisher drops a batch for good — a
// retrying send dies at once, a batch produced after the cancel is never
// sent — so a publish that starts after the cancellation runs detached
// from it, bounded by this window.
//
// One deadline covers the whole shutdown, not one window per publish: the
// loops can start another poll after the cancellation (the select between
// ctx.Done and the ticker picks randomly while both are ready), so
// per-publish windows could stack past the pod's termination grace
// period. A publish that starts before the cancellation runs on the
// caller's context alone — a direct-mode batch keeps its full retry
// window, so the drain never caps steady-state publishes. The pod's
// termination grace period remains the outer bound; the chart renders
// terminationGracePeriodSeconds as 30s with this window written into its
// budget, and the rest of shutdown (metrics server 5s grace, loops
// stopping) fits beside it.
const defaultPublishDrain = 10 * time.Second

// armPublishDrain sets the shared deadline every post-cancellation publish
// draws on. Two racers can set it — ArmPublishDrain's AfterFunc callback,
// and publishDrainContext, which can observe a cancellation that callback
// goroutine has not run for yet — so the store is a compare-and-swap on
// zero: whichever sees the cancellation first fixes the window, and the
// one that arrives late cannot push it out.
func (m *NICHealthMonitor) armPublishDrain() {
	m.shutdownDeadline.CompareAndSwap(0, time.Now().Add(m.publishDrain).UnixNano())
}

// ArmPublishDrain arms the shutdown publish-drain clock against ctx: the
// moment ctx is cancelled, one deadline is set at now plus the drain
// window, and every publish after that draws on the time left on it. Call
// once, with the process-lifetime context.
func (m *NICHealthMonitor) ArmPublishDrain(ctx context.Context) {
	context.AfterFunc(ctx, m.armPublishDrain)
}

// publishDrainContext returns the context a publish runs on. Before the
// cancellation that is the caller's context unchanged — only a publish
// that starts after SIGTERM needs to outlive the cancellation, and it
// then keeps its chance to deliver on the wire or in a retry until the
// shared deadline ends, so a connector that never answers cannot hold
// shutdown open past the window. Values are preserved; only the
// cancellation is cut.
func (m *NICHealthMonitor) publishDrainContext(parent context.Context) (context.Context, context.CancelFunc) {
	if m.shutdownDeadline.Load() == 0 {
		if parent.Err() == nil {
			return parent, func() {}
		}

		// The cancellation is ahead of the AfterFunc callback's goroutine,
		// so this publish would run on the cancelled parent and drop for
		// good. Arm the deadline here; the compare-and-swap keeps the late
		// callback from moving it.
		m.armPublishDrain()
	}

	ns := m.shutdownDeadline.Load()

	return context.WithTimeout(context.WithoutCancel(parent), time.Until(time.Unix(0, ns)))
}
