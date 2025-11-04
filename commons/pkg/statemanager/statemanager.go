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

// Package statemanager manages the NVSentinel node state lifecycle through fault detection,
// draining, and remediation phases using the dgxc.nvidia.com/nvsentinel-state node label.
//
// # State Machine
//
// The state machine tracks nodes through the following states:
//
//	                ┌──────────────────┐
//	                │   [NO LABEL]     │  Healthy node
//	                └────────┬─────────┘
//	                         │
//	                         │ Fault detected
//	                         ▼
//	                ┌──────────────────┐
//	                │   quarantined    │
//	                └────────┬─────────┘
//	                         │
//	                         │ Start drain
//	                         ▼
//	                ┌──────────────────┐
//	   Healthy ◄────┤     draining     │
//	   event        └────────┬─────────┘
//	   (cancel)              │
//	      │                  │ Drain completed
//	      │         ┌────────┴────────┐
//	      │         │                 │
//	      │         ▼                 ▼
//	      │  ┌───────────────┐  ┌─────────────┐
//	      │  │drain-succeeded│  │drain-failed │ [TERMINAL]
//	      │  └──────┬────────┘  └─────────────┘
//	      │         │
//	      │         │ Start remediation
//	      │         ▼
//	      │  ┌──────────────┐    Note: fault-remediation
//	      │  │ remediating  │    only consumes drain-succeeded.
//	      │  └──────┬───────┘    drain-failed is a terminal state.
//	      │         │
//	      │    ┌────┴────────────────┐
//	      │    │                     │
//	      │    ▼                     ▼
//	      │  ┌─────────────┐  ┌──────────────┐
//	      │  │ remediation-│  │ remediation- │ [TERMINAL]
//	      │  │ succeeded   │  │   failed     │
//	      │  └─────┬───────┘  └──────────────┘
//	      │        │
//	      │        │ Healthy event
//	      ▼        ▼
//	┌──────────────────────────────┐
//	│        [NO LABEL]            │
//	│   (removeStateLabel=true     │
//	│    removes from ANY state)   │
//	└──────────────────────────────┘
//
// Notes:
//   - [NO LABEL]: No nvsentinel-state label present (healthy node)
//   - [TERMINAL]: Terminal states with no forward transitions
//   - All state names match the dgxc.nvidia.com/nvsentinel-state label values
//   - Label removal (removeStateLabel=true) bypasses all validation
//
// # Valid State Transitions
//
// Expected transitions (no validation error):
//
//	Entry:
//	  none → quarantined           (fault-quarantine detects fault)
//
//	Drain Phase:
//	  quarantined → draining       (node-drainer starts drain)
//	  draining → drain-succeeded   (node-drainer: drain completed)
//	  draining → drain-failed      (node-drainer: drain failed)
//
//	Remediation Phase:
//	  drain-succeeded → remediating              (fault-remediation starts remediation)
//	  remediating → remediation-succeeded        (fault-remediation: success)
//	  remediating → remediation-failed           (fault-remediation: failure)
//
//	Label Removal (from ANY state):
//	  * → (no label)               (removeStateLabel=true - supports canceled drains)
//
// # Invalid State Transitions
//
// These transitions trigger warnings, Prometheus metrics, and return errors,
// but still update the label (observability-focused, not enforcement):
//
//	Skipping States:
//	  none → draining              (should start with quarantined)
//	  none → remediating           (should start with quarantined)
//	  quarantined → drain-succeeded (should go through draining)
//	  quarantined → remediating    (should go through draining and drain-succeeded)
//	  draining → remediating       (should complete drain first)
//
//	Invalid Transitions:
//	  drain-succeeded → drain-failed           (cannot reverse drain result)
//	  drain-failed → remediating               (terminal state - no remediation)
//	  remediation-succeeded → *                (terminal state)
//	  remediation-failed → *                   (terminal state)
//
// # Example Sequences
//
//  1. Successful remediation:
//     none → quarantined → draining → drain-succeeded → remediating → remediation-succeeded → (no label)
//
//  2. Failed remediation:
//     none → quarantined → draining → drain-succeeded → remediating → remediation-failed [TERMINAL]
//
//  3. Failed draining:
//     none → quarantined → draining → drain-failed [TERMINAL]
//
//  4. Canceled drain (healthy event):
//     none → quarantined → draining → (no label)
//
// # Validation Behavior
//
// When removeStateLabel=true: No validation, label removal allowed from any state
//
// When removeStateLabel=false: Validates transition, but even unexpected transitions:
//   - Return error (for caller metrics)
//   - Emit Prometheus metric: nvsentinel_state_transition_unexpected_total
//   - Log warning
//   - Still update the label (labels reflect reality)
//
// # Terminal States
//
// Three states have no valid forward transitions (only label removal):
//   - drain-failed: Remediation doesn't process failed drains
//   - remediation-succeeded: Final success state
//   - remediation-failed: Final failure state
package statemanager

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	NVSentinelStateLabelKey = "dgxc.nvidia.com/nvsentinel-state"
)

