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

package mapper

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/model/modeltest"
)

func TestNewDeviceFilter_GPUDefault_ListsResourceNamesAndDriver(t *testing.T) {
	filter, err := newDeviceFilter(modeltest.GPUEntityTypes())
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"nvidia.com/gpu": true, "nvidia.com/pgpu": true}, filter.resourceNames)
	assert.Equal(t, map[string]string{model.GPUDRADriverName: "uuid"}, filter.attributeByDriver)
}

func TestNewDeviceFilter_DriverWithTwoAttributes_ReturnsError(t *testing.T) {
	_, err := newDeviceFilter(model.PartialRemediationEntityTypes{
		"ACCEL_SERIAL": {Devices: []model.PartialRemediationDevice{
			{Driver: "accel.example.com", DeviceClasses: []string{"a"}, EntityValueAttribute: "serial"},
			{Driver: "accel.example.com", DeviceClasses: []string{"b"}, EntityValueAttribute: "uuid"},
		}},
	})
	require.ErrorContains(t, err, "accel.example.com has two entityValueAttribute values")
}

func TestAttributeValue_QualifiedAndUnqualifiedNames(t *testing.T) {
	attributes := map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
		"uuid":                                 {StringValue: ptr.To("GPU-1")},
		"resource.kubernetes.io/pciBusID":      {StringValue: ptr.To("0000:3b:00.0")},
		"index":                                {IntValue: ptr.To[int64](3)},
		"empty":                                {StringValue: ptr.To("")},
		"other.example.com/serial":             {StringValue: ptr.To("SN-other")},
		model.GPUDRADriverName + "/productName": {StringValue: ptr.To("H100")},
	}

	for _, tt := range []struct {
		name, attribute, want string
		found                 bool
	}{
		{"unqualified", "uuid", "GPU-1", true},
		{"qualified with the driver domain", model.GPUDRADriverName + "/uuid", "GPU-1", true},
		{"unqualified name of a qualified attribute", "productName", "H100", true},
		{"standard attribute", "resource.kubernetes.io/pciBusID", "0000:3b:00.0", true},
		{"another domain", "serial", "", false},
		{"not a string", "index", "", false},
		{"empty string", "empty", "", false},
		{"missing", "serialNumber", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			value, found := attributeValue(attributes, model.GPUDRADriverName, tt.attribute)
			assert.Equal(t, tt.found, found)
			assert.Equal(t, tt.want, value)
		})
	}
}

func TestCurrentPoolSlices_KeepsOnlyTheNewestGenerationOfEachPool(t *testing.T) {
	slice := func(name, pool string, generation int64) resourcev1.ResourceSlice {
		return resourcev1.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       resourcev1.ResourceSliceSpec{Pool: resourcev1.ResourcePool{Name: pool, Generation: generation}},
		}
	}

	current := currentPoolSlices([]resourcev1.ResourceSlice{
		slice("a-old", "a", 1), slice("a-new-1", "a", 2), slice("a-new-2", "a", 2), slice("b", "b", 5),
	})

	var names []string
	for _, s := range current {
		names = append(names, s.Name)
	}

	assert.Equal(t, []string{"a-new-1", "a-new-2", "b"}, names)
}

// startResourceSliceAPI starts an API server, because a fake client does not apply field selectors.
func startResourceSliceAPI(t *testing.T) kubernetes.Interface {
	t.Helper()

	environment := &envtest.Environment{}
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, environment.Stop()) })

	client, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)

	return client
}

func createResourceSlice(t *testing.T, client kubernetes.Interface, name, node, driver, pool string,
	generation int64, devices map[string]string) {
	t.Helper()

	slice := &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   driver,
			NodeName: ptr.To(node),
			Pool:     resourcev1.ResourcePool{Name: pool, Generation: generation, ResourceSliceCount: 1},
		},
	}

	for device, uuid := range devices {
		slice.Spec.Devices = append(slice.Spec.Devices, resourcev1.Device{
			Name:       device,
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{"uuid": {StringValue: ptr.To(uuid)}},
		})
	}

	_, err := client.ResourceV1().ResourceSlices().Create(t.Context(), slice, metav1.CreateOptions{})
	require.NoError(t, err)
}

