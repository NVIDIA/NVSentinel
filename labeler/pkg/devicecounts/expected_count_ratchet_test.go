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

package devicecounts

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// These tests play #1914 with the GPU class as the labeler chart ships it: a
// fleet of 287 GB200 nodes whose device plugin and GFD report 4 GPUs, two of
// which are switched to MIG, where GFD reports 8. A test that needs a relearn
// ends with the operator's relearn from ADR-071.

const (
	ratchetFleetSize    = 287
	ratchetPlainProduct = "NVIDIA-GB200"
	ratchetMIGProduct   = "NVIDIA-GB200-MIG-3g.93gb"
	ratchetRelearnID    = "2026-11-05-mig"
	ratchetAnnotation   = "nvsentinel.dgxc.nvidia.com/gpu.count.relearn"

	// The partition annotation and the partitions of the gfd source for the
	// plain and the MIG product.
	ratchetPartitionAnnotation = "nvsentinel.dgxc.nvidia.com/gpu.count.partition"
	ratchetPlainPartition      = "source=gfd|node.kubernetes.io/instance-type=gb200|nvidia.com/gpu.product=" +
		ratchetPlainProduct
	ratchetMIGPartition = "source=gfd|node.kubernetes.io/instance-type=gb200|nvidia.com/gpu.product=" +
		ratchetMIGProduct
)

// ratchetRelearnConfig returns the GPU class with a relearn ID, loaded from the
// TOML file that the labeler chart renders, settledExpression included.
func ratchetRelearnConfig(t *testing.T, relearnID string) Config {
	t.Helper()

	path := filepath.Join(t.TempDir(), "expected-device-counts.toml")
	require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, `enabled = true

[[classes]]
name = "gpu"
enabled = true
relearnID = %q
settledExpression = '''%s'''

[classes.labels]
current = %q
expected = %q

[[classes.sources]]
name = "dra"
resourceSliceDriver = %q
groupingLabels = [%q]
currentExpression = '''%s'''
groupingExpression = '''%s'''

[[classes.sources]]
name = "gfd"
groupingLabels = [%q, %q]
currentExpression = '''%s'''
`, relearnID, testGPUSettledExpression, testGPUCountCurrentLabel, testGPUCountExpectedLabel, testGPUDRADriver,
		testInstanceType,
		testDRAGPUCurrentExpression, testDRAGPUGroupingExpression, testInstanceType, testGFDGPUProduct,
		testGFDGPUCurrentExpression), 0o600))

	config, err := LoadConfig(path)
	require.NoError(t, err)

	return config
}

// ratchetFleet returns ClusterPolicy-mode nodes as the gfd source sees them.
func ratchetFleet(size, count int) []*corev1.Node {
	nodes := make([]*corev1.Node, size)
	for i := range nodes {
		nodes[i] = testNode(fmt.Sprintf("node-%03d", i), map[string]string{testInstanceType: "gb200"})
		ratchetSetGFD(nodes[i], ratchetPlainProduct, count)
	}

	return nodes
}

// ratchetSetGFD sets what GFD and the device plugin report on a node.
func ratchetSetGFD(node *corev1.Node, product string, count int) {
	node.Labels[testGFDGPUProduct] = product
	node.Labels[testGFDGPUCount] = fmt.Sprint(count)
	node.Status.Allocatable = testDevicePluginGPUs(fmt.Sprint(count))
}

// ratchetSweep reconciles every node with one shared cache, as the labeler's
// startup reconcile does, and returns the updated nodes.
func ratchetSweep(t *testing.T, manager *Manager, nodes []*corev1.Node) []*corev1.Node {
	t.Helper()

	cache := manager.NewReconcileCache(nodes, nil)
	updated := make([]*corev1.Node, len(nodes))

	for i, node := range nodes {
		updated[i] = node.DeepCopy()
		cache.CalculateAndSetDeviceCountLabels(t.Context(), updated[i])
	}

	return updated
}

// ratchetReconcileOne reconciles one node with a fresh cache over all nodes, as
// a node event does.
func ratchetReconcileOne(t *testing.T, manager *Manager, nodes []*corev1.Node, i int) *corev1.Node {
	t.Helper()

	updated := nodes[i].DeepCopy()
	manager.NewReconcileCache(nodes, nil).CalculateAndSetDeviceCountLabels(t.Context(), updated)

	return updated
}

func ratchetMismatches(nodes []*corev1.Node) int {
	mismatches := 0

	for _, node := range nodes {
		if node.Labels[testGPUCountCurrentLabel] != node.Labels[testGPUCountExpectedLabel] {
			mismatches++
		}
	}

	return mismatches
}

