// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/config"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestTaintAndCordonNodeAndSetAnnotations(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	nodeName := "test-node"

	// Create a test node
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: map[string]string{},
		},
		Spec: v1.NodeSpec{},
	}
	_, err := clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test node: %v", err)
	}

	k8sClient := &FaultQuarantineClient{
		clientset: clientset,
	}

	taints := []config.Taint{
		{
			Key:    "test-key",
			Value:  "test-value",
			Effect: "NoSchedule",
		},
	}
	annotations := map[string]string{
		"test-annotation": "test-value",
	}

	cordonedByLabelKey = "cordon-by"
	cordonedReasonLabelKey = "cordon-reason"
	cordonedTimestampLabelKey = "cordon-timestamp"

	labelsMap := map[string]string{
		cordonedByLabelKey:        common.ServiceName,
		cordonedReasonLabelKey:    "gpu-error",
		cordonedTimestampLabelKey: time.Now().UTC().Format("2006-01-02T15-04-05Z"),
	}
	err = k8sClient.TaintAndCordonNodeAndSetAnnotations(ctx, nodeName, taints, true, annotations, labelsMap)
	if err != nil {
		t.Fatalf("TaintAndCordonNodeAndSetAnnotations failed: %v", err)
	}

	updatedNode, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}

	// Check taints
	if len(updatedNode.Spec.Taints) != 1 {
		t.Errorf("Expected 1 taint, got %d", len(updatedNode.Spec.Taints))
	}
	if updatedNode.Spec.Taints[0].Key != "test-key" {
		t.Errorf("Unexpected taint key: %s", updatedNode.Spec.Taints[0].Key)
	}

	// Check cordon
	if !updatedNode.Spec.Unschedulable {
		t.Errorf("Node should be cordoned")
	}
	if len(updatedNode.Labels) != 3 || updatedNode.Labels[cordonedByLabelKey] != common.ServiceName || updatedNode.Labels[cordonedReasonLabelKey] != "gpu-error" || updatedNode.Labels[cordonedTimestampLabelKey] == "" {
		t.Errorf("Cordoned labels are not applied on node")
	}

	// Check annotations
	if val, ok := updatedNode.Annotations["test-annotation"]; !ok || val != "test-value" {
		t.Errorf("Annotation not set correctly")
	}
}

func TestUnTaintAndUnCordonNodeAndRemoveAnnotations(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	nodeName := "test-node"

	cordonedByLabelKey = "cordon-by"
	cordonedReasonLabelKey = "cordon-reason"
	cordonedTimestampLabelKey = "cordon-timestamp"
	uncordonedByLabelKey = "uncordon-by"
	uncordonedReasonLabelkey = "uncordon-reason"
	uncordonedTimestampLabelKey = "uncordon-timestamp"

	// Create a test node with taints, cordon, and annotations
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"test-annotation": "test-value",
			},
			Labels: map[string]string{
				cordonedByLabelKey:        common.ServiceName,
				cordonedReasonLabelKey:    "gpu-error",
				cordonedTimestampLabelKey: time.Now().UTC().Format("2006-01-02T15-04-05Z"),
			},
		},
		Spec: v1.NodeSpec{
			Unschedulable: true,
			Taints: []v1.Taint{
				{
					Key:    "test-key",
					Value:  "test-value",
					Effect: v1.TaintEffect("NoSchedule"),
				},
			},
		},
	}
	_, err := clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test node: %v", err)
	}

	k8sClient := &FaultQuarantineClient{
		clientset: clientset,
	}

	taints := []config.Taint{
		{
			Key:    "test-key",
			Value:  "test-value",
			Effect: "NoSchedule",
		},
	}
	annotationKeys := []string{"test-annotation"}

	labelsMap := map[string]string{
		uncordonedByLabelKey:        common.ServiceName,
		uncordonedTimestampLabelKey: time.Now().UTC().Format("2006-01-02T15-04-05Z"),
	}

	err = k8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(ctx, nodeName, taints, true, annotationKeys, []string{cordonedByLabelKey, cordonedReasonLabelKey, cordonedTimestampLabelKey}, labelsMap)
	if err != nil {
		t.Fatalf("UnTaintAndUnCordonNodeAndRemoveAnnotations failed: %v", err)
	}

	updatedNode, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}

	if len(updatedNode.Spec.Taints) != 0 {
		t.Errorf("Expected 0 taints, got %d", len(updatedNode.Spec.Taints))
	}

	if updatedNode.Spec.Unschedulable {
		t.Errorf("Node should be uncordoned")
	}

	if _, ok := updatedNode.Annotations["test-annotation"]; ok {
		t.Errorf("Annotation should be removed")
	}

	_, exists1 := updatedNode.Labels[cordonedByLabelKey]
	_, exists2 := updatedNode.Labels[cordonedReasonLabelKey]
	_, exists3 := updatedNode.Labels[cordonedTimestampLabelKey]

	if exists1 || exists2 || exists3 {
		t.Errorf("Expected cordoned labels to be removed from node")
	}

	if len(updatedNode.Labels) != 3 || updatedNode.Labels[uncordonedByLabelKey] != common.ServiceName || updatedNode.Labels[uncordonedReasonLabelkey] != "gpu-error-removed" || updatedNode.Labels[uncordonedTimestampLabelKey] == "" {
		t.Errorf("Expected uncordoned lables to be applied on node")
	}
}

