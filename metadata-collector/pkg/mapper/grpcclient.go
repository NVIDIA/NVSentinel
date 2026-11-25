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
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	grpcresolver "google.golang.org/grpc/resolver"
	"k8s.io/client-go/util/retry"
	v1 "k8s.io/kubelet/pkg/apis/podresources/v1"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

const podResourcesKubeletSocket = "/var/lib/kubelet/pod-resources/kubelet.sock"

type KubeletGRPClient interface {
	ListPodResources() (map[string]*model.DeviceAnnotation, error)
}

type kubeletGRPClient struct {
	ctx context.Context

	connection         *grpc.ClientConn
	podResourcesClient v1.PodResourcesListerClient
	// filter holds the configured devices. The client records no other device.
	filter deviceFilter
	// resolver finds the entity value of DRA devices.
	resolver draDeviceResolver
}

// NewKubeletGRPClient connects to the kubelet PodResources socket. It records the devices that filter lists,
// and finds the entity value of DRA devices through resolver.
func NewKubeletGRPClient(ctx context.Context, filter deviceFilter, resolver draDeviceResolver) (KubeletGRPClient,
	error) {
	_, err := os.Stat(podResourcesKubeletSocket)
	if err != nil {
		return nil, err
	}

	grpcresolver.SetDefaultScheme("passthrough")

	connection, err := grpc.NewClient(
		podResourcesKubeletSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "unix", addr)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failure connecting to '%s'; err: %w", podResourcesKubeletSocket, err)
	}

	client := v1.NewPodResourcesListerClient(connection)

	return &kubeletGRPClient{
		ctx:                ctx,
		connection:         connection,
		podResourcesClient: client,
		filter:             filter,
		resolver:           resolver,
	}, nil
}

