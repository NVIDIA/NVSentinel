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

package statemanager

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

const (
	testNodeName = "test-node"
)

func newTestStateManager(nodeName string, startingNodeLabels map[string]string) (context.Context, *stateManager, error) {
	ctx := context.Background()
	clientSet := fake.NewSimpleClientset()
	// Create a test node
	node := &v1.Node{
		Name:   nodeName,
		Labels: startingNodeLabels,
		Spec:   v1.NodeSpec{},
	}
	_, err := clientSet.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create test node: %v", err)
	}
	return ctx, &stateManager{clientSet: clientSet}, nil
}

func TestUpdateNVSentinelStateNodeLabelWithGetFailure(t *testing.T) {
	ctx := context.Background()
	clientSet := fake.NewSimpleClientset()
	clientSet.Fake.PrependReactor("get", "nodes", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, fmt.Errorf("get node error")
	})
	manager := &stateManager{clientSet: clientSet}
	nodeModified, err := manager.UpdateNVSentinelStateNodeLabel(ctx, testNodeName, QuarantinedLabelValue, false)
	assert.False(t, nodeModified)
	assert.Error(t, err)
}

func TestUpdateNVSentinelStateNodeLabelWithUpdateFailure(t *testing.T) {
	ctx := context.Background()
	clientSet := fake.NewSimpleClientset()
	// Create a test node
	node := &v1.Node{
		Name:   testNodeName,
		Labels: make(map[string]string),
		Spec:   v1.NodeSpec{},
	}
	_, err := clientSet.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	assert.NoError(t, err)
	clientSet.Fake.PrependReactor("update", "nodes", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, fmt.Errorf("update node error")
	})
	manager := &stateManager{clientSet: clientSet}
	nodeModified, err := manager.UpdateNVSentinelStateNodeLabel(ctx, testNodeName, QuarantinedLabelValue, false)
	assert.False(t, nodeModified)
	assert.Error(t, err)
}

func TestUpdateNVSentinelStateNodeLabelWithUpdateConflictRetry(t *testing.T) {
	ctx := context.Background()
	clientSet := fake.NewSimpleClientset()
	// Create a test node
	node := &v1.Node{
		Name:   testNodeName,
		Labels: make(map[string]string),
		Spec:   v1.NodeSpec{},
	}
	_, err := clientSet.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	assert.NoError(t, err)
	callCount := 0
	clientSet.Fake.PrependReactor("update", "nodes", func(action ktesting.Action) (handled bool,
		ret runtime.Object, err error) {
		callCount++
		switch callCount {
		case 1:
			return true, nil, errors.NewConflict(
				schema.GroupResource{Group: "", Resource: "nodes"},
				testNodeName,
				fmt.Errorf("simulated update conflict"))
		case 2:
			return true, nil, nil
		}
		return true, nil, nil
	})
	manager := &stateManager{clientSet: clientSet}
	nodeModified, err := manager.UpdateNVSentinelStateNodeLabel(ctx, testNodeName, QuarantinedLabelValue, false)
	assert.True(t, nodeModified)
	assert.NoError(t, err)
}

func TestUpdateNVSentinelStateNodeLabelWithAddSuccess(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, make(map[string]string))
	assert.NoError(t, err)
	nodeModified, err := manager.UpdateNVSentinelStateNodeLabel(ctx, testNodeName, QuarantinedLabelValue, false)
	assert.True(t, nodeModified)
	assert.NoError(t, err)
}

func TestUpdateNVSentinelStateNodeLabelWithRemoveSuccess(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(QuarantinedLabelValue),
	})
	assert.NoError(t, err)
	nodeModified, err := manager.UpdateNVSentinelStateNodeLabel(ctx, testNodeName, "", true)
	assert.True(t, nodeModified)
	assert.NoError(t, err)
}

func TestUpdateNVSentinelStateNodeLabelWithLabelAlreadyExistingSameValue(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(QuarantinedLabelValue),
	})
	assert.NoError(t, err)
	nodeModified, err := manager.UpdateNVSentinelStateNodeLabel(ctx, testNodeName, QuarantinedLabelValue, false)
	assert.False(t, nodeModified)
	assert.NoError(t, err)
}

