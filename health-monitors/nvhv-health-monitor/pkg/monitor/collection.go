// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"slices"
	"strings"

	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/verdict"
)

// collectionFailureName is the check name of the event sent when NVHV answers
// but keeps failing to collect the data of one kind of check, so each kind has
// its own node condition. The kind is the verdict's own category, so a category
// NVHV adds later gets its own name without a change here: NVHVXIDCollectionFailure,
// NVHVGPUCollectionFailure, NVHVNODECollectionFailure, and so on.
func collectionFailureName(category string) string {
	return CheckNamePrefix + strings.ToUpper(category) + "CollectionFailure"
}

// isCollectionFailure reports whether a result says its check's data could not
// be collected (a sensor timeout, a driver or permission failure, or a source
// stuck in a call), not that the node is faulty: NVHV's first remediation step
// is retry_collection or check_collection, whatever the severity.
func isCollectionFailure(r verdict.Result) bool {
	actions := r.RemediationActions

	return len(actions) > 0 && (actions[0] == "retry_collection" || actions[0] == "check_collection")
}

// handleCollection sends one node-level NVHV<kind>CollectionFailure event per
// kind of check once that kind's collection has been failing on every poll for
// longer than the threshold, and not while NVHVUnreachable is raised. When the
// kind collects again, shown by an evaluated result of that kind and none
// failing, it sends the healthy event. A kind missing from the verdict, or only
// UNKNOWN, recovers nothing: its raised event stays. A failure that was never
// sent just ends its episode.
func (m *Monitor) handleCollection(ctx context.Context, v *verdict.Verdict) {
	failed := map[string][]verdict.Result{}
	collecting := map[string]bool{}

	for _, r := range v.Results {
		switch {
		case r.Skipped:
		case isCollectionFailure(r):
			failed[r.Category] = append(failed[r.Category], r)
		case evaluated(r):
			collecting[r.Category] = true
		}
	}

	for category, ep := range m.collection {
		if _, ok := failed[category]; ok {
			continue
		}

		if !ep.sent {
			delete(m.collection, category)

			continue
		}

		if !collecting[category] {
			continue
		}

		if m.publishOne(ctx, m.outageEvent(collectionFailureName(category), "NVHV collects the data again", true)) {
			delete(m.collection, category)
			m.saveState()
		}
	}

	now := m.cfg.Now()

	for category, results := range failed {
		ep := m.collection[category]
		if ep == nil {
			ep = &collectionEpisode{since: now}
			m.collection[category] = ep
		}

		if ep.sent || m.unreachableSent || now.Sub(ep.since) < m.cfg.CollectionFailureAfter {
			continue
		}

		if m.publishOne(ctx, m.outageEvent(collectionFailureName(category), collectionMessage(results), false)) {
			ep.sent = true
			m.saveState()
		}
	}
}

// collectionMessage is NVHV's own message of the first failing check, by check
// ID so it is stable from poll to poll.
func collectionMessage(failed []verdict.Result) string {
	first := slices.MinFunc(failed, func(a, b verdict.Result) int { return strings.Compare(a.CheckID, b.CheckID) })

	msg := strings.Join(first.Messages, "; ")
	if msg == "" {
		msg = "NVHV could not collect the data of check " + first.CheckID
	}

	return msg
}
