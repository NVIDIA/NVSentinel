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
	"slices"
	"sort"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/resolver"
	"k8s.io/client-go/util/retry"
	v1 "k8s.io/kubelet/pkg/apis/podresources/v1"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

const (
	podResourcesKubeletSocket = "/var/lib/kubelet/pod-resources/kubelet.sock"

	draGPUDriverName = "gpu.nvidia.com"
	// draGPUDeviceNameFormat is how the NVIDIA DRA driver names a full GPU from its minor number, the N in
	// /dev/nvidiaN. It mirrors GpuInfo.CanonicalName() in cmd/gpu-kubelet-plugin/deviceinfo.go
	// (https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/495bf4c/cmd/gpu-kubelet-plugin/deviceinfo.go#L122),
	// which the driver keeps on purpose because the minor is fixed for as long as the GPU stays on the bus.
	draGPUDeviceNameFormat = "gpu-%d"
)

type KubeletGRPClient interface {
	ListPodResources() (map[string]*model.DeviceAnnotation, error)
}

type kubeletGRPClient struct {
	ctx context.Context

	connection         *grpc.ClientConn
	podResourcesClient v1.PodResourcesListerClient
	// draDeviceUUIDs maps this node's DRA device names to GPU UUIDs; an unknown name is skipped.
	draDeviceUUIDs map[string]string
}

// NewKubeletGRPClient connects to the kubelet PodResources socket. draDeviceUUIDs maps this node's DRA device
// names to GPU UUIDs.
func NewKubeletGRPClient(ctx context.Context, uuidsByMinor map[int]string) (KubeletGRPClient, error) {
	_, err := os.Stat(podResourcesKubeletSocket)
	if err != nil {
		return nil, err
	}

	resolver.SetDefaultScheme("passthrough")

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
		draDeviceUUIDs:     draDeviceUUIDs(uuidsByMinor),
	}, nil
}

/*
ListPodResources calls the PodResourcesLister gRPC service listening on a local Unix socket. The metadata-collector
daemonset mounts the host's <kubelet root dir>/pod-resources at /var/lib/kubelet/pod-resources to expose the socket.

Devices allocated by device plugins arrive in each container's devices list keyed by resource name. Devices allocated
through DRA ResourceClaims arrive in dynamic_resources as (driver, pool, device name) and are resolved to GPU UUIDs
through the node's NVML minor numbers, keyed by the driver name.

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
	      "nvidia.com/pgpu": [
	        "GPU-1",
	        "GPU-2"
	      ]
	    }
	  },
	  "default/pod-2": {
	    "devices": {
	      "nvidia.com/gpu": [
	        "GPU-1"
	      ],
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

	devicesPerPod := make(map[string]*model.DeviceAnnotation)

	for _, pod := range listPodResourcesResponse.GetPodResources() {
		podKey := pod.GetNamespace() + "/" + pod.GetName()

		for _, container := range pod.GetContainers() {
			for _, device := range container.GetDevices() {
				if isSupportedResourceName(device.GetResourceName()) {
					addPodDevices(devicesPerPod, podKey, device.GetResourceName(), device.GetDeviceIds()...)
				}
			}

			for _, claim := range container.GetDynamicResources() {
				for _, resource := range claim.GetClaimResources() {
					addDRADevice(devicesPerPod, podKey, resource, client.draDeviceUUIDs)
				}
			}
		}
	}

	sortAndRemoveDuplicateDevices(devicesPerPod)

	return devicesPerPod, nil
}

// addPodDevices records the pod as a device holder even with no IDs, then appends any IDs under resourceName.
func addPodDevices(devicesPerPod map[string]*model.DeviceAnnotation, podKey, resourceName string,
	deviceIDs ...string) {
	annotation, ok := devicesPerPod[podKey]
	if !ok {
		annotation = &model.DeviceAnnotation{Devices: make(map[string][]string)}
		devicesPerPod[podKey] = annotation
	}

	if len(deviceIDs) > 0 {
		annotation.Devices[resourceName] = append(annotation.Devices[resourceName], deviceIDs...)
	}
}

// draDeviceUUIDs maps each DRA device name to its GPU UUID from the node's GPU minor numbers.
func draDeviceUUIDs(uuidsByMinor map[int]string) map[string]string {
	uuidsByName := make(map[string]string, len(uuidsByMinor))
	for minor, uuid := range uuidsByMinor {
		uuidsByName[fmt.Sprintf(draGPUDeviceNameFormat, minor)] = uuid
	}

	return uuidsByName
}

// addDRADevice records a GPU allocated through a ResourceClaim under the DRA driver name. An allocation whose
// device name is unknown is skipped; the pod is annotated once it resolves on a later poll.
func addDRADevice(devicesPerPod map[string]*model.DeviceAnnotation, podKey string, resource *v1.ClaimResource,
	draDeviceUUIDs map[string]string) {
	if resource.GetDriverName() != draGPUDriverName {
		return
	}

	uuid, ok := draDeviceUUIDs[resource.GetDeviceName()]
	if !ok {
		slog.Warn("No GPU matches DRA allocation, retrying on the next poll",
			"podKey", podKey, "pool", resource.GetPoolName(), "device", resource.GetDeviceName())

		return
	}

	addPodDevices(devicesPerPod, podKey, draGPUDriverName, uuid)
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

func isSupportedResourceName(resourceName string) bool {
	for _, resourceNamesForEntityType := range model.EntityTypeToResourceNames {
		if slices.Contains(resourceNamesForEntityType, resourceName) {
			return true
		}
	}

	return false
}

func (client *kubeletGRPClient) Close() {
	client.connection.Close()
}
