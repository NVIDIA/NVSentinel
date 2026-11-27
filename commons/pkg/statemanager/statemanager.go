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
//	                │   quarantined    ├──────────────────────┐
//	                └────────┬─────────┘                      │
//	                         │                                │
//	                         │ Start drain                    │ No pods to drain
//	                         ▼                                │
//	                ┌──────────────────┐                      │
//	   Healthy ◄────┤     draining     │                      │
//	   event        └────────┬─────────┘                      │
//	   (cancel)              │                                │
//	      │                  │ Drain completed                │
//	      │         ┌────────┴────────┐                       │
//	      │         │                 │                       │
//	      │         ▼                 ▼                       │
//	      │  ┌───────────────┐  ┌────────────────┐            │
//	      │  │drain-failed   │  │drain-succeeded │◄───────────┘
//	      │  └───────────────┘  └───────┬────────┘
//	      │    [TERMINAL]              │
//	      │                            │ Start remediation
//	      │                            ▼
//	      │                     ┌──────────────┐  Note: fault-remediation
//	      │                     │ remediating  │  only consumes drain-succeeded.
//	      │                     └──────┬───────┘  drain-failed is a terminal state.
//	      │                            │
//	      │                   ┌────────┴────────┐
//	      │                   │                 │
//	      │                   ▼                 ▼
//	      │            ┌─────────────┐  ┌──────────────┐
//	      │            │ remediation-│◄─┤ remediation- │
//	      │            │ succeeded   ├─►│   failed     │
//	      │            └─────┬───────┘  └──────────────┘
//	      │                  │   partial-recovery recompute
//	      │                  │   (recomputed between the two from remaining failures)
//	      │                  │ Healthy event
//	      ▼                  ▼
//	┌──────────────────────────────┐
//	│        [NO LABEL]            │
//	│   (removeStateLabel=true     │
//	│    removes from ANY state)   │
//	└──────────────────────────────┘
//
// Notes:
//   - [NO LABEL]: No nvsentinel-state label present (healthy node)
//   - [TERMINAL]: Terminal states with no forward transitions
//   - remediation-succeeded and remediation-failed are terminal for a single failure, but
//     fault-remediation may recompute between them on a partial recovery, move either of
//     them to remediation-stopped when every remaining active failure is stopped, and
//     return either of them to remediating when a new remediation cycle starts (see below)
//   - drain-stopped and remediation-stopped record a rule-set stop. They are not terminal.
//     The diagram shows the main flow only. The lists below include the stop transitions.
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
//	  quarantined → drain-succeeded (node-drainer: no pods to drain)
//	  quarantined → drain-stopped  (a rule set stops the drain)
//	  draining → drain-succeeded   (node-drainer: drain completed)
//	  draining → drain-failed      (node-drainer: drain failed)
//	  drain-stopped → draining     (a later event drains the node)
//	  drain-stopped → drain-succeeded (a later event finds no pods to drain)
//
//	Remediation Phase:
//	  drain-succeeded → remediating              (fault-remediation starts remediation)
//	  drain-succeeded → remediation-failed       (recommended action is unsupported)
//	  drain-succeeded → remediation-stopped      (a rule set stops the remediation)
//	  remediating → remediation-succeeded        (fault-remediation: success)
//	  remediating → remediation-failed           (fault-remediation: failure)
//	  remediating → remediation-stopped          (recompute: every remaining active failure is stopped)
//	  remediation-stopped → remediating          (fault-remediation starts remediation for a later event)
//	  remediation-stopped → remediation-succeeded (remediation for a later event succeeds)
//	  remediation-stopped → remediation-failed   (remediation for a later event fails or is unsupported)
//
//	Partial-recovery recompute (node stays quarantined by other active failures):
//	  remediation-failed → remediation-succeeded (fault-remediation: recovered failure was the
//	                                              only failed one; remaining failures are remediated)
//	  remediation-succeeded → remediation-failed (fault-remediation: a remaining active failure is
//	                                              unsupported or failed remediation)
//	  remediation-succeeded → remediation-stopped (fault-remediation: every remaining active failure
//	                                               is stopped)
//	  remediation-failed → remediation-stopped    (fault-remediation: every remaining active failure
//	                                               is stopped)
//
//	Re-remediation (a new remediation cycle starts while the node stays quarantined):
//	  remediation-succeeded → remediating (fault-remediation: a new remediation-ready event
//	                                       arrived after the previous maintenance CR completed,
//	                                       e.g. a post-reboot fault)
//	  remediation-failed → remediating    (fault-remediation: a failed CR is retried with a new CR)
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
//	  quarantined → remediating    (should go through draining and drain-succeeded)
//	  draining → remediating       (should complete drain first)
//
//	Invalid Transitions:
//	  drain-succeeded → drain-failed           (cannot reverse drain result)
//	  drain-failed → remediating               (terminal state - no remediation)
//	  drain-stopped → drain-failed             (a drain must start before it can fail)
//	  drain-stopped → remediating              (the node was not drained)
//	  quarantined → remediation-stopped        (remediation follows a completed drain)
//	  remediation-succeeded → * (except remediation-failed via partial-recovery recompute,
//	                             remediating via re-remediation, and remediation-stopped when
//	                             every remaining active failure is stopped)
//	  remediation-failed → * (except remediation-succeeded via partial-recovery recompute,
//	                          remediating via re-remediation, and remediation-stopped when
//	                          every remaining active failure is stopped)
//
// # Example Sequences
//
//  1. Successful remediation:
//     none → quarantined → draining → drain-succeeded → remediating → remediation-succeeded → (no label)
//
//  2. Failed remediation:
//     none → quarantined → draining → drain-succeeded → remediating → remediation-failed [TERMINAL]
//
//  3. Unsupported remediation:
//     none → quarantined → draining → drain-succeeded → remediation-failed [TERMINAL]
//
//  4. No pods to drain:
//     none → quarantined → drain-succeeded → remediating → remediation-succeeded → (no label)
//
//  5. Failed draining:
//     none → quarantined → draining → drain-failed [TERMINAL]
//
//  6. Canceled drain (healthy event):
//     none → quarantined → draining → (no label)
//
//  7. Stopped drain:
//     none → quarantined → drain-stopped → (no label)
//
//  8. Stopped remediation:
//     none → quarantined → drain-succeeded → remediation-stopped → (no label)
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
// drain-failed has no valid forward transitions (only label removal):
//   - drain-failed: Remediation doesn't process failed drains
//
// remediation-succeeded and remediation-failed are terminal for a single failure. Forward
// transitions are allowed between the two of them, back to remediating, and to
// remediation-stopped. fault-remediation recomputes the node label from the remaining active
// failures during a partial recovery, including when every remaining failure is stopped. It
// also returns to remediating when a new remediation cycle starts (a remediation-ready event
// arrived after the previous maintenance CR completed, or a failed CR is retried with a new CR):
//   - remediation-succeeded: Success state (may be recomputed or re-enter remediating)
//   - remediation-failed: Failure state (may be recomputed or re-enter remediating)
//
// drain-stopped and remediation-stopped are not terminal. A later event in the same quarantine
// session can move drain-stopped to draining or drain-succeeded, and remediation-stopped to
// remediating, remediation-succeeded, or remediation-failed. Label removal still accepts both.
package statemanager

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	corev1 "k8s.io/api/core/v1"
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
	DrainStoppedLabelValue   NVSentinelStateLabelValue = "drain-stopped"

	// Label values applied by the fault-remediation:
	RemediatingLabelValue          NVSentinelStateLabelValue = "remediating"
	RemediationSucceededLabelValue NVSentinelStateLabelValue = "remediation-succeeded"
	RemediationFailedLabelValue    NVSentinelStateLabelValue = "remediation-failed"
	RemediationStoppedLabelValue   NVSentinelStateLabelValue = "remediation-stopped"
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
 3. No pods to drain: quarantined → drain-succeeded → remediating →
    remediation-succeeded → (label removed)
 4. Failed draining: quarantined → draining → drain-failed (terminal state, label remains,
    no remediation)
 5. Canceled drain: quarantined → draining → (label removed via healthy event)
 6. Stopped drain: quarantined → drain-stopped (label remains until the quarantine ends)
 7. Stopped remediation: quarantined → drain-succeeded → remediation-stopped (label remains
    until the quarantine ends)

