// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package informer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/nodecache"
	"github.com/nvidia/nvsentinel/store-client/pkg/testutils"
)

// The API-facing tests run against the package's shared envtest API server, so other tests'
// nodes may exist alongside these. They assert on changes to the GPU total from a baseline,
// and compare the counter with a full recount, rather than on absolute counts.

func countTestNode(name string, gpu bool) *v1.Node {
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"pool": "a"}}}
	if gpu {
		node.Labels[GPUNodeLabel] = GPUNodeLabelValue
	}

	return node
}

// createCountNode creates a node on the envtest API server and deletes it at cleanup.
func createCountNode(ctx context.Context, t *testing.T, name string, gpu bool) {
	t.Helper()

	_, err := testClient.CoreV1().Nodes().Create(ctx, countTestNode(name, gpu), metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = testClient.CoreV1().Nodes().Delete(context.Background(), name, metav1.DeleteOptions{})
	})
}

// setNodeLabel sets or removes a label on a live node.
func setNodeLabel(ctx context.Context, t *testing.T, name, key, value string, remove bool) {
	t.Helper()

	node, err := testClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)

	if remove {
		delete(node.Labels, key)
	} else {
		node.Labels[key] = value
	}

	_, err = testClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
}

// startCountingInformer runs a NodeInformer against the envtest API server.
func startCountingInformer(t *testing.T) *NodeInformer {
	t.Helper()

	ni, err := NewNodeInformer(testClient, 0, GPUNodeLabel, GPUNodeLabelValue, nodecache.Keys{})
	require.NoError(t, err)

	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })
	require.NoError(t, ni.Run(stopCh))

	return ni
}

func gpuTotal(t *testing.T, ni *NodeInformer) int {
	t.Helper()

	total, _, err := ni.GetNodeCounts()
	require.NoError(t, err)

	return total
}

// recountGPUNodes is the listing GetNodeCounts used to do, kept as the reference.
func recountGPUNodes(t *testing.T, ni *NodeInformer) int {
	t.Helper()

	nodes, err := ni.lister.List(labels.Set{GPUNodeLabel: GPUNodeLabelValue}.AsSelector())
	require.NoError(t, err)

	return len(nodes)
}

func requireGPUTotal(t *testing.T, ni *NodeInformer, want int) {
	t.Helper()

	require.Eventually(t, func() bool {
		total, _, err := ni.GetNodeCounts()
		return err == nil && total == want
	}, 10*time.Second, 10*time.Millisecond, "GPU node total never reached %d", want)
}

func TestGetNodeCounts_InitialListIsCountedBeforeSync(t *testing.T) {
	ctx := t.Context()

	createCountNode(ctx, t, testutils.GenerateTestNodeName("count-initial-gpu-1"), true)
	createCountNode(ctx, t, testutils.GenerateTestNodeName("count-initial-gpu-2"), true)
	createCountNode(ctx, t, testutils.GenerateTestNodeName("count-initial-cpu-1"), false)

	ni := startCountingInformer(t)

	// Run returned, so the count must already cover the initial list: no waiting here.
	require.Equal(t, recountGPUNodes(t, ni), gpuTotal(t, ni), "counter and a full recount disagree after sync")
}

func TestGetNodeCounts_TracksAddUpdateDelete(t *testing.T) {
	ctx := t.Context()

	gpu1 := testutils.GenerateTestNodeName("count-track-gpu-1")
	gpu2 := testutils.GenerateTestNodeName("count-track-gpu-2")
	cpu1 := testutils.GenerateTestNodeName("count-track-cpu-1")
	cpu2 := testutils.GenerateTestNodeName("count-track-cpu-2")

	createCountNode(ctx, t, gpu1, true)
	createCountNode(ctx, t, cpu1, false)

	ni := startCountingInformer(t)
	base := gpuTotal(t, ni)

	createCountNode(ctx, t, gpu2, true)
	requireGPUTotal(t, ni, base+1)

	createCountNode(ctx, t, cpu2, false)
	requireGPUTotal(t, ni, base+1)

	setNodeLabel(ctx, t, cpu1, GPUNodeLabel, GPUNodeLabelValue, false) // gains the label
	requireGPUTotal(t, ni, base+2)

	setNodeLabel(ctx, t, gpu1, GPUNodeLabel, "", true) // loses it
	requireGPUTotal(t, ni, base+1)

	setNodeLabel(ctx, t, gpu2, "pool", "b", false) // an update that leaves the label alone

	require.NoError(t, testClient.CoreV1().Nodes().Delete(ctx, gpu2, metav1.DeleteOptions{}))
	require.NoError(t, testClient.CoreV1().Nodes().Delete(ctx, cpu2, metav1.DeleteOptions{}))
	requireGPUTotal(t, ni, base)

	require.Equal(t, recountGPUNodes(t, ni), gpuTotal(t, ni), "counter and a full recount disagree")
}

func TestGetNodeCounts_MatchesRecountAfterMixedChanges(t *testing.T) {
	ctx := t.Context()

	names := make([]string, 40)
	for i := range names {
		names[i] = testutils.GenerateTestNodeName(fmt.Sprintf("count-mixed-%d", i))
		createCountNode(ctx, t, names[i], i%2 == 0)
	}

	ni := startCountingInformer(t)
	base := gpuTotal(t, ni)

	for i, name := range names {
		switch i % 4 {
		case 0: // GPU node loses the label
			setNodeLabel(ctx, t, name, GPUNodeLabel, "", true)
		case 1: // non-GPU node gains it
			setNodeLabel(ctx, t, name, GPUNodeLabel, GPUNodeLabelValue, false)
		case 2, 3: // GPU and non-GPU nodes are deleted
			require.NoError(t, testClient.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{}))
		}
	}

	// 20 GPU nodes: 10 lost the label and 10 were deleted. 10 non-GPU nodes gained it.
	requireGPUTotal(t, ni, base-10)
	require.Equal(t, recountGPUNodes(t, ni), gpuTotal(t, ni), "counter and a full recount disagree")
}

// A real API server cannot be made to emit a tombstone; it is produced only when a watch
// misses a delete, so the handler is exercised directly.
func TestCountDeletedNode_Tombstone(t *testing.T) {
	ni := &NodeInformer{gpuNodeLabelKey: GPUNodeLabel, gpuNodeLabelValue: GPUNodeLabelValue}
	ni.gpuNodes.Store(2)

	ni.countDeletedNode(cache.DeletedFinalStateUnknown{Key: "gpu-1", Obj: countTestNode("gpu-1", true)})
	require.Equal(t, int64(1), ni.gpuNodes.Load(), "a tombstone for a GPU node must uncount it")

	ni.countDeletedNode(cache.DeletedFinalStateUnknown{Key: "cpu-1", Obj: countTestNode("cpu-1", false)})
	require.Equal(t, int64(1), ni.gpuNodes.Load(), "a tombstone for a non-GPU node must not change the count")
}

func TestIsGPUNode_EmptyValueRequiresTheLabel(t *testing.T) {
	ni := &NodeInformer{gpuNodeLabelKey: GPUNodeLabel, gpuNodeLabelValue: ""}

	unlabelled := countTestNode("cpu-1", false)
	require.False(t, ni.isGPUNode(unlabelled), "a node without the label must not match an empty value")

	labelled := countTestNode("gpu-1", false)
	labelled.Labels[GPUNodeLabel] = ""
	require.True(t, ni.isGPUNode(labelled), "a node carrying the label with an empty value must match")
}
