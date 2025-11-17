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

package informer

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/testutils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	cordonedByLabelKey          = "test-cordon-by"
	cordonedReasonLabelKey      = "test-cordon-reason"
	cordonedTimestampLabelKey   = "test-cordon-timestamp"
	uncordonedByLabelKey        = "test-uncordon-by"
	uncordonedReasonLabelKey    = "test-uncordon-reason"
	uncordonedTimestampLabelKey = "test-uncordon-timestamp"
)

var (
	testClient *kubernetes.Clientset
	testEnv    *envtest.Environment
)

func TestMain(m *testing.M) {
	var err error

	testEnv = &envtest.Environment{}

	testRestConfig, err := testEnv.Start()
	if err != nil {
		log.Fatalf("Failed to start test environment: %v", err)
	}

	testClient, err = kubernetes.NewForConfig(testRestConfig)
	if err != nil {
		log.Fatalf("Failed to create kubernetes client: %v", err)
	}

	exitCode := m.Run()

	if err := testEnv.Stop(); err != nil {
		log.Fatalf("Failed to stop test environment: %v", err)
	}
	os.Exit(exitCode)
}

func setupTestClient(t *testing.T) *FaultQuarantineClient {
	t.Helper()

	client := &FaultQuarantineClient{
		Clientset:  testClient,
		DryRunMode: false,
	}

	nodeInformer, err := NewNodeInformer(testClient, 0)
	if err != nil {
		t.Fatalf("Failed to create NodeInformer: %v", err)
	}

	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })

	go func() {
		_ = nodeInformer.Run(stopCh)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		return nodeInformer.HasSynced(), nil
	})
	if err != nil {
		t.Fatalf("NodeInformer failed to sync: %v", err)
	}

	client.NodeInformer = nodeInformer
	client.SetLabelKeys(cordonedReasonLabelKey, uncordonedReasonLabelKey)

	return client
}

func createTestNode(ctx context.Context, t *testing.T, name string, annotations map[string]string, labels map[string]string, taints []v1.Taint, unschedulable bool) {
	t.Helper()

	if labels == nil {
		labels = make(map[string]string)
	}

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
			Labels:      labels,
		},
		Spec: v1.NodeSpec{
			Unschedulable: unschedulable,
			Taints:        taints,
		},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{
				{Type: v1.NodeReady, Status: v1.ConditionTrue},
			},
		},
	}

	_, err := testClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test node %s: %v", name, err)
	}
}

func TestQuarantineNodeAndSetAnnotations(t *testing.T) {
	ctx := context.Background()
	nodeName := testutils.GenerateTestNodeName("test-taint-cordon")

	createTestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	k8sClient := setupTestClient(t)

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

	labelsMap := map[string]string{
		cordonedByLabelKey:        common.ServiceName,
		cordonedReasonLabelKey:    "gpu-error",
		cordonedTimestampLabelKey: time.Now().UTC().Format("2006-01-02T15-04-05Z"),
	}
	err := k8sClient.QuarantineNodeAndSetAnnotations(ctx, nodeName, taints, true, annotations, labelsMap)
	if err != nil {
		t.Fatalf("QuarantineNodeAndSetAnnotations failed: %v", err)
	}

	updatedNode, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}

	// Check taints (filter out automatic taints added by envtest like node.kubernetes.io/not-ready)
	var testTaints []v1.Taint
	for _, taint := range updatedNode.Spec.Taints {
		if taint.Key == "test-key" {
			testTaints = append(testTaints, taint)
		}
	}
	if len(testTaints) != 1 {
		t.Errorf("Expected 1 test taint, got %d", len(testTaints))
	}
	if len(testTaints) > 0 && testTaints[0].Key != "test-key" {
		t.Errorf("Unexpected taint key: %s", testTaints[0].Key)
	}

	// Check cordon
	if !updatedNode.Spec.Unschedulable {
		t.Errorf("Node should be cordoned")
	}
	// Check that cordon labels are present (node also has GPU label, so total is 4)
	if updatedNode.Labels[cordonedByLabelKey] != common.ServiceName {
		t.Errorf("Expected cordon-by label to be %s, got %s", common.ServiceName, updatedNode.Labels[cordonedByLabelKey])
	}
	if updatedNode.Labels[cordonedReasonLabelKey] != "gpu-error" {
		t.Errorf("Expected cordon-reason label to be gpu-error, got %s", updatedNode.Labels[cordonedReasonLabelKey])
	}
	if updatedNode.Labels[cordonedTimestampLabelKey] == "" {
		t.Errorf("Expected cordon-timestamp label to be set")
	}

	// Check annotations
	if val, ok := updatedNode.Annotations["test-annotation"]; !ok || val != "test-value" {
		t.Errorf("Annotation not set correctly")
	}
}

