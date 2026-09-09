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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// healthEventsTotal counts every health event reaching the platform connector, broken
// down by the fields an operator needs to answer "what is actionable right now".
//
// node carries the event's own nodeName rather than relying on a pod-to-node join. The
// join would attribute an event to the node of the connector pod that received it, which
// is the faulty node for the DaemonSet monitors but not for health-events-analyzer: that
// is a Deployment, so its derived events describe other nodes while being received
// wherever it happens to be scheduled. Labelling explicitly makes every agent correct and
// keeps the metric dashboardable on its own.
//
// errorCode is still excluded: it is unbounded (suffixed XIDs such as
// 145.RLW_SRC_TRACK), so it would grow the series set without limit. node is bounded by
// the fleet.
//
// is_fatal is retained even though producers derive it as recommendedAction != NONE:
// keeping both makes a producer that disagrees with that derivation visible rather than
// silent, which is the shape of the XID 45 escalation bug in #1710.
var healthEventsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "health_events_total",
	Help: "Total number of health events received by the platform connector, by node, agent, " +
		"check name, recommended action, and fatal/healthy status",
}, []string{"node", "agent", "check_name", "recommended_action", "is_fatal", "is_healthy"})
