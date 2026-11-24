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

package reconciler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/model/modeltest"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/crstatus"
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

// activeNodeReader returns the node with a quarantine annotation that holds the event.
func activeNodeReader(t *testing.T, doc *events.HealthEventDoc) *MockNodeReader {
	t.Helper()

	return newMockNodeReader(quarantineAnnotationForTest(t, doc.HealthEvent))
}

// mismatchFixture builds a reconciler and store for an event whose partial drain of GPU-1 does not cover its
// action. It counts created CRs and records the status writes.
type mismatchFixture struct {
	r           *FaultRemediationReconciler
	client      *MockK8sClient
	nodeReader  *MockNodeReader
	store       *MockHealthEventStore
	watcher     *MockChangeStreamWatcher
	doc         *events.HealthEventDoc
	token       datastore.EventWithToken
	createCalls int
	decisions   int
	terminal    bool
}

func newMismatchFixture(t *testing.T, nodeName, gpu string, cfg *config.TomlConfig,
	annotations *MockNodeAnnotationManager) *mismatchFixture {
	t.Helper()

	f := &mismatchFixture{
		watcher: &MockChangeStreamWatcher{},
		doc:     componentResetDoc("mismatch-event", nodeName, gpu, partialDrain("GPU-1")),
		token: datastore.EventWithToken{
			Event:       testRawHealthEvent("mismatch-event", nodeName, protos.RecommendedAction_COMPONENT_RESET),
			ResumeToken: []byte("resume-token"),
		},
	}
	f.client = &MockK8sClient{
		configOverride:            cfg,
		annotationManagerOverride: annotations,
		createMaintenanceResourceFn: func(context.Context, *events.HealthEventData,
			*common.EquivalenceGroupConfig) (string, error) {
			f.createCalls++
			return "test-cr", nil
		},
	}
	f.store = &MockHealthEventStore{
		UpdateHealthEventStatusFn: func(_ context.Context, _ string, status datastore.HealthEventStatus) error {
			if status.RemediationDecision != nil {
				f.decisions++
			}
			if status.FaultRemediated != nil && !*status.FaultRemediated {
				f.terminal = true
			}
			return nil
		},
	}
	f.nodeReader = activeNodeReader(t, f.doc)
	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(context.Context, string, statemanager.NVSentinelStateLabelValue,
			bool) (bool, error) {
			return true, nil
		},
	}
	f.r = NewFaultRemediationReconciler(nil, nil, nil,
		ReconcilerConfig{RemediationClient: f.client, NodeReader: f.nodeReader, StateManager: stateManager}, false)

	return f
}

func (f *mismatchFixture) handle(t *testing.T) (ctrl.Result, error) {
	t.Helper()

	return f.r.handleRemediationEvent(context.Background(), f.doc, f.token, f.watcher, f.store)
}

func (f *mismatchFixture) markProcessedCount() int {
	_, count, _, _ := f.watcher.GetCallCounts()
	return count
}

// A partial drain followed by an action it does not cover must not create a CR. While the quarantine holds the
// event, the event stays unprocessed and has no remediation decision, so the retry evaluates it again.
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
			f := newMismatchFixture(t, nodeName, tt.gpu, tt.config, &MockNodeAnnotationManager{})
			before := getCounterVecValue(t, metrics.ProcessingErrors, drainScopeMismatchError, nodeName)

			for range 2 {
				_, err := f.handle(t)
				require.ErrorIs(t, err, ErrDrainDoesNotCoverAction)
			}

			assert.Zero(t, f.createCalls, "no CR may be created")
			assert.Zero(t, f.decisions, "no remediation decision may be recorded")
			assert.False(t, f.terminal, "the event must not be closed")
			assert.Nil(t, f.doc.HealthEventStatus.GetRemediationDecision())
			assert.Zero(t, f.markProcessedCount(), "the event must stay unprocessed so that it is retried")
			assert.Equal(t, before+2, getCounterVecValue(t, metrics.ProcessingErrors, drainScopeMismatchError, nodeName))
		})
	}
}