// ratchetRegroupedFleet returns the fleet as #1914 reports it. Two nodes report
// 8 GPUs while they still carry the plain product, as the reporter saw partway
// through a switch to MIG, then move to their MIG partition. The labeler
// restarts and reconciles the fleet three more times.
func ratchetRegroupedFleet(t *testing.T) []*corev1.Node {
	t.Helper()

	manager := newTestManager(t, testGPUSourcesConfig())
	nodes := ratchetSweep(t, manager, ratchetFleet(ratchetFleetSize, 4))
	require.Zero(t, ratchetMismatches(nodes))

	for _, node := range nodes[:2] {
		ratchetSetGFD(node, ratchetPlainProduct, 8)
	}

	nodes = ratchetSweep(t, manager, nodes)

	for _, node := range nodes[:2] {
		ratchetSetGFD(node, ratchetMIGProduct, 8)
	}

	manager = newTestManager(t, testGPUSourcesConfig())
	for range 3 {
		nodes = ratchetSweep(t, manager, nodes)
	}

	return nodes
}

func TestExpectedCountRatchet_MIGRegroupKeepsTheHigherCountUntilARelearn(t *testing.T) {
	nodes := ratchetRegroupedFleet(t)

	require.Equal(t, ratchetFleetSize-2, ratchetMismatches(nodes))

	for _, node := range nodes[:2] {
		require.Equal(t, "8", node.Labels[testGPUCountCurrentLabel], node.Name)
		require.Equal(t, "8", node.Labels[testGPUCountExpectedLabel], node.Name)
	}

	for _, node := range nodes[2:] {
		require.Equal(t, "4", node.Labels[testGPUCountCurrentLabel], node.Name)
		require.Equal(t, "8", node.Labels[testGPUCountExpectedLabel], node.Name)
	}

	// The operator sets a relearn ID. One reconcile of the fleet relearns the
	// plain partition from current counts, the MIG nodes keep 8, and every
	// node records the ID with its value.
	manager := newTestManager(t, ratchetRelearnConfig(t, ratchetRelearnID))
	nodes = ratchetSweep(t, manager, nodes)

	require.Zero(t, ratchetMismatches(nodes))
	require.Equal(t, "8", nodes[0].Labels[testGPUCountExpectedLabel])
	require.Equal(t, "4", nodes[2].Labels[testGPUCountExpectedLabel])

	for _, node := range nodes {
		require.Equal(t, ratchetRelearnID+"/"+node.Labels[testGPUCountExpectedLabel],
			node.Annotations[ratchetAnnotation], node.Name)
	}

	// A restart with the same ID changes nothing.
	cache := newTestManager(t, ratchetRelearnConfig(t, ratchetRelearnID)).NewReconcileCache(nodes, nil)
	for _, node := range nodes {
		require.False(t, cache.CalculateAndSetDeviceCountLabels(t.Context(), node.DeepCopy()), node.Name)
	}

	// MIG is turned off on the two nodes again. They come back to the plain
	// partition with the 8 that the relearn certified in their MIG partition.
	// That label was learned in another partition, so it counts neither for
	// them nor for the other nodes: the two nodes relearn 4 under the same ID
	// and the 285 others stay at 4.
	for _, node := range nodes[:2] {
		ratchetSetGFD(node, ratchetPlainProduct, 4)
	}

	nodes = ratchetSweep(t, newTestManager(t, ratchetRelearnConfig(t, ratchetRelearnID)), nodes)

	require.Zero(t, ratchetMismatches(nodes))

	for _, node := range nodes {
		require.Equal(t, "4", node.Labels[testGPUCountExpectedLabel], node.Name)
		require.Equal(t, ratchetRelearnID+"/4", node.Annotations[ratchetAnnotation], node.Name)
		require.Equal(t, ratchetPlainPartition, node.Annotations[ratchetPartitionAnnotation], node.Name)
	}
}

