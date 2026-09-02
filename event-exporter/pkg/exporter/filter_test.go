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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/event-exporter/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
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

func TestShouldExport_EvaluationError_FailsOpen(t *testing.T) {
	// A non-boolean result cannot be caught at compile time because event is a dyn map.
	// Failing open is deliberate: exporting an extra event is noise, dropping events on a
	// filter bug is silent data loss.
	e := exporterWithFilter(t, `event.agent`)
	require.NotNil(t, e.filter)

	assert.True(t, e.shouldExport(context.Background(), filterEvent(pb.RecommendedAction_NONE)))
}

func TestShouldExport_MissingFieldAtRuntime_FailsOpen(t *testing.T) {
	e := exporterWithFilter(t, `event.notAField == 'x'`)

	assert.True(t, e.shouldExport(context.Background(), filterEvent(pb.RecommendedAction_NONE)))
}

// TestWorkerPool_FilteredEvent_StillAdvancesResumeToken is the requirement that makes the
// filter usable at all. A filtered event is completed rather than skipped, so the resume
// token moves past it. If it were merely skipped, one filtered event at the head of the
// stream would stall the token and a restart would redeliver everything after it, which
// with a filter dropping 99% of events means never making progress.
func TestWorkerPool_FilteredEvent_StillAdvancesResumeToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source := &mockSource{}

	// Mirrors processEvent's contract: a filtered event returns nil, exactly like a
	// published one, so its sequence completes.
	filtered := func(_ context.Context, _ client.Event) error { return nil }

	pool := newWorkerPool(1, filtered, source, cancel)

	done := make(chan error, 1)

	go func() { done <- pool.run(ctx) }()

	for i := range 3 {
		require.True(t, pool.dispatch(ctx, workItem{
			seq:         uint64(i),
			resumeToken: []byte{byte(i)},
		}))
	}

	pool.closeDispatch()
	require.NoError(t, <-done)

	tokens := source.getTokens()
	require.NotEmpty(t, tokens, "a filtered event must still advance the resume token")
	assert.Equal(t, []byte{2}, tokens[len(tokens)-1],
		"the token should reach the last filtered sequence")
}
