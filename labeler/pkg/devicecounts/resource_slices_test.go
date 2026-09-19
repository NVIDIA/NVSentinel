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

	require.Equal(t, map[string]int{
		"node-a": 1,
		"node-b": 1,
		"node-c": 1,
	}, resourceSliceLookupCountByNode)
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

// TestResourceSliceDriverMakesDualGPUClassesMutuallyExclusive covers the
// GPUCluster (DRA) configuration shipped in the chart: a GFD-label class and a
// ResourceSlice class share one label pair, and exactly one succeeds per node.
func TestResourceSliceDriverMakesDualGPUClassesMutuallyExclusive(t *testing.T) {
	const gpuDRAExpression = `
sum(resourceSlices
  .filter(rs, has(rs.spec.devices))
  .map(rs, rs.spec.devices
    .filter(d,
      has(d.attributes) &&
      'type' in d.attributes &&
      has(d.attributes['type'].string) &&
      d.attributes['type'].string == 'gpu'
    )
    .size()
  ))`

	config := Config{
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
			{
				Name:    "gpu-dra",
				Enabled: true,
				Labels: Labels{
					Current:  testGPUCountCurrentLabel,
					Expected: testGPUCountExpectedLabel,
				},
				GroupingLabels:      []string{"node.kubernetes.io/instance-type"},
				ResourceSliceDriver: "gpu.nvidia.com",
				CurrentExpression:   gpuDRAExpression,
			},
		},
	}

	gpuDevice := func(name, deviceType string) resourcev1.Device {
		return resourcev1.Device{
			Name: name,
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				"type": *stringAttribute(deviceType),
				"uuid": *stringAttribute("GPU-" + name),
			},
		}
	}
	nvidiaSlice := func(name, nodeName string, devices ...resourcev1.Device) *resourcev1.ResourceSlice {
		slice := testResourceSlice(name, nodeName, devices...)
		slice.Spec.Driver = "gpu.nvidia.com"

		return slice
	}

	const instanceTypeLabel = "node.kubernetes.io/instance-type"

	clusterPolicyNode := testNode("cluster-policy", map[string]string{
		instanceTypeLabel: "gpu-8x", "nvidia.com/gpu.count": "8",
	})
	// A ClusterPolicy node that also runs an unrelated DRA driver has slices but
	// no gpu.nvidia.com ones; the DRA class must not write current=0 there.
	clusterPolicyNodeWithNICDRA := testNode("cluster-policy-nic-dra", map[string]string{
		instanceTypeLabel: "gpu-8x", "nvidia.com/gpu.count": "8",
	})
	gpuClusterNode := testNode("gpu-cluster", map[string]string{instanceTypeLabel: "gpu-4x"})
	cpuNode := testNode("cpu", map[string]string{instanceTypeLabel: "cpu-only"})

	resourceSlicesByNode := map[string][]*resourcev1.ResourceSlice{
		clusterPolicyNodeWithNICDRA.Name: {
			testResourceSlice("nic-slice", clusterPolicyNodeWithNICDRA.Name, testDevice("roce-a", stringAttribute("roce"))),
		},
		gpuClusterNode.Name: {
			nvidiaSlice("gpu-slice-a", gpuClusterNode.Name,
				gpuDevice("gpu-0", "gpu"), gpuDevice("gpu-1", "gpu"), gpuDevice("gpu-2", "gpu")),
			nvidiaSlice("gpu-slice-b", gpuClusterNode.Name,
				gpuDevice("gpu-3", "gpu"), gpuDevice("gpu-3-mig-0", "mig")),
			// Unrelated driver on the same node is filtered out by resourceSliceDriver.
			testResourceSlice("nic-slice", gpuClusterNode.Name, testDevice("roce-b", stringAttribute("roce"))),
		},
	}
	allNodes := []*corev1.Node{clusterPolicyNode, clusterPolicyNodeWithNICDRA, gpuClusterNode, cpuNode}
	loadResourceSlices := func(node *corev1.Node) []*resourcev1.ResourceSlice {
		return resourceSlicesByNode[node.Name]
	}

	manager := newTestManager(t, config)
	require.True(t, manager.RequiresResourceSlices())

	cache := manager.NewReconcileCache(allNodes, loadResourceSlices)

	require.True(t, cache.CalculateAndSetDeviceCountLabels(context.Background(), clusterPolicyNode))
	require.Equal(t, "8", clusterPolicyNode.Labels[testGPUCountCurrentLabel], "GFD class serves ClusterPolicy nodes")
	require.Equal(t, "8", clusterPolicyNode.Labels[testGPUCountExpectedLabel])

	require.True(t, cache.CalculateAndSetDeviceCountLabels(context.Background(), clusterPolicyNodeWithNICDRA))
	require.Equal(t, "8", clusterPolicyNodeWithNICDRA.Labels[testGPUCountCurrentLabel],
		"foreign-driver slices must not let the DRA class overwrite the GFD count with 0")
	require.Equal(t, "8", clusterPolicyNodeWithNICDRA.Labels[testGPUCountExpectedLabel])

	require.True(t, cache.CalculateAndSetDeviceCountLabels(context.Background(), gpuClusterNode))
	require.Equal(t, "4", gpuClusterNode.Labels[testGPUCountCurrentLabel],
		"DRA class counts only type=gpu devices from gpu.nvidia.com slices")
	require.Equal(t, "4", gpuClusterNode.Labels[testGPUCountExpectedLabel],
		"expected count is learned only from peers with the same source")

	require.False(t, cache.CalculateAndSetDeviceCountLabels(context.Background(), cpuNode))
	require.NotContains(t, cpuNode.Labels, testGPUCountCurrentLabel)
	require.NotContains(t, cpuNode.Labels, testGPUCountExpectedLabel)
}

func TestResourceSliceDriverWithoutSlicesFromDriverIsMissingSource(t *testing.T) {
	config := Config{
		Enabled: true,
		Classes: []ClassConfig{{
			Name:    "gpu-dra",
			Enabled: true,
			Labels: Labels{
				Current:  testGPUCountCurrentLabel,
				Expected: testGPUCountExpectedLabel,
			},
			ResourceSliceDriver: "gpu.nvidia.com",
			// The expression never names resourceSlices; the driver alone makes
			// the class ResourceSlice-backed so it is skipped, not evaluated to 7.
			CurrentExpression: "7",
		}},
	}

	node := testNode("node-a", map[string]string{})
	foreignSlices := []*resourcev1.ResourceSlice{
		testResourceSlice("nic-slice", node.Name, testDevice("roce-a", stringAttribute("roce"))),
	}

	manager := newTestManager(t, config)
	require.True(t, manager.RequiresResourceSlices())

	updated := manager.CalculateAndSetDeviceCountLabels(
		context.Background(),
		node,
		[]*corev1.Node{node},
		func(*corev1.Node) []*resourcev1.ResourceSlice { return foreignSlices },
	)

	require.False(t, updated)
	require.Empty(t, node.Labels)

	nvidiaSlice := testResourceSlice("gpu-slice", node.Name)
	nvidiaSlice.Spec.Driver = "gpu.nvidia.com"

	updated = manager.CalculateAndSetDeviceCountLabels(
		context.Background(),
		node,
		[]*corev1.Node{node},
		func(*corev1.Node) []*resourcev1.ResourceSlice { return []*resourcev1.ResourceSlice{nvidiaSlice} },
	)

	require.True(t, updated)
	require.Equal(t, "7", node.Labels[testGPUCountCurrentLabel])
}
