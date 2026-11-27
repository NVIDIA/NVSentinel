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
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/nvhvclient"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/verdict"
)

type harness struct {
	m       *Monitor
	batches [][]*pb.HealthEvent
	failing bool
	now     time.Time
}

func (h *harness) config() Config {
	return Config{
		Agent:                  "nvhv-health-monitor",
		NodeName:               "node-1",
		UnreachableAfter:       5 * time.Minute,
		CollectionFailureAfter: 5 * time.Minute,
		Now:                    func() time.Time { return h.now },
		Publish: func(_ context.Context, events []*pb.HealthEvent) error {
			if h.failing {
				return errors.New("publish failed")
			}

			h.batches = append(h.batches, events)

			return nil
		},
	}
}

func newHarness() *harness {
	h := &harness{now: time.Unix(1_000, 0)}
	h.m = New(h.config())

	return h
}

func (h *harness) poll(results ...verdict.Result) {
	h.m.Handle(context.Background(), &verdict.Verdict{Results: results}, nil)
}

func result(id string, sev verdict.Severity, entities ...string) verdict.Result {
	return verdict.Result{
		CheckID:          id,
		Severity:         sev,
		Category:         "gpu",
		Messages:         []string{"msg " + id},
		AffectedEntities: entities,
	}
}

func withAction(r verdict.Result, state string, actions ...string) verdict.Result {
	r.OperationalState, r.RemediationActions = state, actions

	return r
}

func TestEventFields(t *testing.T) {
	h := newHarness()
	r := withAction(result("row_remap", verdict.Critical, "GPU-abc"), "drain", "reset_gpu")
	r.ObservedStrings = map[string]string{"pci": "0000:1b:00.0"}

	h.poll(r)

	require.Len(t, h.batches, 1)
	require.Len(t, h.batches[0], 1)

	e := h.batches[0][0]
	assert.Equal(t, "nvhv-health-monitor", e.Agent)
	assert.Equal(t, "node-1", e.NodeName)
	assert.Equal(t, "GPU", e.ComponentClass)
	assert.Equal(t, "NVHVrow_remap", e.CheckName)
	assert.True(t, e.IsFatal)
	assert.False(t, e.IsHealthy)
	assert.Equal(t, "msg row_remap", e.Message)
	assert.Equal(t, pb.RecommendedAction_COMPONENT_RESET, e.RecommendedAction)
	require.Len(t, e.EntitiesImpacted, 2)
	assert.Equal(t, &pb.Entity{EntityType: "GPU_UUID", EntityValue: "GPU-abc"}, e.EntitiesImpacted[0])
	assert.Equal(t, &pb.Entity{EntityType: "PCI", EntityValue: "0000:1b:00.0"}, e.EntitiesImpacted[1])
	assert.NotNil(t, e.GeneratedTimestamp)
}

func TestRecommendedAction(t *testing.T) {
	act := func(state string, actions ...string) pb.RecommendedAction {
		return recommendedAction(verdict.Result{OperationalState: state, RemediationActions: actions})
	}

	assert.Equal(t, pb.RecommendedAction_COMPONENT_RESET, act("drain", "reset_gpu"))
	assert.Equal(t, pb.RecommendedAction_CONTACT_SUPPORT, act("drain", "service_required"))
	assert.Equal(t, pb.RecommendedAction_RESTART_VM, act("drain", "restart_os"))
	assert.Equal(t, pb.RecommendedAction_CONTACT_SUPPORT, act("drain", "replace_node"))
	assert.Equal(t, pb.RecommendedAction_CONTACT_SUPPORT, act("drain", "stop_p2p_traffic", "reset_gpu"))
	assert.Equal(t, pb.RecommendedAction_CONTACT_SUPPORT, act("drain", "hold"))
	assert.Equal(t, pb.RecommendedAction_NONE, act("monitor", "service_required"))
	assert.Equal(t, pb.RecommendedAction_NONE, act("ready"))
}

func TestSkippedResultIsIgnored(t *testing.T) {
	h := newHarness()
	r := result("c", verdict.Critical)
	r.Skipped = true

	h.poll(r)

	assert.Empty(t, h.batches)
}

