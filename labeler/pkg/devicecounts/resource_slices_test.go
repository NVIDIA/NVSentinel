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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/client-go/tools/cache"
)

func TestExpectedDeviceCountsCountResourceSlices(t *testing.T) {
	config := Config{
		Enabled: true,
		Classes: []ClassConfig{
			{
				Name:    "nic",
				Enabled: true,
				Labels: Labels{
					Current:  testNICCountCurrentLabel,
					Expected: testNICCountExpectedLabel,
				},
				CurrentExpression: `
sum(resourceSlices
  .filter(rs,
    has(rs.spec.driver) &&
    rs.spec.driver == 'dra.networking.k8s.aws' &&
    has(rs.spec.devices)
  )
  .map(rs, rs.spec.devices
    .filter(d,
      has(d.attributes) &&
      'dra.vpc.amazonaws.com/deviceType' in d.attributes &&
      has(d.attributes['dra.vpc.amazonaws.com/deviceType'].string) &&
      d.attributes['dra.vpc.amazonaws.com/deviceType'].string == 'roce'
    )
    .size()
  ))`,
			},
		},
	}

	node := testNode("node-a", map[string]string{})
	resourceSliceStore := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		ResourceSliceNodeNameIndex: ResourceSliceNodeNameIndexFunc,
	})

	require.NoError(t, resourceSliceStore.Add(testResourceSlice("slice-a", "node-a",
		testDevice("roce-a", stringAttribute("roce")),
		testDevice("ethernet-a", stringAttribute("ethernet")),
		testDevice("missing-attribute", nil),
		testDevice("wrong-attribute-type", boolAttribute(true)),
	)))
	require.NoError(t, resourceSliceStore.Add(testResourceSlice("slice-b", "node-a",
		testDevice("roce-b", stringAttribute("roce")),
	)))
	require.NoError(t, resourceSliceStore.Add(testResourceSlice("slice-without-devices", "node-a")))
	require.NoError(t, resourceSliceStore.Add(testResourceSlice("other-node-slice", "node-b",
		testDevice("roce-c", stringAttribute("roce")),
	)))
	require.NoError(t, resourceSliceStore.Add(&resourcev1.ResourceSlice{
		Name: "global-slice",
		Spec: resourcev1.ResourceSliceSpec{
			Driver:  "dra.networking.k8s.aws",
			Devices: []resourcev1.Device{testDevice("global-roce", stringAttribute("roce"))},
		},
	}))

	manager := newTestManager(t, config)
	updated := manager.CalculateAndSetDeviceCountLabels(
		context.Background(),
		node,
		[]*corev1.Node{node},
		func(node *corev1.Node) []*resourcev1.ResourceSlice {
			return ResourceSlicesForNode(resourceSliceStore, node)
		},
	)

	require.True(t, updated)
	require.Equal(t, "2", node.Labels[testNICCountCurrentLabel])
	require.Equal(t, "2", node.Labels[testNICCountExpectedLabel])
}

func TestReconcileCacheSeparatesClassesAndPartitions(t *testing.T) {
	config := Config{
		Enabled: true,
		Classes: []ClassConfig{
			{
				Name:    "nic",
				Enabled: true,
				Labels: Labels{
					Current:  testNICCountCurrentLabel,
					Expected: testNICCountExpectedLabel,
				},
				GroupingLabels:    []string{"hardware"},
				CurrentExpression: "resourceSlices.size()",
			},
			{
				Name:    "scaled-nic",
				Enabled: true,
				Labels: Labels{
					Current:  testGPUCountCurrentLabel,
					Expected: testGPUCountExpectedLabel,
				},
				GroupingLabels:    []string{"hardware"},
				CurrentExpression: "resourceSlices.size() * 10",
			},
		},
	}

	nodesToReconcile := []*corev1.Node{
		testNode("node-a", map[string]string{"hardware": "x"}),
		testNode("node-b", map[string]string{"hardware": "x"}),
		testNode("node-c", map[string]string{"hardware": "y"}),
	}
	resourceSlicesByNode := map[string][]*resourcev1.ResourceSlice{
		"node-a": {testResourceSlice("slice-a", "node-a")},
		"node-b": {
			testResourceSlice("slice-b-1", "node-b"),
			testResourceSlice("slice-b-2", "node-b"),
		},
		"node-c": {
			testResourceSlice("slice-c-1", "node-c"),
			testResourceSlice("slice-c-2", "node-c"),
			testResourceSlice("slice-c-3", "node-c"),
		},
	}
	resourceSliceLookupCountByNode := map[string]int{}

	deviceCountManager := newTestManager(t, config)
	reconcileCache := deviceCountManager.NewReconcileCache(
		nodesToReconcile,
		func(node *corev1.Node) []*resourcev1.ResourceSlice {
			resourceSliceLookupCountByNode[node.Name]++
			return resourceSlicesByNode[node.Name]
		},
	)

	for _, nodeInHardwarePartitionX := range nodesToReconcile[:2] {
		require.True(
			t,
			reconcileCache.CalculateAndSetDeviceCountLabels(context.Background(), nodeInHardwarePartitionX),
		)
		require.Equal(t, "2", nodeInHardwarePartitionX.Labels[testNICCountExpectedLabel])
		require.Equal(t, "20", nodeInHardwarePartitionX.Labels[testGPUCountExpectedLabel])
	}
	nodeInHardwarePartitionY := nodesToReconcile[2]
	require.True(
		t,
		reconcileCache.CalculateAndSetDeviceCountLabels(context.Background(), nodeInHardwarePartitionY),
	)
	require.Equal(t, "3", nodeInHardwarePartitionY.Labels[testNICCountExpectedLabel])
	require.Equal(t, "30", nodeInHardwarePartitionY.Labels[testGPUCountExpectedLabel])

	// Each node is looked up once when it is reconciled and once for each of the
	// two classes when its partition is read in full. Lookups are not reused, so
	// a slice that changes during a sweep is never counted from an earlier one.
	require.Equal(t, map[string]int{
		"node-a": 3,
		"node-b": 3,
		"node-c": 3,
	}, resourceSliceLookupCountByNode)
}

