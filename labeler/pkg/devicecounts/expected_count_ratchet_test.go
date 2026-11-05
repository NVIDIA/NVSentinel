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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// These tests play #1914 with the GPU class as the labeler chart ships it: a
// fleet of 287 GB200 nodes whose device plugin and GFD report 4 GPUs, two of
// which are switched to MIG, where GFD reports 8.

const (
	ratchetFleetSize    = 287
	ratchetPlainProduct = "NVIDIA-GB200"
	ratchetMIGProduct   = "NVIDIA-GB200-MIG-3g.93gb"
)

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
// 8 GPUs while they still carry the plain product, as GFD does halfway through
// a switch to MIG, then move to their MIG partition. The labeler restarts and
// reconciles the fleet three more times.
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

func TestExpectedCountRatchet_MIGRegroupKeepsTheHigherCount(t *testing.T) {
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
}

// Removing the expected label is the relearn path ADR-043 names. The labeler
// does not reconcile a node for a change to a label it manages, and the
// node's next reconcile takes the old value back from the other nodes, so the
// labels cannot be cleared one node at a time.
func TestExpectedCountRatchet_RemovedLabelIsRestoredByTheOtherNodes(t *testing.T) {
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
}

// No transient is needed. When MIG is turned off cleanly, the two nodes bring
// their expected count of 8 into the plain partition and it spreads to every
// node there.
func TestExpectedCountRatchet_CleanMIGDisableSpreadsTheMIGCount(t *testing.T) {
	nodes := ratchetFleet(ratchetFleetSize, 4)
	for _, node := range nodes[:2] {
		ratchetSetGFD(node, ratchetMIGProduct, 8)
	}

	manager := newTestManager(t, testGPUSourcesConfig())
	nodes = ratchetSweep(t, manager, nodes)
	require.Zero(t, ratchetMismatches(nodes))
	require.Equal(t, "8", nodes[0].Labels[testGPUCountExpectedLabel])

	for _, node := range nodes[:2] {
		ratchetSetGFD(node, ratchetPlainProduct, 4)
	}

	nodes = ratchetSweep(t, manager, nodes)

	require.Equal(t, ratchetFleetSize, ratchetMismatches(nodes))

	for _, node := range nodes {
		require.Equal(t, "4", node.Labels[testGPUCountCurrentLabel], node.Name)
		require.Equal(t, "8", node.Labels[testGPUCountExpectedLabel], node.Name)
	}
}