func TestPublishesOnlyChanges(t *testing.T) {
	h := newHarness()
	h.poll(result("c", verdict.Critical, "GPU-1"))
	h.poll(result("c", verdict.Critical, "GPU-1"))
	assert.Len(t, h.batches, 1, "an unchanged fault is not sent again")

	h.poll(result("c", verdict.Critical, "GPU-1"), result("c", verdict.Critical, "GPU-2"))
	require.Len(t, h.batches, 2)
	assert.Len(t, h.batches[1], 1, "only the new GPU's fault is sent")

	h.poll(result("c", verdict.Warning, "GPU-1"))
	assert.Len(t, h.batches, 3, "a changed severity is sent again")
}

func TestMissingOrUnknownNeverClears(t *testing.T) {
	h := newHarness()
	h.poll(result("c", verdict.Critical, "GPU-1"))
	h.poll()                                      // check missing
	h.poll(result("c", verdict.Unknown, "GPU-1")) // check UNKNOWN
	h.poll(result("c", verdict.Critical, "GPU-1"))
	assert.Len(t, h.batches, 1)
}

func TestPassForgetsFaultAndSendsNothing(t *testing.T) {
	h := newHarness()
	h.poll(result("c", verdict.Critical, "GPU-1"))
	h.poll(result("c", verdict.Pass, "GPU-1"))
	assert.Len(t, h.batches, 1, "no recovery event is sent")

	h.poll(result("c", verdict.Critical, "GPU-1"))
	assert.Len(t, h.batches, 2, "the fault is reported again after it recovered")
}

func TestFailedPublishKeepsRecord(t *testing.T) {
	h := newHarness()
	h.failing = true
	h.poll(result("c", verdict.Critical, "GPU-1"))
	assert.Empty(t, h.batches)

	h.failing = false
	h.poll(result("c", verdict.Critical, "GPU-1"))
	assert.Len(t, h.batches, 1, "the fault is sent once publishing works")
}

func (h *harness) fail() { h.m.Handle(context.Background(), nil, errors.New("unavailable")) }

func TestUnreachableThreshold(t *testing.T) {
	h := newHarness()

	h.fail()
	h.now = h.now.Add(4 * time.Minute)
	h.fail()
	assert.Empty(t, h.batches, "below the threshold nothing is sent")

	h.now = h.now.Add(2 * time.Minute)
	h.fail()
	require.Len(t, h.batches, 1)

	e := h.batches[0][0]
	assert.Equal(t, UnreachableCheckName, e.CheckName)
	assert.Equal(t, "Node", e.ComponentClass)
	assert.True(t, e.IsFatal)
	assert.False(t, e.IsHealthy)
	assert.Equal(t, pb.RecommendedAction_CONTACT_SUPPORT, e.RecommendedAction)
	assert.Empty(t, e.EntitiesImpacted)

	h.fail()
	assert.Len(t, h.batches, 1, "sent once per outage")

	h.poll() // NVHV answers again: the healthy event follows
	require.Len(t, h.batches, 2)

	r := h.batches[1][0]
	assert.Equal(t, UnreachableCheckName, r.CheckName)
	assert.True(t, r.IsHealthy)
	assert.False(t, r.IsFatal)
	assert.Equal(t, pb.RecommendedAction_NONE, r.RecommendedAction)
	assert.Empty(t, r.EntitiesImpacted)

	h.poll()
	assert.Len(t, h.batches, 2, "the healthy event is sent once")

	h.fail()
	h.now = h.now.Add(6 * time.Minute)
	h.fail()
	assert.Len(t, h.batches, 3, "a new outage sends a new event")
}

func TestUnreachableMetrics(t *testing.T) {
	h := newHarness()
	unreachable := metrics.Unreachable.WithLabelValues("node-1")
	failures := metrics.EvaluateFailures.WithLabelValues("node-1", codes.Unavailable.String())
	before := testutil.ToFloat64(failures)

	// A gRPC status wrapped the way the poller wraps it still gives its code.
	h.m.Handle(context.Background(), nil, fmt.Errorf("evaluate: %w", status.Error(codes.Unavailable, "down")))
	assert.InDelta(t, 1, testutil.ToFloat64(unreachable), 0)
	assert.InDelta(t, before+1, testutil.ToFloat64(failures), 0)

	h.poll()
	assert.InDelta(t, 0, testutil.ToFloat64(unreachable), 0)
}