// A fix to the configuration lets a retried event continue: once the action covers the partial drain, the next
// pass records the decision and creates the CR.
func TestHandleRemediationEvent_PartialDrainDoesNotCoverAction_ConfigFixCreatesCR(t *testing.T) {
	nodeName := "scope-mismatch-config-fix"
	// The mock's default config maps COMPONENT_RESET to an action on the full node.
	f := newMismatchFixture(t, nodeName, "GPU-1", nil, &MockNodeAnnotationManager{})

	for range 2 {
		_, err := f.handle(t)
		require.ErrorIs(t, err, ErrDrainDoesNotCoverAction)
	}

	require.Zero(t, f.createCalls)

	f.client.configOverride = scopedResetConfig()

	_, err := f.handle(t)
	require.NoError(t, err)
	assert.Equal(t, 1, f.createCalls, "the fixed configuration must create the CR")
	assert.Equal(t, 1, f.decisions, "the fixed configuration must record the decision")
	assert.Equal(t, 1, f.markProcessedCount())
}

// No configuration fix can help a mismatched event once its node is deleted or its quarantine no longer holds
// it, and a fix must not create a CR for a node that is back in service. So the event is closed without a CR
// and without a remediation decision, like the other events of a finished session.
func TestHandleRemediationEvent_PartialDrainDoesNotCoverAction_InactiveQuarantineClosesEvent(t *testing.T) {
	tests := []struct {
		name       string
		nodeReader func(nodeName string) *MockNodeReader
	}{
		{
			name: "node deleted",
			nodeReader: func(nodeName string) *MockNodeReader {
				return &MockNodeReader{getFn: func(context.Context, ctrlclient.ObjectKey, ctrlclient.Object) error {
					return nodeNotFoundErr(nodeName)
				}}
			},
		},
		{
			name:       "quarantine ended",
			nodeReader: func(string) *MockNodeReader { return newMockNodeReader(nil) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeName := "scope-mismatch-inactive-" + strings.ReplaceAll(tt.name, " ", "-")
			f := newMismatchFixture(t, nodeName, "GPU-1", nil, &MockNodeAnnotationManager{})
			before := getCounterVecValue(t, metrics.ProcessingErrors, drainScopeMismatchError, nodeName)

			// The quarantine still holds the event on the first pass, so the event is retried.
			_, err := f.handle(t)
			require.ErrorIs(t, err, ErrDrainDoesNotCoverAction)

			*f.nodeReader = *tt.nodeReader(nodeName)

			result, err := f.handle(t)
			require.NoError(t, err)
			assert.True(t, result.IsZero())
			assert.Zero(t, f.createCalls, "no CR may be created")
			assert.Zero(t, f.decisions, "no remediation decision may be recorded")
			assert.True(t, f.terminal, "expected faultRemediated=false to be written")
			assert.Equal(t, 1, f.markProcessedCount(), "expected resume token to be marked processed")
			assert.Equal(t, before+1, getCounterVecValue(t, metrics.ProcessingErrors, drainScopeMismatchError, nodeName))
		})
	}
}

// A CR of the event's equivalence group that already covers it holds or closes the mismatched event, as it does
// for any event. No CR is created and no mismatch is counted.
func TestHandleRemediationEvent_PartialDrainDoesNotCoverAction_CoveringCRHandlesEvent(t *testing.T) {
	tests := []struct {
		name          string
		crState       crstatus.CRState
		wantProcessed int
		wantRequeue   bool
	}{
		{name: "covering CR in progress holds the event", crState: crstatus.CRStateInProgress, wantRequeue: true},
		{name: "covering CR succeeded closes the event", crState: crstatus.CRStateSucceeded, wantProcessed: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeName := "scope-mismatch-covering-" + strings.ReplaceAll(tt.name, " ", "-")
			// The mock's default config maps COMPONENT_RESET to the restart group on the full node. Another event
			// of the session created the restart CR after this event.
			f := newMismatchFixture(t, nodeName, "GPU-1", nil, &MockNodeAnnotationManager{
				existingCRs:       map[string]string{"restart": "restart-cr"},
				existingCRCreated: time.Now(),
			})
			f.client.mockStatusChecker = &mockStatusChecker{stateByCR: map[string]crstatus.CRState{"restart-cr": tt.crState}}
			f.doc.CreatedAt = time.Now().Add(-time.Hour)
			before := getCounterVecValue(t, metrics.ProcessingErrors, drainScopeMismatchError, nodeName)

			result, err := f.handle(t)
			require.NoError(t, err)
			assert.Equal(t, tt.wantRequeue, !result.IsZero())
			assert.Zero(t, f.createCalls, "no CR may be created")
			assert.Zero(t, f.decisions, "no remediation decision may be recorded")
			assert.Equal(t, tt.wantProcessed, f.markProcessedCount())
			assert.Equal(t, before, getCounterVecValue(t, metrics.ProcessingErrors, drainScopeMismatchError, nodeName))
		})
	}
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