func TestUpdateNVSentinelStateNodeLabelWithLabelAlreadyExistingDifferentValue(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(QuarantinedLabelValue),
	})
	assert.NoError(t, err)
	nodeModified, err := manager.UpdateNVSentinelStateNodeLabel(ctx, testNodeName, DrainingLabelValue, false)
	assert.True(t, nodeModified)
	assert.NoError(t, err)
}

func TestUpdateNVSentinelStateNodeLabelWithLabelAlreadyRemoved(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, make(map[string]string))
	assert.NoError(t, err)
	nodeModified, err := manager.UpdateNVSentinelStateNodeLabel(ctx, testNodeName, "", true)
	assert.False(t, nodeModified)
	assert.NoError(t, err)
}

func TestRemoveNVSentinelStateNodeLabelIfMatch_MatchingValue_RemovesLabel(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(RemediationFailedLabelValue),
	})
	assert.NoError(t, err)

	nodeModified, err := manager.RemoveNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		RemediatingLabelValue,
		RemediationSucceededLabelValue,
		RemediationFailedLabelValue,
	)

	assert.True(t, nodeModified)
	assert.NoError(t, err)
	node, err := manager.clientSet.CoreV1().Nodes().Get(ctx, testNodeName, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, node.Labels, NVSentinelStateLabelKey)
}

func TestRemoveNVSentinelStateNodeLabelIfMatch_NewerNonMatchingValue_PreservesLabel(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(QuarantinedLabelValue),
	})
	assert.NoError(t, err)

	nodeModified, err := manager.RemoveNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		RemediatingLabelValue,
		RemediationSucceededLabelValue,
		RemediationFailedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.NoError(t, err)
	node, err := manager.clientSet.CoreV1().Nodes().Get(ctx, testNodeName, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, string(QuarantinedLabelValue), node.Labels[NVSentinelStateLabelKey])
}

func TestRemoveNVSentinelStateNodeLabelIfMatch_EmptyExpectedList_DoesNotReadNode(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(QuarantinedLabelValue),
	})
	assert.NoError(t, err)

	clientSet := manager.clientSet.(*fake.Clientset)
	actionsBefore := len(clientSet.Actions())

	nodeModified, err := manager.RemoveNVSentinelStateNodeLabelIfMatch(ctx, testNodeName)

	assert.False(t, nodeModified)
	assert.NoError(t, err)
	assert.Len(t, clientSet.Actions(), actionsBefore)
	assertStateLabel(t, ctx, manager, QuarantinedLabelValue)
}

func TestRemoveNVSentinelStateNodeLabelIfMatch_ConcurrentNewerValue_PreservesLabelAfterConflict(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(RemediationFailedLabelValue),
	})
	assert.NoError(t, err)

	clientSet := manager.clientSet.(*fake.Clientset)
	updateCalls := 0
	clientSet.Fake.PrependReactor("update", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		updateCalls++
		newerNode := &v1.Node{
			Name: testNodeName,
			Labels: map[string]string{
				NVSentinelStateLabelKey: string(QuarantinedLabelValue),
			},
		}
		err := clientSet.Tracker().Update(v1.SchemeGroupVersion.WithResource("nodes"), newerNode, "")
		assert.NoError(t, err)

		return true, nil, errors.NewConflict(
			schema.GroupResource{Group: "", Resource: "nodes"},
			testNodeName,
			fmt.Errorf("simulated concurrent label update"),
		)
	})

	nodeModified, err := manager.RemoveNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		RemediatingLabelValue,
		RemediationSucceededLabelValue,
		RemediationFailedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.NoError(t, err)
	assert.Equal(t, 1, updateCalls, "retry should observe the newer non-matching value and skip deletion")
	node, err := manager.clientSet.CoreV1().Nodes().Get(ctx, testNodeName, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, string(QuarantinedLabelValue), node.Labels[NVSentinelStateLabelKey])
}