func TestGetNodeAnnotations(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	nodeName := "test-node"

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"test-annotation": "test-value",
			},
		},
		Spec: v1.NodeSpec{},
	}
	_, err := clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test node: %v", err)
	}

	k8sClient := &FaultQuarantineClient{
		clientset: clientset,
	}

	annotations, err := k8sClient.GetNodeAnnotations(ctx, nodeName)
	if err != nil {
		t.Fatalf("GetNodeAnnotations failed: %v", err)
	}

	if val, exists := annotations["test-annotation"]; !exists {
		t.Errorf("Expected 'test-annotation' to exist")
	} else if val != "test-value" {
		t.Errorf("Expected 'test-value', got '%s'", val)
	}

	if val, exists := annotations["non-existent"]; exists {
		t.Errorf("Expected 'non-existent' to not exist, but got '%s'", val)
	}

	// ensure that modifying the returned map does not affect the original node annotations
	annotations["new-annotation"] = "new-value"
	originalAnnotations, err := k8sClient.GetNodeAnnotations(ctx, nodeName)
	if err != nil {
		t.Fatalf("GetNodeAnnotations failed: %v", err)
	}
	if _, exists := originalAnnotations["new-annotation"]; exists {
		t.Errorf("Modifying the returned annotations map should not affect the original node annotations")
	}
}

func TestGetNodesWithAnnotation(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	node1 := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Annotations: map[string]string{
				"test-annotation": "value1",
			},
		},
		Spec: v1.NodeSpec{},
	}
	node2 := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node2",
			Annotations: map[string]string{
				"test-annotation": "value2",
			},
		},
		Spec: v1.NodeSpec{},
	}
	node3 := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node3",
			Annotations: map[string]string{
				"other-annotation": "value3",
			},
		},
		Spec: v1.NodeSpec{},
	}
	clientset.CoreV1().Nodes().Create(ctx, node1, metav1.CreateOptions{})
	clientset.CoreV1().Nodes().Create(ctx, node2, metav1.CreateOptions{})
	clientset.CoreV1().Nodes().Create(ctx, node3, metav1.CreateOptions{})

	k8sClient := &FaultQuarantineClient{
		clientset: clientset,
	}

	nodes, err := k8sClient.GetNodesWithAnnotation(ctx, "test-annotation")
	if err != nil {
		t.Fatalf("GetNodesWithAnnotation failed: %v", err)
	}

	if len(nodes) != 2 {
		t.Errorf("Expected 2 nodes, got %d", len(nodes))
	}

	// Check if nodes are correct
	expectedNodes := map[string]bool{"node1": true, "node2": true}
	for _, nodeName := range nodes {
		if !expectedNodes[nodeName] {
			t.Errorf("Unexpected node: %s", nodeName)
		}
	}
}

func TestTaintAndCordonNode_NodeNotFound(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	k8sClient := &FaultQuarantineClient{clientset: clientset}

	err := k8sClient.TaintAndCordonNodeAndSetAnnotations(ctx, "non-existent-node", nil, false, nil, map[string]string{})
	if err == nil {
		t.Errorf("Expected error when node does not exist, got nil")
	}
}

