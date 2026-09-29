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

package mapper

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func testResourceSlice(name, node, pool string, devices map[string]string) *resourcev1.ResourceSlice {
	slice := &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   draGPUDriverName,
			NodeName: ptr.To(node),
			Pool:     resourcev1.ResourcePool{Name: pool},
		},
	}
	for device, uuid := range devices {
		attributes := map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{}
		if uuid != "" {
			attributes[draGPUUUIDAttribute] = resourcev1.DeviceAttribute{StringValue: ptr.To(uuid)}
		}
		slice.Spec.Devices = append(slice.Spec.Devices, resourcev1.Device{Name: device, Attributes: attributes})
	}
	return slice
}

func TestNewDRADeviceResolver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A cluster without resource.k8s.io/v1 yields no resolver and no error.
	resolve, err := newDRADeviceResolver(ctx, fake.NewSimpleClientset(), "node-a")
	require.NoError(t, err)
	assert.Nil(t, resolve)

	client := fake.NewSimpleClientset(
		testResourceSlice("node-a-slice", "node-a", "node-a", map[string]string{"gpu-0": "GPU-a0", "gpu-1": ""}),
		testResourceSlice("node-b-slice", "node-b", "node-b", map[string]string{"gpu-0": "GPU-b0"}),
	)
	client.Fake.Resources = []*metav1.APIResourceList{{GroupVersion: resourcev1.SchemeGroupVersion.String()}}

	resolve, err = newDRADeviceResolver(ctx, client, "node-a")
	require.NoError(t, err)
	require.NotNil(t, resolve)

	uuid, ok := resolve("node-a", "gpu-0")
	assert.True(t, ok)
	assert.Equal(t, "GPU-a0", uuid)

	_, ok = resolve("node-a", "gpu-1") // device without a uuid attribute
	assert.False(t, ok)
	_, ok = resolve("node-a", "gpu-2") // unknown device
	assert.False(t, ok)
	_, ok = resolve("node-c", "gpu-0") // unknown pool
	assert.False(t, ok)

	_, err = newDRADeviceResolver(ctx, client, "")
	assert.Error(t, err)
}
