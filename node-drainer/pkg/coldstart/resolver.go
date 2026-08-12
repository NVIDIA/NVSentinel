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

package coldstart

import (
	"context"
	"fmt"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
)

// sessionEndFinder is the subset of datastore.HealthEventStore needed to look up
// quarantine session-ending events. It exists to keep quarantineSessionResolver testable.
type sessionEndFinder interface {
	FindLatestHealthEventByQuery(
		ctx context.Context, builder datastore.QueryBuilder,
	) (*datastore.HealthEventWithStatus, error)
}

// quarantineSessionResolver answers, per node, whether a quarantine record predates a
// later UnQuarantined/Cancelled event (i.e. its quarantine session has already ended).
// Results are cached per node because a cold-start batch commonly contains multiple
// records for the same node.
type quarantineSessionResolver struct {
	finder sessionEndFinder
	cache  map[string]sessionEndInfo
}

// sessionEndInfo captures the most recent quarantine session end observed for a node.
type sessionEndInfo struct {
	latest time.Time
	exists bool
}

func newQuarantineSessionResolver(finder sessionEndFinder) *quarantineSessionResolver {
	return &quarantineSessionResolver{
		finder: finder,
		cache:  make(map[string]sessionEndInfo),
	}
}

// quarantineSessionEnded reports whether an UnQuarantined/Cancelled event exists for the
// node that is strictly newer than eventCreatedAt. When the candidate has no usable
// timestamp, it returns false so the caller re-queues rather than dropping the event.
func (r *quarantineSessionResolver) quarantineSessionEnded(
	ctx context.Context, nodeName string, eventCreatedAt time.Time,
) (bool, error) {
	if eventCreatedAt.IsZero() {
		return false, nil
	}

	info, ok := r.cache[nodeName]
	if !ok {
		var err error

		info, err = r.lookupLatestSessionEnd(ctx, nodeName)
		if err != nil {
			return false, err
		}

		r.cache[nodeName] = info
	}

	if !info.exists {
		return false, nil
	}

	return info.latest.After(eventCreatedAt), nil
}

// lookupLatestSessionEnd finds the newest UnQuarantined/Cancelled event for a node. The
// sort+limit is pushed to the datastore so a node with many past sessions does not load
// every session-end record into memory.
func (r *quarantineSessionResolver) lookupLatestSessionEnd(
	ctx context.Context, nodeName string,
) (sessionEndInfo, error) {
	q := query.New().Build(
		query.And(
			query.Eq("healthevent.nodename", nodeName),
			query.In("healtheventstatus.nodequarantined",
				[]interface{}{string(model.UnQuarantined), string(model.Cancelled)}),
		),
	)

	event, err := r.finder.FindLatestHealthEventByQuery(ctx, q)
	if err != nil {
		return sessionEndInfo{}, fmt.Errorf("failed to look up quarantine session end for node %s: %w", nodeName, err)
	}

	// No session-end marker, or one without a usable timestamp: treat as "not ended"
	// so the caller re-queues rather than dropping the event.
	if event == nil || event.CreatedAt.IsZero() {
		return sessionEndInfo{}, nil
	}

	return sessionEndInfo{latest: event.CreatedAt, exists: true}, nil
}