func TestTaintAndCordonNode_NoChanges(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	nodeName := "no-change-node"
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: map[string]string{},
		},
	}
	clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})

	k8sClient := &FaultQuarantineClient{clientset: clientset}

	err := k8sClient.TaintAndCordonNodeAndSetAnnotations(ctx, nodeName, nil, false, nil, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	updatedNode, _ := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if len(updatedNode.Spec.Taints) != 0 {
		t.Errorf("Expected no taints, got %d", len(updatedNode.Spec.Taints))
	}
	if updatedNode.Spec.Unschedulable {
		t.Errorf("Expected node to remain schedulable")
	}
	if len(updatedNode.Annotations) != 0 {
		t.Errorf("Expected no annotations, got %v", updatedNode.Annotations)
	}
}

func TestUnTaintAndUnCordonNode_NoChanges(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	nodeName := "no-change-untaint-node"
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: map[string]string{},
		},
		Spec: v1.NodeSpec{Unschedulable: false},
	}
	clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})

	k8sClient := &FaultQuarantineClient{clientset: clientset}

	// No taints to remove, node is already uncordoned, and no annotations to remove
	err := k8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(ctx, nodeName, nil, false, nil, []string{}, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, _ := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if len(updatedNode.Spec.Taints) != 0 {
		t.Errorf("Expected no taints, got %d", len(updatedNode.Spec.Taints))
	}
	if updatedNode.Spec.Unschedulable {
		t.Errorf("Expected node to remain uncordoned")
	}
	if len(updatedNode.Annotations) != 0 {
		t.Errorf("Expected no annotations, got %v", updatedNode.Annotations)
	}
}

func TestUnTaintAndUnCordonNode_PartialTaintRemoval(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	nodeName := "partial-taint-removal-node"
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: map[string]string{},
		},
		Spec: v1.NodeSpec{
			Unschedulable: true,
			Taints: []v1.Taint{
				{Key: "taint1", Value: "val1", Effect: v1.TaintEffectNoSchedule},
				{Key: "taint2", Value: "val2", Effect: v1.TaintEffectPreferNoSchedule},
			},
		},
	}
	clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})

	k8sClient := &FaultQuarantineClient{clientset: clientset}

	taintsToRemove := []config.Taint{{Key: "taint1", Value: "val1", Effect: "NoSchedule"}}
	err := k8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(ctx, nodeName, taintsToRemove, false, nil, []string{}, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, _ := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if len(updatedNode.Spec.Taints) != 1 {
		t.Errorf("Expected 1 taint remaining, got %d", len(updatedNode.Spec.Taints))
	}
	if updatedNode.Spec.Taints[0].Key != "taint2" {
		t.Errorf("Expected taint2 to remain, got %s", updatedNode.Spec.Taints[0].Key)
	}
}

func TestUnTaintAndUnCordonNode_PartialAnnotationRemoval(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	cordonedByLabelKey = "cordon-by"
	cordonedReasonLabelKey = "cordon-reason"
	cordonedTimestampLabelKey = "cordon-timestamp"
	uncordonedByLabelKey = "uncordon-by"
	uncordonedReasonLabelkey = "uncordon-reason"
	uncordonedTimestampLabelKey = "uncordon-timestamp"

	nodeName := "partial-annotation-removal-node"
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"annotation1": "val1",
				"annotation2": "val2",
			},
			Labels: map[string]string{
				cordonedByLabelKey:        common.ServiceName,
				cordonedReasonLabelKey:    "gpu-error",
				cordonedTimestampLabelKey: time.Now().UTC().Format("2006-01-02T15-04-05Z"),
			},
		},
		Spec: v1.NodeSpec{
			Unschedulable: true,
		},
	}
	clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})

	k8sClient := &FaultQuarantineClient{clientset: clientset}

	annotationsToRemove := []string{"annotation1"}
	labelsMap := map[string]string{
		uncordonedByLabelKey:        common.ServiceName,
		uncordonedTimestampLabelKey: time.Now().UTC().Format("2006-01-02T15-04-05Z"),
	}
	err := k8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(ctx, nodeName, nil, true, annotationsToRemove, []string{cordonedByLabelKey, cordonedReasonLabelKey, cordonedTimestampLabelKey}, labelsMap)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, _ := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if updatedNode.Annotations["annotation1"] != "" {
		t.Errorf("Expected annotation1 to be removed")
	}
	if updatedNode.Annotations["annotation2"] != "val2" {
		t.Errorf("Expected annotation2 to remain")
	}
	if updatedNode.Spec.Unschedulable {
		t.Errorf("Expected node to be uncordoned")
	}
}

