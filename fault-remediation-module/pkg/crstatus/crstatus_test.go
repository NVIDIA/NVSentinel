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

package crstatus

import (
	"testing"

	platformconnector "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCRStatusConstants(t *testing.T) {
	assert.Equal(t, CRStatus("Succeeded"), CRStatusSucceeded)
	assert.Equal(t, CRStatus("InProgress"), CRStatusInProgress)
	assert.Equal(t, CRStatus("Failed"), CRStatusFailed)
	assert.Equal(t, CRStatus("NotFound"), CRStatusNotFound)
}

func TestNewCRStatusCheckerFactory(t *testing.T) {
	factory := NewCRStatusCheckerFactory(nil, nil, false)
	assert.NotNil(t, factory)
	assert.False(t, factory.dryRun)

	factoryDryRun := NewCRStatusCheckerFactory(nil, nil, true)
	assert.NotNil(t, factoryDryRun)
	assert.True(t, factoryDryRun.dryRun)
}

func TestCRStatusCheckerFactory_GetStatusChecker(t *testing.T) {
	factory := NewCRStatusCheckerFactory(nil, nil, false)

	tests := []struct {
		name        string
		action      platformconnector.RecommenedAction
		expectError bool
	}{
		{
			name:        "COMPONENT_RESET returns RebootNode checker",
			action:      platformconnector.RecommenedAction_COMPONENT_RESET,
			expectError: false,
		},
		{
			name:        "RESTART_VM returns RebootNode checker",
			action:      platformconnector.RecommenedAction_RESTART_VM,
			expectError: false,
		},
		{
			name:        "RESTART_BM returns RebootNode checker",
			action:      platformconnector.RecommenedAction_RESTART_BM,
			expectError: false,
		},
		{
			name:        "CONTACT_SUPPORT returns error (no checker available)",
			action:      platformconnector.RecommenedAction_CONTACT_SUPPORT,
			expectError: true,
		},
		{
			name:        "NONE returns error (no checker available)",
			action:      platformconnector.RecommenedAction_NONE,
			expectError: true,
		},
		{
			name:        "UNKNOWN returns error (no checker available)",
			action:      platformconnector.RecommenedAction_UNKNOWN,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker, err := factory.GetStatusChecker(tt.action)
			if tt.expectError {
				assert.Error(t, err)
				assert.Nil(t, checker)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, checker)
				assert.IsType(t, &RebootNodeCRStatusChecker{}, checker)
			}
		})
	}
}

func TestNewRebootNodeCRStatusChecker(t *testing.T) {
	checker := NewRebootNodeCRStatusChecker(nil, nil, false)
	assert.NotNil(t, checker)
	assert.False(t, checker.dryRun)

	checkerDryRun := NewRebootNodeCRStatusChecker(nil, nil, true)
	assert.NotNil(t, checkerDryRun)
	assert.True(t, checkerDryRun.dryRun)
}

func TestExtractRebootNodeStatus(t *testing.T) {
	checker := NewRebootNodeCRStatusChecker(nil, nil, false)

	tests := []struct {
		name           string
		cr             *unstructured.Unstructured
		expectedStatus CRStatus
		expectError    bool
	}{
		{
			name: "CR with no status returns InProgress",
			cr: &unstructured.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"name": "test-cr",
					},
				},
			},
			expectedStatus: CRStatusInProgress,
			expectError:    false,
		},
		{
			name: "CR with succeeded condition and completion time returns Succeeded",
			cr: &unstructured.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"name": "test-cr",
					},
					"status": map[string]any{
						"completionTime": "2024-01-01T00:00:00Z",
						"conditions": []any{
							map[string]any{
								"type":   "SignalSent",
								"status": "True",
							},
							map[string]any{
								"type":   "NodeReady",
								"status": "True",
							},
						},
					},
				},
			},
			expectedStatus: CRStatusSucceeded,
			expectError:    false,
		},
		{
			name: "CR with failed signal send returns Failed",
			cr: &unstructured.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"name": "test-cr",
					},
					"status": map[string]any{
						"completionTime": "2024-01-01T00:00:00Z",
						"conditions": []any{
							map[string]any{
								"type":   "SignalSent",
								"status": "False",
								"reason": "Failed",
							},
						},
					},
				},
			},
			expectedStatus: CRStatusFailed,
			expectError:    false,
		},
		{
			name: "CR with node not ready and completion time returns Failed",
			cr: &unstructured.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"name": "test-cr",
					},
					"status": map[string]any{
						"completionTime": "2024-01-01T00:00:00Z",
						"conditions": []any{
							map[string]any{
								"type":   "SignalSent",
								"status": "True",
							},
							map[string]any{
								"type":   "NodeReady",
								"status": "False",
							},
						},
					},
				},
			},
			expectedStatus: CRStatusFailed,
			expectError:    false,
		},
		{
			name: "CR with signal sent but not complete returns InProgress",
			cr: &unstructured.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"name": "test-cr",
					},
					"status": map[string]any{
						"conditions": []any{
							map[string]any{
								"type":   "SignalSent",
								"status": "True",
							},
						},
					},
				},
			},
			expectedStatus: CRStatusInProgress,
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, err := checker.extractRebootNodeStatus(tt.cr)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedStatus, status)
			}
		})
	}
}

