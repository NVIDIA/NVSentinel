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

package devicecounts

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
)

// known and learn let tests inspect and set a partition directly. Labeler
// itself goes through getOrLearn.

// known returns a partition's expected count, or false when labeler would have
// to read the partition in full first.
func (c *learnedCounts) known(key partitionExpectedKey) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	partition, ok := c.partitions[key]
	if !ok || !partition.known {
		return 0, false
	}

	return partition.expected, true
}

// learn stores the result of reading a partition in full.
func (c *learnedCounts) learn(key partitionExpectedKey, peerCounts map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.learnLocked(key, peerCounts)
}

var testPartition = partitionExpectedKey{classIndex: 0, partitionKey: "node.kubernetes.io/instance-type=p5"}

func TestLearnedCounts_Known_FalseUntilPartitionRead(t *testing.T) {
	counts := newLearnedCounts()

	counts.setNode(testPartition, "node-a", 8)

	_, ok := counts.known(testPartition)
	require.False(t, ok, "one remembered node does not replace reading the partition in full")

	counts.learn(testPartition, map[string]int{"node-a": 8, "node-b": 4})

	expected, ok := counts.known(testPartition)
	require.True(t, ok)
	require.Equal(t, 8, expected)
}

func TestLearnedCounts_SetNode_HigherCountRaisesExpected(t *testing.T) {
	counts := newLearnedCounts()
	counts.learn(testPartition, map[string]int{"node-a": 8, "node-b": 8})

	counts.setNode(testPartition, "node-c", 16)

	expected, ok := counts.known(testPartition)
	require.True(t, ok)
	require.Equal(t, 16, expected)
}

func TestLearnedCounts_SetNode_NodeWithHighestCountRaisingItStaysKnown(t *testing.T) {
	counts := newLearnedCounts()
	counts.learn(testPartition, map[string]int{"node-a": 8, "node-b": 4})

	counts.setNode(testPartition, "node-a", 16)

	expected, ok := counts.known(testPartition)
	require.True(t, ok)
	require.Equal(t, 16, expected)
}

func TestLearnedCounts_RemoveNode(t *testing.T) {
	tests := []struct {
		name         string
		peers        map[string]int
		forget       string
		wantKnown    bool
		wantExpected int
	}{
		{
			name:         "one of several nodes with the highest count keeps the partition known",
			peers:        map[string]int{"node-a": 8, "node-b": 8, "node-c": 4},
			forget:       "node-a",
			wantKnown:    true,
			wantExpected: 8,
		},
		{
			name:         "a node below the highest count keeps the partition known",
			peers:        map[string]int{"node-a": 8, "node-c": 4},
			forget:       "node-c",
			wantKnown:    true,
			wantExpected: 8,
		},
		{
			name:      "the last node with the highest count makes the partition unknown",
			peers:     map[string]int{"node-a": 8, "node-c": 4},
			forget:    "node-a",
			wantKnown: false,
		},
		{
			name:         "an unknown node changes nothing",
			peers:        map[string]int{"node-a": 8},
			forget:       "node-z",
			wantKnown:    true,
			wantExpected: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counts := newLearnedCounts()
			counts.learn(testPartition, tt.peers)

			counts.removeNode(testPartition.classIndex, tt.forget)

			expected, ok := counts.known(testPartition)
			require.Equal(t, tt.wantKnown, ok)

			if tt.wantKnown {
				require.Equal(t, tt.wantExpected, expected)
			}
		})
	}
}

func TestLearnedCounts_SetNode_NodeWithHighestCountMovingMakesOldPartitionUnknown(t *testing.T) {
	other := partitionExpectedKey{classIndex: 0, partitionKey: "node.kubernetes.io/instance-type=p4"}
	counts := newLearnedCounts()
	counts.learn(testPartition, map[string]int{"node-a": 8, "node-b": 4})
	counts.learn(other, map[string]int{"node-c": 2})

	counts.setNode(other, "node-a", 8)

	_, ok := counts.known(testPartition)
	require.False(t, ok)

	expected, ok := counts.known(other)
	require.True(t, ok)
	require.Equal(t, 8, expected)
}

func TestLearnedCounts_Learn_ReplacesNodesRememberedBefore(t *testing.T) {
	counts := newLearnedCounts()
	counts.learn(testPartition, map[string]int{"node-a": 8, "node-b": 4})
	counts.removeNode(testPartition.classIndex, "node-a")

	// node-b was remembered before the second read, which no longer finds it.
	counts.learn(testPartition, map[string]int{"node-c": 2})

	expected, ok := counts.known(testPartition)
	require.True(t, ok)
	require.Equal(t, 2, expected)
}

func TestLearnedCounts_ConcurrentUse(t *testing.T) {
	counts := newLearnedCounts()
	counts.learn(testPartition, map[string]int{"node-0": 8})

	var wg sync.WaitGroup

	for worker := range 8 {
		wg.Go(func() {
			for i := range 200 {
				nodeName := fmt.Sprintf("node-%d-%d", worker, i)
				counts.setNode(testPartition, nodeName, i%9)
				counts.known(testPartition)
				counts.removeNode(testPartition.classIndex, nodeName)
			}
		})
	}

	wg.Wait()

	expected, ok := counts.known(testPartition)
	require.True(t, ok)
	require.Equal(t, 8, expected)
}