type NVSentinelStateLabelValue string

const (
	// Label values applied by the fault-quarantine:
	QuarantinedLabelValue NVSentinelStateLabelValue = "quarantined"

	// Label values applied by the node-drainer:
	DrainingLabelValue       NVSentinelStateLabelValue = "draining"
	DrainSucceededLabelValue NVSentinelStateLabelValue = "drain-succeeded"
	DrainFailedLabelValue    NVSentinelStateLabelValue = "drain-failed"

	// Label values applied by the fault-remediation:
	RemediatingLabelValue          NVSentinelStateLabelValue = "remediating"
	RemediationSucceededLabelValue NVSentinelStateLabelValue = "remediation-succeeded"
	RemediationFailedLabelValue    NVSentinelStateLabelValue = "remediation-failed"
)

/*
The StateManager interface is leveraged by both the node-drainer and the fault-remediation to manage the
lifecycle of the dgxc.nvidia.com/nvsentinel-state node label. Note that the fault-quarantine relies on its
existing node object update calls to add and remove this label.

Example label sequences:
 1. Successful remediation: quarantined → draining → drain-succeeded → remediating →
    remediation-succeeded → (label removed)
 2. Failed remediation: quarantined → draining → drain-succeeded → remediating →
    remediation-failed (terminal state, label remains)
 3. Failed draining: quarantined → draining → drain-failed (terminal state, label remains,
    no remediation)
 4. Canceled drain: quarantined → draining → (label removed via healthy event)

Terminal states (drain-failed, remediation-failed, remediation-succeeded) have no valid forward transitions.
The fault-remediation only consumes drain-succeeded; drain-failed nodes are not remediated.

State transition validation: UpdateNVSentinelStateNodeLabel validates state transitions for observability (emits
metrics/errors for unexpected transitions) but does NOT validate when removing labels (removeStateLabel=true). This
allows canceled drains and healthy events to remove labels from any state without triggering validation errors.
*/
type StateManager interface {
	UpdateNVSentinelStateNodeLabel(ctx context.Context, nodeName string,
		newStateLabelValue NVSentinelStateLabelValue, removeStateLabel bool) (bool, error)
}

type stateManager struct {
	clientSet kubernetes.Interface
}

func NewStateManager(clientSet kubernetes.Interface) StateManager {
	return &stateManager{
		clientSet: clientSet,
	}
}

