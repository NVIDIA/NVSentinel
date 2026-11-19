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

func relearnTestConfig(relearnID string) Config {
	class := testGPUSourcesClass()
	class.RelearnID = relearnID

	return Config{Enabled: true, Classes: []ClassConfig{class}}
}

// relearnUncertified counts the nodes whose expected label the relearn
// annotation does not certify under relearnID.
func relearnUncertified(nodes []*corev1.Node, relearnID string) int {
	uncertified := 0

	for _, node := range nodes {
		if node.Annotations[RelearnAnnotationKey("gpu")] != relearnID+"/"+node.Labels[testGPUCountExpectedLabel] {
			uncertified++
		}
	}

	return uncertified
}

func TestRelearn_AnnotationKeyMatchesTheLabels(t *testing.T) {
	require.Equal(t, ratchetAnnotation, RelearnAnnotationKey("gpu"))
	require.Equal(t, []string{ratchetAnnotation},
		newTestManager(t, relearnTestConfig(ratchetRelearnID)).RelearnAnnotationKeys())
	require.Empty(t, newTestManager(t, testGPUSourcesConfig()).RelearnAnnotationKeys())
}

// Without an ID the labeler records only the partition of each label.
func TestRelearn_WithoutAnIDOnlyThePartitionIsRecorded(t *testing.T) {
	nodes := ratchetSweep(t, newTestManager(t, relearnTestConfig("")), ratchetFleet(3, 4))

	for _, node := range nodes {
		require.Equal(t, map[string]string{ratchetPartitionAnnotation: ratchetPlainPartition}, node.Annotations,
			node.Name)
	}
}

// A relearn that stops halfway, event reconciles that read the old labels of
// other nodes, and a restart with the same ID all converge, and the old count
// never comes back.
func TestRelearn_StopsHalfwayAndResumes(t *testing.T) {
	nodes := ratchetRegroupedFleet(t)
	manager := newTestManager(t, relearnTestConfig(ratchetRelearnID))

	// The startup reconcile gets through 100 nodes, then the labeler stops.
	cache := manager.NewReconcileCache(nodes, nil)
	progressed := make([]*corev1.Node, len(nodes))

	for i, node := range nodes {
		progressed[i] = node.DeepCopy()
		if i < 100 {
			cache.CalculateAndSetDeviceCountLabels(t.Context(), progressed[i])
		}
	}

	nodes = progressed
	require.Equal(t, ratchetFleetSize-100, relearnUncertified(nodes, ratchetRelearnID))

	// Event reconciles of relearned and old nodes, each over 185 nodes still at 8.
	for _, i := range []int{5, 50, 99, 150, 286} {
		nodes[i] = ratchetReconcileOne(t, manager, nodes, i)
		require.Equal(t, "4", nodes[i].Labels[testGPUCountExpectedLabel], nodes[i].Name)
	}

	nodes = ratchetSweep(t, newTestManager(t, relearnTestConfig(ratchetRelearnID)), nodes)

	require.Zero(t, ratchetMismatches(nodes))
	require.Zero(t, relearnUncertified(nodes, ratchetRelearnID))
}

// The ID is the request. Keeping it set never relearns again, and learning
// goes on as ADR-043 describes: a higher count is learned and kept.
func TestRelearn_OncePerIDThenLearningResumes(t *testing.T) {
	manager := newTestManager(t, relearnTestConfig(ratchetRelearnID))
	nodes := ratchetSweep(t, manager, ratchetRegroupedFleet(t))
	require.Zero(t, ratchetMismatches(nodes))

	ratchetSetGFD(nodes[2], ratchetPlainProduct, 6)
	nodes = ratchetSweep(t, manager, nodes)
	require.Equal(t, "6", nodes[3].Labels[testGPUCountExpectedLabel])

	ratchetSetGFD(nodes[2], ratchetPlainProduct, 4)

	manager = newTestManager(t, relearnTestConfig(ratchetRelearnID))
	for range 3 {
		nodes = ratchetSweep(t, manager, nodes)
	}

	for _, node := range nodes[2:] {
		require.Equal(t, "6", node.Labels[testGPUCountExpectedLabel], node.Name)
		require.Equal(t, ratchetRelearnID+"/6", node.Annotations[ratchetAnnotation], node.Name)
	}
}

func TestRelearn_NodeMissingAGPUIsStillFlagged(t *testing.T) {
	nodes := ratchetRegroupedFleet(t)
	ratchetSetGFD(nodes[50], ratchetPlainProduct, 3)

	nodes = ratchetSweep(t, newTestManager(t, relearnTestConfig(ratchetRelearnID)), nodes)

	require.Equal(t, "3", nodes[50].Labels[testGPUCountCurrentLabel])
	require.Equal(t, "4", nodes[50].Labels[testGPUCountExpectedLabel])
	require.Equal(t, 1, ratchetMismatches(nodes))
}

