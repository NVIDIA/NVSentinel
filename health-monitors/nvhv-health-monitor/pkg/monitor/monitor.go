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
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"time"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/nvhvclient"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/verdict"
)

// UnreachableCheckName is the check name of the event sent when NVHV stays
// unreachable.
const UnreachableCheckName = "NVHVUnreachable"

// PublishFunc delivers one batch of events; it returns nil only once the batch
// was delivered.
type PublishFunc func(ctx context.Context, events []*pb.HealthEvent) error

// Config configures a Monitor.
type Config struct {
	Agent    string
	NodeName string
	// UnreachableAfter is how long NVHV may stay unreachable before the
	// NVHVUnreachable event is sent.
	UnreachableAfter time.Duration
	// CollectionFailureAfter is how long a kind of check may keep failing to
	// collect its data, while NVHV answers, before its
	// NVHV<kind>CollectionFailure event is sent.
	CollectionFailureAfter time.Duration
	Publish                PublishFunc
	// Restore is the state saved by an earlier instance: the outage events it
	// had raised and not yet cleared.
	Restore statefile.State
	// Save is called whenever the raised outage events change. A failure is
	// logged and does not stop the monitor.
	Save func(statefile.State) error
	// ProcessingStrategy is set on every event; STORE_ONLY when unset.
	ProcessingStrategy pb.ProcessingStrategy
	// Now is time.Now unless a test sets it.
	Now func() time.Time
}

// Monitor keeps what has been reported and publishes only what changed.
type Monitor struct {
	cfg Config

	// reported maps a fault's key to its fingerprint. It is updated only after
	// a batch was delivered, so a failed publish is recomputed on the next poll.
	reported map[string]fault

	failingSince    time.Time // zero while NVHV answers
	unreachableSent bool

	// lastTo is the end, in Unix seconds, of the last Xid window that was read
	// and delivered; windowTo is the end of the window of the poll in flight.
	lastTo, windowTo int64

	// collection holds the running collection-failure episode of each kind of
	// check; a kind is absent while none of its checks fails to collect.
	collection map[string]*collectionEpisode
}

type collectionEpisode struct {
	since time.Time
	sent  bool
}

// New returns a Monitor.
func New(cfg Config) *Monitor {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	if cfg.ProcessingStrategy == pb.ProcessingStrategy_UNSPECIFIED {
		cfg.ProcessingStrategy = pb.ProcessingStrategy_STORE_ONLY
	}

	m := &Monitor{cfg: cfg, reported: map[string]fault{}, collection: map[string]*collectionEpisode{}}
	m.restore(cfg.Restore)

	return m
}

// Handle processes the result of one poll: exactly one of verdict and err is
// set.
func (m *Monitor) Handle(ctx context.Context, v *verdict.Verdict, err error) {
	if err != nil {
		metrics.Unreachable.WithLabelValues(m.cfg.NodeName).Set(1)
		metrics.EvaluateFailures.WithLabelValues(m.cfg.NodeName, failureCode(err)).Inc()
		m.handleUnreachable(ctx)

		return
	}

	metrics.Unreachable.WithLabelValues(m.cfg.NodeName).Set(0)

	m.failingSince = time.Time{}

	m.handleReachable(ctx)
	delivered := m.handleVerdict(ctx, v)
	if delivered && xidCollected(v) {
		m.advanceWindow()
	}
}

// handleReachable sends the healthy NVHVUnreachable event, once, after NVHV
// answers again following a raised NVHVUnreachable. It is retried on the next
// poll if it was not delivered.
func (m *Monitor) handleReachable(ctx context.Context) {
	if !m.unreachableSent {
		return
	}

	if err := m.cfg.Publish(ctx, []*pb.HealthEvent{m.outageEvent(UnreachableCheckName, "NVHV is reachable again", true)}); err != nil {
		slog.Warn("Publish healthy NVHVUnreachable failed; retrying next poll", "error", err)

		return
	}

	m.unreachableSent = false
	m.saveState()
}

// outageEvent is a node-level event of an outage check (NVHVUnreachable,
// NVHV<kind>CollectionFailure): fatal and CONTACT_SUPPORT while it lasts, and
// the same check name, healthy and with no entities, to clear it.
func (m *Monitor) outageEvent(checkName, message string, healthy bool) *pb.HealthEvent {
	action := pb.RecommendedAction_CONTACT_SUPPORT
	if healthy {
		action = pb.RecommendedAction_NONE
	}

	return &pb.HealthEvent{
		Version:            1,
		Agent:              m.cfg.Agent,
		NodeName:           m.cfg.NodeName,
		ComponentClass:     "Node",
		CheckName:          checkName,
		IsFatal:            !healthy,
		IsHealthy:          healthy,
		Message:            message,
		RecommendedAction:  action,
		GeneratedTimestamp: timestamppb.New(m.cfg.Now()),
		ProcessingStrategy: m.cfg.ProcessingStrategy,
	}
}

