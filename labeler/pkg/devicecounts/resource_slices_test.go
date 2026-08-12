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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	resourceSliceStore := cache.NewStore(cache.MetaNamespaceKeyFunc)

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

	manager := newTestManager(t, config)
	updated := manager.ReconcileNodeLabelsInPlace(
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

func testResourceSlice(name, nodeName string, devices ...resourcev1.Device) *resourcev1.ResourceSlice {
	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
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