func TestParseRebootConditions(t *testing.T) {
	tests := []struct {
		name                 string
		conditions           []any
		isComplete           bool
		expectedSignalStatus string
		expectedNodeStatus   string
		expectedEarlyStatus  *CRStatus
	}{
		{
			name: "signal sent and node ready",
			conditions: []any{
				map[string]any{
					"type":   "SignalSent",
					"status": "True",
				},
				map[string]any{
					"type":   "NodeReady",
					"status": "True",
				},
			},
			isComplete:           false,
			expectedSignalStatus: "True",
			expectedNodeStatus:   "True",
			expectedEarlyStatus:  nil,
		},
		{
			name: "signal failed to send with completion returns early fail",
			conditions: []any{
				map[string]any{
					"type":   "SignalSent",
					"status": "False",
					"reason": "Failed",
				},
			},
			isComplete:           true,
			expectedSignalStatus: "False",
			expectedNodeStatus:   "",
			expectedEarlyStatus:  func() *CRStatus { s := CRStatusFailed; return &s }(),
		},
		{
			name: "node not ready with completion does not return early fail",
			conditions: []any{
				map[string]any{
					"type":   "NodeReady",
					"status": "False",
					"reason": "Failed",
				},
			},
			isComplete:           true,
			expectedSignalStatus: "",
			expectedNodeStatus:   "False",
			expectedEarlyStatus:  nil, // NodeReady failures don't cause early exit
		},
		{
			name:                 "empty conditions",
			conditions:           []any{},
			isComplete:           false,
			expectedSignalStatus: "",
			expectedNodeStatus:   "",
			expectedEarlyStatus:  nil,
		},
		{
			name: "invalid condition type",
			conditions: []any{
				"invalid",
			},
			isComplete:           false,
			expectedSignalStatus: "",
			expectedNodeStatus:   "",
			expectedEarlyStatus:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signalStatus, nodeStatus, earlyStatus := parseRebootConditions(tt.conditions, tt.isComplete)
			assert.Equal(t, tt.expectedSignalStatus, signalStatus)
			assert.Equal(t, tt.expectedNodeStatus, nodeStatus)
			if tt.expectedEarlyStatus == nil {
				assert.Nil(t, earlyStatus)
			} else {
				require.NotNil(t, earlyStatus)
				assert.Equal(t, *tt.expectedEarlyStatus, *earlyStatus)
			}
		})
	}
}

func TestDetermineRebootStatus(t *testing.T) {
	tests := []struct {
		name            string
		signalStatus    string
		nodeReadyStatus string
		isComplete      bool
		expectedStatus  CRStatus
	}{
		{
			name:            "both conditions true and complete = Succeeded",
			signalStatus:    "True",
			nodeReadyStatus: "True",
			isComplete:      true,
			expectedStatus:  CRStatusSucceeded,
		},
		{
			name:            "signal true, node false, complete = Failed",
			signalStatus:    "True",
			nodeReadyStatus: "False",
			isComplete:      true,
			expectedStatus:  CRStatusFailed,
		},
		{
			name:            "signal false, complete = InProgress (signal not true yet)",
			signalStatus:    "False",
			nodeReadyStatus: "",
			isComplete:      true,
			expectedStatus:  CRStatusInProgress,
		},
		{
			name:            "signal true, no node status, complete = InProgress (waiting for node ready)",
			signalStatus:    "True",
			nodeReadyStatus: "",
			isComplete:      true,
			expectedStatus:  CRStatusInProgress,
		},
		{
			name:            "signal true, not complete = InProgress",
			signalStatus:    "True",
			nodeReadyStatus: "",
			isComplete:      false,
			expectedStatus:  CRStatusInProgress,
		},
		{
			name:            "no conditions, not complete = InProgress",
			signalStatus:    "",
			nodeReadyStatus: "",
			isComplete:      false,
			expectedStatus:  CRStatusInProgress,
		},
		{
			name:            "no conditions, complete = InProgress (no signal status means not ready)",
			signalStatus:    "",
			nodeReadyStatus: "",
			isComplete:      true,
			expectedStatus:  CRStatusInProgress,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := determineRebootStatus(tt.signalStatus, tt.nodeReadyStatus, tt.isComplete)
			assert.Equal(t, tt.expectedStatus, status)
		})
	}
}