/*
ListPodResources calls the PodResourcesLister gRPC service listening on a local Unix socket. The metadata-collector
daemonset mounts the host's <kubelet root dir>/pod-resources at /var/lib/kubelet/pod-resources to expose the socket.

Devices allocated by device plugins arrive in each container's devices list keyed by resource name. The client records
them for the configured resource names, and the device IDs are the entity values. Devices allocated through DRA
ResourceClaims arrive in dynamic_resources as (driver, pool, device name). The client records them for the configured
drivers, under the driver name, with the value of the configured ResourceSlice attribute (ADR-071).

A pod with a DRA device that does not resolve is left out, so it gets no annotation. A partial annotation would make
node-drainer miss the pod. Without an annotation, node-drainer sees a pod with a claim and no annotation, and the
partial drain waits instead.

This function returns a mapping from pods to all devices used by any container in that pod. Additionally, it will
ensure that each pod has a unique list of devices (even if multiple containers are allocated the same device) and
it will ensure that the device list is sorted so that callers of this function can directly check for changes to the
devices allocated to any pod. It's possible that the same device is allocated to multiple pods.

Example output:

	{
	  "default/pod-1": {
	    "devices": {
	      "nvidia.com/gpu": [
	        "GPU-1",
	        "GPU-2",
	        "GPU-3"
	      ],
	      "gpu.nvidia.com": [
	        "GPU-4"
	      ]
	    }
	  },
	  "default/pod-2": {
	    "devices": {
	      "nvidia.com/pgpu": [
	        "GPU-7"
	      ]
	    }
	  }
	}
*/
func (client *kubeletGRPClient) ListPodResources() (map[string]*model.DeviceAnnotation, error) {
	var listPodResourcesResponse *v1.ListPodResourcesResponse

	var listError error

	err := retry.OnError(retry.DefaultRetry, retryAllErrors, func() error {
		listPodResourcesResponse, listError = client.podResourcesClient.List(client.ctx, &v1.ListPodResourcesRequest{})
		if listError != nil {
			return fmt.Errorf("got an error calling ListPodResources: %w", listError)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	allocations, draDevices := client.collectAllocations(listPodResourcesResponse)

	var resolved map[draDevice]string
	if len(draDevices) > 0 {
		resolved = client.resolver.resolve(client.ctx, draDevices)
	}

	devicesPerPod := make(map[string]*model.DeviceAnnotation, len(allocations))

	for podKey, allocation := range allocations {
		if annotation, ok := allocation.annotation(podKey, resolved); ok {
			devicesPerPod[podKey] = annotation
		}
	}

	sortAndRemoveDuplicateDevices(devicesPerPod)

	return devicesPerPod, nil
}

// podAllocation holds the configured devices of one pod: device-plugin IDs by resource name, and DRA devices.
type podAllocation struct {
	devices    map[string][]string
	draDevices []draDevice
}

// collectAllocations returns the configured devices of each pod, and every DRA device to resolve.
func (client *kubeletGRPClient) collectAllocations(response *v1.ListPodResourcesResponse) (
	map[string]*podAllocation, []draDevice) {
	allocations := make(map[string]*podAllocation)

	var draDevices []draDevice

	for _, pod := range response.GetPodResources() {
		podKey := pod.GetNamespace() + "/" + pod.GetName()

		for _, container := range pod.GetContainers() {
			for _, device := range container.GetDevices() {
				if client.filter.resourceNames[device.GetResourceName()] {
					allocationOf(allocations, podKey).addDevicePluginIDs(device.GetResourceName(),
						device.GetDeviceIds())
				}
			}

			for _, claim := range container.GetDynamicResources() {
				for _, resource := range claim.GetClaimResources() {
					if _, ok := client.filter.attributeByDriver[resource.GetDriverName()]; !ok {
						continue
					}

					device := draDevice{
						driver: resource.GetDriverName(),
						pool:   resource.GetPoolName(),
						name:   resource.GetDeviceName(),
					}
					allocation := allocationOf(allocations, podKey)
					allocation.draDevices = append(allocation.draDevices, device)
					draDevices = append(draDevices, device)
				}
			}
		}
	}

	return allocations, draDevices
}

// allocationOf returns the allocation of the pod, and records the pod as a device holder.
func allocationOf(allocations map[string]*podAllocation, podKey string) *podAllocation {
	allocation, ok := allocations[podKey]
	if !ok {
		allocation = &podAllocation{devices: make(map[string][]string)}
		allocations[podKey] = allocation
	}

	return allocation
}

func (allocation *podAllocation) addDevicePluginIDs(resourceName string, ids []string) {
	if len(ids) > 0 {
		allocation.devices[resourceName] = append(allocation.devices[resourceName], ids...)
	}
}

// annotation returns the device annotation of the pod, or false when a DRA device of the pod is unresolved.
func (allocation *podAllocation) annotation(podKey string, resolved map[draDevice]string) (
	*model.DeviceAnnotation, bool) {
	annotation := &model.DeviceAnnotation{Devices: allocation.devices}

	for _, device := range allocation.draDevices {
		value, ok := resolved[device]
		if !ok {
			slog.Warn("DRA device has no entity value yet; the pod gets no device annotation until it resolves",
				"podKey", podKey, "driver", device.driver, "pool", device.pool, "device", device.name)

			return nil, false
		}

		annotation.Devices[device.driver] = append(annotation.Devices[device.driver], value)
	}

	return annotation, true
}

func sortAndRemoveDuplicateDevices(devicesPerPod map[string]*model.DeviceAnnotation) {
	for _, deviceAnnotation := range devicesPerPod {
		for resourceName, devices := range deviceAnnotation.Devices {
			sort.Strings(devices)

			j := 0
			for i := 1; i < len(devices); i++ {
				if devices[i] != devices[j] {
					j++
					devices[j] = devices[i]
				}
			}

			deviceAnnotation.Devices[resourceName] = devices[:j+1]
		}
	}
}

func (client *kubeletGRPClient) Close() {
	client.connection.Close()
}
