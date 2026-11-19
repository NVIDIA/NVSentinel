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
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// testGPUSettledExpression matches settledExpression of the GPU class in
	// the labeler chart values.yaml.
	testGPUSettledExpression = `
!('nvidia.com/mig.config.state' in node.metadata.labels) ||
node.metadata.labels['nvidia.com/mig.config.state'] == 'success'`

	// testMIGConfigState is the node label that the GPU Operator MIG manager
	// sets to pending, rebooting, success or failed.
	testMIGConfigState = "nvidia.com/mig.config.state"
)

// settledClassConfig loads a GPU class without sources and with the given
// settledExpression from TOML, as the labeler loads its configuration.
func settledClassConfig(t *testing.T, settledExpression string) Config {
	t.Helper()

	path := filepath.Join(t.TempDir(), "expected-device-counts.toml")
	require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, `enabled = true

[[classes]]
name = "gpu"
enabled = true
groupingLabels = [%q, %q]
currentExpression = '''int(node.metadata.labels['nvidia.com/gpu.count'])'''
settledExpression = '''%s'''

[classes.labels]
current = %q
expected = %q
`, testInstanceType, testGFDGPUProduct, settledExpression, testGPUCountCurrentLabel, testGPUCountExpectedLabel),
		0o600))

	config, err := LoadConfig(path)
	require.NoError(t, err)

	return config
}

// A node that the MIG manager marks pending raises nobody, even when a count
// source still reports 8 GPUs under the plain product. Under the GPU Operator
// the MIG manager stops GFD and the device plugin while it works, so the count
// here models a source that it does not pause. The two nodes report their
// current count and keep their expected count of 4, and no other node learns
// 8. Once the manager reports success the two nodes are in their MIG partition
// and learn 8 there.
func TestSettled_PendingNodeRaisesNobody(t *testing.T) {
	manager := newTestManager(t, ratchetRelearnConfig(t, ""))
	nodes := ratchetSweep(t, manager, ratchetFleet(ratchetFleetSize, 4))
	require.Zero(t, ratchetMismatches(nodes))

	for _, node := range nodes[:2] {
		node.Labels[testMIGConfigState] = "pending"
		ratchetSetGFD(node, ratchetPlainProduct, 8)
	}

	unsettled := skippedUpdates(t, "gpu", "unsettled")
	nodes = ratchetSweep(t, manager, nodes)
	unsettled = skippedUpdates(t, "gpu", "unsettled") - unsettled

	for _, node := range nodes[:2] {
		require.Equal(t, "8", node.Labels[testGPUCountCurrentLabel], node.Name)
		require.Equal(t, "4", node.Labels[testGPUCountExpectedLabel], node.Name)
		require.Equal(t, ratchetPlainPartition, node.Annotations[ratchetPartitionAnnotation], node.Name)
	}

	for _, node := range nodes[2:] {
		require.Equal(t, "4", node.Labels[testGPUCountExpectedLabel], node.Name)
	}

	require.Positive(t, unsettled)

	for _, node := range nodes[:2] {
		ratchetSetGFD(node, ratchetMIGProduct, 8)
		node.Labels[testMIGConfigState] = "success"
	}

	nodes = ratchetSweep(t, manager, nodes)

	require.Zero(t, ratchetMismatches(nodes))

	for _, node := range nodes[:2] {
		require.Equal(t, "8", node.Labels[testGPUCountExpectedLabel], node.Name)
		require.Equal(t, ratchetMIGPartition, node.Annotations[ratchetPartitionAnnotation], node.Name)
	}

	for _, node := range nodes[2:] {
		require.Equal(t, "4", node.Labels[testGPUCountExpectedLabel], node.Name)
	}
}

// While a node is not settled the labeler writes only its current label. Its
// expected label and both annotations stay as they are, although the relearn
// would rewrite them, and its count raises nobody. Once it settles it is
// relearned like any other node.
func TestSettled_UnsettledNodeKeepsItsExpectedLabelAndAnnotations(t *testing.T) {
	nodes := ratchetRegroupedFleet(t)
	before := nodes[50].DeepCopy()

	nodes[50].Labels[testMIGConfigState] = "pending"
	ratchetSetGFD(nodes[50], ratchetPlainProduct, 6)

	manager := newTestManager(t, ratchetRelearnConfig(t, ratchetRelearnID))
	nodes = ratchetSweep(t, manager, nodes)

	require.Equal(t, "6", nodes[50].Labels[testGPUCountCurrentLabel])
	require.Equal(t, "8", nodes[50].Labels[testGPUCountExpectedLabel])
	require.Equal(t, before.Annotations, nodes[50].Annotations)
	require.Equal(t, "4", nodes[51].Labels[testGPUCountExpectedLabel], "the 6 of the pending node raises nobody")
	require.Equal(t, 1, relearnUncertified(nodes, ratchetRelearnID))
	require.False(t, manager.NewReconcileCache(nodes, nil).CalculateAndSetDeviceCountLabels(
		t.Context(), nodes[50].DeepCopy()), "nothing changes while the node stays pending")

	nodes[50].Labels[testMIGConfigState] = "success"
	ratchetSetGFD(nodes[50], ratchetPlainProduct, 4)
	nodes[50] = ratchetReconcileOne(t, manager, nodes, 50)

	require.Equal(t, "4", nodes[50].Labels[testGPUCountExpectedLabel])
	require.Zero(t, relearnUncertified(nodes, ratchetRelearnID))
	require.Zero(t, ratchetMismatches(nodes))
}

