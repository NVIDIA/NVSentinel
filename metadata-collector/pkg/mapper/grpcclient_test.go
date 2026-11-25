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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	v1 "k8s.io/kubelet/pkg/apis/podresources/v1"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/model/modeltest"
)

/*
Covers the same GPU being allocated twice to the same container, the same
GPU being allocated to different containers in the same pod, and the same
GPU being allocated to 2 different pods.
*/
var (
	podDevices = map[string]map[string][]deviceInfo{
		"pod-1": {
			"container-1": {
				{
					resourceName: "nvidia.com/gpu",
					id:           "GPU-3",
				},
				{
					resourceName: "nvidia.com/gpu",
					id:           "GPU-2",
				},
				{
					resourceName: "nvidia.com/pgpu",
					id:           "GPU-1",
				},
				{
					resourceName: "nvidia.com/pgpu",
					id:           "GPU-1",
				},
				{
					resourceName: "unsupported-device",
					id:           "device-4",
				},
				{
					resourceName: "nvidia.com/gpu",
					id:           "GPU-1",
				},
			},
			"container-2": {
				{
					resourceName: "nvidia.com/pgpu",
					id:           "GPU-2",
				},
				{
					resourceName: "nvidia.com/gpu",
					id:           "GPU-1",
				},
			},
		},
		"pod-2": {
			"container-3": {
				{
					resourceName: "nvidia.com/gpu",
					id:           "GPU-1",
				},
				{
					resourceName: "nvidia.com/pgpu",
					id:           "GPU-7",
				},
			},
		},
		"pod-3": {},
		"pod-4": {
			"container-4": {
				{
					resourceName: "unsupported-device",
					id:           "device-7",
				},
			},
		},
	}
)

type MockPodResourcesServer struct {
	err            error
	errReturnCount int
	// pod -> container -> list of devices
	podDevices map[string]map[string][]deviceInfo
	// embed the interface we need to implement so we don't need to implement all methods
	v1.PodResourcesListerClient
}

type deviceInfo struct {
	resourceName string
	id           string
	// driverName set marks a DRA allocation: id is the DRA device name and resourceName the pool.
	driverName string
}

func NewMockPodResourcesServer(err error, errReturnCount int, podDevices map[string]map[string][]deviceInfo) *MockPodResourcesServer {
	return &MockPodResourcesServer{
		err:            err,
		errReturnCount: errReturnCount,
		podDevices:     podDevices,
	}
}

func (mock *MockPodResourcesServer) List(ctx context.Context,
	req *v1.ListPodResourcesRequest, options ...grpc.CallOption) (*v1.ListPodResourcesResponse, error) {
	if mock.err != nil && mock.errReturnCount > 0 {
		mock.errReturnCount--
		return nil, mock.err
	}
	var podResourceList []*v1.PodResources
	for podName, containerToDevices := range mock.podDevices {
		podResources := &v1.PodResources{
			Name:      podName,
			Namespace: "default",
		}
		for containerName, devices := range containerToDevices {
			containerResources := &v1.ContainerResources{
				Name: containerName,
			}
			for _, device := range devices {
				if device.driverName != "" {
					containerResources.DynamicResources = append(containerResources.DynamicResources, &v1.DynamicResource{
						ClaimName: "claim",
						ClaimResources: []*v1.ClaimResource{{
							DriverName: device.driverName,
							PoolName:   device.resourceName,
							DeviceName: device.id,
						}},
					})
					continue
				}
				containerResources.Devices = append(containerResources.Devices, &v1.ContainerDevices{
					ResourceName: device.resourceName,
					DeviceIds:    []string{device.id},
				})
			}
			podResources.Containers = append(podResources.Containers, containerResources)
		}
		podResourceList = append(podResourceList, podResources)

	}
	return &v1.ListPodResourcesResponse{
		PodResources: podResourceList,
	}, nil
}

