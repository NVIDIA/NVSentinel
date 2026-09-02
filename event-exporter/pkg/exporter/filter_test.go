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

package exporter

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/event-exporter/pkg/config"
)

// exporterWithFilter builds an exporter carrying only what the filter path needs.
func exporterWithFilter(t *testing.T, expression string) *HealthEventsExporter {
	t.Helper()

	cfg := &config.Config{}
	cfg.Exporter.Filter.Expression = expression

	filter, err := cfg.Exporter.Filter.Compile()
	require.NoError(t, err)

	return &HealthEventsExporter{cfg: cfg, filter: filter}
}

func filterEvent(action pb.RecommendedAction, codes ...string) *pb.HealthEvent {
	return &pb.HealthEvent{
		Agent:             "syslog-health-monitor",
		CheckName:         "SysLogsXIDError",
		NodeName:          "node-1",
		ErrorCode:         codes,
		RecommendedAction: action,
		IsFatal:           action != pb.RecommendedAction_NONE,
	}
}

func TestShouldExport_NoExpression_ExportsEverything(t *testing.T) {
	e := exporterWithFilter(t, "")

	assert.True(t, e.shouldExport(context.Background(), filterEvent(pb.RecommendedAction_NONE, "45")))
	assert.True(t, e.shouldExport(context.Background(),
		filterEvent(pb.RecommendedAction_CONTACT_SUPPORT, "31")))
}

func TestShouldExport_WhitespaceOnlyExpression_ExportsEverything(t *testing.T) {
	e := exporterWithFilter(t, "   ")

	assert.Nil(t, e.filter, "a blank expression must not compile to a program")
	assert.True(t, e.shouldExport(context.Background(), filterEvent(pb.RecommendedAction_NONE)))
}

func TestShouldExport_ActionableOnlyFilter_DropsTheNonActionableMajority(t *testing.T) {
	// The motivating case from #1702: 99.1% of this fleet's events are NONE.
	e := exporterWithFilter(t, `event.recommendedAction != 'NONE'`)

	assert.False(t, e.shouldExport(context.Background(), filterEvent(pb.RecommendedAction_NONE, "45")))
	assert.True(t, e.shouldExport(context.Background(),
		filterEvent(pb.RecommendedAction_CONTACT_SUPPORT, "31")))
	assert.True(t, e.shouldExport(context.Background(),
		filterEvent(pb.RecommendedAction_RESTART_VM, "74")))
}

func TestShouldExport_ErrorCodeExclusion_UsesListMembership(t *testing.T) {
	e := exporterWithFilter(t, `event.recommendedAction != 'NONE' && !('45' in event.errorCode)`)

	assert.False(t, e.shouldExport(context.Background(),
		filterEvent(pb.RecommendedAction_CONTACT_SUPPORT, "45")))
	assert.True(t, e.shouldExport(context.Background(),
		filterEvent(pb.RecommendedAction_CONTACT_SUPPORT, "31")))
	// A multi-code event is excluded when any of its codes matches.
	assert.False(t, e.shouldExport(context.Background(),
		filterEvent(pb.RecommendedAction_CONTACT_SUPPORT, "31", "45")))
}

func TestCompile_BareFieldRead_IsRejectedBeforeStartup(t *testing.T) {
	// A bare field read is untyped, so it never becomes a running filter. Rejected at
	// config time rather than failing open per event.
	filter := config.FilterConfig{Expression: `event.agent`}

	_, err := filter.Compile()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "must return boolean")
}

func TestShouldExport_MissingFieldAtRuntime_FailsOpen(t *testing.T) {
	e := exporterWithFilter(t, `event.notAField == 'x'`)

	assert.True(t, e.shouldExport(context.Background(), filterEvent(pb.RecommendedAction_NONE)))
}

// fakeEvent is a client.Event carrying one health event, so the real processEvent can be
// driven through the worker pool rather than a stand-in callback.
type fakeEvent struct {
	healthEvent *pb.HealthEvent
	token       []byte
}

func (f fakeEvent) GetDocumentID() (string, error) { return "doc-1", nil }
func (f fakeEvent) GetRecordUUID() (string, error) { return "uuid-1", nil }
func (f fakeEvent) GetNodeName() (string, error)   { return f.healthEvent.GetNodeName(), nil }
func (f fakeEvent) GetResumeToken() []byte         { return f.token }

func (f fakeEvent) UnmarshalDocument(v any) error {
	target, ok := v.(*model.HealthEventWithStatus)
	if !ok {
		return fmt.Errorf("unexpected unmarshal target %T", v)
	}

	target.HealthEvent = f.healthEvent
	target.HealthEventStatus = &pb.HealthEventStatus{}

	return nil
}

// TestWorkerPool_FilteredEvent_StillAdvancesResumeToken is the requirement that makes the
// filter usable at all. A filtered event is completed rather than skipped, so the resume
// token moves past it. If it were merely skipped, one filtered event at the head of the
// stream would stall the token and a restart would redeliver everything after it, which
// with a filter dropping 99% of events means never making progress.
//
// This drives the real HealthEventsExporter.processEvent, not a stand-in that returns nil,
// so it would catch processEvent returning an error or publishing on the filtered path.
// The exporter has a nil sink and nil transformer on purpose: if a filtered event ever
// reached publishWithRetry this test would panic rather than quietly pass.
func TestWorkerPool_FilteredEvent_StillAdvancesResumeToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exp := exporterWithFilter(t, `event.recommendedAction != 'NONE'`)
	require.NotNil(t, exp.filter)

	source := &mockSource{}
	pool := newWorkerPool(1, exp.processEvent, source, cancel)

	done := make(chan error, 1)

	go func() { done <- pool.run(ctx) }()

	// Every one of these is filtered out: NONE does not match the expression.
	for i := range 3 {
		require.True(t, pool.dispatch(ctx, workItem{
			seq: uint64(i),
			event: fakeEvent{
				healthEvent: filterEvent(pb.RecommendedAction_NONE, "45"),
				token:       []byte{byte(i)},
			},
			resumeToken: []byte{byte(i)},
		}))
	}

	pool.closeDispatch()
	require.NoError(t, <-done, "a filtered event must not be a fatal process error")

	tokens := source.getTokens()
	require.NotEmpty(t, tokens, "a filtered event must still advance the resume token")
	assert.Equal(t, []byte{2}, tokens[len(tokens)-1],
		"the token should reach the last filtered sequence")
}