func TestReconcileCache_SliceChangeDuringSweep_CountsTheNewSlices(t *testing.T) {
	config := Config{
		Enabled: true,
		Classes: []ClassConfig{
			{
				Name:    "nic",
				Enabled: true,
				Labels: Labels{
					Current:  testNICCountCurrentLabel,
					Expected: testNICCountExpectedLabel,
				},
				GroupingLabels:    []string{"hardware"},
				CurrentExpression: "resourceSlices.size()",
			},
		},
	}

	nodeA := testNode("node-a", map[string]string{"hardware": "x"})
	nodeB := testNode("node-b", map[string]string{"hardware": "x"})
	resourceSlicesByNode := map[string][]*resourcev1.ResourceSlice{
		"node-a": {testResourceSlice("slice-a", "node-a")},
		"node-b": {testResourceSlice("slice-b-1", "node-b")},
	}

	manager := newTestManager(t, config)
	sweep := manager.NewReconcileCache(
		[]*corev1.Node{nodeA, nodeB},
		func(node *corev1.Node) []*resourcev1.ResourceSlice { return resourceSlicesByNode[node.Name] },
	)

	// The sweep reconciles node-a first, which reads node-b's single slice.
	sweep.CalculateAndSetDeviceCountLabels(context.Background(), nodeA)

	// node-b gets a second slice before the sweep reaches it.
	resourceSlicesByNode["node-b"] = append(resourceSlicesByNode["node-b"],
		testResourceSlice("slice-b-2", "node-b"))

	sweep.CalculateAndSetDeviceCountLabels(context.Background(), nodeB)
	require.Equal(t, "2", nodeB.Labels[testNICCountCurrentLabel],
		"the sweep must count node-b's slices as they are when it reaches node-b")
	require.Equal(t, "2", nodeB.Labels[testNICCountExpectedLabel])
}

func TestMissingPeerResourceSlicesDoNotLowerExpectedDeviceCount(t *testing.T) {
	config := Config{
		Enabled: true,
		Classes: []ClassConfig{{
			Name:    "nic",
			Enabled: true,
			Labels: Labels{
				Current:  testNICCountCurrentLabel,
				Expected: testNICCountExpectedLabel,
			},
			CurrentExpression: "resourceSlices.size()",
		}},
	}

	targetNode := testNode("target", map[string]string{
		testNICCountExpectedLabel: "5",
	})
	peerNodeWithoutResourceSlices := testNode("peer", map[string]string{})
	targetResourceSlices := []*resourcev1.ResourceSlice{
		testResourceSlice("target-slice", targetNode.Name),
	}

	deviceCountManager := newTestManager(t, config)
	labelsChanged := deviceCountManager.CalculateAndSetDeviceCountLabels(
		context.Background(),
		targetNode,
		[]*corev1.Node{targetNode, peerNodeWithoutResourceSlices},
		func(node *corev1.Node) []*resourcev1.ResourceSlice {
			if node.Name == targetNode.Name {
				return targetResourceSlices
			}

			return nil
		},
	)

	require.True(t, labelsChanged)
	require.Equal(t, "1", targetNode.Labels[testNICCountCurrentLabel])
	require.Equal(t, "5", targetNode.Labels[testNICCountExpectedLabel])
}

func testResourceSlice(name, nodeName string, devices ...resourcev1.Device) *resourcev1.ResourceSlice {
	return &resourcev1.ResourceSlice{
		Name: name,
		Spec: resourcev1.ResourceSliceSpec{
			Driver: "dra.networking.k8s.aws",
			Pool: resourcev1.ResourcePool{
				Name:               nodeName,
				Generation:         1,
				ResourceSliceCount: 1,
			},
			NodeName: &nodeName,
			Devices:  devices,
		},
	}
}

func testDevice(name string, attribute *resourcev1.DeviceAttribute) resourcev1.Device {
	device := resourcev1.Device{
		Name: name,
	}

	if attribute != nil {
		device.Attributes = map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"dra.vpc.amazonaws.com/deviceType": *attribute,
		}
	}

	return device
}

func stringAttribute(value string) *resourcev1.DeviceAttribute {
	return &resourcev1.DeviceAttribute{StringValue: &value}
}

func boolAttribute(value bool) *resourcev1.DeviceAttribute {
	return &resourcev1.DeviceAttribute{BoolValue: &value}
}