func TestResourceSliceResolver_ResolvesDevicesOfItsNodeAndDriver(t *testing.T) {
	client := startResourceSliceAPI(t)
	createResourceSlice(t, client, "node-a-gpus", "node-a", model.GPUDRADriverName, "node-a", 2,
		map[string]string{"gpu-0": "GPU-a0", "gpu-1": "GPU-a1"})
	createResourceSlice(t, client, "node-a-gpus-stale", "node-a", model.GPUDRADriverName, "node-a", 1,
		map[string]string{"gpu-2": "GPU-stale"})
	createResourceSlice(t, client, "node-b-gpus", "node-b", model.GPUDRADriverName, "node-a", 2,
		map[string]string{"gpu-3": "GPU-b3"})
	createResourceSlice(t, client, "node-a-other", "node-a", "other.example.com", "node-a", 2,
		map[string]string{"gpu-4": "OTHER-4"})

	resolver := newResourceSliceResolver(client, "node-a",
		map[string]string{model.GPUDRADriverName: "uuid", "other.example.com": "uuid"})

	gpu0, gpu1 := gpuDRADevice("gpu-0"), gpuDRADevice("gpu-1")
	stale, otherNode := gpuDRADevice("gpu-2"), gpuDRADevice("gpu-3")
	otherDriverName := draDevice{driver: model.GPUDRADriverName, pool: "node-a", name: "gpu-4"}

	resolved := resolver.resolve(t.Context(), []draDevice{gpu0, gpu1, stale, otherNode, otherDriverName})
	assert.Equal(t, map[draDevice]string{gpu0: "GPU-a0", gpu1: "GPU-a1"}, resolved,
		"stale generations, other nodes and other drivers do not resolve")
}

func TestResourceSliceResolver_ResolvedDevice_IsNotListedAgain(t *testing.T) {
	client := startResourceSliceAPI(t)
	createResourceSlice(t, client, "node-a-gpus", "node-a", model.GPUDRADriverName, "node-a", 1,
		map[string]string{"gpu-0": "GPU-a0"})

	resolver := newResourceSliceResolver(client, "node-a", map[string]string{model.GPUDRADriverName: "uuid"})
	gpu0, gpu1 := gpuDRADevice("gpu-0"), gpuDRADevice("gpu-1")

	assert.Equal(t, map[draDevice]string{gpu0: "GPU-a0"}, resolver.resolve(t.Context(), []draDevice{gpu0, gpu1}),
		"a device that the driver has not published yet stays unresolved")

	require.NoError(t, client.ResourceV1().ResourceSlices().Delete(t.Context(), "node-a-gpus",
		metav1.DeleteOptions{}))
	createResourceSlice(t, client, "node-a-gpus-2", "node-a", model.GPUDRADriverName, "node-a", 2,
		map[string]string{"gpu-1": "GPU-a1"})

	assert.Equal(t, map[draDevice]string{gpu0: "GPU-a0", gpu1: "GPU-a1"},
		resolver.resolve(t.Context(), []draDevice{gpu0, gpu1}),
		"gpu-0 comes from the cache, and gpu-1 resolves on the next poll")
}

func TestResourceSliceResolver_NoNodeName_LeavesDevicesUnresolved(t *testing.T) {
	client := startResourceSliceAPI(t)
	createResourceSlice(t, client, "node-a-gpus", "node-a", model.GPUDRADriverName, "node-a", 1,
		map[string]string{"gpu-0": "GPU-a0"})

	resolver := newResourceSliceResolver(client, "", map[string]string{model.GPUDRADriverName: "uuid"})
	assert.Empty(t, resolver.resolve(t.Context(), []draDevice{gpuDRADevice("gpu-0")}))
}