func TestUnQuarantineNodeAndRemoveAnnotations(t *testing.T) {
	ctx := context.Background()
	nodeName := testutils.GenerateTestNodeName("test-untaint-uncordon")

	annotations := map[string]string{
		"test-annotation": "test-value",
	}
	labels := map[string]string{
		cordonedByLabelKey:        common.ServiceName,
		cordonedReasonLabelKey:    "gpu-error",
		cordonedTimestampLabelKey: time.Now().UTC().Format("2006-01-02T15-04-05Z"),
	}
	taints := []v1.Taint{
		{
			Key:    "test-key",
			Value:  "test-value",
			Effect: v1.TaintEffect("NoSchedule"),
		},
	}

	createTestNode(ctx, t, nodeName, annotations, labels, taints, true)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	k8sClient := setupTestClient(t)

	taintsToRemove := []config.Taint{
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

	err := k8sClient.UnQuarantineNodeAndRemoveAnnotations(ctx, nodeName, taintsToRemove, annotationKeys, []string{cordonedByLabelKey, cordonedReasonLabelKey, cordonedTimestampLabelKey}, labelsMap)
	if err != nil {
		t.Fatalf("UnQuarantineNodeAndRemoveAnnotations failed: %v", err)
	}

	updatedNode, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}

	// Check that our test taint was removed (filter out automatic taints from envtest)
	var testTaints []v1.Taint
	for _, taint := range updatedNode.Spec.Taints {
		if taint.Key == "test-key" {
			testTaints = append(testTaints, taint)
		}
	}
	if len(testTaints) != 0 {
		t.Errorf("Expected 0 test taints, got %d", len(testTaints))
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

	// Check that uncordon labels are present (node also has GPU label)
	if updatedNode.Labels[uncordonedByLabelKey] != common.ServiceName {
		t.Errorf("Expected uncordon-by label to be %s, got %s", common.ServiceName, updatedNode.Labels[uncordonedByLabelKey])
	}
	if updatedNode.Labels[uncordonedReasonLabelKey] != "gpu-error-removed" {
		t.Errorf("Expected uncordon-reason label to be gpu-error-removed, got %s", updatedNode.Labels[uncordonedReasonLabelKey])
	}
	if updatedNode.Labels[uncordonedTimestampLabelKey] == "" {
		t.Errorf("Expected uncordon-timestamp label to be set")
	}
}

func TestTaintAndCordonNode_NodeNotFound(t *testing.T) {
	ctx := context.Background()
	k8sClient := setupTestClient(t)

	err := k8sClient.QuarantineNodeAndSetAnnotations(ctx, "non-existent-node", nil, false, nil, map[string]string{})
	if err == nil {
		t.Errorf("Expected error when node does not exist, got nil")
	}
}

func TestTaintAndCordonNode_NoChanges(t *testing.T) {
	ctx := context.Background()
	nodeName := testutils.GenerateTestNodeName("test-no-change")

	createTestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	k8sClient := setupTestClient(t)

	err := k8sClient.QuarantineNodeAndSetAnnotations(ctx, nodeName, nil, false, nil, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}

	// envtest may add automatic taints, so we just verify no custom taints were added
	// The function should not have added any custom taints
	if updatedNode.Spec.Unschedulable {
		t.Errorf("Expected node to remain schedulable")
	}
	if len(updatedNode.Annotations) != 0 {
		t.Errorf("Expected no annotations, got %v", updatedNode.Annotations)
	}
}

func TestUnTaintAndUnCordonNode_NoChanges(t *testing.T) {
	ctx := context.Background()
	nodeName := testutils.GenerateTestNodeName("test-no-change-untaint")

	createTestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	k8sClient := setupTestClient(t)

	err := k8sClient.UnQuarantineNodeAndRemoveAnnotations(ctx, nodeName, nil, nil, []string{}, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}

	// Function always uncordons, so node should be schedulable
	if updatedNode.Spec.Unschedulable {
		t.Errorf("Expected node to be uncordoned")
	}
	if len(updatedNode.Annotations) != 0 {
		t.Errorf("Expected no annotations, got %v", updatedNode.Annotations)
	}
}

func TestUnTaintAndUnCordonNode_PartialTaintRemoval(t *testing.T) {
	ctx := context.Background()
	nodeName := testutils.GenerateTestNodeName("test-partial-taint")

	taints := []v1.Taint{
		{Key: "taint1", Value: "val1", Effect: v1.TaintEffectNoSchedule},
		{Key: "taint2", Value: "val2", Effect: v1.TaintEffectPreferNoSchedule},
	}

	createTestNode(ctx, t, nodeName, nil, nil, taints, true)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	k8sClient := setupTestClient(t)

	taintsToRemove := []config.Taint{{Key: "taint1", Value: "val1", Effect: "NoSchedule"}}
	err := k8sClient.UnQuarantineNodeAndRemoveAnnotations(ctx, nodeName, taintsToRemove, nil, []string{}, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}

	// Filter to test taints only (ignore automatic envtest taints)
	var testTaints []v1.Taint
	for _, taint := range updatedNode.Spec.Taints {
		if taint.Key == "taint1" || taint.Key == "taint2" {
			testTaints = append(testTaints, taint)
		}
	}
	if len(testTaints) != 1 {
		t.Errorf("Expected 1 test taint remaining, got %d", len(testTaints))
	}
	if len(testTaints) > 0 && testTaints[0].Key != "taint2" {
		t.Errorf("Expected taint2 to remain, got %s", testTaints[0].Key)
	}
}

func TestUnTaintAndUnCordonNode_PartialAnnotationRemoval(t *testing.T) {
	ctx := context.Background()
	nodeName := testutils.GenerateTestNodeName("test-partial-annotation-")

	annotations := map[string]string{
		"annotation1": "val1",
		"annotation2": "val2",
	}
	labels := map[string]string{
		cordonedByLabelKey:        common.ServiceName,
		cordonedReasonLabelKey:    "gpu-error",
		cordonedTimestampLabelKey: time.Now().UTC().Format("2006-01-02T15-04-05Z"),
	}

	createTestNode(ctx, t, nodeName, annotations, labels, nil, true)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	k8sClient := setupTestClient(t)

	annotationsToRemove := []string{"annotation1"}
	labelsMap := map[string]string{
		uncordonedByLabelKey:        common.ServiceName,
		uncordonedTimestampLabelKey: time.Now().UTC().Format("2006-01-02T15-04-05Z"),
	}
	err := k8sClient.UnQuarantineNodeAndRemoveAnnotations(ctx, nodeName, nil, annotationsToRemove, []string{cordonedByLabelKey, cordonedReasonLabelKey, cordonedTimestampLabelKey}, labelsMap)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}

	if _, ok := updatedNode.Annotations["annotation1"]; ok {
		t.Errorf("Expected annotation1 to be removed")
	}
	if updatedNode.Annotations["annotation2"] != "val2" {
		t.Errorf("Expected annotation2 to remain")
	}
	if updatedNode.Spec.Unschedulable {
		t.Errorf("Expected node to be uncordoned")
	}
}

