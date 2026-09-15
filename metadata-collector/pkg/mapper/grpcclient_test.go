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
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	v1 "k8s.io/kubelet/pkg/apis/podresources/v1"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
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
	return &kubeletGRPClient{
		ctx:                context.Background(),
		podResourcesClient: mockPodResourcesServer,
		requestTimeout:     testCallTimeout,
	}
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

type podResourcesFixture struct {
	v1.UnimplementedPodResourcesListerServer
	response *v1.ListPodResourcesResponse
}

func (s *podResourcesFixture) List(context.Context, *v1.ListPodResourcesRequest) (*v1.ListPodResourcesResponse, error) {
	return s.response, nil
}

// startPodResourcesFixture serves the real gRPC protocol on an isolated Unix socket.
func startPodResourcesFixture(t *testing.T, socket string, fixture v1.PodResourcesListerServer) *grpc.Server {
	t.Helper()

	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	server := grpc.NewServer()
	v1.RegisterPodResourcesListerServer(server, fixture)
	go func() {
		assert.NoError(t, server.Serve(listener))
	}()
	t.Cleanup(server.Stop)

	return server
}

// testPodResourcesSocket keeps the Unix socket path below macOS's 104-byte limit.
func testPodResourcesSocket(t *testing.T) string {
	t.Helper()

	require.NoError(t, os.MkdirAll("tmp", 0o700))
	dir, err := os.MkdirTemp("tmp", "mc-")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, os.RemoveAll(dir)) })

	return filepath.Join(dir, "k.sock")
}

func TestPodResources_DelayedSocketAndRestart_Reconnects(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	socket := testPodResourcesSocket(t)
	client, err := newKubeletGRPClient(ctx, socket)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := client.ListPodResources()
		done <- err
	}()

	// Prove a request reached the absent socket before bringing the fixture up.
	require.Eventually(t, func() bool {
		return client.connection.GetState() == connectivity.TransientFailure
	}, 5*time.Second, time.Millisecond)
	fixture := &podResourcesFixture{response: &v1.ListPodResourcesResponse{}}
	server := startPodResourcesFixture(t, socket, fixture)
	require.NoError(t, <-done)
	server.Stop()

	startPodResourcesFixture(t, socket, fixture)
	_, err = client.ListPodResources()
	require.NoError(t, err)

	cancel()
	require.Eventually(t, func() bool {
		return client.connection.GetState() == connectivity.Shutdown
	}, time.Second, time.Millisecond)
}

func TestPodResources_UnavailableSocket_RequestIsBounded(t *testing.T) {
	client, err := newKubeletGRPClient(t.Context(), testPodResourcesSocket(t))
	require.NoError(t, err)
	client.requestTimeout = 100 * time.Millisecond

	_, err = client.ListPodResources()
	require.ErrorContains(t, err, "DeadlineExceeded")
}

func TestPodResources_CancelledContext_StopsWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	client, err := newKubeletGRPClient(ctx, testPodResourcesSocket(t))
	require.NoError(t, err)
	cancel()

	_, err = client.ListPodResources()
	require.Error(t, err)
	require.Eventually(t, func() bool {
		return client.connection.GetState() == connectivity.Shutdown
	}, time.Second, time.Millisecond)
}