// UpdateNVSentinelStateNodeLabel will update the given node to the given value for the dgxc.nvidia.com/nvsentinel-state
// label or it will remove the given label if removeStateLabel is true.
func (manager *stateManager) UpdateNVSentinelStateNodeLabel(ctx context.Context, nodeName string,
	newStateLabelValue NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
	nodeModified := false

	err := retry.OnError(retry.DefaultRetry, errors.IsConflict, func() error {
		node, err := manager.clientSet.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		currentValue, exists := node.Labels[NVSentinelStateLabelKey]

		if removeStateLabel {
			if !exists {
				slog.Info("Label already absent",
					"node", nodeName,
					"label", NVSentinelStateLabelKey)

				return nil
			}

			delete(node.Labels, NVSentinelStateLabelKey)

			_, err = manager.clientSet.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to update node %s to remove label: %w", nodeName, err)
			}

			nodeModified = true

			slog.Info("Label removed successfully for node",
				"label", NVSentinelStateLabelKey,
				"node", nodeName)

			return nil
		}

		slog.Info("Labeling node", "node", nodeName, "from", currentValue, "to", newStateLabelValue)

		if exists && currentValue == string(newStateLabelValue) {
			slog.Info("No update needed for node", "node", nodeName, "label", NVSentinelStateLabelKey,
				"value", newStateLabelValue)

			return nil
		}

		// Check for unexpected state transitions (for observability)
		// We'll return the error AFTER updating the label, so callers can emit error metrics
		// while still having the label reflect what modules are actually doing
		validationErr := validateStateTransition(nodeName, currentValue, exists, newStateLabelValue)
		if validationErr != nil {
			slog.Warn("Invalid state transition", "node", nodeName,
				"from", currentValue, "to", newStateLabelValue, "error", validationErr)
		}

		node.Labels[NVSentinelStateLabelKey] = string(newStateLabelValue)

		// Update the node (this happens regardless of validation result)
		_, err = manager.clientSet.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update node %s with new label: %w", nodeName, err)
		}

		nodeModified = true

		slog.Info("Label updated successfully for node",
			"label", NVSentinelStateLabelKey,
			"node", nodeName)

		// Return validation error AFTER successful label update
		// This allows callers to emit error metrics while the label reflects reality
		if validationErr != nil {
			return validationErr
		}

		return nil
	})

	return nodeModified, err
}

// validateStateTransition detects unexpected state transitions for observability.
// Returns an error for unexpected transitions, but the caller updates the label anyway.
// This allows callers to emit error metrics while still reflecting what modules are actually doing.
func validateStateTransition(nodeName, currentValue string, exists bool, targetState NVSentinelStateLabelValue) error {
	fromState := "none"
	if exists {
		fromState = currentValue
	}

	// If no label exists, only Quarantined is the expected first state
	if !exists {
		if targetState != QuarantinedLabelValue {
			stateTransitionUnexpected.WithLabelValues(fromState, string(targetState), nodeName).Inc()

			return fmt.Errorf("unexpected state transition: %s -> %s (expected first state: %s)",
				fromState, targetState, QuarantinedLabelValue)
		}

		return nil
	}

	// Define expected transitions based on the normal state machine flow
	validTransitions := map[NVSentinelStateLabelValue][]NVSentinelStateLabelValue{
		QuarantinedLabelValue:          {DrainingLabelValue},
		DrainingLabelValue:             {DrainSucceededLabelValue, DrainFailedLabelValue},
		DrainSucceededLabelValue:       {RemediatingLabelValue},
		DrainFailedLabelValue:          {}, // Terminal state - fault-remediation doesn't consume drain-failed
		RemediatingLabelValue:          {RemediationSucceededLabelValue, RemediationFailedLabelValue},
		RemediationSucceededLabelValue: {}, // Terminal state
		RemediationFailedLabelValue:    {}, // Terminal state
	}

	currentState := NVSentinelStateLabelValue(currentValue)

	allowedStates, ok := validTransitions[currentState]
	if !ok {
		stateTransitionUnexpected.WithLabelValues(string(currentState), string(targetState), nodeName).Inc()

		return fmt.Errorf("unexpected state transition: unknown current state %s -> %s",
			currentState, targetState)
	}

	// Check if target state is in the expected transitions
	for _, allowed := range allowedStates {
		if targetState == allowed {
			return nil // Expected transition
		}
	}

	// Unexpected transition
	stateTransitionUnexpected.WithLabelValues(string(currentState), string(targetState), nodeName).Inc()

	return fmt.Errorf("unexpected state transition: %s -> %s (expected one of: %v)",
		currentState, targetState, allowedStates)
}
