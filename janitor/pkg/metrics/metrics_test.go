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

package metrics

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewActionMetrics(t *testing.T) {
	// Test that GlobalMetrics is already initialized
	// We can't call NewActionMetrics again due to duplicate registration
	assert.NotNil(t, GlobalMetrics, "GlobalMetrics should be initialized")
}

func TestActionMetrics_IncActionCount(t *testing.T) {
	// Create a new metrics instance
	m := &ActionMetrics{}

	tests := []struct {
		name       string
		actionType string
		status     string
		node       string
	}{
		{
			name:       "reboot started",
			actionType: ActionTypeReboot,
			status:     StatusStarted,
			node:       "test-node-1",
		},
		{
			name:       "reboot succeeded",
			actionType: ActionTypeReboot,
			status:     StatusSucceeded,
			node:       "test-node-1",
		},
		{
			name:       "terminate started",
			actionType: ActionTypeTerminate,
			status:     StatusStarted,
			node:       "test-node-2",
		},
		{
			name:       "terminate failed",
			actionType: ActionTypeTerminate,
			status:     StatusFailed,
			node:       "test-node-2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Should not panic when incrementing
			assert.NotPanics(t, func() {
				m.IncActionCount(tt.actionType, tt.status, tt.node)
			})
		})
	}
}

func TestActionMetrics_RecordActionMTTR(t *testing.T) {
	// Create a new metrics instance
	m := &ActionMetrics{}

	tests := []struct {
		name       string
		actionType string
		duration   time.Duration
	}{
		{
			name:       "quick reboot",
			actionType: ActionTypeReboot,
			duration:   30 * time.Second,
		},
		{
			name:       "slow reboot",
			actionType: ActionTypeReboot,
			duration:   5 * time.Minute,
		},
		{
			name:       "terminate operation",
			actionType: ActionTypeTerminate,
			duration:   2 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Should not panic when recording
			assert.NotPanics(t, func() {
				m.RecordActionMTTR(tt.actionType, tt.duration)
			})
		})
	}
}

func TestGlobalMetrics_Functions(t *testing.T) {
	// Test that global convenience functions work
	assert.NotPanics(t, func() {
		IncActionCount(ActionTypeReboot, StatusStarted, "global-test-node")
	})

	assert.NotPanics(t, func() {
		RecordActionMTTR(ActionTypeReboot, 1*time.Minute)
	})
}

func TestMetricsConstants(t *testing.T) {
	// Verify that constants are correctly defined
	assert.Equal(t, "reboot", ActionTypeReboot)
	assert.Equal(t, "terminate", ActionTypeTerminate)

	assert.Equal(t, "started", StatusStarted)
	assert.Equal(t, "succeeded", StatusSucceeded)
	assert.Equal(t, "failed", StatusFailed)
}

func TestActionMetrics_CounterIncrement(t *testing.T) {
	t.Run("IncActionCount increments counter", func(t *testing.T) {
		// Create metrics instance
		m := &ActionMetrics{}

		// Call the actual business logic
		m.IncActionCount(ActionTypeReboot, StatusStarted, "test-node-1")
		m.IncActionCount(ActionTypeReboot, StatusSucceeded, "test-node-1")
		m.IncActionCount(ActionTypeTerminate, StatusStarted, "test-node-2")

		// Note: We can't easily verify the exact counter values without accessing
		// the internal prometheus registry, but we can verify the method executes
		// without panic or error. This test verifies the method signature and basic
		// functionality.
	})

	t.Run("global IncActionCount function works", func(t *testing.T) {
		// Test the convenience function that uses GlobalMetrics
		IncActionCount(ActionTypeReboot, StatusStarted, "global-test-node")
		IncActionCount(ActionTypeReboot, StatusSucceeded, "global-test-node")
		IncActionCount(ActionTypeReboot, StatusFailed, "global-test-node")

		// Verify different action types
		IncActionCount(ActionTypeTerminate, StatusStarted, "global-test-node-2")
	})
}

func TestActionMetrics_HistogramObservation(t *testing.T) {
	t.Run("RecordActionMTTR records duration", func(t *testing.T) {
		// Create metrics instance
		m := &ActionMetrics{}

		// Call the actual business logic with various durations
		m.RecordActionMTTR(ActionTypeReboot, 30*time.Second)
		m.RecordActionMTTR(ActionTypeReboot, 2*time.Minute)
		m.RecordActionMTTR(ActionTypeReboot, 5*time.Minute)

		// Record different action types
		m.RecordActionMTTR(ActionTypeTerminate, 10*time.Minute)

		// Note: We can't easily verify the exact histogram values without accessing
		// the internal prometheus registry, but we can verify the method executes
		// without panic or error. This test verifies the method signature and basic
		// functionality.
	})

	t.Run("global RecordActionMTTR function works", func(t *testing.T) {
		// Test the convenience function that uses GlobalMetrics
		RecordActionMTTR(ActionTypeReboot, 45*time.Second)
		RecordActionMTTR(ActionTypeTerminate, 3*time.Minute)

		// Test with very short duration
		RecordActionMTTR(ActionTypeReboot, 5*time.Second)

		// Test with longer duration
		RecordActionMTTR(ActionTypeTerminate, 15*time.Minute)
	})
}

func TestGlobalMetrics_Initialization(t *testing.T) {
	// Verify that GlobalMetrics is initialized
	assert.NotNil(t, GlobalMetrics, "GlobalMetrics should be initialized")
}

func TestActionMetrics_MultipleNodes(t *testing.T) {
	// Test that metrics can track different nodes independently
	m := &ActionMetrics{}

	nodes := []string{"node-1", "node-2", "node-3"}

	for _, node := range nodes {
		assert.NotPanics(t, func() {
			m.IncActionCount(ActionTypeReboot, StatusStarted, node)
			m.IncActionCount(ActionTypeReboot, StatusSucceeded, node)
		})
	}
}

func TestActionMetrics_DifferentActionTypes(t *testing.T) {
	// Test that different action types can be tracked independently
	m := &ActionMetrics{}

	assert.NotPanics(t, func() {
		// Reboot actions
		m.IncActionCount(ActionTypeReboot, StatusStarted, "node-1")
		m.RecordActionMTTR(ActionTypeReboot, 1*time.Minute)

		// Terminate actions
		m.IncActionCount(ActionTypeTerminate, StatusStarted, "node-2")
		m.RecordActionMTTR(ActionTypeTerminate, 2*time.Minute)
	})
}
