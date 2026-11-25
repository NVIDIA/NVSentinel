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

package statemanager

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestUpdateNVSentinelStateNodeLabelIfMatch_RealAPIConflict_PreservesNewerValue(t *testing.T) {
	ctx := context.Background()
	testEnv := &envtest.Environment{}

	restConfig, err := testEnv.Start()
	require.NoError(t, err, "envtest is not available; run make dev-env-setup and export KUBEBUILDER_ASSETS")
	t.Cleanup(func() {
		require.NoError(t, testEnv.Stop())
	})

	clientSet, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNodeName,
			Labels: map[string]string{
				NVSentinelStateLabelKey: string(DrainSucceededLabelValue),
			},
		},
	}
	_, err = clientSet.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err)

	injectedNodes := &conflictInjectingNodes{
		NodeInterface: clientSet.CoreV1().Nodes(),
		nodeName:      testNodeName,
		newerValue:    RemediatingLabelValue,
	}
	manager := &stateManager{
		clientSet: newConflictInjectingClientSet(clientSet, injectedNodes),
	}

	nodeModified, err := manager.UpdateNVSentinelStateNodeLabelIfMatch(
		ctx,
		testNodeName,
		RemediationStoppedLabelValue,
		DrainSucceededLabelValue,
		RemediationStoppedLabelValue,
	)

	assert.False(t, nodeModified)
	assert.NoError(t, err)
	assert.True(t, injectedNodes.conflictObserved, "the real API server must reject the stale update")

	latestNode, err := clientSet.CoreV1().Nodes().Get(ctx, testNodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, string(RemediatingLabelValue), latestNode.Labels[NVSentinelStateLabelKey])
}

type conflictInjectingClientSet struct {
	kubernetes.Interface
	coreV1 typedcorev1.CoreV1Interface
}

func newConflictInjectingClientSet(
	clientSet kubernetes.Interface,
	nodes typedcorev1.NodeInterface,
) kubernetes.Interface {
	return &conflictInjectingClientSet{
		Interface: clientSet,
		coreV1: &conflictInjectingCoreV1{
			CoreV1Interface: clientSet.CoreV1(),
			nodes:           nodes,
		},
	}
}

func (clientSet *conflictInjectingClientSet) CoreV1() typedcorev1.CoreV1Interface {
	return clientSet.coreV1
}

type conflictInjectingCoreV1 struct {
	typedcorev1.CoreV1Interface
	nodes typedcorev1.NodeInterface
}

func (client *conflictInjectingCoreV1) Nodes() typedcorev1.NodeInterface {
	return client.nodes
}

type conflictInjectingNodes struct {
	typedcorev1.NodeInterface
	injectOnce       sync.Once
	nodeName         string
	newerValue       NVSentinelStateLabelValue
	conflictObserved bool
}

func (nodes *conflictInjectingNodes) Update(
	ctx context.Context,
	staleNode *corev1.Node,
	options metav1.UpdateOptions,
) (*corev1.Node, error) {
	var injectErr error

	nodes.injectOnce.Do(func() {
		latestNode, err := nodes.NodeInterface.Get(ctx, nodes.nodeName, metav1.GetOptions{})
		if err != nil {
			injectErr = fmt.Errorf("get node before injected update: %w", err)

			return
		}

		latestNode.Labels[NVSentinelStateLabelKey] = string(nodes.newerValue)
		if _, err = nodes.NodeInterface.Update(ctx, latestNode, metav1.UpdateOptions{}); err != nil {
			injectErr = fmt.Errorf("inject newer node state: %w", err)
		}
	})
	if injectErr != nil {
		return nil, injectErr
	}

	updatedNode, err := nodes.NodeInterface.Update(ctx, staleNode, options)
	nodes.conflictObserved = apierrors.IsConflict(err)

	return updatedNode, err
}