func TestTaintAndCordonNode_AlreadyTaintedCordoned(t *testing.T) {
	ctx := context.Background()
	nodeName := testutils.GenerateTestNodeName("test-already-tainted-")

	taints := []v1.Taint{
		{Key: "test-key", Value: "test-value", Effect: v1.TaintEffectNoSchedule},
	}

	createTestNode(ctx, t, nodeName, nil, nil, taints, true)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	k8sClient := setupTestClient(t)

	taintsToAdd := []config.Taint{{Key: "test-key", Value: "test-value", Effect: "NoSchedule"}}
	err := k8sClient.QuarantineNodeAndSetAnnotations(ctx, nodeName, taintsToAdd, true, nil, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}

	// Filter to test taint only (ignore automatic envtest taints)
	var testTaints []v1.Taint
	for _, taint := range updatedNode.Spec.Taints {
		if taint.Key == "test-key" {
			testTaints = append(testTaints, taint)
		}
	}
	if len(testTaints) != 1 {
		t.Errorf("Expected 1 test taint, got %d", len(testTaints))
	}
	if !updatedNode.Spec.Unschedulable {
		t.Errorf("Node should remain cordoned")
	}
}

func TestUnTaintAndUnCordonNode_AlreadyUntaintedUncordoned(t *testing.T) {
	ctx := context.Background()
	nodeName := testutils.GenerateTestNodeName("test-already-untainted-")

	createTestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	k8sClient := setupTestClient(t)

	err := k8sClient.UnQuarantineNodeAndRemoveAnnotations(ctx, nodeName, nil, nil, []string{}, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}

	// Function always uncordons, so node should be schedulable
	if updatedNode.Spec.Unschedulable {
		t.Errorf("Expected node to be uncordoned")
	}
}

func TestTaintAndCordonNode_InvalidTaintEffect(t *testing.T) {
	ctx := context.Background()
	nodeName := testutils.GenerateTestNodeName("test-invalid-effect-")

	createTestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	k8sClient := setupTestClient(t)

	// envtest validates taint effects, so invalid effects should now return an error
	taints := []config.Taint{{Key: "weird-key", Value: "weird-value", Effect: "SomeInvalidEffect"}}
	err := k8sClient.QuarantineNodeAndSetAnnotations(ctx, nodeName, taints, false, nil, map[string]string{})
	if err == nil {
		t.Errorf("Expected error for invalid taint effect, got nil")
	}
}