drain-failed has no valid forward transitions; fault-remediation only consumes drain-succeeded and does
not remediate drain-failed nodes. remediation-failed and remediation-succeeded are terminal for a single
failure, but fault-remediation may recompute between them during a partial recovery (a tracked failure
clears while the node stays quarantined by other active failures). drain-stopped and
remediation-stopped are not terminal: a later event in the same quarantine can move the node on.

State transition validation: UpdateNVSentinelStateNodeLabel validates state transitions for observability (emits
metrics/errors for unexpected transitions) but does NOT validate when removing labels (removeStateLabel=true). This
allows canceled drains and healthy events to remove labels from any state without triggering validation errors.

UpdateNVSentinelStateNodeLabelIfMatch sets the label only when the current value is one of the given
values. The check and the write share one conflict-retried loop, so a concurrent writer cannot have a
later state overwritten. The same transition check runs when the label is written.
*/
type StateManager interface {
	UpdateNVSentinelStateNodeLabel(ctx context.Context, nodeName string,
		newStateLabelValue NVSentinelStateLabelValue, removeStateLabel bool) (bool, error)
	RemoveNVSentinelStateNodeLabelIfMatch(ctx context.Context, nodeName string,
		expectedValues ...NVSentinelStateLabelValue) (bool, error)
	UpdateNVSentinelStateNodeLabelIfMatch(ctx context.Context, nodeName string,
		newStateLabelValue NVSentinelStateLabelValue, expectedValues ...NVSentinelStateLabelValue) (bool, error)
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

// RemoveNVSentinelStateNodeLabelIfMatch atomically removes the state label only when its latest
// value is one of expectedValues. The ownership check and deletion share the same conflict-retried
// read/update loop, so a concurrent writer cannot have its newer state removed.
func (manager *stateManager) RemoveNVSentinelStateNodeLabelIfMatch(
	ctx context.Context,
	nodeName string,
	expectedValues ...NVSentinelStateLabelValue,
) (bool, error) {
	return manager.updateStateLabelIfMatch(
		ctx,
		nodeName,
		expectedValues,
		"Label already absent",
		"Skipping conditional label removal because the current value is not owned",
		func(node *corev1.Node, _ string) bool {
			delete(node.Labels, NVSentinelStateLabelKey)

			return true
		},
		func(err error) error {
			return fmt.Errorf("failed to update node %s to conditionally remove label: %w", nodeName, err)
		},
		func(previousValue string) error {
			slog.Info("Label conditionally removed successfully for node",
				"label", NVSentinelStateLabelKey,
				"previousValue", previousValue,
				"node", nodeName)

			return nil
		},
	)
}

// UpdateNVSentinelStateNodeLabelIfMatch sets the state label only when its latest value is one of
// expectedValues. The check and the write share one conflict-retried loop. An unexpected transition
// still writes the label, then returns the validation error, matching UpdateNVSentinelStateNodeLabel.
// An empty newStateLabelValue writes nothing: label removal has its own methods.
func (manager *stateManager) UpdateNVSentinelStateNodeLabelIfMatch(
	ctx context.Context,
	nodeName string,
	newStateLabelValue NVSentinelStateLabelValue,
	expectedValues ...NVSentinelStateLabelValue,
) (bool, error) {
	if newStateLabelValue == "" {
		return false, fmt.Errorf("new state label value is empty for node %s", nodeName)
	}

	return manager.updateStateLabelIfMatch(
		ctx,
		nodeName,
		expectedValues,
		"Skipping conditional label update because the state label is absent",
		"Skipping conditional label update because the current value is not expected",
		func(node *corev1.Node, currentValue string) bool {
			return prepareMatchedStateLabel(node, nodeName, currentValue, newStateLabelValue)
		},
		func(err error) error {
			return fmt.Errorf("failed to update node %s to conditionally set label: %w", nodeName, err)
		},
		func(previousValue string) error {
			validationErr := validateStateTransition(nodeName, previousValue, true, newStateLabelValue)
			if validationErr != nil {
				slog.Warn("Invalid state transition",
					"node", nodeName,
					"from", previousValue,
					"to", newStateLabelValue,
					"error", validationErr)
			}

			slog.Info("Label updated successfully for node",
				"label", NVSentinelStateLabelKey,
				"previousValue", previousValue,
				"value", newStateLabelValue,
				"node", nodeName)

			return validationErr
		},
	)
}

// updateStateLabelIfMatch reads the node and applies apply only when the current state label is
// one of expectedValues. A conflict retries the read and the check. apply reports whether the node
// changed. onWritten runs only after a successful write and can return an observability error.
func (manager *stateManager) updateStateLabelIfMatch(
	ctx context.Context,
	nodeName string,
	expectedValues []NVSentinelStateLabelValue,
	missingMessage string,
	skipMessage string,
	apply func(node *corev1.Node, currentValue string) bool,
	wrapUpdateErr func(error) error,
	onWritten func(previousValue string) error,
) (bool, error) {
	if len(expectedValues) == 0 {
		return false, nil
	}

	expected := labelValueSet(expectedValues)
	nodeModified := false
	getFailed := false

	err := retry.OnError(retry.DefaultRetry, errors.IsConflict, func() error {
		getFailed = false

		node, currentValue, matched, readErr := manager.matchingStateLabel(
			ctx,
			nodeName,
			expected,
			missingMessage,
			skipMessage,
		)
		if readErr != nil {
			getFailed = true

			return readErr
		}

		if !matched {
			return nil
		}

		if !apply(node, currentValue) {
			return nil
		}

		_, err := manager.clientSet.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		if err != nil {
			if errors.IsConflict(err) {
				return err
			}

			return wrapUpdateErr(err)
		}

		nodeModified = true

		return onWritten(currentValue)
	})
	if getFailed {
		return nodeModified, fmt.Errorf(
			"failed to get node %s for conditional state label change: %w",
			nodeName,
			err,
		)
	}

	if errors.IsConflict(err) {
		return nodeModified, wrapUpdateErr(err)
	}

	return nodeModified, err
}

func (manager *stateManager) matchingStateLabel(
	ctx context.Context,
	nodeName string,
	expected map[string]struct{},
	missingMessage string,
	skipMessage string,
) (*corev1.Node, string, bool, error) {
	node, err := manager.clientSet.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, "", false, err
	}

	currentValue, exists := node.Labels[NVSentinelStateLabelKey]
	if !exists {
		slog.Info(missingMessage,
			"node", nodeName,
			"label", NVSentinelStateLabelKey)

		return nil, "", false, nil
	}

	if _, matches := expected[currentValue]; !matches {
		slog.Info(skipMessage,
			"node", nodeName,
			"label", NVSentinelStateLabelKey,
			"value", currentValue)

		return nil, currentValue, false, nil
	}

	return node, currentValue, true, nil
}