// The default expression of the chart follows the MIG manager: a node without
// its state label, or with success, is settled, and every other state is not.
func TestSettled_ChartExpressionFollowsTheMIGManager(t *testing.T) {
	tests := []struct {
		state   string
		settled bool
	}{
		{"", true},
		{"success", true},
		{"pending", false},
		{"rebooting", false},
		{"failed", false},
	}

	manager := newTestManager(t, ratchetRelearnConfig(t, ""))

	for _, tt := range tests {
		t.Run("state="+tt.state, func(t *testing.T) {
			node := ratchetSweep(t, manager, ratchetFleet(1, 4))[0]
			if tt.state != "" {
				node.Labels[testMIGConfigState] = tt.state
			}

			ratchetSetGFD(node, ratchetPlainProduct, 6)

			require.True(t, manager.CalculateAndSetDeviceCountLabels(t.Context(), node, []*corev1.Node{node.DeepCopy()}, nil))
			require.Equal(t, "6", node.Labels[testGPUCountCurrentLabel])

			want := "4"
			if tt.settled {
				want = "6"
			}

			require.Equal(t, want, node.Labels[testGPUCountExpectedLabel])
		})
	}
}

// A settledExpression that fails leaves the labels of the node as they are, as
// a failed currentExpression does, and the node teaches nothing: neither its
// count nor its expected label of 8 raises another node.
func TestSettled_EvaluationErrorIsHandledLikeACurrentExpressionError(t *testing.T) {
	// The expression reads a label without checking that it exists, so it fails
	// on a node without the label.
	manager := newTestManager(t, settledClassConfig(t, "node.metadata.labels['example.com/settled'] == 'true'"))

	failing := testNode("node-a", map[string]string{
		testInstanceType:          "gb200",
		testGFDGPUProduct:         ratchetPlainProduct,
		testGFDGPUCount:           "8",
		testGPUCountExpectedLabel: "8",
	})
	peer := testNode("node-b", map[string]string{
		testInstanceType:      "gb200",
		testGFDGPUProduct:     ratchetPlainProduct,
		testGFDGPUCount:       "4",
		"example.com/settled": "true",
	})

	evaluationErrors := skippedUpdates(t, "gpu", "evaluation_error")
	cache := manager.NewReconcileCache([]*corev1.Node{failing, peer}, nil)

	updated := failing.DeepCopy()
	require.False(t, cache.CalculateAndSetDeviceCountLabels(t.Context(), updated))
	require.Equal(t, failing.Labels, updated.Labels)
	require.Empty(t, updated.Annotations)

	updated = peer.DeepCopy()
	require.True(t, cache.CalculateAndSetDeviceCountLabels(t.Context(), updated))
	require.Equal(t, "4", updated.Labels[testGPUCountExpectedLabel], "the failing node teaches nothing")

	require.Positive(t, skippedUpdates(t, "gpu", "evaluation_error")-evaluationErrors)
}

// The labeler watches what settledExpression reads: the ResourceSlices of a
// node, and changes to its allocatable or capacity resources.
func TestSettled_InputsOfTheExpressionAreWatched(t *testing.T) {
	require.True(t, newTestManager(t, settledClassConfig(t, "resourceSlices.size() > 0")).RequiresResourceSlices())
	require.False(t, newTestManager(t, settledClassConfig(t, "true")).RequiresResourceSlices())

	manager := newTestManager(t, settledClassConfig(t, "int(node.status.allocatable['example.com/ready']) > 0"))
	oldNode := testNode("node-a", nil)
	newNode := testNode("node-a", nil)
	newNode.Status.Allocatable = corev1.ResourceList{"example.com/ready": resource.MustParse("1")}

	require.True(t, manager.NodeResourcesAffectDeviceCounts(oldNode, newNode))
}