// handleUnreachable sends one NVHVUnreachable event once NVHV has been
// unreachable for longer than the threshold. Nothing else changes: a failed
// call neither raises nor clears a fault.
func (m *Monitor) handleUnreachable(ctx context.Context) {
	now := m.cfg.Now()
	if m.failingSince.IsZero() {
		m.failingSince = now
	}

	if m.unreachableSent || now.Sub(m.failingSince) < m.cfg.UnreachableAfter {
		return
	}

	event := m.outageEvent(UnreachableCheckName, "NVHV is unreachable; check its logs and the GPU driver state", false)

	if err := m.cfg.Publish(ctx, []*pb.HealthEvent{event}); err != nil {
		slog.Warn("Publish NVHVUnreachable failed; retrying next poll", "error", err)

		return
	}

	m.unreachableSent = true
	m.saveState()
}

// handleVerdict publishes the faults that are new or changed since the last
// delivered batch. A PASS result forgets its faults, so they are reported
// again if they come back; nothing is sent for it. Faults missing from the
// verdict are kept: only a PASS ends one.
func (m *Monitor) handleVerdict(ctx context.Context, v *verdict.Verdict) bool {
	m.handleCollection(ctx, v)

	next := maps.Clone(m.reported)

	var events []*pb.HealthEvent

	for _, r := range v.Results {
		switch {
		case isCollectionFailure(r):
			// Reported by handleCollection, not as a fault of the node.
		case r.Severity == verdict.Pass && !r.Skipped:
			forget(next, r)
		case reportable(r):
			f := newFault(r)
			if prev, ok := next[f.key()]; !ok || prev.fingerprint != f.fingerprint {
				next[f.key()] = f
				events = append(events, m.faultEvent(r))
			}
		}
	}

	if len(events) > 0 {
		if err := m.cfg.Publish(ctx, events); err != nil {
			slog.Warn("Publish failed; the record is unchanged and events are recomputed next poll",
				"events", len(events), "error", err)

			return false
		}

		for _, e := range events {
			slog.Info("Published", "check", e.GetCheckName(), "fatal", e.GetIsFatal(),
				"action", e.GetRecommendedAction().String(), "message", e.GetMessage())
		}
	}

	m.reported = next

	return true
}

// forget drops the reported faults a PASS result covers: those of its check
// that share an entity with it. NVHV gives a PASS no affected entities and names
// its GPU only in the observed gpu_id, so that counts as its entity. A PASS that
// names no entity at all covers every fault of the check.
func forget(reported map[string]fault, r verdict.Result) {
	entities := r.AffectedEntities
	if len(entities) == 0 && r.ObservedStrings[observedGPU] != "" {
		entities = []string{r.ObservedStrings[observedGPU]}
	}

	for k, f := range reported {
		if f.checkID != r.CheckID {
			continue
		}

		if len(entities) == 0 || slices.ContainsFunc(f.entities, func(e string) bool { return slices.Contains(entities, e) }) {
			delete(reported, k)
		}
	}
}

// restore applies the state of an earlier instance. A restored outage counts as
// raised, so its healthy event is sent when the outage ends and its unhealthy
// event is not sent again.
func (m *Monitor) restore(s statefile.State) {
	m.unreachableSent = s.UnreachableRaised
	m.lastTo = s.XidLastTo

	for _, category := range s.CollectionRaised {
		m.collection[category] = &collectionEpisode{since: m.cfg.Now(), sent: true}
	}
}

// saveState writes which outage events are raised.
func (m *Monitor) saveState() {
	if m.cfg.Save == nil {
		return
	}

	s := statefile.State{UnreachableRaised: m.unreachableSent, CollectionRaised: []string{}, XidLastTo: m.lastTo}

	for category, ep := range m.collection {
		if ep.sent {
			s.CollectionRaised = append(s.CollectionRaised, category)
		}
	}

	sort.Strings(s.CollectionRaised)

	if err := m.cfg.Save(s); err != nil {
		slog.Warn("Save state failed", "error", err)
	}
}

// failureCode labels a failed call: the gRPC status code, or IncompatibleSchema
// when NVHV's schema lacks something the monitor needs.
func failureCode(err error) string {
	if errors.Is(err, nvhvclient.ErrIncompatibleSchema) {
		return "IncompatibleSchema"
	}

	return status.Code(err).String()
}

// Request is the request of the next poll. Its Xid window is (lastTo, to]: it
// starts where the last delivered window ended, so an Xid line is in exactly one
// verdict, and a window that failed is read again, as part of the next one. With
// no saved position the first window starts at the end of the journal, so Xids
// logged before the monitor started are not replayed.
func (m *Monitor) Request() verdict.Request {
	now := m.cfg.Now().Unix()
	if m.lastTo == 0 {
		m.lastTo = now - 1
	}

	m.windowTo = max(now, m.lastTo+1)
	from, to := m.lastTo, m.windowTo

	return verdict.Request{XidFrom: &from, XidTo: &to}
}

// advanceWindow makes the window of the poll that just finished the last one
// read.
func (m *Monitor) advanceWindow() {
	if m.windowTo <= m.lastTo {
		return
	}

	m.lastTo = m.windowTo
	m.saveState()
}

// xidCollected reports whether the Xid checks read their window: an UNKNOWN or
// collection-failure result of the xid category means the window failed, so it
// must be read again, and the Xid lines in it must not be lost.
func xidCollected(v *verdict.Verdict) bool {
	for _, r := range v.Results {
		if r.Category == "xid" && (r.Severity == verdict.Unknown || isCollectionFailure(r)) {
			return false
		}
	}

	return true
}
