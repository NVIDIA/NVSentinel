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

// expected and learn give tests direct access to the partition state.
// Production code reads and learns a partition only through learnWith.

// expected returns a partition's learned expected count, or false when the
// partition must be learned by a peer walk first.
func (m *partitionMaxima) expected(key partitionExpectedKey) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	partition, ok := m.partitions[key]
	if !ok || !partition.learned {
		return 0, false
	}

	return partition.max, true
}

// learn replaces a partition's contributions with the result of a peer walk.
// peers holds the contribution of every peer the walk found in the partition,
// keyed by node name.
func (m *partitionMaxima) learn(key partitionExpectedKey, peers map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.learnLocked(key, peers)
}

var testPartition = partitionExpectedKey{classIndex: 0, partitionKey: "node.kubernetes.io/instance-type=p5"}

func TestPartitionMaxima_Expected_UnknownUntilLearned(t *testing.T) {
	maxima := newPartitionMaxima()

	maxima.record(testPartition, "node-a", 8)

	_, ok := maxima.expected(testPartition)
	require.False(t, ok, "a recorded node alone must not stand in for a peer walk")

	maxima.learn(testPartition, map[string]int{"node-a": 8, "node-b": 4})

	expected, ok := maxima.expected(testPartition)
	require.True(t, ok)
	require.Equal(t, 8, expected)
}

func TestPartitionMaxima_Record_HigherValueRaisesExpected(t *testing.T) {
	maxima := newPartitionMaxima()
	maxima.learn(testPartition, map[string]int{"node-a": 8, "node-b": 8})

	maxima.record(testPartition, "node-c", 16)

	expected, ok := maxima.expected(testPartition)
	require.True(t, ok)
	require.Equal(t, 16, expected)
}

func TestPartitionMaxima_Record_HolderRaisingItsValueStaysLearned(t *testing.T) {
	maxima := newPartitionMaxima()
	maxima.learn(testPartition, map[string]int{"node-a": 8, "node-b": 4})

	maxima.record(testPartition, "node-a", 16)

	expected, ok := maxima.expected(testPartition)
	require.True(t, ok)
	require.Equal(t, 16, expected)
}

func TestPartitionMaxima_Forget(t *testing.T) {
	tests := []struct {
		name         string
		peers        map[string]int
		forget       string
		wantLearned  bool
		wantExpected int
	}{
		{
			name:         "one of several holders keeps the partition learned",
			peers:        map[string]int{"node-a": 8, "node-b": 8, "node-c": 4},
			forget:       "node-a",
			wantLearned:  true,
			wantExpected: 8,
		},
		{
			name:         "a node below the maximum keeps the partition learned",
			peers:        map[string]int{"node-a": 8, "node-c": 4},
			forget:       "node-c",
			wantLearned:  true,
			wantExpected: 8,
		},
		{
			name:        "the last holder makes the partition relearn",
			peers:       map[string]int{"node-a": 8, "node-c": 4},
			forget:      "node-a",
			wantLearned: false,
		},
		{
			name:         "an unknown node changes nothing",
			peers:        map[string]int{"node-a": 8},
			forget:       "node-z",
			wantLearned:  true,
			wantExpected: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maxima := newPartitionMaxima()
			maxima.learn(testPartition, tt.peers)

			maxima.forget(testPartition.classIndex, tt.forget)

			expected, ok := maxima.expected(testPartition)
			require.Equal(t, tt.wantLearned, ok)

			if tt.wantLearned {
				require.Equal(t, tt.wantExpected, expected)
			}
		})
	}
}

func TestPartitionMaxima_Record_HolderMovingPartitionMakesOldPartitionRelearn(t *testing.T) {
	other := partitionExpectedKey{classIndex: 0, partitionKey: "node.kubernetes.io/instance-type=p4"}
	maxima := newPartitionMaxima()
	maxima.learn(testPartition, map[string]int{"node-a": 8, "node-b": 4})
	maxima.learn(other, map[string]int{"node-c": 2})

	maxima.record(other, "node-a", 8)

	_, ok := maxima.expected(testPartition)
	require.False(t, ok)

	expected, ok := maxima.expected(other)
	require.True(t, ok)
	require.Equal(t, 8, expected)
}

func TestPartitionMaxima_Learn_ReplacesStaleContributions(t *testing.T) {
	maxima := newPartitionMaxima()
	maxima.learn(testPartition, map[string]int{"node-a": 8, "node-b": 4})
	maxima.forget(testPartition.classIndex, "node-a")

	// node-b was recorded before the relearn; the walk no longer finds it.
	maxima.learn(testPartition, map[string]int{"node-c": 2})

	expected, ok := maxima.expected(testPartition)
	require.True(t, ok)
	require.Equal(t, 2, expected)
}

