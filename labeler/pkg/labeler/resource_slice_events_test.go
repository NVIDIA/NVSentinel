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

package labeler

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	listersv1 "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/labeler/pkg/devicecounts"
)

func TestResourceSliceMetadataOnlyUpdateDoesNotReconcile(t *testing.T) {
	assignedNode := testNodeWithDeviceCountLabels("node-a", "", "")
	assignedNode.Labels[KataRuntimeDefaultLabel] = LabelValueTrue
	resourceSliceBeforeUpdate := testResourceSliceForNode("slice-a", assignedNode.Name)
	clientset := newEnvtestClientWithNodes(t, assignedNode)
	labeler := newTestLabelerWithResourceSlices(
		t,
		clientset,
		[]*corev1.Node{assignedNode},
		resourceSliceBeforeUpdate,
	)
	resourceSliceAfterMetadataUpdate := resourceSliceBeforeUpdate.DeepCopy()
	resourceSliceAfterMetadataUpdate.Labels = map[string]string{"changed": "true"}

	require.NoError(t, labeler.resourceSliceInformer.GetIndexer().Update(resourceSliceAfterMetadataUpdate))
	labeler.newResourceSliceEventHandlers().UpdateFunc(
		resourceSliceBeforeUpdate,
		resourceSliceAfterMetadataUpdate,
	)

	nodeAfterMetadataUpdate, err := clientset.CoreV1().Nodes().Get(
		context.Background(),
		assignedNode.Name,
		metav1.GetOptions{},
	)
	require.NoError(t, err)
	require.Equal(t, LabelValueFalse, nodeAfterMetadataUpdate.Labels[KataEnabledLabel])
}

func TestResourceSliceNodeReassignmentReconcilesBothNodes(t *testing.T) {
	previouslyAssignedNode := testNodeWithDeviceCountLabels("node-a", "4", "8")
	previouslyAssignedNode.Labels[KataRuntimeDefaultLabel] = LabelValueTrue
	newlyAssignedNode := testNodeWithDeviceCountLabels("node-b", "", "")
	resourceSliceBeforeReassignment := testResourceSliceForNode("slice", previouslyAssignedNode.Name)
	resourceSliceAfterReassignment := testResourceSliceForNode("slice", newlyAssignedNode.Name)
	clientset := newEnvtestClientWithNodes(t, previouslyAssignedNode, newlyAssignedNode)
	labeler := newTestLabelerWithResourceSlices(
		t,
		clientset,
		[]*corev1.Node{previouslyAssignedNode, newlyAssignedNode},
		resourceSliceBeforeReassignment,
	)

	require.NoError(t, labeler.resourceSliceInformer.GetIndexer().Update(resourceSliceAfterReassignment))
	labeler.newResourceSliceEventHandlers().UpdateFunc(
		resourceSliceBeforeReassignment,
		resourceSliceAfterReassignment,
	)

	previousNodeAfterReconciliation, err := labeler.clientset.CoreV1().Nodes().Get(
		context.Background(),
		previouslyAssignedNode.Name,
		metav1.GetOptions{},
	)
	require.NoError(t, err)
	require.Equal(t, LabelValueTrue, previousNodeAfterReconciliation.Labels[KataEnabledLabel])
	require.Equal(t, "4", previousNodeAfterReconciliation.Labels[testDeviceCountCurrentLabel])
	require.Equal(t, "8", previousNodeAfterReconciliation.Labels[testDeviceCountExpectedLabel])

	newNodeAfterReconciliation, err := labeler.clientset.CoreV1().Nodes().Get(
		context.Background(),
		newlyAssignedNode.Name,
		metav1.GetOptions{},
	)
	require.NoError(t, err)
	require.Equal(t, "1", newNodeAfterReconciliation.Labels[testDeviceCountCurrentLabel])
	require.Equal(t, "8", newNodeAfterReconciliation.Labels[testDeviceCountExpectedLabel])
}

func TestEventReconciliationUsesFreshDeviceCountCache(t *testing.T) {
	nodeReceivingEvent := testNodeWithDeviceCountLabels("node-a", "", "")
	peerNodeWithHighestDeviceCount := testNodeWithDeviceCountLabels("node-b", "", "")
	clientset := newEnvtestClientWithNodes(t, nodeReceivingEvent, peerNodeWithHighestDeviceCount)
	labeler := newTestLabelerWithResourceSlices(
		t,
		clientset,
		[]*corev1.Node{nodeReceivingEvent, peerNodeWithHighestDeviceCount},
		testResourceSliceForNode("slice-a", nodeReceivingEvent.Name),
		testResourceSliceForNode("slice-b-1", peerNodeWithHighestDeviceCount.Name),
		testResourceSliceForNode("slice-b-2", peerNodeWithHighestDeviceCount.Name),
	)

	labeler.reconcileAllNodes()
	require.NoError(t, labeler.resourceSliceInformer.GetIndexer().Add(
		testResourceSliceForNode("slice-b-3", peerNodeWithHighestDeviceCount.Name),
	))

	require.NoError(t, labeler.handleNodeEvent(nodeReceivingEvent))

	reconciledNode, err := labeler.clientset.CoreV1().Nodes().Get(
		context.Background(),
		nodeReceivingEvent.Name,
		metav1.GetOptions{},
	)
	require.NoError(t, err)
	require.Equal(t, "3", reconciledNode.Labels[testDeviceCountExpectedLabel])
}