func TestRemoveNVSentinelStateNodeLabelIfMatch_LabelAbsent_DoesNotWrite(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{})
	assert.NoError(t, err)

	nodeModified, err := manager.RemoveNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		RemediationFailedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.NoError(t, err)
	assertStateLabelAbsent(t, ctx, manager)

	for _, action := range manager.clientSet.(*fake.Clientset).Actions() {
		assert.NotEqual(t, "update", action.GetVerb())
	}
}

func TestRemoveNVSentinelStateNodeLabelIfMatch_GetFailure_ReturnsContext(t *testing.T) {
	ctx := context.Background()
	clientSet := fake.NewSimpleClientset()
	clientSet.Fake.PrependReactor("get", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("get node error")
	})
	manager := &stateManager{clientSet: clientSet}

	nodeModified, err := manager.RemoveNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		RemediationFailedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.Error(t, err)
	assert.ErrorContains(t, err, "failed to get node test-node for conditional state label change")
}

func TestRemoveNVSentinelStateNodeLabelIfMatch_UpdateFailure_ReturnsContext(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(RemediationFailedLabelValue),
	})
	assert.NoError(t, err)

	clientSet := manager.clientSet.(*fake.Clientset)
	clientSet.Fake.PrependReactor("update", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("update node error")
	})

	nodeModified, err := manager.RemoveNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		RemediationFailedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.Error(t, err)
	assert.ErrorContains(t, err, "failed to update node test-node to conditionally remove label")
	assertStateLabel(t, ctx, manager, RemediationFailedLabelValue)
}

func TestRemoveNVSentinelStateNodeLabelIfMatch_ConflictsExhausted_ReturnsContext(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(RemediationFailedLabelValue),
	})
	assert.NoError(t, err)

	clientSet := manager.clientSet.(*fake.Clientset)
	clientSet.Fake.PrependReactor("update", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.NewConflict(
			schema.GroupResource{Group: "", Resource: "nodes"},
			testNodeName,
			fmt.Errorf("simulated persistent update conflict"),
		)
	})

	nodeModified, err := manager.RemoveNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		RemediationFailedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.Error(t, err)
	assert.ErrorContains(t, err, "failed to update node test-node to conditionally remove label")
	assert.True(t, errors.IsConflict(err))
	assertStateLabel(t, ctx, manager, RemediationFailedLabelValue)
}

// Test that label removal from any state doesn't trigger validation errors.
// This is important for canceled drains: quarantined -> draining -> label removed (healthy event).
func TestLabelRemovalFromAnyStateDoesNotTriggerValidation(t *testing.T) {
	states := []NVSentinelStateLabelValue{
		QuarantinedLabelValue,
		DrainingLabelValue,
		DrainSucceededLabelValue,
		DrainFailedLabelValue,
		RemediatingLabelValue,
		RemediationSucceededLabelValue,
		RemediationFailedLabelValue,
		DrainStoppedLabelValue,
		RemediationStoppedLabelValue,
	}

	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
				NVSentinelStateLabelKey: string(state),
			})
			assert.NoError(t, err)

			// Remove label from this state - should not trigger validation error
			nodeModified, err := manager.UpdateNVSentinelStateNodeLabel(ctx, testNodeName, "", true)
			assert.True(t, nodeModified)
			assert.NoError(t, err, "Removing label from %s state should not trigger validation error", state)

			// Verify label was actually removed
			node, getErr := manager.clientSet.CoreV1().Nodes().Get(ctx, testNodeName, metav1.GetOptions{})
			assert.NoError(t, getErr)
			_, exists := node.Labels[NVSentinelStateLabelKey]
			assert.False(t, exists, "Label should be removed from node in %s state", state)
		})
	}
}

