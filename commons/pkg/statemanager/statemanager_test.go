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
	"testing"

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
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: startingNodeLabels,
		},
		Spec: v1.NodeSpec{},
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
		ObjectMeta: metav1.ObjectMeta{
			Name:   testNodeName,
			Labels: make(map[string]string),
		},
		Spec: v1.NodeSpec{},
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
		ObjectMeta: metav1.ObjectMeta{
			Name:   testNodeName,
			Labels: make(map[string]string),
		},
		Spec: v1.NodeSpec{},
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
		{"Remediating to RemediationSucceeded", string(RemediatingLabelValue), RemediationSucceededLabelValue, true, false},
		{"Remediating to RemediationFailed", string(RemediatingLabelValue), RemediationFailedLabelValue, true, false},

		// Unexpected progressions (return error but label is still updated)
		// This allows callers to emit error metrics while labels reflect reality
		{"NoState to Draining", "", DrainingLabelValue, false, true},
		{"NoState to Remediating", "", RemediatingLabelValue, false, true},
		{"Quarantined to DrainSucceeded", string(QuarantinedLabelValue), DrainSucceededLabelValue, true, true},
		{"Quarantined to Remediating", string(QuarantinedLabelValue), RemediatingLabelValue, true, true},
		{"Draining to Remediating", string(DrainingLabelValue), RemediatingLabelValue, true, true},
		// DrainFailed is a terminal state - fault-remediation-module only consumes drain-succeeded
		{"DrainFailed to Remediating", string(DrainFailedLabelValue), RemediatingLabelValue, true, true},
		{"DrainSucceeded to DrainFailed", string(DrainSucceededLabelValue), DrainFailedLabelValue, true, true},
		{"RemediationSucceeded to Draining", string(RemediationSucceededLabelValue), DrainingLabelValue, true, true},
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