func newTestKubeletGRPCClient(err error, errReturnCount int, podDevices map[string]map[string][]deviceInfo) *kubeletGRPClient {
	mockPodResourcesServer := NewMockPodResourcesServer(err, errReturnCount, podDevices)
	filter, filterErr := newDeviceFilter(modeltest.GPUEntityTypes())
	if filterErr != nil {
		panic(filterErr)
	}

	return &kubeletGRPClient{
		ctx:                context.Background(),
		podResourcesClient: mockPodResourcesServer,
		filter:             filter,
		resolver:           &fakeDRAResolver{},
	}
}

// fakeDRAResolver resolves the devices in values and records every device that it was asked for.
type fakeDRAResolver struct {
	values    map[draDevice]string
	requested []draDevice
}

func (f *fakeDRAResolver) resolve(_ context.Context, devices []draDevice) map[draDevice]string {
	f.requested = append(f.requested, devices...)
	resolved := map[draDevice]string{}

	for _, device := range devices {
		if value, ok := f.values[device]; ok {
			resolved[device] = value
		}
	}

	return resolved
}

func TestListPodResources(t *testing.T) {
	kubeletGRPCClient := newTestKubeletGRPCClient(nil, 0, podDevices)
	devicesPerPod, err := kubeletGRPCClient.ListPodResources()
	assert.NoError(t, err)
	assert.Equal(t, 2, len(devicesPerPod))

	expectedDevicesPerPod := map[string]*model.DeviceAnnotation{
		"default/pod-1": &model.DeviceAnnotation{
			Devices: map[string][]string{
				"nvidia.com/gpu":  {"GPU-1", "GPU-2", "GPU-3"},
				"nvidia.com/pgpu": {"GPU-1", "GPU-2"},
			},
		},
		"default/pod-2": &model.DeviceAnnotation{
			Devices: map[string][]string{
				"nvidia.com/gpu":  {"GPU-1"},
				"nvidia.com/pgpu": {"GPU-7"},
			},
		},
	}
	assert.Equal(t, devicesPerPod, expectedDevicesPerPod)
}

func TestListPodResourcesError(t *testing.T) {
	kubeletGRPCClient := newTestKubeletGRPCClient(errors.New("ListPodResources error"), 10, podDevices)
	_, err := kubeletGRPCClient.ListPodResources()
	assert.Error(t, err)
}

func TestListPodResourcesErrorWithRetry(t *testing.T) {
	kubeletGRPCClient := newTestKubeletGRPCClient(errors.New("ListPodResources error"), 1, podDevices)
	devicesPerPod, err := kubeletGRPCClient.ListPodResources()
	assert.NoError(t, err)
	assert.NotNil(t, devicesPerPod)
}

func gpuDRADevice(name string) draDevice {
	return draDevice{driver: model.GPUDRADriverName, pool: "node-a", name: name}
}

func TestListPodResources_DRADevices_RecordsTheAttributeValueUnderTheDriver(t *testing.T) {
	draDevices := map[string]map[string][]deviceInfo{
		"dra-pod": {
			"container-1": {
				{driverName: model.GPUDRADriverName, resourceName: "node-a", id: "gpu-1"},
				{driverName: model.GPUDRADriverName, resourceName: "node-a", id: "gpu-0"},
				{driverName: model.GPUDRADriverName, resourceName: "node-a", id: "gpu-1"},
				{driverName: "other.example.com", resourceName: "node-a", id: "gpu-0"},
				{resourceName: "nvidia.com/gpu", id: "GPU-plugin"},
			},
		},
	}

	resolver := &fakeDRAResolver{values: map[draDevice]string{
		gpuDRADevice("gpu-0"): "GPU-a0",
		gpuDRADevice("gpu-1"): "GPU-a1",
	}}
	client := newTestKubeletGRPCClient(nil, 0, draDevices)
	client.resolver = resolver

	devicesPerPod, err := client.ListPodResources()
	require.NoError(t, err)
	assert.Equal(t, map[string]*model.DeviceAnnotation{
		"default/dra-pod": {Devices: map[string][]string{
			model.GPUDRADriverName: {"GPU-a0", "GPU-a1"},
			"nvidia.com/gpu":       {"GPU-plugin"},
		}},
	}, devicesPerPod)
	assert.NotContains(t, resolver.requested, draDevice{driver: "other.example.com", pool: "node-a", name: "gpu-0"},
		"a driver that the configuration does not list is not resolved")
}