// Test state progression - expected transitions succeed without error,
// unexpected transitions return error but still update the label
func TestStateTransitionValidProgression(t *testing.T) {
	tests := []struct {
		name         string
		initialState string
		targetState  NVSentinelStateLabelValue
		labelExists  bool
		expectError  bool // Whether this transition should return an error
	}{
		// Expected progressions (normal flow, no error)
		{"NoState to Quarantined", "", QuarantinedLabelValue, false, false},
		{"Quarantined to Draining", string(QuarantinedLabelValue), DrainingLabelValue, true, false},
		{"Draining to DrainSucceeded", string(DrainingLabelValue), DrainSucceededLabelValue, true, false},
		{"Draining to DrainFailed", string(DrainingLabelValue), DrainFailedLabelValue, true, false},
		{"DrainSucceeded to Remediating", string(DrainSucceededLabelValue), RemediatingLabelValue, true, false},
		// Unsupported actions do not start remediation, so they transition directly to the
		// terminal failure state to indicate that operator intervention is required.
		{"DrainSucceeded to RemediationFailed", string(DrainSucceededLabelValue), RemediationFailedLabelValue, true, false},
		{"Remediating to RemediationSucceeded", string(RemediatingLabelValue), RemediationSucceededLabelValue, true, false},
		{"Remediating to RemediationFailed", string(RemediatingLabelValue), RemediationFailedLabelValue, true, false},
		{"Quarantined to DrainSucceeded", string(QuarantinedLabelValue), DrainSucceededLabelValue, true, false},
		// Partial-recovery recompute between terminal remediation outcomes (fault-remediation)
		{"RemediationFailed to RemediationSucceeded", string(RemediationFailedLabelValue), RemediationSucceededLabelValue, true, false},
		{"RemediationSucceeded to RemediationFailed", string(RemediationSucceededLabelValue), RemediationFailedLabelValue, true, false},
		// A new remediation cycle can start after a terminal remediation outcome while the node
		// stays quarantined: a post-session event creates a new CR after the previous CR
		// completed (issue #1536), and a failed CR is retried with a new CR (fault-remediation).
		{"RemediationSucceeded to Remediating", string(RemediationSucceededLabelValue), RemediatingLabelValue, true, false},
		{"RemediationFailed to Remediating", string(RemediationFailedLabelValue), RemediatingLabelValue, true, false},
		// A rule set can stop the drain or the remediation. A later event in the same quarantine
		// can move the node on, and a recompute can return a remediation state to remediation-stopped.
		{"Quarantined to DrainStopped", string(QuarantinedLabelValue), DrainStoppedLabelValue, true, false},
		{"DrainStopped to Draining", string(DrainStoppedLabelValue), DrainingLabelValue, true, false},
		{"DrainStopped to DrainSucceeded", string(DrainStoppedLabelValue), DrainSucceededLabelValue, true, false},
		{"DrainSucceeded to RemediationStopped", string(DrainSucceededLabelValue), RemediationStoppedLabelValue, true, false},
		{"RemediationStopped to Remediating", string(RemediationStoppedLabelValue), RemediatingLabelValue, true, false},
		{"RemediationStopped to RemediationSucceeded", string(RemediationStoppedLabelValue), RemediationSucceededLabelValue, true, false},
		{"RemediationStopped to RemediationFailed", string(RemediationStoppedLabelValue), RemediationFailedLabelValue, true, false},
		{"Remediating to RemediationStopped", string(RemediatingLabelValue), RemediationStoppedLabelValue, true, false},
		{"RemediationSucceeded to RemediationStopped", string(RemediationSucceededLabelValue), RemediationStoppedLabelValue, true, false},
		{"RemediationFailed to RemediationStopped", string(RemediationFailedLabelValue), RemediationStoppedLabelValue, true, false},

		// Unexpected progressions (return error but label is still updated)
		// This allows callers to emit error metrics while labels reflect reality
		{"NoState to Draining", "", DrainingLabelValue, false, true},
		{"NoState to Remediating", "", RemediatingLabelValue, false, true},
		{"Quarantined to Remediating", string(QuarantinedLabelValue), RemediatingLabelValue, true, true},
		{"Draining to Remediating", string(DrainingLabelValue), RemediatingLabelValue, true, true},
		// DrainFailed is a terminal state - fault-remediation only consumes drain-succeeded
		{"DrainFailed to Remediating", string(DrainFailedLabelValue), RemediatingLabelValue, true, true},
		{"DrainSucceeded to DrainFailed", string(DrainSucceededLabelValue), DrainFailedLabelValue, true, true},
		{"RemediationSucceeded to Draining", string(RemediationSucceededLabelValue), DrainingLabelValue, true, true},
		// A stop is not a failure, and remediation does not start before the drain completes.
		{"DrainStopped to DrainFailed", string(DrainStoppedLabelValue), DrainFailedLabelValue, true, true},
		{"DrainStopped to Remediating", string(DrainStoppedLabelValue), RemediatingLabelValue, true, true},
		{"Quarantined to RemediationStopped", string(QuarantinedLabelValue), RemediationStoppedLabelValue, true, true},
		{"NoState to DrainStopped", "", DrainStoppedLabelValue, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := make(map[string]string)
			if tt.labelExists {
				labels[NVSentinelStateLabelKey] = tt.initialState
			}
			ctx, manager, err := newTestStateManager(testNodeName, labels)
			assert.NoError(t, err)

			nodeModified, err := manager.UpdateNVSentinelStateNodeLabel(ctx, testNodeName, tt.targetState, false)

			// Check if error matches expectation
			if tt.expectError {
				assert.Error(t, err, "Expected error for unexpected transition from %s to %s", tt.initialState, tt.targetState)
				assert.Contains(t, err.Error(), "unexpected state transition",
					"Error should indicate unexpected transition")
			} else {
				assert.NoError(t, err, "Expected no error for valid transition from %s to %s", tt.initialState, tt.targetState)
			}

			// Even with error, node should be modified and label should be set
			assert.True(t, nodeModified, "Node should be modified even for unexpected transitions (from %s to %s)",
				tt.initialState, tt.targetState)

			// Verify the label was actually set regardless of validation error
			node, getErr := manager.clientSet.CoreV1().Nodes().Get(ctx, testNodeName, metav1.GetOptions{})
			assert.NoError(t, getErr)
			assert.Equal(t, string(tt.targetState), node.Labels[NVSentinelStateLabelKey],
				"Label should be set to target state even for unexpected transitions")
		})
	}
}