func TestPartitionMaxima_ConcurrentUse(t *testing.T) {
	maxima := newPartitionMaxima()
	maxima.learn(testPartition, map[string]int{"node-0": 8})

	var wg sync.WaitGroup

	for worker := range 8 {
		wg.Go(func() {
			for i := range 200 {
				nodeName := fmt.Sprintf("node-%d-%d", worker, i)
				maxima.record(testPartition, nodeName, i%9)
				maxima.expected(testPartition)
				maxima.forget(testPartition.classIndex, nodeName)
			}
		})
	}

	wg.Wait()

	expected, ok := maxima.expected(testPartition)
	require.True(t, ok)
	require.Equal(t, 8, expected)
}

// countingSliceLoader counts the nodes whose ResourceSlices a ReconcileCache
// loads. A peer walk loads every peer, so the count shows whether an event
// walked the partition.
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

func TestReconcileCache_LaterEvent_DoesNotWalkPeers(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t, peerLearningConfig())
	nodes := p5Fleet(8, 8, 8, 8, 4)
	loader := &countingSliceLoader{}

	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, nodes[4])
	require.Equal(t, len(nodes), loader.reset(), "the first event learns the partition from every peer")
	require.Equal(t, "8", nodes[4].Labels[testGPUCountExpectedLabel])

	// A later event gets a fresh cache, as a node event does in the labeler.
	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, nodes[3])
	require.Equal(t, 1, loader.reset(), "a later event reads only the reconciled node")
	require.Equal(t, "8", nodes[3].Labels[testGPUCountExpectedLabel])
}

func TestReconcileCache_NewNodeWithHigherCount_RaisesExpectedWithoutWalk(t *testing.T) {
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
		"a peer's higher count reaches the partition without a walk")
	require.Equal(t, 2, loader.reset())
}

func TestReconcileCache_ForgottenLastHolder_RelearnsFromRemainingPeers(t *testing.T) {
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
	require.Equal(t, len(nodes), loader.reset(), "losing the last holder relearns the partition by a walk")
	require.Equal(t, "4", nodes[1].Labels[testGPUCountExpectedLabel])
}

func TestManager_ForgetNode_DisabledManagerIsSafe(t *testing.T) {
	var manager *Manager

	require.NotPanics(t, func() { manager.ForgetNode("node-a") })
}

func TestPartitionMaxima_Learn_PeerFromAnotherPartitionLeavesIt(t *testing.T) {
	other := partitionExpectedKey{classIndex: 0, partitionKey: "node.kubernetes.io/instance-type=p4"}
	maxima := newPartitionMaxima()
	maxima.learn(testPartition, map[string]int{"node-a": 8, "node-b": 4})

	// A walk of the other partition finds node-a there, so node-a has moved.
	maxima.learn(other, map[string]int{"node-a": 8})

	_, ok := maxima.expected(testPartition)
	require.False(t, ok, "the partition node-a left must relearn instead of keeping its maximum")
}

func TestPartitionMaxima_LearnWith_KeepsContributionRecordedDuringWalk(t *testing.T) {
	maxima := newPartitionMaxima()
	recording := make(chan struct{})
	recorded := make(chan struct{})

	go func() {
		close(recording)
		maxima.record(testPartition, "node-b", 16)
		close(recorded)
	}()

	expected := maxima.learnWith(testPartition, func() map[string]int {
		// The walk read node-b before node-b's reconciliation recorded 16.
		<-recording
		return map[string]int{"node-a": 8, "node-b": 8}
	})
	require.Equal(t, 8, expected)

	<-recorded

	expected, ok := maxima.expected(testPartition)
	require.True(t, ok)
	require.Equal(t, 16, expected, "a contribution recorded during a walk must not be overwritten by it")
}

func TestPartitionMaxima_LearnWith_SkipsWalkWhenAlreadyLearned(t *testing.T) {
	maxima := newPartitionMaxima()
	maxima.learn(testPartition, map[string]int{"node-a": 8})

	expected := maxima.learnWith(testPartition, func() map[string]int {
		require.Fail(t, "a learned partition must not be walked again")
		return nil
	})
	require.Equal(t, 8, expected)
}

func TestReconcileCache_RecordContributions_UpdatesPartitionWithoutLabels(t *testing.T) {
	ctx := context.Background()
	manager := newTestManager(t, peerLearningConfig())
	nodes := p5Fleet(4, 8)
	loader := &countingSliceLoader{}

	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, nodes[0])
	require.Equal(t, "8", nodes[0].Labels[testGPUCountExpectedLabel])

	// node-1 is opted out: labeler records its new count but writes no labels.
	optedOut := nodes[1].DeepCopy()
	optedOut.Labels["nvidia.com/gpu.count"] = "16"
	manager.NewReconcileCache(nodes, loader.load).RecordContributions(ctx, optedOut)
	require.NotContains(t, optedOut.Labels, testGPUCountExpectedLabel)
	require.NotContains(t, optedOut.Labels, testGPUCountCurrentLabel)

	manager.NewReconcileCache(nodes, loader.load).CalculateAndSetDeviceCountLabels(ctx, nodes[0])
	require.Equal(t, "16", nodes[0].Labels[testGPUCountExpectedLabel])
}
