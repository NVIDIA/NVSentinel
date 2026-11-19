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
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	partitionTestNICAnnotation = "nvsentinel.dgxc.nvidia.com/nic.count.partition"
	partitionTestH100Product   = "NVIDIA-H100-80GB-HBM3"
)

// skippedUpdates reads labeler_device_count_skipped_updates_total for a class
// and a reason from the default registry.
func skippedUpdates(t *testing.T, className, reason string) float64 {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != "labeler_device_count_skipped_updates_total" {
			continue
		}

		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}

			if labels["class"] == className && labels["reason"] == reason {
				return metric.GetCounter().GetValue()
			}
		}
	}

	return 0
}

// A node that leaves MIG brings the 8 it learned in its MIG partition. That
// label does not count in the plain partition: a plain node reconciled before
// the two nodes ignores it, the two nodes relearn 4 and record the plain
// partition, and no plain node changes.
func TestPartition_NodeLeavingMIGRelearnsInThePlainPartition(t *testing.T) {
	manager := newTestManager(t, testGPUSourcesConfig())
	nodes := ratchetFleet(ratchetFleetSize, 4)

	for _, node := range nodes[:2] {
		ratchetSetGFD(node, ratchetMIGProduct, 8)
	}

	nodes = ratchetSweep(t, manager, nodes)
	require.Zero(t, ratchetMismatches(nodes))
	require.Equal(t, "8", nodes[0].Labels[testGPUCountExpectedLabel])

	learnedIn := []string{
		nodes[0].Annotations[ratchetPartitionAnnotation],
		nodes[2].Annotations[ratchetPartitionAnnotation],
	}

	for _, node := range nodes[:2] {
		ratchetSetGFD(node, ratchetPlainProduct, 4)
	}

	ignored := skippedUpdates(t, "gpu", "partition_changed")

	nodes[10] = ratchetReconcileOne(t, manager, nodes, 10)
	require.Equal(t, "4", nodes[10].Labels[testGPUCountExpectedLabel], "a plain node does not take the MIG count")

	ignored = skippedUpdates(t, "gpu", "partition_changed") - ignored

	for i := range 2 {
		nodes[i] = ratchetReconcileOne(t, manager, nodes, i)
		require.Equal(t, "4", nodes[i].Labels[testGPUCountCurrentLabel], nodes[i].Name)
		require.Equal(t, "4", nodes[i].Labels[testGPUCountExpectedLabel], nodes[i].Name)
	}

	require.Zero(t, ratchetMismatches(nodes))

	// The annotations that kept the MIG count out of the plain partition.
	require.Equal(t, []string{ratchetMIGPartition, ratchetPlainPartition}, learnedIn)
	require.InDelta(t, 2, ignored, 0, "the labels the two nodes learned in their MIG partition are not used")

	for _, node := range nodes[:2] {
		require.Equal(t, ratchetPlainPartition, node.Annotations[ratchetPartitionAnnotation], node.Name)
	}

	cache := manager.NewReconcileCache(nodes, nil)
	for _, node := range nodes {
		require.False(t, cache.CalculateAndSetDeviceCountLabels(t.Context(), node.DeepCopy()), node.Name)
	}
}

// The same holds for a class without sources. A node rebuilt as another
// instance type with fewer NICs keeps the labels from its old instance type,
// and its old expected count does not count in the new partition.
func TestPartition_NodeMovedToAnotherInstanceTypeRelearns(t *testing.T) {
	manager := newTestManager(t, Config{Enabled: true, Classes: []ClassConfig{{
		Name:              "nic",
		Enabled:           true,
		Labels:            Labels{Current: testNICCountCurrentLabel, Expected: testNICCountExpectedLabel},
		GroupingLabels:    []string{testInstanceType},
		CurrentExpression: "int(node.status.allocatable['nvidia.com/mlnxnics'])",
	}}})

	setNICs := func(node *corev1.Node, instanceType, count string) {
		node.Labels[testInstanceType] = instanceType
		node.Status.Allocatable = corev1.ResourceList{"nvidia.com/mlnxnics": resource.MustParse(count)}
	}

	nodes := make([]*corev1.Node, 6)
	for i := range nodes {
		nodes[i] = testNode(fmt.Sprintf("node-%d", i), map[string]string{})

		if i < 3 {
			setNICs(nodes[i], "large", "8")
		} else {
			setNICs(nodes[i], "small", "4")
		}
	}

	nodes = ratchetSweep(t, manager, nodes)
	require.Equal(t, "8", nodes[0].Labels[testNICCountExpectedLabel])

	learnedIn := nodes[0].Annotations[partitionTestNICAnnotation]

	setNICs(nodes[0], "small", "4")

	nodes[3] = ratchetReconcileOne(t, manager, nodes, 3)
	require.Equal(t, "4", nodes[3].Labels[testNICCountExpectedLabel], "a small node does not take the large count")

	nodes = ratchetSweep(t, manager, nodes)

	require.Equal(t, "4", nodes[0].Labels[testNICCountCurrentLabel])
	require.Equal(t, "4", nodes[0].Labels[testNICCountExpectedLabel])

	for _, node := range nodes[1:3] {
		require.Equal(t, "8", node.Labels[testNICCountExpectedLabel], node.Name)
	}

	for _, node := range nodes[3:] {
		require.Equal(t, "4", node.Labels[testNICCountExpectedLabel], node.Name)
	}

	require.Equal(t, testInstanceType+"=large", learnedIn)
	require.Equal(t, testInstanceType+"=small", nodes[0].Annotations[partitionTestNICAnnotation])
}