// Removing the expected label is the relearn path ADR-043 names. The labeler
// does not reconcile a node for a change to a label it manages, and the
// node's next reconcile takes the old value back from the other nodes, so the
// labels cannot be cleared one node at a time. Under a relearn ID the old
// labels of the other nodes no longer count.
func TestExpectedCountRatchet_RemovedLabelIsRestoredUntilARelearn(t *testing.T) {
	nodes := ratchetRegroupedFleet(t)
	manager := newTestManager(t, testGPUSourcesConfig())

	for i := 2; i < 12; i++ {
		before := nodes[i].DeepCopy()
		delete(nodes[i].Labels, testGPUCountExpectedLabel)
		require.False(t, manager.NodeLabelsAffectDeviceCounts(before.Labels, nodes[i].Labels))

		nodes[i] = ratchetReconcileOne(t, manager, nodes, i)
		require.Equal(t, "8", nodes[i].Labels[testGPUCountExpectedLabel], nodes[i].Name)
	}

	require.Equal(t, ratchetFleetSize-2, ratchetMismatches(nodes))

	// Each node reconciled on its own while 275 others still read 8 gets the
	// count of its partition, and keeps it on its next reconcile.
	manager = newTestManager(t, ratchetRelearnConfig(t, ratchetRelearnID))

	for range 2 {
		for i := 2; i < 12; i++ {
			nodes[i] = ratchetReconcileOne(t, manager, nodes, i)
			require.Equal(t, "4", nodes[i].Labels[testGPUCountExpectedLabel], nodes[i].Name)
		}
	}

	require.Equal(t, ratchetFleetSize-12, ratchetMismatches(nodes))
}

// No transient is needed for the other path. When MIG is turned off cleanly,
// the two nodes come back to the plain partition with the expected count of 8
// that they learned in their MIG partition. That label does not count in the
// plain partition, so the two nodes relearn 4 and no other node changes, with
// no relearn ID.
func TestExpectedCountRatchet_CleanMIGDisableKeepsTheMIGCountInItsPartition(t *testing.T) {
	nodes := ratchetFleet(ratchetFleetSize, 4)
	for _, node := range nodes[:2] {
		ratchetSetGFD(node, ratchetMIGProduct, 8)
	}

	manager := newTestManager(t, testGPUSourcesConfig())
	nodes = ratchetSweep(t, manager, nodes)
	require.Zero(t, ratchetMismatches(nodes))
	require.Equal(t, "8", nodes[0].Labels[testGPUCountExpectedLabel])

	learnedIn := nodes[0].Annotations[ratchetPartitionAnnotation]

	for _, node := range nodes[:2] {
		ratchetSetGFD(node, ratchetPlainProduct, 4)
	}

	nodes = ratchetSweep(t, manager, nodes)

	require.Zero(t, ratchetMismatches(nodes))

	for _, node := range nodes {
		require.Equal(t, "4", node.Labels[testGPUCountExpectedLabel], node.Name)
	}

	require.Equal(t, ratchetMIGPartition, learnedIn)

	for _, node := range nodes {
		require.Equal(t, ratchetPlainPartition, node.Annotations[ratchetPartitionAnnotation], node.Name)
	}
}

// The same story with the MIG manager. It marks the two nodes pending while it
// switches them back to the plain product, and a count source that it does not
// pause briefly reports 8 GPUs under the plain product. GFD under the GPU
// Operator would be stopped at that point. Nothing is learned from the two
// nodes until the manager reports success, and then the 8 they carry stays in
// their MIG partition. The two nodes relearn 4 under the same ID and no other
// node moves.
func TestExpectedCountRatchet_MIGDisableWhilePendingNeedsNoNewID(t *testing.T) {
	manager := newTestManager(t, ratchetRelearnConfig(t, ratchetRelearnID))
	nodes := ratchetSweep(t, manager, ratchetRegroupedFleet(t))
	require.Zero(t, ratchetMismatches(nodes))

	for _, node := range nodes[:2] {
		node.Labels[testMIGConfigState] = "pending"
		ratchetSetGFD(node, ratchetPlainProduct, 8)
	}

	nodes = ratchetSweep(t, newTestManager(t, ratchetRelearnConfig(t, ratchetRelearnID)), nodes)

	for _, node := range nodes[2:] {
		require.Equal(t, "4", node.Labels[testGPUCountExpectedLabel], node.Name)
	}

	for _, node := range nodes[:2] {
		require.Equal(t, "8", node.Labels[testGPUCountExpectedLabel], node.Name)
		require.Equal(t, ratchetMIGPartition, node.Annotations[ratchetPartitionAnnotation], node.Name)
		ratchetSetGFD(node, ratchetPlainProduct, 4)
	}

	nodes = ratchetSweep(t, manager, nodes)
	require.Equal(t, 2, ratchetMismatches(nodes), "the two nodes report 4 and keep 8 while they are pending")

	for _, node := range nodes[:2] {
		node.Labels[testMIGConfigState] = "success"
	}

	nodes = ratchetSweep(t, manager, nodes)

	require.Zero(t, ratchetMismatches(nodes))

	for _, node := range nodes {
		require.Equal(t, "4", node.Labels[testGPUCountExpectedLabel], node.Name)
		require.Equal(t, ratchetRelearnID+"/4", node.Annotations[ratchetAnnotation], node.Name)
		require.Equal(t, ratchetPlainPartition, node.Annotations[ratchetPartitionAnnotation], node.Name)
	}
}