func collectionResult(id string, sev verdict.Severity, action string) verdict.Result {
	return collectionResultIn(id, "xid", sev, action)
}

func collectionResultIn(id, category string, sev verdict.Severity, action string) verdict.Result {
	r := withAction(result(id, sev), "drain", action)
	r.Category = category
	r.Messages = []string{"measurement unavailable: journalctl timed out"}

	return r
}

func TestCollectionFailureAfterThreshold(t *testing.T) {
	h := newHarness()
	fail := func() { h.poll(collectionResult("xid", verdict.Unknown, "retry_collection")) }

	fail()
	h.now = h.now.Add(4 * time.Minute)
	fail()
	assert.Empty(t, h.batches, "below the threshold nothing is sent")

	h.now = h.now.Add(2 * time.Minute)
	fail()
	require.Len(t, h.batches, 1)

	e := h.batches[0][0]
	assert.Equal(t, "NVHVXIDCollectionFailure", e.CheckName)
	assert.Equal(t, "Node", e.ComponentClass)
	assert.True(t, e.IsFatal)
	assert.False(t, e.IsHealthy)
	assert.Equal(t, pb.RecommendedAction_CONTACT_SUPPORT, e.RecommendedAction)
	assert.Empty(t, e.EntitiesImpacted)
	assert.Equal(t, "measurement unavailable: journalctl timed out", e.Message)

	fail()
	assert.Len(t, h.batches, 1, "sent once per episode")

	h.poll() // the kind is missing from the verdict: nothing is evaluated, nothing recovers
	assert.Len(t, h.batches, 1)

	h.poll(collectionResultIn("xid", "xid", verdict.Pass, "reset_gpu")) // collects again
	require.Len(t, h.batches, 2)

	r := h.batches[1][0]
	assert.Equal(t, "NVHVXIDCollectionFailure", r.CheckName)
	assert.True(t, r.IsHealthy)
	assert.False(t, r.IsFatal)

	fail()
	h.now = h.now.Add(6 * time.Minute)
	fail()
	assert.Len(t, h.batches, 3, "a new episode sends a new event")
}

func TestCollectionFailureIsNotAFault(t *testing.T) {
	h := newHarness()
	// A CRITICAL result whose first step is check_collection is a collection
	// failure, not a CONTACT_SUPPORT fault.
	h.poll(collectionResult("xid", verdict.Critical, "check_collection"))

	assert.Empty(t, h.batches)
}

func TestCollectionFailureNeedsEveryPollToFail(t *testing.T) {
	h := newHarness()
	h.poll(collectionResult("xid", verdict.Unknown, "retry_collection"))
	h.now = h.now.Add(4 * time.Minute)
	h.poll() // collection recovered: the clock restarts
	h.now = h.now.Add(2 * time.Minute)
	h.poll(collectionResult("xid", verdict.Unknown, "retry_collection"))

	assert.Empty(t, h.batches)
}

func TestCollectionFailureWaitsWhileUnreachableIsRaised(t *testing.T) {
	h := newHarness()
	h.poll(collectionResult("xid", verdict.Unknown, "retry_collection"))

	// NVHV goes unreachable and the event is raised.
	h.fail()
	h.now = h.now.Add(6 * time.Minute)
	h.fail()
	require.Len(t, h.batches, 1)
	assert.Equal(t, UnreachableCheckName, h.batches[0][0].CheckName)

	// NVHV answers again, still failing to collect: the healthy NVHVUnreachable
	// goes first, and the collection event follows in the same poll.
	h.poll(collectionResult("xid", verdict.Unknown, "retry_collection"))
	require.Len(t, h.batches, 3)
	assert.True(t, h.batches[1][0].IsHealthy)
	assert.Equal(t, UnreachableCheckName, h.batches[1][0].CheckName)
	assert.Equal(t, "NVHVXIDCollectionFailure", h.batches[2][0].CheckName)
}

func TestCollectionMessageIsTheFirstChecksOwn(t *testing.T) {
	h := newHarness()
	poll := func() {
		h.poll(
			collectionResult("b", verdict.Unknown, "retry_collection"),
			collectionResult("a", verdict.Unknown, "check_collection"),
		)
	}

	poll()
	h.now = h.now.Add(6 * time.Minute)
	poll()

	require.Len(t, h.batches, 1)
	assert.Equal(t, "measurement unavailable: journalctl timed out", h.batches[0][0].Message)
}