// countingSliceLoader counts the nodes whose ResourceSlices a ReconcileCache
// loads. Reading a partition in full loads every node in it, so the count
// shows whether an event read the whole partition.
type countingSliceLoader struct {
	mu    sync.Mutex
	calls int
}

func (l *countingSliceLoader) load(*corev1.Node) []*resourcev1.ResourceSlice {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.calls++

	return nil
}

func (l *countingSliceLoader) reset() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	calls := l.calls
	l.calls = 0

	return calls
}

func peerLearningConfig() Config {
	return Config{
		Enabled: true,
		Classes: []ClassConfig{
			{
				Name:    "gpu",
				Enabled: true,
				Labels: Labels{
					Current:  testGPUCountCurrentLabel,
					Expected: testGPUCountExpectedLabel,
				},
				GroupingLabels:    []string{"node.kubernetes.io/instance-type"},
				CurrentExpression: "int(node.metadata.labels['nvidia.com/gpu.count'])",
			},
		},
	}
}

func p5Fleet(counts ...int) []*corev1.Node {
	nodes := make([]*corev1.Node, 0, len(counts))
	for i, count := range counts {
		nodes = append(nodes, testNode(fmt.Sprintf("node-%d", i), map[string]string{
			"node.kubernetes.io/instance-type": "p5",
			"nvidia.com/gpu.count":             fmt.Sprint(count),
		}))
	}

	return nodes
}

func TestReconcileCache_LaterEvent_ReadsOnlyItsOwnNode(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t, peerLearningConfig())
	nodes := p5Fleet(8, 8, 8, 8, 4)
	loader := &countingSliceLoader{}

	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, nodes[4])
	require.Equal(t, len(nodes)+1, loader.reset(),
		"the first event looks up its own node, then every node in the partition")
	require.Equal(t, "8", nodes[4].Labels[testGPUCountExpectedLabel])

	// A later event gets a fresh cache, as a node event does in the labeler.
	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, nodes[3])
	require.Equal(t, 1, loader.reset(), "a later event reads only the reconciled node")
	require.Equal(t, "8", nodes[3].Labels[testGPUCountExpectedLabel])
}

func TestReconcileCache_NewNodeWithHigherCount_RaisesExpectedWithoutFullRead(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t, peerLearningConfig())
	nodes := p5Fleet(8, 8, 4)
	loader := &countingSliceLoader{}

	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, nodes[2])
	require.Equal(t, "8", nodes[2].Labels[testGPUCountExpectedLabel])
	loader.reset()

	bigger := p5Fleet(16)[0]
	bigger.Name = "node-new"
	nodes = append(nodes, bigger)

	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, bigger)
	require.Equal(t, "16", bigger.Labels[testGPUCountExpectedLabel])

	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, nodes[2])
	require.Equal(t, "16", nodes[2].Labels[testGPUCountExpectedLabel],
		"a node's higher count reaches the partition without reading every node")
	require.Equal(t, 2, loader.reset())
}

func TestReconcileCache_ForgottenNodeWithHighestCount_RereadsPartition(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t, peerLearningConfig())
	nodes := p5Fleet(4, 4)
	loader := &countingSliceLoader{}

	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, nodes[0])

	bigger := p5Fleet(16)[0]
	bigger.Name = "node-new"
	manager.NewReconcileCache(append(nodes, bigger), loader.load).CalculateAndSetDeviceCountLabels(ctx, bigger)
	loader.reset()

	// The only node that reported 16 is deleted before any peer learned 16.
	manager.ForgetNode(bigger.Name)

	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, nodes[1])
	require.Equal(t, len(nodes)+1, loader.reset(),
		"losing the last node with the highest count makes labeler read the partition again")
	require.Equal(t, "4", nodes[1].Labels[testGPUCountExpectedLabel])
}

func TestManager_ForgetNode_DisabledManagerIsSafe(t *testing.T) {
	var manager *Manager

	require.NotPanics(t, func() { manager.ForgetNode("node-a") })
}

func TestLearnedCounts_Learn_PeerFromAnotherPartitionLeavesIt(t *testing.T) {
	other := partitionExpectedKey{classIndex: 0, partitionKey: "node.kubernetes.io/instance-type=p4"}
	counts := newLearnedCounts()
	counts.learn(testPartition, map[string]int{"node-a": 8, "node-b": 4})

	// Reading the other partition finds node-a there, so node-a has moved.
	counts.learn(other, map[string]int{"node-a": 8})

	_, ok := counts.known(testPartition)
	require.False(t, ok, "the partition node-a left must not keep node-a's count as its expected count")
}