func TestTaintAndCordonNode_AlreadyTaintedCOrdonned(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	nodeName := "already-tainted-cordoned-node"

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: map[string]string{},
		},
		Spec: v1.NodeSpec{
			Unschedulable: true,
			Taints: []v1.Taint{
				{Key: "test-key", Value: "test-value", Effect: v1.TaintEffectNoSchedule},
			},
		},
	}
	clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	k8sClient := &FaultQuarantineClient{clientset: clientset}

	taints := []config.Taint{{Key: "test-key", Value: "test-value", Effect: "NoSchedule"}}
	err := k8sClient.TaintAndCordonNodeAndSetAnnotations(ctx, nodeName, taints, true, nil, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, _ := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	// Should remain unchanged
	if len(updatedNode.Spec.Taints) != 1 {
		t.Errorf("Expected 1 taint, got %d", len(updatedNode.Spec.Taints))
	}
	if !updatedNode.Spec.Unschedulable {
		t.Errorf("Node should remain cordoned")
	}
}

func TestUnTaintAndUnCordonNode_AlreadyUntaintedUncordoned(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	nodeName := "already-untainted-uncordoned-node"

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: map[string]string{},
		},
		Spec: v1.NodeSpec{Unschedulable: false},
	}
	clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	k8sClient := &FaultQuarantineClient{clientset: clientset}

	err := k8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(ctx, nodeName, nil, true, nil, []string{}, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, _ := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if len(updatedNode.Spec.Taints) != 0 {
		t.Errorf("Expected no taints, got %d", len(updatedNode.Spec.Taints))
	}
	if updatedNode.Spec.Unschedulable {
		t.Errorf("Node should remain uncordoned")
	}
}

func TestTaintAndCordonNode_InvalidTaintEffect(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	nodeName := "invalid-effect-node"

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: map[string]string{},
		},
	}
	clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	k8sClient := &FaultQuarantineClient{clientset: clientset}

	// Provide an invalid effect
	taints := []config.Taint{{Key: "weird-key", Value: "weird-value", Effect: "SomeInvalidEffect"}}
	err := k8sClient.TaintAndCordonNodeAndSetAnnotations(ctx, nodeName, taints, false, nil, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error adding invalid effect taint, got %v", err)
	}

	updatedNode, _ := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if len(updatedNode.Spec.Taints) != 1 {
		t.Errorf("Expected 1 taint, got %d", len(updatedNode.Spec.Taints))
	}
	if string(updatedNode.Spec.Taints[0].Effect) != "SomeInvalidEffect" {
		t.Errorf("Expected effect 'SomeInvalidEffect', got '%s'", updatedNode.Spec.Taints[0].Effect)
	}
}

func TestTaintAndCordonNode_OverwriteAnnotation(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	nodeName := "overwrite-annotation-node"

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: map[string]string{"existing-key": "old-value"},
		},
	}
	clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	k8sClient := &FaultQuarantineClient{clientset: clientset}

	annotations := map[string]string{"existing-key": "new-value"}
	err := k8sClient.TaintAndCordonNodeAndSetAnnotations(ctx, nodeName, nil, false, annotations, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, _ := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if updatedNode.Annotations["existing-key"] != "new-value" {
		t.Errorf("Annotation value was not updated correctly")
	}
}

func TestUnTaintAndUnCordonNode_NonExistentTaintRemoval(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	nodeName := "non-existent-taint-removal-node"

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: map[string]string{},
		},
		Spec: v1.NodeSpec{
			Taints: []v1.Taint{
				{Key: "taint1", Value: "val1", Effect: v1.TaintEffectNoSchedule},
			},
		},
	}
	clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	k8sClient := &FaultQuarantineClient{clientset: clientset}

	// Attempt to remove a taint that doesn't exist
	taintsToRemove := []config.Taint{{Key: "taint-nonexistent", Value: "valX", Effect: "NoSchedule"}}
	err := k8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(ctx, nodeName, taintsToRemove, false, nil, []string{}, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, _ := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	// Original taint should remain as we tried to remove a non-existent taint
	if len(updatedNode.Spec.Taints) != 1 {
		t.Errorf("Expected 1 taint to remain, got %d", len(updatedNode.Spec.Taints))
	}
}