func TestValidateStateTransition_AllStateCombinations_MatchesExpectedTransitions(t *testing.T) {
	targetStates := []NVSentinelStateLabelValue{
		QuarantinedLabelValue,
		DrainingLabelValue,
		DrainSucceededLabelValue,
		DrainFailedLabelValue,
		DrainStoppedLabelValue,
		RemediatingLabelValue,
		RemediationSucceededLabelValue,
		RemediationFailedLabelValue,
		RemediationStoppedLabelValue,
	}
	sourceStates := []struct {
		name   string
		value  string
		exists bool
	}{
		{name: "none"},
		{name: string(QuarantinedLabelValue), value: string(QuarantinedLabelValue), exists: true},
		{name: string(DrainingLabelValue), value: string(DrainingLabelValue), exists: true},
		{name: string(DrainSucceededLabelValue), value: string(DrainSucceededLabelValue), exists: true},
		{name: string(DrainFailedLabelValue), value: string(DrainFailedLabelValue), exists: true},
		{name: string(DrainStoppedLabelValue), value: string(DrainStoppedLabelValue), exists: true},
		{name: string(RemediatingLabelValue), value: string(RemediatingLabelValue), exists: true},
		{
			name:   string(RemediationSucceededLabelValue),
			value:  string(RemediationSucceededLabelValue),
			exists: true,
		},
		{name: string(RemediationFailedLabelValue), value: string(RemediationFailedLabelValue), exists: true},
		{name: string(RemediationStoppedLabelValue), value: string(RemediationStoppedLabelValue), exists: true},
	}
	expectedTransitions := map[string][]NVSentinelStateLabelValue{
		"none": {QuarantinedLabelValue},
		string(QuarantinedLabelValue): {
			DrainingLabelValue,
			DrainSucceededLabelValue,
			DrainStoppedLabelValue,
		},
		string(DrainingLabelValue): {
			DrainSucceededLabelValue,
			DrainFailedLabelValue,
		},
		string(DrainSucceededLabelValue): {
			RemediatingLabelValue,
			RemediationFailedLabelValue,
			RemediationStoppedLabelValue,
		},
		string(DrainFailedLabelValue): {},
		string(DrainStoppedLabelValue): {
			DrainingLabelValue,
			DrainSucceededLabelValue,
		},
		string(RemediatingLabelValue): {
			RemediationSucceededLabelValue,
			RemediationFailedLabelValue,
			RemediationStoppedLabelValue,
		},
		string(RemediationSucceededLabelValue): {
			RemediationFailedLabelValue,
			RemediatingLabelValue,
			RemediationStoppedLabelValue,
		},
		string(RemediationFailedLabelValue): {
			RemediationSucceededLabelValue,
			RemediatingLabelValue,
			RemediationStoppedLabelValue,
		},
		string(RemediationStoppedLabelValue): {
			RemediatingLabelValue,
			RemediationSucceededLabelValue,
			RemediationFailedLabelValue,
		},
	}

	for _, source := range sourceStates {
		for _, target := range targetStates {
			t.Run(source.name+"_to_"+string(target), func(t *testing.T) {
				err := validateStateTransition(testNodeName, source.value, source.exists, target)

				if slices.Contains(expectedTransitions[source.name], target) {
					assert.NoError(t, err)

					return
				}

				assert.Error(t, err)
			})
		}
	}
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_LabelAbsent_DoesNotWrite(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{})
	assert.NoError(t, err)

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		DrainStoppedLabelValue,
		QuarantinedLabelValue,
		DrainStoppedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.NoError(t, err)
	assertStateLabelAbsent(t, ctx, manager)
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_ValueNotExpected_PreservesLabel(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(RemediatingLabelValue),
	})
	assert.NoError(t, err)

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		RemediationStoppedLabelValue,
		DrainSucceededLabelValue,
		RemediationStoppedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.NoError(t, err)
	assertStateLabel(t, ctx, manager, RemediatingLabelValue)
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_ValueAlreadyTarget_DoesNotWrite(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(DrainStoppedLabelValue),
	})
	assert.NoError(t, err)

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		DrainStoppedLabelValue,
		QuarantinedLabelValue,
		DrainStoppedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.NoError(t, err)
	assertStateLabel(t, ctx, manager, DrainStoppedLabelValue)
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_ExpectedValue_SetsLabel(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(QuarantinedLabelValue),
	})
	assert.NoError(t, err)

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		DrainStoppedLabelValue,
		QuarantinedLabelValue,
		DrainStoppedLabelValue,
	)

	assert.True(t, nodeModified)
	assert.NoError(t, err)
	assertStateLabel(t, ctx, manager, DrainStoppedLabelValue)
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_UnexpectedTransition_SetsLabelAndReturnsError(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(QuarantinedLabelValue),
	})
	assert.NoError(t, err)

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		RemediationStoppedLabelValue,
		QuarantinedLabelValue,
	)

	assert.True(t, nodeModified)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected state transition")
	assertStateLabel(t, ctx, manager, RemediationStoppedLabelValue)
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_EmptyExpectedList_DoesNotReadNode(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(DrainSucceededLabelValue),
	})
	assert.NoError(t, err)

	clientSet := manager.clientSet.(*fake.Clientset)
	actionsBefore := len(clientSet.Actions())

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		RemediationStoppedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.NoError(t, err)
	assert.Len(t, clientSet.Actions(), actionsBefore)
	assertStateLabel(t, ctx, manager, DrainSucceededLabelValue)
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_EmptyTarget_ReturnsError(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(DrainSucceededLabelValue),
	})
	assert.NoError(t, err)

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		"",
		DrainSucceededLabelValue,
	)

	assert.False(t, nodeModified)
	assert.Error(t, err)
	assertStateLabel(t, ctx, manager, DrainSucceededLabelValue)
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_ConflictMovesValueOutsideList_PreservesLabel(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(DrainSucceededLabelValue),
	})
	assert.NoError(t, err)

	clientSet := manager.clientSet.(*fake.Clientset)
	updateCalls := 0
	clientSet.Fake.PrependReactor("update", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		updateCalls++
		newerNode := &v1.Node{
			Name: testNodeName,
			Labels: map[string]string{
				NVSentinelStateLabelKey: string(RemediatingLabelValue),
			},
		}
		err := clientSet.Tracker().Update(v1.SchemeGroupVersion.WithResource("nodes"), newerNode, "")
		assert.NoError(t, err)

		return true, nil, errors.NewConflict(
			schema.GroupResource{Group: "", Resource: "nodes"},
			testNodeName,
			fmt.Errorf("simulated concurrent label update"),
		)
	})

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		RemediationStoppedLabelValue,
		DrainSucceededLabelValue,
		RemediationStoppedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.NoError(t, err)
	assert.Equal(t, 1, updateCalls, "retry should observe the newer non-matching value and skip the write")
	assertStateLabel(t, ctx, manager, RemediatingLabelValue)
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_ConflictRemovesLabel_DoesNotRestoreLabel(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(DrainSucceededLabelValue),
	})
	assert.NoError(t, err)

	clientSet := manager.clientSet.(*fake.Clientset)
	updateCalls := 0
	clientSet.Fake.PrependReactor("update", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		updateCalls++
		newerNode := &v1.Node{
			Name:   testNodeName,
			Labels: map[string]string{},
		}
		err := clientSet.Tracker().Update(v1.SchemeGroupVersion.WithResource("nodes"), newerNode, "")
		assert.NoError(t, err)

		return true, nil, errors.NewConflict(
			schema.GroupResource{Group: "", Resource: "nodes"},
			testNodeName,
			fmt.Errorf("simulated concurrent label removal"),
		)
	})

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		RemediationStoppedLabelValue,
		DrainSucceededLabelValue,
		RemediationStoppedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.NoError(t, err)
	assert.Equal(t, 1, updateCalls, "retry should observe the removed label and skip the write")
	assertStateLabelAbsent(t, ctx, manager)
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_ConflictSetsTarget_DoesNotWriteAgain(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(QuarantinedLabelValue),
	})
	assert.NoError(t, err)

	clientSet := manager.clientSet.(*fake.Clientset)
	updateCalls := 0
	clientSet.Fake.PrependReactor("update", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		updateCalls++
		newerNode := &v1.Node{
			Name: testNodeName,
			Labels: map[string]string{
				NVSentinelStateLabelKey: string(DrainStoppedLabelValue),
			},
		}
		err := clientSet.Tracker().Update(v1.SchemeGroupVersion.WithResource("nodes"), newerNode, "")
		assert.NoError(t, err)

		return true, nil, errors.NewConflict(
			schema.GroupResource{Group: "", Resource: "nodes"},
			testNodeName,
			fmt.Errorf("simulated concurrent target update"),
		)
	})

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		DrainStoppedLabelValue,
		QuarantinedLabelValue,
		DrainStoppedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.NoError(t, err)
	assert.Equal(t, 1, updateCalls, "retry should observe the target value and skip another write")
	assertStateLabel(t, ctx, manager, DrainStoppedLabelValue)
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_ConflictSkipsWrite_DoesNotCountTransition(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(QuarantinedLabelValue),
	})
	assert.NoError(t, err)

	counter := stateTransitionUnexpected.WithLabelValues(
		string(QuarantinedLabelValue),
		string(RemediationStoppedLabelValue),
		testNodeName,
	)
	countBefore := testutil.ToFloat64(counter)

	clientSet := manager.clientSet.(*fake.Clientset)
	clientSet.Fake.PrependReactor("update", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		newerNode := &v1.Node{
			Name: testNodeName,
			Labels: map[string]string{
				NVSentinelStateLabelKey: string(DrainingLabelValue),
			},
		}
		err := clientSet.Tracker().Update(v1.SchemeGroupVersion.WithResource("nodes"), newerNode, "")
		assert.NoError(t, err)

		return true, nil, errors.NewConflict(
			schema.GroupResource{Group: "", Resource: "nodes"},
			testNodeName,
			fmt.Errorf("simulated concurrent label update"),
		)
	})

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		RemediationStoppedLabelValue,
		QuarantinedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.NoError(t, err)
	assert.Equal(t, countBefore, testutil.ToFloat64(counter))
	assertStateLabel(t, ctx, manager, DrainingLabelValue)
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_ConflictWhileValueStillExpected_SetsLabel(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(QuarantinedLabelValue),
	})
	assert.NoError(t, err)

	clientSet := manager.clientSet.(*fake.Clientset)
	updateCalls := 0
	clientSet.Fake.PrependReactor("update", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		updateCalls++
		if updateCalls == 1 {
			return true, nil, errors.NewConflict(
				schema.GroupResource{Group: "", Resource: "nodes"},
				testNodeName,
				fmt.Errorf("simulated update conflict"),
			)
		}

		return false, nil, nil
	})

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		DrainStoppedLabelValue,
		QuarantinedLabelValue,
		DrainStoppedLabelValue,
	)

	assert.True(t, nodeModified)
	assert.NoError(t, err)
	assert.Equal(t, 2, updateCalls)
	assertStateLabel(t, ctx, manager, DrainStoppedLabelValue)
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_ConflictsExhausted_ReturnsContext(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(QuarantinedLabelValue),
	})
	assert.NoError(t, err)

	clientSet := manager.clientSet.(*fake.Clientset)
	clientSet.Fake.PrependReactor("update", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.NewConflict(
			schema.GroupResource{Group: "", Resource: "nodes"},
			testNodeName,
			fmt.Errorf("simulated persistent update conflict"),
		)
	})

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		DrainStoppedLabelValue,
		QuarantinedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.Error(t, err)
	assert.ErrorContains(t, err, "failed to update node test-node to conditionally set label")
	assert.True(t, errors.IsConflict(err))
	assertStateLabel(t, ctx, manager, QuarantinedLabelValue)
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_GetFailure_ReturnsError(t *testing.T) {
	ctx := context.Background()
	clientSet := fake.NewSimpleClientset()
	clientSet.Fake.PrependReactor("get", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("get node error")
	})
	manager := &stateManager{clientSet: clientSet}

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		DrainStoppedLabelValue,
		QuarantinedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.Error(t, err)
	assert.ErrorContains(t, err, "failed to get node test-node for conditional state label change")
}