// A lost GPU never lowers the expected count without a new ID: not on one
// node, not on every node of the partition, not across a restart with the ID
// already applied, and not without an ID. A new ID accepts what every node
// reports, which is the operator's decision.
func TestRelearn_LostGPUsAreNotAbsorbedWithoutANewID(t *testing.T) {
	manager := newTestManager(t, relearnTestConfig(ratchetRelearnID))
	nodes := ratchetSweep(t, manager, ratchetFleet(3, 8))
	require.Zero(t, ratchetMismatches(nodes))

	ratchetSetGFD(nodes[0], ratchetPlainProduct, 7)
	nodes = ratchetSweep(t, manager, nodes)
	require.Equal(t, "8", nodes[0].Labels[testGPUCountExpectedLabel])

	for _, node := range nodes {
		ratchetSetGFD(node, ratchetPlainProduct, 7)
	}

	nodes = ratchetSweep(t, newTestManager(t, relearnTestConfig(ratchetRelearnID)), nodes)
	require.Equal(t, 3, ratchetMismatches(nodes), "the applied ID does not relearn again")

	nodes = ratchetSweep(t, newTestManager(t, testGPUSourcesConfig()), nodes)
	require.Equal(t, 3, ratchetMismatches(nodes), "without an ID the ADR-043 rule holds")

	nodes = ratchetSweep(t, newTestManager(t, relearnTestConfig(ratchetRelearnID+"-2")), nodes)
	require.Zero(t, ratchetMismatches(nodes), "a new ID accepts what every node reports")
}

// A value written by someone else, such as an older labeler during a rollout
// or a hand edit, is not certified. The other nodes ignore it, the change
// triggers a reconcile of the node, and that reconcile repairs it.
func TestRelearn_ForeignWriteIsIgnoredAndRepaired(t *testing.T) {
	manager := newTestManager(t, relearnTestConfig(ratchetRelearnID))
	nodes := ratchetSweep(t, manager, ratchetRegroupedFleet(t))

	before := nodes[10].DeepCopy()
	nodes[10].Labels[testGPUCountExpectedLabel] = "8"

	require.True(t, manager.RelearnCertificationLost(before, nodes[10]))
	require.False(t, manager.NodeLabelsAffectDeviceCounts(before.Labels, nodes[10].Labels))

	require.Equal(t, "4", ratchetReconcileOne(t, manager, nodes, 11).Labels[testGPUCountExpectedLabel])

	nodes[10] = ratchetReconcileOne(t, manager, nodes, 10)
	require.Equal(t, "4", nodes[10].Labels[testGPUCountExpectedLabel])
	require.Equal(t, ratchetRelearnID+"/4", nodes[10].Annotations[ratchetAnnotation])
}

func TestRelearn_CertificationLostOnlyWhenSomeoneElseChangesIt(t *testing.T) {
	manager := newTestManager(t, relearnTestConfig(ratchetRelearnID))
	node := func(expected, annotation string) *corev1.Node {
		n := testNode("node-a", map[string]string{})
		if expected != "" {
			n.Labels[testGPUCountExpectedLabel] = expected
		}

		if annotation != "" {
			n.Annotations = map[string]string{ratchetAnnotation: annotation}
		}

		return n
	}

	certified4 := ratchetRelearnID + "/4"

	tests := []struct {
		name     string
		old, new *corev1.Node
		want     bool
	}{
		{"the relearn's own write", node("8", ""), node("4", certified4), false},
		{"the labeler's own rise", node("4", certified4), node("6", ratchetRelearnID+"/6"), false},
		{"another labeler's ID with the same value", node("4", certified4), node("4", "other-id/4"), false},
		{"label changed by someone else", node("4", certified4), node("8", certified4), true},
		{"label removed", node("4", certified4), node("", certified4), true},
		{"annotation removed", node("4", certified4), node("4", ""), true},
		{"label that was never certified", node("8", ""), node("9", ""), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, manager.RelearnCertificationLost(tt.old, tt.new))
		})
	}

	require.False(t, newTestManager(t, testGPUSourcesConfig()).RelearnCertificationLost(
		node("4", certified4), node("8", certified4)), "without an ID nothing is certified")
}