func TestSettled_ConfigValidation(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		wantErr    string
	}{
		{"returns an int", "int(node.metadata.labels['nvidia.com/gpu.count'])", "settledExpression must return bool"},
		{"returns a string", "'success'", "settledExpression must return bool"},
		{"does not compile", "node.metadata.labels[", "compile settledExpression"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewManager(settledClassConfig(t, tt.expression))
			require.ErrorContains(t, err, tt.wantErr)
		})
	}

	// A dyn result compiles, and a value that is not a bool fails when it is
	// evaluated, like any other failed settledExpression.
	t.Run("a dyn result is checked when it is evaluated", func(t *testing.T) {
		manager := newTestManager(t, settledClassConfig(t, "node.metadata.labels['example.com/settled']"))
		node := testNode("node-a", map[string]string{
			testInstanceType:      "gb200",
			testGFDGPUProduct:     ratchetPlainProduct,
			testGFDGPUCount:       "4",
			"example.com/settled": "yes",
		})

		require.False(t, manager.CalculateAndSetDeviceCountLabels(
			t.Context(), node, []*corev1.Node{node.DeepCopy()}, nil))
		require.NotContains(t, node.Labels, testGPUCountExpectedLabel)
	})
}

// settledGateConfig loads a class without sources, grouped by instance type,
// with the given currentExpression and settledExpression from TOML.
func settledGateConfig(t *testing.T, currentExpression, settledExpression string) Config {
	t.Helper()

	path := filepath.Join(t.TempDir(), "expected-device-counts.toml")
	require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, `enabled = true

[[classes]]
name = "gpu"
enabled = true
groupingLabels = [%q]
currentExpression = '''%s'''
settledExpression = '''%s'''

[classes.labels]
current = %q
expected = %q
`, testInstanceType, currentExpression, settledExpression, testGPUCountCurrentLabel, testGPUCountExpectedLabel),
		0o600))

	config, err := LoadConfig(path)
	require.NoError(t, err)

	return config
}

// A node that is not settled teaches nobody, also when it has no count. Its
// expected label of 8 does not raise the other node of its partition, whether
// its count source is missing or its count expression returns -1, and its own
// labels are left as they are. A settled node without a count still raises its
// partition through its label, as before. In a class with sources a node
// without a count has no partition, so it was never a peer.
func TestSettled_NodeWithoutACountTeachesNothingWhileNotSettled(t *testing.T) {
	const notReconfiguring = "!('example.com/reconfiguring' in node.metadata.labels)"

	labelCount := "'example.com/count' in node.metadata.labels ? int(node.metadata.labels['example.com/count']) : -1"

	tests := []struct {
		name      string
		config    func(*testing.T) Config
		settled   bool
		peerLabel string
		slices    map[string][]*resourcev1.ResourceSlice
		want      string
	}{
		{
			name:   "missing count source",
			config: func(t *testing.T) Config { return settledGateConfig(t, testDRAGPUCurrentExpression, notReconfiguring) },
			slices: map[string][]*resourcev1.ResourceSlice{
				"node-b": {testGPUSlice("gpu", "node-b", 1, 1, testGPUDevices(4, testH100ProductName)...)},
			},
			want: "4",
		},
		{
			name:      "count expression returns -1",
			config:    func(t *testing.T) Config { return settledGateConfig(t, labelCount, notReconfiguring) },
			peerLabel: "4",
			want:      "4",
		},
		{
			name:      "a settled node without a count still counts its label",
			config:    func(t *testing.T) Config { return settledGateConfig(t, labelCount, notReconfiguring) },
			settled:   true,
			peerLabel: "4",
			want:      "8",
		},
		{
			name: "a class with sources gives a node without a count no partition",
			config: func(t *testing.T) Config {
				config := ratchetRelearnConfig(t, "")
				config.Classes[0].SettledExpression = notReconfiguring

				return config
			},
			want: "4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newTestManager(t, tt.config(t))

			unsettled := testNode("node-a", map[string]string{
				testInstanceType:          "gb200",
				testGFDGPUProduct:         ratchetPlainProduct,
				testGPUCountExpectedLabel: "8",
			})
			if !tt.settled {
				unsettled.Labels["example.com/reconfiguring"] = "true"
			}

			peer := testNode("node-b", map[string]string{testInstanceType: "gb200"})
			if tt.peerLabel != "" {
				peer.Labels["example.com/count"] = tt.peerLabel
			}

			// The peer of the class with sources counts through the gfd source.
			ratchetSetGFD(peer, ratchetPlainProduct, 4)

			cache := manager.NewReconcileCache([]*corev1.Node{unsettled, peer},
				func(node *corev1.Node) []*resourcev1.ResourceSlice { return tt.slices[node.Name] })

			updated := unsettled.DeepCopy()
			require.False(t, cache.CalculateAndSetDeviceCountLabels(t.Context(), updated))
			require.Equal(t, unsettled.Labels, updated.Labels, "the node without a count keeps its labels")

			updated = peer.DeepCopy()
			cache.CalculateAndSetDeviceCountLabels(t.Context(), updated)
			require.Equal(t, tt.want, updated.Labels[testGPUCountExpectedLabel])
		})
	}
}