func labelValueSet(values []NVSentinelStateLabelValue) map[string]struct{} {
	expected := make(map[string]struct{}, len(values))

	for _, value := range values {
		expected[string(value)] = struct{}{}
	}

	return expected
}

// prepareMatchedStateLabel assigns newValue when it differs from the current value.
// The bool reports whether the caller must write the node.
func prepareMatchedStateLabel(
	node *corev1.Node,
	nodeName string,
	currentValue string,
	newValue NVSentinelStateLabelValue,
) bool {
	slog.Info("Labeling node", "node", nodeName, "from", currentValue, "to", newValue)

	if currentValue == string(newValue) {
		slog.Info("No update needed for node",
			"node", nodeName,
			"label", NVSentinelStateLabelKey,
			"value", newValue)

		return false
	}

	node.Labels[NVSentinelStateLabelKey] = string(newValue)

	return true
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
		QuarantinedLabelValue: {DrainingLabelValue, DrainSucceededLabelValue, DrainStoppedLabelValue},
		DrainingLabelValue:    {DrainSucceededLabelValue, DrainFailedLabelValue},
		DrainSucceededLabelValue: {
			RemediatingLabelValue, RemediationFailedLabelValue, RemediationStoppedLabelValue,
		},
		DrainFailedLabelValue: {}, // Terminal state - fault-remediation doesn't consume drain-failed
		// drain-stopped is not terminal. A later event can drain the node after the label that
		// selected the stop no longer matches. A drain must pass through draining before it can fail.
		DrainStoppedLabelValue: {DrainingLabelValue, DrainSucceededLabelValue},
		RemediatingLabelValue: {
			RemediationSucceededLabelValue, RemediationFailedLabelValue, RemediationStoppedLabelValue,
		},
		// remediation-succeeded and remediation-failed are terminal for a single failure, but a
		// partial recovery (a tracked failure clears while the node stays quarantined) lets
		// fault-remediation recompute the node label from the remaining active failures, which can
		// move between the two terminal remediation outcomes or to remediation-stopped when every
		// remaining failure is stopped. Both states can also return to remediating: a new
		// remediation-ready event can arrive after an equivalent maintenance CR completed (for
		// example a post-reboot fault while the node is still quarantined), and a failed CR is
		// retried with a new CR, so fault-remediation legitimately starts another remediation cycle
		// within the same quarantine session.
		RemediationSucceededLabelValue: {
			RemediationFailedLabelValue, RemediatingLabelValue, RemediationStoppedLabelValue,
		},
		RemediationFailedLabelValue: {
			RemediationSucceededLabelValue, RemediatingLabelValue, RemediationStoppedLabelValue,
		},
		// remediation-stopped is not terminal. A later event can move the node into an active or
		// completed remediation state.
		RemediationStoppedLabelValue: {
			RemediatingLabelValue, RemediationSucceededLabelValue, RemediationFailedLabelValue,
		},
	}

	currentState := NVSentinelStateLabelValue(currentValue)

	allowedStates, ok := validTransitions[currentState]
	if !ok {
		stateTransitionUnexpected.WithLabelValues(string(currentState), string(targetState), nodeName).Inc()

		return fmt.Errorf("unexpected state transition: unknown current state %s -> %s",
			currentState, targetState)
	}

	// Check if target state is in the expected transitions
	if slices.Contains(allowedStates, targetState) {
		return nil // Expected transition
	}

	// Unexpected transition
	stateTransitionUnexpected.WithLabelValues(string(currentState), string(targetState), nodeName).Inc()

	return fmt.Errorf("unexpected state transition: %s -> %s (expected one of: %v)",
		currentState, targetState, allowedStates)
}