func TestReconcileNodeLabelsRecalculatesDriverLabelAfterConflict(t *testing.T) {
	cachedNodeBeforeConflict := testNodeWithDeviceCountLabels("node-a", "", "")
	clientset := fake.NewSimpleClientset(cachedNodeBeforeConflict.DeepCopy())
	labeler := newTestLabelerWithResourceSlices(
		t,
		clientset,
		[]*corev1.Node{cachedNodeBeforeConflict},
		testResourceSliceForNode("slice-a", cachedNodeBeforeConflict.Name),
	)
	labeler.assumeDriverInstalled = true

	liveNodeBeforeConflict, err := labeler.clientset.CoreV1().Nodes().Get(
		context.Background(),
		cachedNodeBeforeConflict.Name,
		metav1.GetOptions{},
	)
	require.NoError(t, err)
	liveNodeBeforeConflict.Labels[gpuPresentLabel] = LabelValueTrue
	_, err = labeler.clientset.CoreV1().Nodes().Update(
		context.Background(),
		liveNodeBeforeConflict,
		metav1.UpdateOptions{},
	)
	require.NoError(t, err)

	patchAttempts := 0
	clientset.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		patchAttempts++
		if patchAttempts == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Resource: "nodes"},
				cachedNodeBeforeConflict.Name,
				errors.New("simulated conflict"),
			)
		}

		return false, nil, nil
	})

	require.NoError(t, labeler.reconcileNodeLabels(
		cachedNodeBeforeConflict.Name,
		labeler.newDeviceCountReconcileCache(),
	))
	require.Equal(t, 2, patchAttempts)

	nodeAfterConflictRetry, err := labeler.clientset.CoreV1().Nodes().Get(
		context.Background(),
		cachedNodeBeforeConflict.Name,
		metav1.GetOptions{},
	)
	require.NoError(t, err)
	require.Equal(t, LabelValueTrue, nodeAfterConflictRetry.Labels[DriverInstalledLabel])
}

const (
	testDeviceCountCurrentLabel  = "test.nvsentinel/current"
	testDeviceCountExpectedLabel = "test.nvsentinel/expected"
)

func newTestLabelerWithResourceSlices(
	t *testing.T,
	clientset kubernetes.Interface,
	nodes []*corev1.Node,
	resourceSlices ...*resourcev1.ResourceSlice,
) *Labeler {
	t.Helper()

	nodeInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Node{}, 0, cache.Indexers{})
	for _, node := range nodes {
		require.NoError(t, nodeInformer.GetIndexer().Add(node.DeepCopy()))
	}

	podInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Pod{}, 0, cache.Indexers{
		NodeDCGMIndex:   podNodeIndexerByLabel("app", "nvidia-dcgm"),
		NodeDriverIndex: podNodeIndexerByLabel("app", "nvidia-driver-daemonset"),
	})
	crdDriverInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Pod{}, 0, cache.Indexers{
		NodeDriverIndex: podNodeIndexerByLabel(driverComponentLabel, driverComponentValue),
	})
	gkeInstallerInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Pod{}, 0, cache.Indexers{
		NodeGKEDriverInstallerIndex: podNodeIndexerByLabel("k8s-app", "nvidia-driver-installer"),
	})
	resourceSliceInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{},
		&resourcev1.ResourceSlice{},
		0,
		cache.Indexers{
			devicecounts.ResourceSliceNodeNameIndex: devicecounts.ResourceSliceNodeNameIndexFunc,
		},
	)
	for _, resourceSlice := range resourceSlices {
		require.NoError(t, resourceSliceInformer.GetIndexer().Add(resourceSlice))
	}

	manager, err := devicecounts.NewManager(testResourceSliceDeviceCountConfig())
	require.NoError(t, err)

	return &Labeler{
		clientset:             clientset,
		podInformer:           podInformer,
		crdDriverInformer:     crdDriverInformer,
		nodeInformer:          nodeInformer,
		nodeLister:            listersv1.NewNodeLister(nodeInformer.GetIndexer()),
		gkeInstallerInformer:  gkeInstallerInformer,
		resourceSliceInformer: resourceSliceInformer,
		informersSynced: []cache.InformerSynced{
			func() bool { return true },
		},
		ctx:          context.Background(),
		kataLabels:   []string{KataRuntimeDefaultLabel},
		deviceCounts: manager,
	}
}

func newEnvtestClientWithNodes(t *testing.T, nodes ...*corev1.Node) kubernetes.Interface {
	t.Helper()

	testEnvironment := &envtest.Environment{}
	restConfig, err := testEnvironment.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, testEnvironment.Stop())
	})

	clientset, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)
	for _, node := range nodes {
		_, err := clientset.CoreV1().Nodes().Create(
			context.Background(),
			node.DeepCopy(),
			metav1.CreateOptions{},
		)
		require.NoError(t, err)
	}

	return clientset
}

func testNodeWithDeviceCountLabels(
	name string,
	currentCountLabelValue string,
	expectedCountLabelValue string,
) *corev1.Node {
	node := &corev1.Node{
		Name: name,
		Labels: map[string]string{
			KataEnabledLabel: LabelValueFalse,
		},
	}

	if currentCountLabelValue != "" {
		node.Labels[testDeviceCountCurrentLabel] = currentCountLabelValue
	}
	if expectedCountLabelValue != "" {
		node.Labels[testDeviceCountExpectedLabel] = expectedCountLabelValue
	}

	return node
}

func testResourceSliceForNode(name, nodeName string) *resourcev1.ResourceSlice {
	return &resourcev1.ResourceSlice{
		Name: name,
		Spec: resourcev1.ResourceSliceSpec{
			NodeName: &nodeName,
			Pool: resourcev1.ResourcePool{
				Name:               nodeName,
				Generation:         1,
				ResourceSliceCount: 1,
			},
			Driver: "test.example.com",
		},
	}
}
