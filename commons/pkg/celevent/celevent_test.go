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

package celevent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func testEvent() *pb.HealthEvent {
	return &pb.HealthEvent{
		Agent:             "syslog-health-monitor",
		CheckName:         "SysLogsXIDError",
		ComponentClass:    "GPU",
		NodeName:          "node-1",
		ErrorCode:         []string{"45", "145.RLW_SRC_TRACK"},
		IsFatal:           true,
		IsHealthy:         false,
		RecommendedAction: pb.RecommendedAction_CONTACT_SUPPORT,
		Message:           "GPU 3 fell off the bus",
		Metadata:          map[string]string{"chassis_serial": "CHASSIS-1"},
	}
}

func evaluate(t *testing.T, expression string, event *pb.HealthEvent) bool {
	t.Helper()

	filter, err := Compile(expression)
	require.NoError(t, err)

	matched, err := filter.Matches(event)
	require.NoError(t, err)

	return matched
}

func TestEvaluateBool_FieldExpressions_MatchTheEventsValues(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		want       bool
	}{
		{"agent matches", `event.agent == 'syslog-health-monitor'`, true},
		{"agent does not match", `event.agent == 'gpu-health-monitor'`, false},
		{"checkName matches", `event.checkName == 'SysLogsXIDError'`, true},
		{"componentClass matches", `event.componentClass == 'GPU'`, true},
		{"nodeName matches", `event.nodeName == 'node-1'`, true},
		{"isFatal compared explicitly", `event.isFatal == true`, true},
		{"isHealthy compared explicitly", `event.isHealthy == false`, true},
		{"recommendedAction is the enum name", `event.recommendedAction == 'CONTACT_SUPPORT'`, true},
		{"recommendedAction not NONE", `event.recommendedAction != 'NONE'`, true},
		{"message is readable", `event.message.contains('fell off the bus')`, true},
		{"metadata is a map", `event.metadata['chassis_serial'] == 'CHASSIS-1'`, true},
		{"absent metadata key", `!('trace_id' in event.metadata)`, true},
		// The motivating filter from #1702.
		{
			"actionable and not XID 45",
			`event.recommendedAction != 'NONE' && !('45' in event.errorCode)`,
			false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evaluate(t, tc.expression, testEvent()))
		})
	}
}

func TestEvaluateBool_ErrorCode_BindsAsAListNotAString(t *testing.T) {
	// errorCode is a repeated proto field. Documenting this in a test because the obvious
	// expression, event.errorCode == '45', is a type error rather than a false match.
	event := testEvent()

	assert.True(t, evaluate(t, `'45' in event.errorCode`, event))
	assert.True(t, evaluate(t, `event.errorCode[0] == '45'`, event))
	assert.True(t, evaluate(t, `event.errorCode.size() == 2`, event))
	assert.False(t, evaluate(t, `'31' in event.errorCode`, event))

	// A suffixed XID is why errorCode is unsuitable as a metric label, but fine here.
	filter, err := Compile(`'145.RLW_SRC_TRACK' in event.errorCode`)
	require.NoError(t, err)

	matched, err := filter.Matches(event)
	require.NoError(t, err)
	assert.True(t, matched)
}

func TestCompileBool_ConcreteNonBooleanType_IsRejectedAtCompileTime(t *testing.T) {
	_, err := Compile(`1 + 1`)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "must return boolean")
}

func TestCompile_BareFieldRead_IsRejectedWithGuidance(t *testing.T) {
	// "event" is map[string]dyn, so a bare read is untyped and rejected even when the field
	// is semantically boolean. The error has to say how to fix it, because `event.isFatal`
	// is the obvious thing for an operator to write.
	for _, expression := range []string{`event.agent`, `event.isFatal`} {
		_, err := Compile(expression)

		require.Error(t, err, expression)
		assert.Contains(t, err.Error(), "must return boolean")
		assert.Contains(t, err.Error(), "event.isFatal == true",
			"the error should show the working form")
	}
}

func TestCompileBool_MalformedExpression_IsRejected(t *testing.T) {
	_, err := Compile(`event.agent ==`)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "compilation failed")
}

func TestCompileBool_UnknownField_IsRejectedAtCompileTime(t *testing.T) {
	// Fields resolve dynamically, so an unknown one cannot be caught at compile time and
	// fails at evaluation instead. Asserted so the boundary is explicit rather than assumed.
	filter, err := Compile(`event.notAField == 'x'`)
	require.NoError(t, err, "dynamic map access compiles")

	_, err = filter.Matches(testEvent())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "evaluation failed")
}

func TestBuildEventMap_ZeroValueEvent_DoesNotPanic(t *testing.T) {
	assert.NotPanics(t, func() { BuildEventMap(&pb.HealthEvent{}) })
	assert.NotPanics(t, func() { BuildEventMap(nil) })
}

func TestBuildEventMap_ErrorCodeIsCloned_SoCallersCannotMutateTheEvent(t *testing.T) {
	event := testEvent()

	eventMap := BuildEventMap(event)
	codes, ok := eventMap["errorCode"].([]string)
	require.True(t, ok)
	require.NotEmpty(t, codes)

	codes[0] = "MUTATED"

	assert.Equal(t, "45", event.GetErrorCode()[0],
		"the event's own errorCode slice must be unaffected")
}

func TestBuildEventMap_MetadataIsCloned_SoCallersCannotMutateTheEvent(t *testing.T) {
	event := testEvent()

	eventMap := BuildEventMap(event)
	metadata, ok := eventMap["metadata"].(map[string]string)
	require.True(t, ok)

	metadata["chassis_serial"] = "MUTATED"

	assert.Equal(t, "CHASSIS-1", event.GetMetadata()["chassis_serial"],
		"the event's own metadata must be unaffected")
}
