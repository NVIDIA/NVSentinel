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

// defaultPublishDrain is the window a publish keeps after the shutdown
// context is cancelled. The events nearest shutdown are the ones nothing
// else can report: the poll-stall event is the only record that the node
// was blind while liveness restarted this container, and a poll that
// completes during shutdown carries the recoveries that ended the stall.
// On the cancelled context the publisher drops a batch for good — a
// retrying send dies at once, a batch produced after the cancel is never
// sent — so each publish runs on a context of its own: detached from the
// cancellation, bounded by this window.
//
// The window is per publish, not per shutdown: a connector that stays
// wedged can consume one window per publish, and the pod's termination
// grace period remains the outer bound. The chart leaves
// terminationGracePeriodSeconds at the 30s default, and the rest of
// shutdown (metrics server 5s grace, loops stopping) fits beside 10s.
const defaultPublishDrain = 10 * time.Second

// publishDrainContext returns the context a publish runs on: detached from
// parent's cancellation, so a send on the wire or a retry in a backoff
// keeps its chance to deliver once SIGTERM lands, and bounded by the drain
// window, so a connector that never answers cannot hold shutdown open.
// Values are preserved; only cancellation is cut.
func (m *NICHealthMonitor) publishDrainContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), m.publishDrain)
}
