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

package reconciler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/model/modeltest"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/events"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func partialDrain(value string) *protos.DrainDecision {
	return &protos.DrainDecision{
		Method: model.DrainMethodBuiltIn,
		Scope:  &protos.Entity{EntityType: model.GPUUUIDEntityType, EntityValue: value},
	}
}

func TestDrainCoversAction(t *testing.T) {
	scopedGPU1 := &common.EquivalenceGroupConfig{ImpactedEntityScope: "GPU_UUID", ImpactedEntityScopeValue: "GPU-1"}
	fullNode := &common.EquivalenceGroupConfig{EffectiveEquivalenceGroup: "restart"}

	tests := []struct {
		name        string
		decision    *protos.DrainDecision
		groupConfig *common.EquivalenceGroupConfig
		want        bool
	}{
		{"no drain decision is not checked", nil, fullNode, true},
		{"full drain covers an action on the full node", &protos.DrainDecision{Method: model.DrainMethodBuiltIn}, fullNode, true},
		{"full drain covers a scoped action", &protos.DrainDecision{Method: model.DrainMethodBuiltIn}, scopedGPU1, true},
		{"partial drain covers the action on the same entity", partialDrain("GPU-1"), scopedGPU1, true},
		{"partial drain does not cover an action on the full node", partialDrain("GPU-1"), fullNode, false},
		{"partial drain does not cover an action on another entity", partialDrain("GPU-2"), scopedGPU1, false},
		{
			name:     "partial drain does not cover an action scoped to another entity type",
			decision: partialDrain("GPU-1"),
			groupConfig: &common.EquivalenceGroupConfig{
				ImpactedEntityScope: "ACCEL_SERIAL", ImpactedEntityScopeValue: "GPU-1",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, drainCoversAction(tt.decision, tt.groupConfig))
		})
	}
}

// scopedResetConfig maps COMPONENT_RESET to an action scoped to GPU_UUID, as a partial drain needs.
func scopedResetConfig() *config.TomlConfig {
	return &config.TomlConfig{
		RemediationActions: map[string]config.MaintenanceResource{
			protos.RecommendedAction_COMPONENT_RESET.String(): {
				EquivalenceGroup:    "reset",
				ImpactedEntityScope: "GPU_UUID",
			},
		},
		PartialRemediationEntityTypes: modeltest.GPUEntityTypes(),
	}
}

func componentResetDoc(eventID, nodeName, gpu string, decision *protos.DrainDecision) *events.HealthEventDoc {
	return &events.HealthEventDoc{
		ID: eventID,
		HealthEventWithStatus: model.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName:          nodeName,
				RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
				EntitiesImpacted:  []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: gpu}},
			},
			HealthEventStatus: &protos.HealthEventStatus{DrainDecision: decision},
		},
	}
}

// A partial drain followed by an action it does not cover must not create a CR. The event stays
// unprocessed and has no remediation decision, so the retry evaluates it again.
func TestHandleRemediationEvent_PartialDrainDoesNotCoverAction_RetriesWithoutCR(t *testing.T) {
	tests := []struct {
		name   string
		config *config.TomlConfig
		gpu    string
	}{
		// The mock's default config maps COMPONENT_RESET to an action on the full node.
		{name: "action on the full node", gpu: "GPU-1"},
		{name: "scoped action on another GPU", config: scopedResetConfig(), gpu: "GPU-2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeName := "scope-mismatch-node-" + tt.gpu
			createCalls := 0
			mockK8sClient := &MockK8sClient{
				configOverride: tt.config,
				// The node exists, so the event is retried.
				annotationManagerOverride: &MockNodeAnnotationManager{},
				createMaintenanceResourceFn: func(context.Context, *events.HealthEventData,
					*common.EquivalenceGroupConfig) (string, error) {
					createCalls++
					return "test-cr", nil
				},
			}
			r := NewFaultRemediationReconciler(nil, nil, nil, ReconcilerConfig{RemediationClient: mockK8sClient}, false)

			writes := 0
			mockStore := &MockHealthEventStore{
				UpdateHealthEventStatusFn: func(context.Context, string, datastore.HealthEventStatus) error {
					writes++
					return nil
				},
			}
			mockWatcher := &MockChangeStreamWatcher{}
			doc := componentResetDoc("mismatch-event", nodeName, tt.gpu, partialDrain("GPU-1"))
			eventWithToken := datastore.EventWithToken{
				Event:       testRawHealthEvent("mismatch-event", nodeName, protos.RecommendedAction_COMPONENT_RESET),
				ResumeToken: []byte("resume-token"),
			}
			before := getCounterVecValue(t, metrics.ProcessingErrors, drainScopeMismatchError, nodeName)

			for range 2 {
				_, err := r.handleRemediationEvent(context.Background(), doc, eventWithToken, mockWatcher, mockStore)
				require.ErrorIs(t, err, ErrDrainDoesNotCoverAction)
			}

			assert.Zero(t, createCalls, "no CR may be created")
			assert.Zero(t, writes, "no remediation decision or status may be written")
			assert.Nil(t, doc.HealthEventStatus.GetRemediationDecision())
			_, markProcessedCount, _, _ := mockWatcher.GetCallCounts()
			assert.Zero(t, markProcessedCount, "the event must stay unprocessed so that it is retried")
			assert.Equal(t, before+2, getCounterVecValue(t, metrics.ProcessingErrors, drainScopeMismatchError, nodeName))
		})
	}
}