func TestLearnedCounts_GetOrLearn_KeepsCountRememberedDuringRead(t *testing.T) {
	counts := newLearnedCounts()
	recorded := make(chan struct{})

	expected := counts.getOrLearn(testPartition, func() map[string]int {
		// node-b's own reconciliation remembers 16 while the read, which saw 8,
		// is still running. Started here, setNode has to wait for the lock that
		// getOrLearn holds, so it always runs after the read is stored.
		go func() {
			counts.setNode(testPartition, "node-b", 16)
			close(recorded)
		}()

		return map[string]int{"node-a": 8, "node-b": 8}
	})
	require.Equal(t, 8, expected)

	<-recorded

	expected, ok := counts.known(testPartition)
	require.True(t, ok)
	require.Equal(t, 16, expected, "a count remembered during a full read must not be overwritten by the read")
}

func TestLearnedCounts_GetOrLearn_KnownPartitionIsNotReadAgain(t *testing.T) {
	counts := newLearnedCounts()
	counts.learn(testPartition, map[string]int{"node-a": 8})

	expected := counts.getOrLearn(testPartition, func() map[string]int {
		require.Fail(t, "a known partition must not be read again")
		return nil
	})
	require.Equal(t, 8, expected)
}

func TestReconcileCache_RememberPeerCounts_UpdatesPartitionWithoutLabels(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t, peerLearningConfig())
	nodes := p5Fleet(4, 8)
	loader := &countingSliceLoader{}

	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, nodes[0])
	require.Equal(t, "8", nodes[0].Labels[testGPUCountExpectedLabel])

	// node-1 is opted out: labeler records its new count but writes no labels.
	optedOut := nodes[1].DeepCopy()
	optedOut.Labels["nvidia.com/gpu.count"] = "16"
	manager.NewReconcileCache(nodes, loader.load).RememberPeerCounts(ctx, optedOut)
	require.NotContains(t, optedOut.Labels, testGPUCountExpectedLabel)
	require.NotContains(t, optedOut.Labels, testGPUCountCurrentLabel)

	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, nodes[0])
	require.Equal(t, "16", nodes[0].Labels[testGPUCountExpectedLabel])
}

func TestReconcileCache_NodeLoweringItsOwnHighestCount_GetsTheLowerCount(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t, peerLearningConfig())
	nodes := p5Fleet(16)
	loader := &countingSliceLoader{}

	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, nodes[0])
	require.Equal(t, "16", nodes[0].Labels[testGPUCountExpectedLabel])

	// The node is the only one in its partition, and both its count and its
	// expected label drop to 4, for example after someone corrects the label.
	nodes[0].Labels["nvidia.com/gpu.count"] = "4"
	nodes[0].Labels[testGPUCountExpectedLabel] = "4"

	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, nodes[0])
	require.Equal(t, "4", nodes[0].Labels[testGPUCountExpectedLabel],
		"the node must not get its own old count back from what labeler remembered")
}

func TestLearnedCounts_RemoveNode_LastNodeRemovesPartition(t *testing.T) {
	counts := newLearnedCounts()
	counts.learn(testPartition, map[string]int{"node-a": 8})

	counts.removeNode(testPartition.classIndex, "node-a")

	require.Empty(t, counts.partitions, "an empty partition must not stay in the map")
}

func TestLearnedCounts_SetNode_LastNodeMovingAwayRemovesPartition(t *testing.T) {
	other := partitionExpectedKey{classIndex: 0, partitionKey: "node.kubernetes.io/instance-type=p4"}
	counts := newLearnedCounts()
	counts.learn(testPartition, map[string]int{"node-a": 8})

	counts.setNode(other, "node-a", 8)

	require.NotContains(t, counts.partitions, testPartition)
	require.Contains(t, counts.partitions, other)
}

func TestReconcileCache_LazySweep_ListsNodesForEveryFullRead(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t, peerLearningConfig())

	p5Node := p5Fleet(8)[0]
	p4Node := testNode("p4-a", map[string]string{
		"node.kubernetes.io/instance-type": "p4",
		"nvidia.com/gpu.count":             "4",
	})
	deletedP4Node := testNode("p4-deleted", map[string]string{
		"node.kubernetes.io/instance-type": "p4",
		"nvidia.com/gpu.count":             "16",
	})

	// The first list still has p4-deleted; it is deleted before the sweep reads
	// the p4 partition, so every later list leaves it out.
	lists := 0
	listNodes := func() []*corev1.Node {
		lists++
		if lists == 1 {
			return []*corev1.Node{p5Node, p4Node, deletedP4Node}
		}

		return []*corev1.Node{p5Node, p4Node}
	}

	sweep := manager.NewLazyReconcileCache(listNodes, (&countingSliceLoader{}).load)
	sweep.CalculateAndSetDeviceCountLabels(ctx, p5Node)
	sweep.CalculateAndSetDeviceCountLabels(ctx, p4Node)

	require.Equal(t, 2, lists, "each partition read must list the nodes again")
	require.Equal(t, "4", p4Node.Labels[testGPUCountExpectedLabel],
		"a node deleted before its partition was read must not raise the expected count")
}