// Labels written before the partition annotation existed still count, so an
// upgrade changes no expected count. Each node gets the annotation at its next
// reconcile, which is one extra write per node, and nothing after that.
func TestPartition_LabelWithoutAnnotationStillCountsAndGetsOne(t *testing.T) {
	nodes := ratchetFleet(4, 4)
	for _, node := range nodes {
		node.Labels[testGPUCountCurrentLabel] = "4"
		node.Labels[testGPUCountExpectedLabel] = "4"
	}

	nodes[3].Labels[testGPUCountExpectedLabel] = "6"

	manager := newTestManager(t, testGPUSourcesConfig())
	cache := manager.NewReconcileCache(nodes, nil)
	updated := make([]*corev1.Node, len(nodes))

	for i, node := range nodes {
		updated[i] = node.DeepCopy()
		require.True(t, cache.CalculateAndSetDeviceCountLabels(t.Context(), updated[i]), node.Name)
		require.Equal(t, "6", updated[i].Labels[testGPUCountExpectedLabel], node.Name)
		require.Equal(t, ratchetPlainPartition, updated[i].Annotations[ratchetPartitionAnnotation], node.Name)
	}

	require.Equal(t, nodes[3].Labels, updated[3].Labels, "the node that held 6 only gets its annotation")

	cache = manager.NewReconcileCache(updated, nil)
	for _, node := range updated {
		require.False(t, cache.CalculateAndSetDeviceCountLabels(t.Context(), node.DeepCopy()), node.Name)
	}
}

// The expected label and its annotations change in the same update, so the
// labeler writes them in one patch: when the label is first written, when the
// count rises, and when the node changes partition.
func TestPartition_LabelAndAnnotationsChangeInOneUpdate(t *testing.T) {
	for _, relearnID := range []string{"", ratchetRelearnID} {
		t.Run("relearnID="+relearnID, func(t *testing.T) {
			manager := newTestManager(t, relearnTestConfig(relearnID))
			node := ratchetFleet(1, 4)[0]

			reconcile := func(expected, partition string) {
				t.Helper()

				want := map[string]string{ratchetPartitionAnnotation: partition}
				if relearnID != "" {
					want[ratchetAnnotation] = relearnID + "/" + expected
				}

				updated := node.DeepCopy()
				require.True(t, manager.CalculateAndSetDeviceCountLabels(t.Context(), updated, []*corev1.Node{node}, nil))
				require.Equal(t, expected, updated.Labels[testGPUCountExpectedLabel])
				require.Equal(t, want, updated.Annotations)

				require.False(t, manager.CalculateAndSetDeviceCountLabels(
					t.Context(), updated.DeepCopy(), []*corev1.Node{updated}, nil))

				node = updated
			}

			reconcile("4", ratchetPlainPartition)

			ratchetSetGFD(node, ratchetPlainProduct, 6)
			reconcile("6", ratchetPlainPartition)

			ratchetSetGFD(node, ratchetMIGProduct, 8)
			reconcile("8", ratchetMIGPartition)
		})
	}
}