func TestCollectionFailureIsPerKindOfCheck(t *testing.T) {
	h := newHarness()
	xid := collectionResultIn("xid", "xid", verdict.Unknown, "retry_collection")
	gpu := collectionResultIn("dcgm", "gpu", verdict.Unknown, "check_collection")
	busy := collectionResultIn("collection_source_busy", "node", verdict.Unknown, "retry_collection")

	h.poll(xid, gpu, busy)
	h.now = h.now.Add(6 * time.Minute)
	h.poll(xid, gpu, busy)

	names := map[string]bool{}
	for _, b := range h.batches {
		for _, e := range b {
			names[e.CheckName] = true
		}
	}

	assert.Equal(t, map[string]bool{
		"NVHVXIDCollectionFailure":  true,
		"NVHVGPUCollectionFailure":  true,
		"NVHVNODECollectionFailure": true,
	}, names)

	// Xid collection recovers while the GPU kind keeps failing: only Xid gets
	// its healthy event, and the GPU one is not sent again.
	before := len(h.batches)
	xidPass := collectionResultIn("xid", "xid", verdict.Pass, "reset_gpu")
	h.poll(xidPass, gpu, busy)
	require.Len(t, h.batches, before+1)
	assert.Equal(t, "NVHVXIDCollectionFailure", h.batches[before][0].CheckName)
	assert.True(t, h.batches[before][0].IsHealthy)

	// A new Xid episode is reported again after the threshold.
	h.poll(xid, gpu, busy)
	h.now = h.now.Add(6 * time.Minute)
	h.poll(xid, gpu, busy)
	require.Len(t, h.batches, before+2)
	assert.False(t, h.batches[before+1][0].IsHealthy)
}

func TestHealthyEventsAreRetriedAfterFailedPublish(t *testing.T) {
	h := newHarness()
	xid := collectionResult("xid", verdict.Unknown, "retry_collection")
	pass := collectionResultIn("xid", "xid", verdict.Pass, "reset_gpu")

	h.poll(xid)
	h.now = h.now.Add(6 * time.Minute)
	h.poll(xid)
	require.Len(t, h.batches, 1)

	h.failing = true
	h.poll(pass)
	assert.Len(t, h.batches, 1)

	h.failing = false
	h.poll(pass)
	require.Len(t, h.batches, 2)
	assert.True(t, h.batches[1][0].IsHealthy)

	// The same for NVHVUnreachable.
	h.fail()
	h.now = h.now.Add(6 * time.Minute)
	h.fail()
	require.Len(t, h.batches, 3)

	h.failing = true
	h.poll()
	assert.Len(t, h.batches, 3)

	h.failing = false
	h.poll()
	require.Len(t, h.batches, 4)
	assert.True(t, h.batches[3][0].IsHealthy)
}

// restarted builds a second Monitor over the state the first one saved.
func (h *harness) restarted(saved *statefile.State) *Monitor {
	cfg := h.config()
	cfg.Restore = *saved

	return New(cfg)
}

func TestStateIsSavedAndRestored(t *testing.T) {
	h := newHarness()

	var saved statefile.State

	h.m.cfg.Save = func(s statefile.State) error { saved = s; return nil }

	xid := collectionResult("xid", verdict.Unknown, "retry_collection")
	h.poll(xid)
	h.now = h.now.Add(6 * time.Minute)
	h.poll(xid)
	assert.Equal(t, []string{"xid"}, saved.CollectionRaised)

	h.fail()
	h.now = h.now.Add(6 * time.Minute)
	h.fail()
	assert.True(t, saved.UnreachableRaised)

	// A restarted monitor does not send either event again while the outage
	// lasts, and sends the healthy events once it ends.
	before := len(h.batches)
	m2 := h.restarted(&saved)

	m2.Handle(context.Background(), nil, errors.New("unavailable"))
	assert.Len(t, h.batches, before, "the raised NVHVUnreachable is not sent again")

	pass := collectionResultIn("xid", "xid", verdict.Pass, "reset_gpu")
	m2.Handle(context.Background(), &verdict.Verdict{Results: []verdict.Result{xid}}, nil)
	require.Len(t, h.batches, before+1, "only the healthy NVHVUnreachable: the collection failure is still raised")
	assert.Equal(t, UnreachableCheckName, h.batches[before][0].CheckName)
	assert.True(t, h.batches[before][0].IsHealthy)

	m2.Handle(context.Background(), &verdict.Verdict{Results: []verdict.Result{pass}}, nil)
	require.Len(t, h.batches, before+2)
	assert.Equal(t, "NVHVXIDCollectionFailure", h.batches[before+1][0].CheckName)
	assert.True(t, h.batches[before+1][0].IsHealthy)
}