// A node without a count keeps its labels, as without an ID. Its old label is
// not certified, so it raises nothing, and the node is relearned when its
// count returns.
func TestRelearn_NodeWithoutACountKeepsItsLabelAndRaisesNothing(t *testing.T) {
	nodes := ratchetRegroupedFleet(t)
	nodes[20].Status.Allocatable = nil

	manager := newTestManager(t, relearnTestConfig(ratchetRelearnID))
	nodes = ratchetSweep(t, manager, nodes)

	require.Equal(t, "8", nodes[20].Labels[testGPUCountExpectedLabel])
	require.Equal(t, 1, relearnUncertified(nodes, ratchetRelearnID))
	require.Equal(t, 1, ratchetMismatches(nodes))
	require.Equal(t, "4", nodes[21].Labels[testGPUCountExpectedLabel])

	nodes[20].Status.Allocatable = testDevicePluginGPUs("4")
	nodes[20] = ratchetReconcileOne(t, manager, nodes, 20)

	require.Equal(t, "4", nodes[20].Labels[testGPUCountExpectedLabel])
	require.Zero(t, relearnUncertified(nodes, ratchetRelearnID))
}

func TestRelearn_OverridesStillWinAndAreCertified(t *testing.T) {
	config := relearnTestConfig(ratchetRelearnID)
	config.Classes[0].ExpectedCountOverrides = []ExpectedCountOverride{{
		MatchLabels: map[string]string{testGFDGPUProduct: ratchetMIGProduct},
		Count:       8,
	}}

	nodes := ratchetSweep(t, newTestManager(t, config), ratchetRegroupedFleet(t))

	require.Zero(t, ratchetMismatches(nodes))
	require.Equal(t, ratchetRelearnID+"/8", nodes[0].Annotations[ratchetAnnotation])
	require.Equal(t, ratchetRelearnID+"/4", nodes[2].Annotations[ratchetAnnotation])
}

// A clean MIG disable after a relearn needs no new ID. The 8 that the two
// nodes carry was certified in their MIG partition, and the partition
// annotation keeps it there: they relearn 4 under the same ID, the other nodes
// stay at 4, and a restart with the same ID changes nothing.
func TestRelearn_MIGDisableAfterARelearnNeedsNoNewID(t *testing.T) {
	nodes := ratchetSweep(t, newTestManager(t, relearnTestConfig(ratchetRelearnID)), ratchetRegroupedFleet(t))
	require.Zero(t, ratchetMismatches(nodes))
	require.Equal(t, ratchetRelearnID+"/8", nodes[0].Annotations[ratchetAnnotation])

	for _, node := range nodes[:2] {
		ratchetSetGFD(node, ratchetPlainProduct, 4)
	}

	nodes = ratchetSweep(t, newTestManager(t, relearnTestConfig(ratchetRelearnID)), nodes)
	require.Zero(t, ratchetMismatches(nodes))
	require.Zero(t, relearnUncertified(nodes, ratchetRelearnID))
	require.Equal(t, "4", nodes[0].Labels[testGPUCountExpectedLabel])

	cache := newTestManager(t, relearnTestConfig(ratchetRelearnID)).NewReconcileCache(nodes, nil)
	for _, node := range nodes {
		require.False(t, cache.CalculateAndSetDeviceCountLabels(t.Context(), node.DeepCopy()), node.Name)
	}
}

func TestRelearn_LoadConfigReadsTheID(t *testing.T) {
	config := ratchetRelearnConfig(t, ratchetRelearnID)

	require.Equal(t, ratchetRelearnID, config.Classes[0].RelearnID)
	require.Equal(t, []string{ratchetAnnotation}, newTestManager(t, config).RelearnAnnotationKeys())
}

func TestRelearn_ConfigValidation(t *testing.T) {
	tests := []struct {
		name      string
		className string
		relearnID string
		wantErr   string
	}{
		{"ID with a space", "gpu", "bad id", "relearnID"},
		{"ID with a slash", "gpu", "2026/11/05", "relearnID"},
		{"ID longer than 63 characters", "gpu", fmt.Sprintf("%064d", 1), "relearnID"},
		{"class name that cannot form an annotation", "gpu class", ratchetRelearnID, "relearn annotation"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := relearnTestConfig(tt.relearnID)
			config.Classes[0].Name = tt.className

			_, err := NewManager(config)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}

	t.Run("two classes with the same annotation", func(t *testing.T) {
		config := relearnTestConfig(ratchetRelearnID)
		second := config.Classes[0]
		second.Labels = Labels{Current: "example.com/other.current", Expected: "example.com/other.expected"}
		config.Classes = append(config.Classes, second)

		_, err := NewManager(config)
		require.ErrorContains(t, err, "another class already uses relearn annotation")
	})
}
