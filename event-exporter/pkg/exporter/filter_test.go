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

// TestShouldExport drives the filter decision over the fields an operator actually writes
// expressions against. The CEL semantics themselves live in commons/pkg/celevent's tests; what
// is exercised here is this package's use of them, including the fail-open behaviour.
func TestShouldExport(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		action     pb.RecommendedAction
		codes      []string
		want       bool
	}{
		{
			name:   "no expression exports a non-actionable event",
			action: pb.RecommendedAction_NONE,
			codes:  []string{"45"},
			want:   true,
		},
		{
			name:   "no expression exports an actionable event",
			action: pb.RecommendedAction_CONTACT_SUPPORT,
			codes:  []string{"31"},
			want:   true,
		},
		{
			name:       "whitespace-only expression exports everything",
			expression: "   ",
			action:     pb.RecommendedAction_NONE,
			want:       true,
		},
		{
			// The motivating case from #1702: 99.1% of this fleet's events are NONE.
			name:       "actionable-only drops NONE",
			expression: `event.recommendedAction != 'NONE'`,
			action:     pb.RecommendedAction_NONE,
			codes:      []string{"45"},
			want:       false,
		},
		{
			name:       "actionable-only keeps CONTACT_SUPPORT",
			expression: `event.recommendedAction != 'NONE'`,
			action:     pb.RecommendedAction_CONTACT_SUPPORT,
			codes:      []string{"31"},
			want:       true,
		},
		{
			name:       "actionable-only keeps RESTART_VM",
			expression: `event.recommendedAction != 'NONE'`,
			action:     pb.RecommendedAction_RESTART_VM,
			codes:      []string{"74"},
			want:       true,
		},
		{
			name:       "errorCode exclusion drops the excluded code",
			expression: `event.recommendedAction != 'NONE' && !('45' in event.errorCode)`,
			action:     pb.RecommendedAction_CONTACT_SUPPORT,
			codes:      []string{"45"},
			want:       false,
		},
		{
			name:       "errorCode exclusion keeps other codes",
			expression: `event.recommendedAction != 'NONE' && !('45' in event.errorCode)`,
			action:     pb.RecommendedAction_CONTACT_SUPPORT,
			codes:      []string{"31"},
			want:       true,
		},
		{
			// Membership, not equality: any matching code excludes the event.
			name:       "errorCode exclusion drops a multi-code event containing the code",
			expression: `event.recommendedAction != 'NONE' && !('45' in event.errorCode)`,
			action:     pb.RecommendedAction_CONTACT_SUPPORT,
			codes:      []string{"31", "45"},
			want:       false,
		},
		{
			// Fails open deliberately: exporting an extra event is noise, dropping events on a
			// filter bug is silent data loss.
			name:       "a field missing at runtime fails open",
			expression: `event.notAField == 'x'`,
			action:     pb.RecommendedAction_NONE,
			want:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := exporterWithFilter(t, tc.expression)

			got := e.shouldExport(context.Background(), filterEvent(tc.action, tc.codes...))

			assert.Equal(t, tc.want, got)
		})
	}
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
