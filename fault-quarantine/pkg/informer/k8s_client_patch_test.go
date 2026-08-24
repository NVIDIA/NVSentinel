// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package informer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

func cachedNodeInformer(t *testing.T, node *v1.Node) *NodeInformer {
	t.Helper()

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, indexer.Add(node))

	return &NodeInformer{
		lister:         corelisters.NewNodeLister(indexer),
		informerSynced: func() bool { return true },
	}
}

func TestUpdateNode_CachedNode_UsesPatchAndSkipsNoOp(t *testing.T) {
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", ResourceVersion: "1"}}
	clientset := fake.NewSimpleClientset(node.DeepCopy())
	client := &FaultQuarantineClient{
		Clientset:    clientset,
		NodeInformer: cachedNodeInformer(t, node.DeepCopy()),
	}

	clientset.ClearActions()
	_, err := client.QuarantineNodeAndSetAnnotations(
		context.Background(),
		node.Name,
		nil,
		true,
		nil,
		nil,
	)
	require.NoError(t, err)

	actions := clientset.Actions()
	require.Len(t, actions, 1)
	patchAction, ok := actions[0].(k8stesting.PatchAction)
	require.True(t, ok)
	assert.Equal(t, types.MergePatchType, patchAction.GetPatchType())
	assert.JSONEq(t, `{"spec":{"unschedulable":true}}`, string(patchAction.GetPatch()))

	updated, err := clientset.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, updated.Spec.Unschedulable)

	client.NodeInformer = cachedNodeInformer(t, updated.DeepCopy())
	clientset.ClearActions()
	_, err = client.QuarantineNodeAndSetAnnotations(
		context.Background(),
		node.Name,
		nil,
		true,
		nil,
		nil,
	)
	require.NoError(t, err)
	assert.Empty(t, clientset.Actions())
}

func TestNodeForPatch_PreviousWriteNotInCache_FallsBackToLiveGet(t *testing.T) {
	cached := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", ResourceVersion: "1"}}
	live := cached.DeepCopy()
	live.ResourceVersion = "2"
	live.Annotations = map[string]string{"first-write": "preserved"}

	clientset := fake.NewSimpleClientset(live)
	client := &FaultQuarantineClient{
		Clientset:    clientset,
		NodeInformer: cachedNodeInformer(t, cached),
	}
	client.lastWrittenNodeVersion.Store(cached.Name, live.ResourceVersion)
	clientset.ClearActions()

	node, err := client.nodeForPatch(t.Context(), cached.Name)
	require.NoError(t, err)
	assert.Equal(t, live.ResourceVersion, node.ResourceVersion)
	assert.Equal(t, "preserved", node.Annotations["first-write"])
	require.Len(t, clientset.Actions(), 1)
	assert.Equal(t, "get", clientset.Actions()[0].GetVerb())
}