func TestSaveIsCalledOnlyWhenOutagesChange(t *testing.T) {
	h := newHarness()
	saves := 0
	h.m.cfg.Save = func(statefile.State) error { saves++; return nil }

	h.poll(result("c", verdict.Critical, "GPU-1")) // a fault is not an outage
	h.poll()
	assert.Zero(t, saves)

	h.fail()
	h.now = h.now.Add(6 * time.Minute)
	h.fail()
	assert.Equal(t, 1, saves, "raised")

	h.poll()
	assert.Equal(t, 2, saves, "cleared")
}

func TestProcessingStrategyIsSetOnEveryEvent(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  pb.ProcessingStrategy
		want pb.ProcessingStrategy
	}{
		{"default is STORE_ONLY", pb.ProcessingStrategy_UNSPECIFIED, pb.ProcessingStrategy_STORE_ONLY},
		{"EXECUTE_REMEDIATION", pb.ProcessingStrategy_EXECUTE_REMEDIATION, pb.ProcessingStrategy_EXECUTE_REMEDIATION},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			h.m.cfg.ProcessingStrategy = tc.set
			h.m = New(h.m.cfg)

			xid := collectionResult("xid", verdict.Unknown, "retry_collection")
			pass := collectionResultIn("xid", "xid", verdict.Pass, "reset_gpu")

			h.poll(result("fault", verdict.Critical, "GPU-1"), xid) // a fault
			h.now = h.now.Add(6 * time.Minute)
			h.poll(xid)  // collection failure
			h.poll(pass) // its healthy event
			h.fail()
			h.now = h.now.Add(6 * time.Minute)
			h.fail()     // NVHVUnreachable
			h.poll(pass) // its healthy event

			require.GreaterOrEqual(t, len(h.batches), 4)

			for _, b := range h.batches {
				for _, e := range b {
					assert.Equal(t, tc.want, e.ProcessingStrategy, e.CheckName)
				}
			}
		})
	}
}

func TestIncompatibleSchemaIsCountedApart(t *testing.T) {
	h := newHarness()
	count := func() float64 {
		return testutil.ToFloat64(metrics.EvaluateFailures.WithLabelValues("node-1", "IncompatibleSchema"))
	}
	before := count()

	h.m.Handle(context.Background(), nil, fmt.Errorf("%w: CheckVerdict.check_id must be a string", nvhvclient.ErrIncompatibleSchema))

	assert.InDelta(t, before+1, count(), 0)
}

func withGPU(r verdict.Result, gpu string) verdict.Result {
	r.ObservedStrings = map[string]string{"gpu_id": gpu}

	return r
}

// NVHV returns one result per GPU for the same check: CRITICAL with the GPU as
// its affected entity, and PASS, with no affected entity but the GPU in gpu_id,
// for the others. A PASS for another GPU must not clear the fault.
func TestPassForAnotherGPUDoesNotClearAFault(t *testing.T) {
	h := newHarness()
	fault1 := result("c", verdict.Critical, "GPU-1")
	pass2 := withGPU(result("c", verdict.Pass), "GPU-2")

	h.poll(fault1, pass2)
	h.poll(fault1, pass2)
	h.poll(pass2, fault1) // the other order in a verdict
	assert.Len(t, h.batches, 1, "the fault is sent once, not on every poll")

	// GPU-1 passes: its fault is forgotten, so a return is reported again.
	h.poll(withGPU(result("c", verdict.Pass), "GPU-1"), pass2)
	h.poll(fault1, pass2)
	assert.Len(t, h.batches, 2)
}