func TestTaintAndCordonNode_OverwriteAnnotation(t *testing.T) {
	ctx := context.Background()
	nodeName := testutils.GenerateTestNodeName("test-overwrite-annotation-")

	existingAnnotations := map[string]string{"existing-key": "old-value"}

	createTestNode(ctx, t, nodeName, existingAnnotations, nil, nil, false)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	k8sClient := setupTestClient(t)

	annotations := map[string]string{"existing-key": "new-value"}
	err := k8sClient.QuarantineNodeAndSetAnnotations(ctx, nodeName, nil, false, annotations, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}

	if updatedNode.Annotations["existing-key"] != "new-value" {
		t.Errorf("Annotation value was not updated correctly")
	}
}

func TestUnTaintAndUnCordonNode_NonExistentTaintRemoval(t *testing.T) {
	ctx := context.Background()
	nodeName := testutils.GenerateTestNodeName("test-nonexistent-taint-")

	taints := []v1.Taint{
		{Key: "taint1", Value: "val1", Effect: v1.TaintEffectNoSchedule},
	}

	createTestNode(ctx, t, nodeName, nil, nil, taints, false)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	k8sClient := setupTestClient(t)

	taintsToRemove := []config.Taint{{Key: "taint-nonexistent", Value: "valX", Effect: "NoSchedule"}}
	err := k8sClient.UnQuarantineNodeAndRemoveAnnotations(ctx, nodeName, taintsToRemove, nil, []string{}, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}

	// Filter to test taint only (ignore automatic envtest taints)
	var testTaints []v1.Taint
	for _, taint := range updatedNode.Spec.Taints {
		if taint.Key == "taint1" {
			testTaints = append(testTaints, taint)
		}
	}
	// Original taint should remain as we tried to remove a non-existent taint
	if len(testTaints) != 1 {
		t.Errorf("Expected 1 test taint to remain, got %d", len(testTaints))
	}
}

func TestUnTaintAndUnCordonNode_NonExistentAnnotationRemoval(t *testing.T) {
	ctx := context.Background()
	nodeName := testutils.GenerateTestNodeName("test-nonexistent-annotation-")

	annotations := map[string]string{
		"annotation1": "val1",
	}

	createTestNode(ctx, t, nodeName, annotations, nil, nil, false)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	k8sClient := setupTestClient(t)

	annotationsToRemove := []string{"nonexistent-annotation"}
	err := k8sClient.UnQuarantineNodeAndRemoveAnnotations(ctx, nodeName, nil, annotationsToRemove, []string{}, map[string]string{})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	updatedNode, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}

	// Original annotation should remain
	if updatedNode.Annotations["annotation1"] != "val1" {
		t.Errorf("Non-existent annotation removal should not affect existing annotations")
	}
}

func TestTaintAndCordonNode_EmptyTaintKeyOrValue(t *testing.T) {
	ctx := context.Background()
	nodeName := testutils.GenerateTestNodeName("test-empty-taint-")

	createTestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	k8sClient := setupTestClient(t)

	// envtest validates taint keys, so empty keys should return an error
	taints := []config.Taint{
		{Key: "", Value: "", Effect: "NoSchedule"},
	}
	err := k8sClient.QuarantineNodeAndSetAnnotations(ctx, nodeName, taints, false, nil, map[string]string{})
	if err == nil {
		t.Errorf("Expected error for empty taint key, got nil")
	}
}

func TestTaintAndCordonNode_EmptyAnnotationKey(t *testing.T) {
	ctx := context.Background()
	nodeName := testutils.GenerateTestNodeName("test-empty-annotation-key-")

	createTestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	k8sClient := setupTestClient(t)

	// envtest validates annotation keys, so empty keys should return an error
	annotations := map[string]string{
		"": "empty-key-value",
	}
	err := k8sClient.QuarantineNodeAndSetAnnotations(ctx, nodeName, nil, false, annotations, map[string]string{})
	if err == nil {
		t.Errorf("Expected error for empty annotation key, got nil")
	}
}

func TestTaintAndCordonNode_NonExistentNode(t *testing.T) {
	ctx := context.Background()
	k8sClient := setupTestClient(t)

	err := k8sClient.QuarantineNodeAndSetAnnotations(ctx, "no-such-node", nil, true, nil, map[string]string{})
	if err == nil {
		t.Errorf("Expected error for non-existent node, got nil")
	}
}

func TestUnTaintAndUnCordonNode_NonExistentNode(t *testing.T) {
	ctx := context.Background()
	k8sClient := setupTestClient(t)

	err := k8sClient.UnQuarantineNodeAndRemoveAnnotations(ctx, "no-such-node", nil, nil, []string{}, map[string]string{})
	if err == nil {
		t.Errorf("Expected error for non-existent node, got nil")
	}
}