// A mismatched event on a deleted node cannot continue after a configuration fix, so it is closed like the
// other events of a deleted node instead of being retried. It still gets no CR and no remediation decision.
func TestHandleRemediationEvent_PartialDrainDoesNotCoverAction_DeletedNodeClosesEvent(t *testing.T) {
	nodeName := "scope-mismatch-deleted-node"
	createCalls := 0
	mockK8sClient := &MockK8sClient{
		annotationManagerOverride: &MockNodeAnnotationManager{getRemediationStateErr: nodeNotFoundErr(nodeName)},
		createMaintenanceResourceFn: func(context.Context, *events.HealthEventData,
			*common.EquivalenceGroupConfig) (string, error) {
			createCalls++
			return "test-cr", nil
		},
	}
	r := NewFaultRemediationReconciler(nil, nil, nil, ReconcilerConfig{RemediationClient: mockK8sClient}, false)

	decisionRecorded := false
	terminalWritten := false
	mockStore := &MockHealthEventStore{
		UpdateHealthEventStatusFn: func(_ context.Context, _ string, status datastore.HealthEventStatus) error {
			if status.RemediationDecision != nil {
				decisionRecorded = true
			}
			if status.FaultRemediated != nil && !*status.FaultRemediated {
				terminalWritten = true
			}
			return nil
		},
	}
	mockWatcher := &MockChangeStreamWatcher{}
	// The mock's default config maps COMPONENT_RESET to an action on the full node.
	doc := componentResetDoc("mismatch-event", nodeName, "GPU-1", partialDrain("GPU-1"))
	eventWithToken := datastore.EventWithToken{
		Event:       testRawHealthEvent("mismatch-event", nodeName, protos.RecommendedAction_COMPONENT_RESET),
		ResumeToken: []byte("resume-token"),
	}
	before := getCounterVecValue(t, metrics.ProcessingErrors, drainScopeMismatchError, nodeName)

	result, err := r.handleRemediationEvent(context.Background(), doc, eventWithToken, mockWatcher, mockStore)
	require.NoError(t, err)
	assert.True(t, result.IsZero())
	assert.Zero(t, createCalls, "no CR may be created")
	assert.False(t, decisionRecorded, "no remediation decision may be recorded")
	assert.True(t, terminalWritten, "expected faultRemediated=false to be written")
	_, markProcessedCount, _, _ := mockWatcher.GetCallCounts()
	assert.Equal(t, 1, markProcessedCount, "expected resume token to be marked processed")
	assert.Equal(t, before, getCounterVecValue(t, metrics.ProcessingErrors, drainScopeMismatchError, nodeName))
}

// The check does not stop events that it covers. Each event reaches the CR check, which finds the node
// deleted and closes the event.
func TestHandleRemediationEvent_DrainCoversAction_Continues(t *testing.T) {
	tests := []struct {
		name     string
		config   *config.TomlConfig
		decision *protos.DrainDecision
	}{
		{name: "partial drain of the GPU that the action resets", config: scopedResetConfig(), decision: partialDrain("GPU-1")},
		{name: "full drain before an action on the full node", decision: &protos.DrainDecision{Method: model.DrainMethodBuiltIn}},
		{name: "event drained before drain decisions were recorded"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeName := "deleted-node"
			mockK8sClient := &MockK8sClient{
				configOverride:            tt.config,
				annotationManagerOverride: &MockNodeAnnotationManager{getRemediationStateErr: nodeNotFoundErr(nodeName)},
			}
			r := NewFaultRemediationReconciler(nil, nil, nil, ReconcilerConfig{RemediationClient: mockK8sClient}, false)

			decisionRecorded := false
			mockStore := &MockHealthEventStore{
				UpdateHealthEventStatusFn: func(_ context.Context, _ string, status datastore.HealthEventStatus) error {
					if status.RemediationDecision != nil {
						decisionRecorded = true
					}
					return nil
				},
			}
			mockWatcher := &MockChangeStreamWatcher{}
			doc := componentResetDoc("covered-event", nodeName, "GPU-1", tt.decision)
			eventWithToken := datastore.EventWithToken{
				Event:       testRawHealthEvent("covered-event", nodeName, protos.RecommendedAction_COMPONENT_RESET),
				ResumeToken: []byte("resume-token"),
			}
			before := getCounterVecValue(t, metrics.ProcessingErrors, drainScopeMismatchError, nodeName)

			_, err := r.handleRemediationEvent(context.Background(), doc, eventWithToken, mockWatcher, mockStore)
			require.NoError(t, err)
			assert.True(t, decisionRecorded, "the event passed the check and recorded its decision")
			_, markProcessedCount, _, _ := mockWatcher.GetCallCounts()
			assert.Equal(t, 1, markProcessedCount)
			assert.Equal(t, before, getCounterVecValue(t, metrics.ProcessingErrors, drainScopeMismatchError, nodeName))
		})
	}
}