func TestUpdateNVSentinelStateNodeLabelIfMatch_UpdateFailure_ReturnsError(t *testing.T) {
	ctx, manager, err := newTestStateManager(testNodeName, map[string]string{
		NVSentinelStateLabelKey: string(QuarantinedLabelValue),
	})
	assert.NoError(t, err)

	clientSet := manager.clientSet.(*fake.Clientset)
	clientSet.Fake.PrependReactor("update", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("update node error")
	})

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		DrainStoppedLabelValue,
		QuarantinedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.Error(t, err)
	assertStateLabel(t, ctx, manager, QuarantinedLabelValue)
}

func assertStateLabel(
	t *testing.T,
	ctx context.Context,
	manager *stateManager,
	expected NVSentinelStateLabelValue,
) {
	t.Helper()

	node, err := manager.clientSet.CoreV1().Nodes().Get(ctx, testNodeName, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, string(expected), node.Labels[NVSentinelStateLabelKey])
}

func assertStateLabelAbsent(t *testing.T, ctx context.Context, manager *stateManager) {
	t.Helper()

	node, err := manager.clientSet.CoreV1().Nodes().Get(ctx, testNodeName, metav1.GetOptions{})
	assert.NoError(t, err)
	_, exists := node.Labels[NVSentinelStateLabelKey]
	assert.False(t, exists)
}