// A change between the sources of a class, such as a GPU Operator mode change,
// changes the partition key but not the hardware. As #2108 does, the expected
// count carries over: a node that comes back from the switch with a GPU
// missing is still flagged, and its label counts for its new peers.
func TestPartition_ModeChangeIsNotAPartitionChange(t *testing.T) {
	manager := newTestManager(t, testGPUSourcesConfig())
	moved := testNode("node-a", map[string]string{})
	draSlices := []*resourcev1.ResourceSlice{
		testGPUSlice("gpu", moved.Name, 1, 1, testGPUDevices(8, testH100ProductName)...),
	}

	require.True(t, manager.CalculateAndSetDeviceCountLabels(t.Context(), moved, []*corev1.Node{moved.DeepCopy()},
		func(*corev1.Node) []*resourcev1.ResourceSlice { return draSlices }))
	require.Equal(t, "8", moved.Labels[testGPUCountExpectedLabel])

	learnedIn := moved.Annotations[ratchetPartitionAnnotation]

	setGFD := func(node *corev1.Node, count string) {
		node.Labels[testGFDGPUCount] = count
		node.Labels[testGFDGPUProduct] = partitionTestH100Product
		node.Status.Allocatable = testDevicePluginGPUs(count)
	}

	// The node comes back in ClusterPolicy mode with a GPU missing, next to a
	// node that never ran in GPUCluster mode and is missing one too.
	setGFD(moved, "7")

	peer := testNode("node-b", map[string]string{})
	setGFD(peer, "7")

	nodes := ratchetSweep(t, manager, []*corev1.Node{moved, peer})

	require.Equal(t, "7", nodes[0].Labels[testGPUCountCurrentLabel])
	require.Equal(t, "8", nodes[0].Labels[testGPUCountExpectedLabel], "the count learned in GPUCluster mode carries over")
	require.Equal(t, "8", nodes[1].Labels[testGPUCountExpectedLabel], "and counts for the node's new peers")

	require.Equal(t, "source=dra|"+testInstanceType+"=|hardware="+testH100ProductName, learnedIn)
	require.Equal(t, "source=gfd|"+testInstanceType+"=|"+testGFDGPUProduct+"="+partitionTestH100Product,
		nodes[0].Annotations[ratchetPartitionAnnotation])
}

// A node whose grouping fails has no partition and learns only from itself.
// Its label still counts for it, and it keeps the partition it was learned in.
func TestPartition_NodeWithoutAPartitionKeepsItsLabelAndItsPartition(t *testing.T) {
	manager := newTestManager(t, testGPUSourcesConfig())
	learnedIn := "source=dra|" + testInstanceType + "=|hardware=" + testH100ProductName

	node := testNode("node-a", map[string]string{
		testGPUCountCurrentLabel:  "8",
		testGPUCountExpectedLabel: "8",
	})
	node.Annotations = map[string]string{ratchetPartitionAnnotation: learnedIn}

	// The DRA driver publishes seven GPUs without a product name, so the
	// grouping expression returns no values.
	devices := testGPUDevices(7, testH100ProductName)
	for i := range devices {
		delete(devices[i].Attributes, "productName")
	}

	slices := []*resourcev1.ResourceSlice{testGPUSlice("gpu", node.Name, 1, 1, devices...)}

	require.True(t, manager.CalculateAndSetDeviceCountLabels(t.Context(), node, []*corev1.Node{node.DeepCopy()},
		func(*corev1.Node) []*resourcev1.ResourceSlice { return slices }))
	require.Equal(t, "7", node.Labels[testGPUCountCurrentLabel])
	require.Equal(t, "8", node.Labels[testGPUCountExpectedLabel])
	require.Equal(t, map[string]string{ratchetPartitionAnnotation: learnedIn}, node.Annotations)
}

// Every enabled class writes the partition annotation, so its name must form a
// valid annotation key and two classes cannot share one.
func TestPartition_ConfigValidation(t *testing.T) {
	tests := []struct {
		name      string
		className string
		wantErr   string
	}{
		{"name with a space", "gpu class", "partition annotation"},
		{"name too long for an annotation", strings.Repeat("g", 48), "partition annotation"},
		{"longest name that fits", strings.Repeat("g", 47), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := testGPUSourcesConfig()
			config.Classes[0].Name = tt.className

			_, err := NewManager(config)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, tt.wantErr)
		})
	}

	t.Run("two classes with the same name", func(t *testing.T) {
		config := testGPUSourcesConfig()
		second := config.Classes[0]
		second.Labels = Labels{Current: "example.com/other.current", Expected: "example.com/other.expected"}
		config.Classes = append(config.Classes, second)

		_, err := NewManager(config)
		require.ErrorContains(t, err, "another class already uses partition annotation")
	})

	t.Run("a disabled class is not checked", func(t *testing.T) {
		config := testGPUSourcesConfig()
		disabled := config.Classes[0]
		disabled.Name = "gpu class"
		disabled.Enabled = false
		config.Classes = append(config.Classes, disabled)

		_, err := NewManager(config)
		require.NoError(t, err)
	})
}
