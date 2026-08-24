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
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

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
	assert.JSONEq(t,
		`{"metadata":{"resourceVersion":"1"},"spec":{"unschedulable":true}}`,
		string(patchAction.GetPatch()),
	)

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

func TestUpdateNode_ConcurrentTaintUpdate_RetriesFromLiveNode(t *testing.T) {
	const nodeName = "concurrent-taint-update"

	ctx := t.Context()
	createTestNode(ctx, t, nodeName, nil, nil, nil, false)
	t.Cleanup(func() {
		_ = testClient.CoreV1().Nodes().Delete(
			context.Background(),
			nodeName,
			metav1.DeleteOptions{},
		)
	})

	client := setupTestClient(t)
	fqTaint := v1.Taint{Key: "fq", Value: "quarantined", Effect: v1.TaintEffectNoSchedule}
	concurrentTaint := v1.Taint{Key: "other-controller", Value: "updated", Effect: v1.TaintEffectNoExecute}

	// Intercept the first FQ PATCH and advance the live Node immediately before
	// forwarding it. This deterministically makes the first ResourceVersion stale.
	restConfig := rest.CopyConfig(testConfig)
	var injectOnce sync.Once
	var injectErr error
	patchRequests := 0
	restConfig.Wrap(func(delegate http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			if request.Method == http.MethodPatch {
				patchRequests++
				injectOnce.Do(func() {
					liveNode, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
					if err != nil {
						injectErr = err
						return
					}

					liveNode.Spec.Taints = append(liveNode.Spec.Taints, concurrentTaint)
					_, injectErr = testClient.CoreV1().Nodes().Update(ctx, liveNode, metav1.UpdateOptions{})
				})
				if injectErr != nil {
					return nil, injectErr
				}
			}

			return delegate.RoundTrip(request)
		})
	})

	patchingClient, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)
	client.Clientset = patchingClient

	attempts := 0
	err = client.UpdateNode(ctx, nodeName, func(node *v1.Node) error {
		attempts++
		node.Spec.Taints = append(node.Spec.Taints, fqTaint)

		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, attempts, "the stale PATCH must conflict exactly once")
	assert.Equal(t, 2, patchRequests)

	updated, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, updated.Spec.Taints, fqTaint)
	assert.Contains(t, updated.Spec.Taints, concurrentTaint)
}
