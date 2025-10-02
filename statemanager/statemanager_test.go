// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