func TestUnTaintAndUnCordonNode_NonExistentAnnotationRemoval(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	nodeName := "non-existent-annotation-removal-node"

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"annotation1": "val1",
			},
		},
		Spec: v1.NodeSpec{},
	}
	clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	k8sClient := &FaultQuarantineClient{clientset: clientset}

	// Attempt to remove an annotation that doesn't exist
	annotationsToRemove := []string{"nonexistent-annotation"}
	err := k8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(ctx, nodeName, nil, false, annotationsToRemove, []string{}, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, _ := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	// Original annotation should remain
	if updatedNode.Annotations["annotation1"] != "val1" {
		t.Errorf("Non-existent annotation removal should not affect existing annotations")
	}
}

func TestTaintAndCordonNode_EmptyTaintKeyOrValue(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	nodeName := "empty-taint-key-value-node"

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: map[string]string{},
		},
	}
	clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	k8sClient := &FaultQuarantineClient{clientset: clientset}

	// Taint with empty key and value
	taints := []config.Taint{
		{Key: "", Value: "", Effect: "NoSchedule"},
	}
	err := k8sClient.TaintAndCordonNodeAndSetAnnotations(ctx, nodeName, taints, false, nil, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, _ := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if len(updatedNode.Spec.Taints) != 1 {
		t.Errorf("Expected 1 taint, got %d", len(updatedNode.Spec.Taints))
	}
	if updatedNode.Spec.Taints[0].Key != "" || updatedNode.Spec.Taints[0].Value != "" {
		t.Errorf("Expected empty key and value taint")
	}
}

func TestTaintAndCordonNode_EmptyAnnotationKey(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	nodeName := "empty-annotation-key-node"

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: map[string]string{},
		},
	}
	clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	k8sClient := &FaultQuarantineClient{clientset: clientset}

	annotations := map[string]string{
		"": "empty-key-value",
	}
	err := k8sClient.TaintAndCordonNodeAndSetAnnotations(ctx, nodeName, nil, false, annotations, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, _ := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if updatedNode.Annotations[""] != "empty-key-value" {
		t.Errorf("Expected empty key annotation to be set")
	}
}

func TestGetNodesWithAnnotation_NoMatches(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	// Create a node without the target annotation
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node-without-annotation",
			Annotations: map[string]string{"some-other-annotation": "value"},
		},
	}
	clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})

	k8sClient := &FaultQuarantineClient{clientset: clientset}

	nodes, err := k8sClient.GetNodesWithAnnotation(ctx, "non-existent-annotation")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if len(nodes) != 0 {
		t.Errorf("Expected no nodes, got %d", len(nodes))
	}
}

func TestGetNodesWithAnnotation_EmptyAnnotationKey(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node-with-empty-key-annotation",
			Annotations: map[string]string{"": "empty-key-annotation"},
		},
	}
	clientset.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	k8sClient := &FaultQuarantineClient{clientset: clientset}

	nodes, err := k8sClient.GetNodesWithAnnotation(ctx, "")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if len(nodes) != 1 {
		t.Errorf("Expected 1 node with empty key annotation, got %d", len(nodes))
	}
	if nodes[0] != "node-with-empty-key-annotation" {
		t.Errorf("Unexpected node returned: %s", nodes[0])
	}
}

func TestTaintAndCordonNode_NonExistentNode(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	k8sClient := &FaultQuarantineClient{clientset: clientset}

	// Attempt to taint a node that doesn't exist
	err := k8sClient.TaintAndCordonNodeAndSetAnnotations(ctx, "no-such-node", nil, true, nil, map[string]string{})
	if err == nil {
		t.Errorf("Expected error for non-existent node, got nil")
	}
}

func TestUnTaintAndUnCordonNode_NonExistentNode(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	k8sClient := &FaultQuarantineClient{clientset: clientset}

	// Attempt to untaint a node that doesn't exist
	err := k8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(ctx, "no-such-node", nil, true, nil, []string{}, map[string]string{})
	if err == nil {
		t.Errorf("Expected error for non-existent node, got nil")
	}
}

func TestGetNodeAnnotations_NonExistentNode(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	k8sClient := &FaultQuarantineClient{clientset: clientset}

	_, err := k8sClient.GetNodeAnnotations(ctx, "no-such-node")
	if err == nil {
		t.Errorf("Expected error for non-existent node, got nil")
	}
}
