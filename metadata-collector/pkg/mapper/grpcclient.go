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
	"net"
	"os"
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
)

type KubeletGRPClient interface {
	ListPodResources() (map[string]*model.DeviceAnnotation, error)
}

type kubeletGRPClient struct {
	ctx context.Context

	connection         *grpc.ClientConn
	podResourcesClient v1.PodResourcesListerClient
}

func NewKubeletGRPClient(ctx context.Context) (KubeletGRPClient, error) {
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
	}, nil
}

/*
ListPodResources calls the PodResourcesLister gRPC service listening on a local Unix socket. The metadata-collector
daemonset requires a HostPath volume configured to mount the /var/lib/kubelet/pod-resources/kubelet.sock Unix socket.

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
		for _, container := range pod.GetContainers() {
			for _, device := range container.GetDevices() {
				resourceName := device.GetResourceName()
				if isSupportedResourceName(resourceName) {
					podKey := pod.GetNamespace() + "/" + pod.GetName()
					if _, ok := devicesPerPod[podKey]; !ok {
						devicesPerPod[podKey] = &model.DeviceAnnotation{
							Devices: make(map[string][]string),
						}
					}

					if len(device.GetDeviceIds()) > 0 {
						devicesPerPod[podKey].Devices[resourceName] = append(devicesPerPod[podKey].Devices[resourceName],
							device.GetDeviceIds()...)
					}
				}
			}
		}
	}

	sortAndRemoveDuplicateDevices(devicesPerPod)

	return devicesPerPod, nil
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
		for _, currentResourceName := range resourceNamesForEntityType {
			if resourceName == currentResourceName {
				return true
			}
		}
	}

	return false
}

func (client *kubeletGRPClient) Close() {
	client.connection.Close()
}