// A partial annotation would make node-drainer miss the pod, so a pod with an unresolved DRA device gets none,
// even when its other devices resolve.
func TestListPodResources_UnresolvedDRADevice_LeavesThePodOut(t *testing.T) {
	draDevices := map[string]map[string][]deviceInfo{
		"mixed-pod": {
			"container-1": {
				{resourceName: "nvidia.com/gpu", id: "GPU-plugin"},
				{driverName: model.GPUDRADriverName, resourceName: "node-a", id: "gpu-0"},
				{driverName: model.GPUDRADriverName, resourceName: "node-a", id: "gpu-9"},
			},
		},
		"plugin-pod": {
			"container-2": {{resourceName: "nvidia.com/gpu", id: "GPU-other"}},
		},
	}

	client := newTestKubeletGRPCClient(nil, 0, draDevices)
	client.resolver = &fakeDRAResolver{values: map[draDevice]string{gpuDRADevice("gpu-0"): "GPU-a0"}}

	devicesPerPod, err := client.ListPodResources()
	require.NoError(t, err)
	assert.Equal(t, map[string]*model.DeviceAnnotation{
		"default/plugin-pod": {Devices: map[string][]string{"nvidia.com/gpu": {"GPU-other"}}},
	}, devicesPerPod)
}

func TestListPodResources_ConfiguredEntityType_RecordsOnlyItsDevices(t *testing.T) {
	entityTypes := model.PartialRemediationEntityTypes{
		"ACCEL_SERIAL": {
			Devices: []model.PartialRemediationDevice{
				{ResourceNames: []string{"example.com/accel"}},
				{Driver: "accel.example.com", DeviceClasses: []string{"accel"}, EntityValueAttribute: "serial"},
			},
		},
	}
	filter, err := newDeviceFilter(entityTypes)
	require.NoError(t, err)

	accel := draDevice{driver: "accel.example.com", pool: "node-a", name: "accel-0"}
	client := newTestKubeletGRPCClient(nil, 0, map[string]map[string][]deviceInfo{
		"accel-pod": {
			"container-1": {
				{resourceName: "example.com/accel", id: "SN-plugin"},
				{driverName: "accel.example.com", resourceName: "node-a", id: "accel-0"},
				{resourceName: "nvidia.com/gpu", id: "GPU-1"},
				{driverName: model.GPUDRADriverName, resourceName: "node-a", id: "gpu-0"},
			},
		},
		"gpu-pod": {
			"container-2": {{resourceName: "nvidia.com/gpu", id: "GPU-2"}},
		},
	})
	client.filter = filter
	client.resolver = &fakeDRAResolver{values: map[draDevice]string{accel: "SN-dra"}}

	devicesPerPod, err := client.ListPodResources()
	require.NoError(t, err)
	assert.Equal(t, map[string]*model.DeviceAnnotation{
		"default/accel-pod": {Devices: map[string][]string{
			"example.com/accel": {"SN-plugin"},
			"accel.example.com": {"SN-dra"},
		}},
	}, devicesPerPod)
}

func TestListPodResources_NoDRADevices_DoesNotCallTheResolver(t *testing.T) {
	resolver := &fakeDRAResolver{}
	client := newTestKubeletGRPCClient(nil, 0, podDevices)
	client.resolver = resolver

	_, err := client.ListPodResources()
	require.NoError(t, err)
	assert.Empty(t, resolver.requested)
}