func TestPassNamingNoEntityClearsEveryFaultOfTheCheck(t *testing.T) {
	h := newHarness()
	h.poll(result("c", verdict.Critical, "GPU-1"), result("c", verdict.Critical, "GPU-2"))
	require.Len(t, h.batches, 1)
	require.Len(t, h.batches[0], 2)

	h.poll(result("c", verdict.Pass)) // node-level PASS: no entity, no gpu_id
	h.poll(result("c", verdict.Critical, "GPU-1"))
	assert.Len(t, h.batches, 2, "the fault is reported again after the check passed")
}

func window(t *testing.T, r verdict.Request) (from, to int64) {
	t.Helper()
	require.NotNil(t, r.XidFrom)
	require.NotNil(t, r.XidTo)

	return *r.XidFrom, *r.XidTo
}

func TestWindowAdvancesAfterADeliveredPoll(t *testing.T) {
	h := newHarness()

	h.m.Request()
	h.now = h.now.Add(15 * time.Second)
	h.poll() // nothing to deliver, nothing failed

	from, to := window(t, h.m.Request())
	assert.Equal(t, int64(1000), from, "the next window starts where the last one ended")
	assert.Equal(t, int64(1015), to)
}

func TestWindowDoesNotAdvanceWhenTheCallFailed(t *testing.T) {
	h := newHarness()

	h.m.Request()
	h.now = h.now.Add(15 * time.Second)
	h.fail()

	h.now = h.now.Add(15 * time.Second)
	from, to := window(t, h.m.Request())
	assert.Equal(t, int64(999), from, "the failed window is read again")
	assert.Equal(t, int64(1030), to, "as part of a longer one")
}

func TestWindowDoesNotAdvanceWhenPublishingFailed(t *testing.T) {
	h := newHarness()

	h.m.Request()
	h.now = h.now.Add(15 * time.Second)
	h.failing = true
	h.poll(result("c", verdict.Critical, "GPU-1")) // the event is not delivered

	h.now = h.now.Add(15 * time.Second)
	from, _ := window(t, h.m.Request())
	assert.Equal(t, int64(999), from, "an Xid line must not be lost")

	h.failing = false
	h.poll(result("c", verdict.Critical, "GPU-1"))
	assert.Len(t, h.batches, 1)

	from, _ = window(t, h.m.Request())
	assert.Equal(t, int64(1030), from, "it advances once the event was delivered")
}

func TestWindowDoesNotAdvanceWhenAnXidCheckFailedToCollect(t *testing.T) {
	for name, r := range map[string]verdict.Result{
		"unknown xid result":      collectionResultIn("x", "xid", verdict.Unknown, "reset_gpu"),
		"xid collection failure":  collectionResultIn("x", "xid", verdict.Critical, "retry_collection"),
		"xid check, UNKNOWN only": {CheckID: "gpu_xid_48", Category: "xid", Severity: verdict.Unknown},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()

			h.m.Request()
			h.now = h.now.Add(15 * time.Second)
			h.poll(r)

			from, _ := window(t, h.m.Request())
			assert.Equal(t, int64(999), from)
		})
	}

	// A check of another kind that failed to collect does not hold the Xid window.
	h := newHarness()
	h.m.Request()
	h.now = h.now.Add(15 * time.Second)
	h.poll(collectionResultIn("d", "gpu", verdict.Unknown, "check_collection"))

	from, _ := window(t, h.m.Request())
	assert.Equal(t, int64(1000), from)
}

func TestWindowPositionIsSavedAndRestored(t *testing.T) {
	h := newHarness()

	var saved statefile.State

	h.m.cfg.Save = func(s statefile.State) error { saved = s; return nil }

	h.m.Request() // the window ends at 1000, when the request is built
	h.now = h.now.Add(15 * time.Second)
	h.poll()
	assert.Equal(t, int64(1000), saved.XidLastTo)

	// A restarted monitor resumes from the saved position, so the downtime is
	// covered by its first window.
	h.now = h.now.Add(5 * time.Minute)
	m2 := h.restarted(&saved)

	from, to := window(t, m2.Request())
	assert.Equal(t, int64(1000), from)
	assert.Equal(t, int64(1315), to)
}
