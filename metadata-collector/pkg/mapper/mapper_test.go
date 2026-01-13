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
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

type mockKubeletHTTPSClient struct {
	pods []corev1.Pod
	err  error
}

func (m *mockKubeletHTTPSClient) ListPods() ([]corev1.Pod, error) {
	return m.pods, m.err
}

type mockKubeletGRPClient struct {
	devicesPerPod map[string]*model.DeviceAnnotation
	err           error
}

func (m *mockKubeletGRPClient) ListPodResources() (map[string]*model.DeviceAnnotation, error) {
	return m.devicesPerPod, m.err
}

type kubeletResponses struct {
	pods          []corev1.Pod
	httpClientErr error
	devicesPerPod map[string]*model.DeviceAnnotation
	grpcClientErr error
}

var (
	testPodTemplate = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "example-pod",
			Namespace: "default",
			Labels: map[string]string{
				"app": "demo",
			},
			Annotations: map[string]string{
				"example.com/owner": "team-a",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:1.25",
					Ports: []corev1.ContainerPort{
						{
							ContainerPort: 80,
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyAlways,
		},
	}
)

func newTestDeviceMapper(testCase *kubeletResponses, testPods []*corev1.Pod) (*podDeviceMapper, error) {
	mockHTTPSClient := &mockKubeletHTTPSClient{
		pods: testCase.pods,
		err:  testCase.httpClientErr,
	}
	mockGRPClient := &mockKubeletGRPClient{
		devicesPerPod: testCase.devicesPerPod,
		err:           testCase.grpcClientErr,
	}
	ctx := context.TODO()
	clientSet := fake.NewSimpleClientset()
	for _, pod := range testPods {
		_, err := clientSet.CoreV1().Pods(pod.GetNamespace()).Create(ctx, pod, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to create test pod: %v", err)
		}
	}
	return &podDeviceMapper{
		ctx:                ctx,
		kubeletHTTPSClient: mockHTTPSClient,
		kubeletGRPCClient:  mockGRPClient,
		kubernetesClient:   clientSet,
	}, nil
}

func TestUpdatePodDevicesAnnotationsWithListPodsError(t *testing.T) {
	existingTestPod := testPodTemplate.DeepCopy()
	testCase := &kubeletResponses{
		pods:          nil,
		httpClientErr: fmt.Errorf("list pods error"),
		devicesPerPod: nil,
		grpcClientErr: nil,
	}
	mapper, err := newTestDeviceMapper(testCase, []*corev1.Pod{existingTestPod})
	assert.NoError(t, err)

	numUpdates, err := mapper.UpdatePodDevicesAnnotations()
	assert.Error(t, err)
	assert.Equal(t, numUpdates, 0)
}

func TestUpdatePodDevicesAnnotationsWithListPodResourcesError(t *testing.T) {
	existingTestPod := testPodTemplate.DeepCopy()
	testCase := &kubeletResponses{
		pods:          []corev1.Pod{*existingTestPod},
		httpClientErr: nil,
		devicesPerPod: nil,
		grpcClientErr: fmt.Errorf("list pod resources error"),
	}
	mapper, err := newTestDeviceMapper(testCase, []*corev1.Pod{existingTestPod})
	assert.NoError(t, err)

	numUpdates, err := mapper.UpdatePodDevicesAnnotations()
	assert.Error(t, err)
	assert.Equal(t, numUpdates, 0)
}

func TestUpdatePodDevicesAnnotationsWithAnnotationAdded(t *testing.T) {
	podKey := "default/example-pod"
	devices, expectedDeviceAnnotationJSON, err := getDeviceAnnotation(nil, podKey, "GPU-123")
	assert.NoError(t, err)

	existingTestPod := testPodTemplate.DeepCopy()
	testCase := &kubeletResponses{
		pods:          []corev1.Pod{*existingTestPod},
		httpClientErr: nil,
		devicesPerPod: devices,
		grpcClientErr: nil,
	}
	mapper, err := newTestDeviceMapper(testCase, []*corev1.Pod{existingTestPod})
	assert.NoError(t, err)

	numUpdates, err := mapper.UpdatePodDevicesAnnotations()
	assert.NoError(t, err)
	assert.Equal(t, numUpdates, 1)
	pod, err := mapper.kubernetesClient.CoreV1().Pods(existingTestPod.GetNamespace()).
		Get(context.TODO(), existingTestPod.GetName(), metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, expectedDeviceAnnotationJSON, pod.GetAnnotations()[model.PodDeviceAnnotationName])
}

func TestUpdatePodDevicesAnnotationsWithAnnotationUpdated(t *testing.T) {
	podKey := "default/example-pod"
	newDevices, newDeviceAnnotationJSON, err := getDeviceAnnotation(nil, podKey, "GPU-123")
	assert.NoError(t, err)
	_, existingDeviceAnnotationJSON, err := getDeviceAnnotation(nil, podKey, "GPU-456")
	assert.NoError(t, err)

	existingTestPod := testPodTemplate.DeepCopy()
	existingTestPod.SetAnnotations(map[string]string{
		model.PodDeviceAnnotationName: existingDeviceAnnotationJSON,
	})
	testCase := &kubeletResponses{
		pods:          []corev1.Pod{*existingTestPod},
		httpClientErr: nil,
		devicesPerPod: newDevices,
		grpcClientErr: nil,
	}
	mapper, err := newTestDeviceMapper(testCase, []*corev1.Pod{existingTestPod})
	assert.NoError(t, err)

	numUpdates, err := mapper.UpdatePodDevicesAnnotations()
	assert.NoError(t, err)
	assert.Equal(t, numUpdates, 1)
	pod, err := mapper.kubernetesClient.CoreV1().Pods(existingTestPod.GetNamespace()).
		Get(context.TODO(), existingTestPod.GetName(), metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, newDeviceAnnotationJSON, pod.GetAnnotations()[model.PodDeviceAnnotationName])
}

func TestUpdatePodDevicesAnnotationsWithAnnotationNotChanged(t *testing.T) {
	podKey := "default/example-pod"
	existingDevices, existingDeviceAnnotationJSON, err := getDeviceAnnotation(nil, podKey, "GPU-456")
	assert.NoError(t, err)

	existingTestPod := testPodTemplate.DeepCopy()
	existingTestPod.SetAnnotations(map[string]string{
		model.PodDeviceAnnotationName: existingDeviceAnnotationJSON,
	})
	testCase := &kubeletResponses{
		pods:          []corev1.Pod{*existingTestPod},
		httpClientErr: nil,
		devicesPerPod: existingDevices,
		grpcClientErr: nil,
	}
	mapper, err := newTestDeviceMapper(testCase, []*corev1.Pod{existingTestPod})
	assert.NoError(t, err)

	numUpdates, err := mapper.UpdatePodDevicesAnnotations()
	assert.NoError(t, err)
	assert.Equal(t, numUpdates, 0)
	pod, err := mapper.kubernetesClient.CoreV1().Pods(existingTestPod.GetNamespace()).
		Get(context.TODO(), existingTestPod.GetName(), metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, existingDeviceAnnotationJSON, pod.GetAnnotations()[model.PodDeviceAnnotationName])
}

func TestUpdatePodDevicesAnnotationsWithAnnotationRemoved(t *testing.T) {
	podKey := "default/example-pod"
	_, existingDeviceAnnotationJSON, err := getDeviceAnnotation(nil, podKey, "GPU-456")
	assert.NoError(t, err)

	existingTestPod := testPodTemplate.DeepCopy()
	existingTestPod.SetAnnotations(map[string]string{
		model.PodDeviceAnnotationName: existingDeviceAnnotationJSON,
	})
	testCase := &kubeletResponses{
		pods:          []corev1.Pod{*existingTestPod},
		httpClientErr: nil,
		devicesPerPod: make(map[string]*model.DeviceAnnotation),
		grpcClientErr: nil,
	}
	mapper, err := newTestDeviceMapper(testCase, []*corev1.Pod{existingTestPod})
	assert.NoError(t, err)

	numUpdates, err := mapper.UpdatePodDevicesAnnotations()
	assert.NoError(t, err)
	assert.Equal(t, numUpdates, 1)
	pod, err := mapper.kubernetesClient.CoreV1().Pods(existingTestPod.GetNamespace()).
		Get(context.TODO(), existingTestPod.GetName(), metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, "", pod.GetAnnotations()[model.PodDeviceAnnotationName])
}

func TestUpdatePodDevicesAnnotationsWithNoAnnotation(t *testing.T) {
	existingTestPod := testPodTemplate.DeepCopy()

	testCase := &kubeletResponses{
		pods:          []corev1.Pod{*existingTestPod},
		httpClientErr: nil,
		devicesPerPod: make(map[string]*model.DeviceAnnotation),
		grpcClientErr: nil,
	}
	mapper, err := newTestDeviceMapper(testCase, []*corev1.Pod{existingTestPod})
	assert.NoError(t, err)

	numUpdates, err := mapper.UpdatePodDevicesAnnotations()
	assert.NoError(t, err)
	assert.Equal(t, numUpdates, 0)
	pod, err := mapper.kubernetesClient.CoreV1().Pods(existingTestPod.GetNamespace()).
		Get(context.TODO(), existingTestPod.GetName(), metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, "", pod.GetAnnotations()[model.PodDeviceAnnotationName])
}

func TestUpdatePodDevicesAnnotationsWithRetryablePatchError(t *testing.T) {
	podKey := "default/example-pod"
	devices, _, err := getDeviceAnnotation(nil, podKey, "GPU-123")
	assert.NoError(t, err)

	existingTestPod := testPodTemplate.DeepCopy()
	testCase := &kubeletResponses{
		pods:          []corev1.Pod{*existingTestPod},
		httpClientErr: nil,
		devicesPerPod: devices,
		grpcClientErr: nil,
	}
	mapper, err := newTestDeviceMapper(testCase, []*corev1.Pod{existingTestPod})
	assert.NoError(t, err)

	ctx := context.TODO()
	clientSet := fake.NewSimpleClientset()
	_, err = clientSet.CoreV1().Pods(existingTestPod.GetNamespace()).Create(ctx, existingTestPod, metav1.CreateOptions{})
	assert.NoError(t, err)

	callCount := 0
	clientSet.Fake.PrependReactor("patch", "pods", func(action ktesting.Action) (handled bool,
		ret runtime.Object, err error) {
		callCount++
		switch callCount {
		case 1:
			fmt.Println("error 1")
			return true, nil, errors.NewConflict(
				schema.GroupResource{Group: "", Resource: "pods"},
				"example-pod",
				fmt.Errorf("simulated update conflict"))
		default:
			return true, nil, nil
		}
	})
	mapper.kubernetesClient = clientSet

	numUpdates, err := mapper.UpdatePodDevicesAnnotations()
	assert.NoError(t, err)
	assert.Equal(t, numUpdates, 1)
}

func TestUpdatePodDevicesAnnotationsWithNonRetryablePatchError(t *testing.T) {
	podKey := "default/example-pod"
	devices, _, err := getDeviceAnnotation(nil, podKey, "GPU-123")
	assert.NoError(t, err)

	existingTestPod := testPodTemplate.DeepCopy()
	testCase := &kubeletResponses{
		pods:          []corev1.Pod{*existingTestPod},
		httpClientErr: nil,
		devicesPerPod: devices,
		grpcClientErr: nil,
	}
	mapper, err := newTestDeviceMapper(testCase, []*corev1.Pod{existingTestPod})
	assert.NoError(t, err)

	ctx := context.TODO()
	clientSet := fake.NewSimpleClientset()
	_, err = clientSet.CoreV1().Pods(existingTestPod.GetNamespace()).Create(ctx, existingTestPod, metav1.CreateOptions{})
	assert.NoError(t, err)

	callCount := 0
	clientSet.Fake.PrependReactor("patch", "pods", func(action ktesting.Action) (handled bool,
		ret runtime.Object, err error) {
		callCount++
		switch callCount {
		case 1:
			fmt.Println("error 1")
			return true, nil, errors.NewUnauthorized("unauthorized")
		default:
			return true, nil, nil
		}
	})
	mapper.kubernetesClient = clientSet

	numUpdates, err := mapper.UpdatePodDevicesAnnotations()
	assert.Error(t, err)
	assert.Equal(t, numUpdates, 0)
}

func TestUpdatePodDevicesAnnotationsWithMultiplePodsResourcesAndDevices(t *testing.T) {
	podKey1 := "default/example-pod"
	podKey2 := "default/example-pod2"
	deviceAnnotation := map[string]*model.DeviceAnnotation{
		podKey1: &model.DeviceAnnotation{
			Devices: map[string][]string{
				"nvidia.com/gpu": {
					"GPU-123",
					"GPU-456",
				},
				"nvidia.com/pgpu": {
					"GPU-789",
				},
			},
		},
		podKey2: &model.DeviceAnnotation{
			Devices: map[string][]string{
				"nvidia.com/gpu": {
					"GPU-123",
				},
			},
		},
	}

	_, newDeviceAnnotationJSONPod1, err := getDeviceAnnotation(deviceAnnotation, podKey1, "")
	assert.NoError(t, err)
	_, newDeviceAnnotationJSONPod2, err := getDeviceAnnotation(deviceAnnotation, podKey2, "")
	assert.NoError(t, err)

	existingTestPod1 := testPodTemplate.DeepCopy()
	existingTestPod2 := testPodTemplate.DeepCopy()
	existingTestPod2.SetName("example-pod2")

	testCase := &kubeletResponses{
		pods:          []corev1.Pod{*existingTestPod1, *existingTestPod2},
		httpClientErr: nil,
		devicesPerPod: deviceAnnotation,
		grpcClientErr: nil,
	}
	mapper, err := newTestDeviceMapper(testCase, []*corev1.Pod{existingTestPod1, existingTestPod2})
	assert.NoError(t, err)

	numUpdates, err := mapper.UpdatePodDevicesAnnotations()
	assert.NoError(t, err)

	assert.Equal(t, numUpdates, 2)
	pod, err := mapper.kubernetesClient.CoreV1().Pods(existingTestPod1.GetNamespace()).
		Get(context.TODO(), existingTestPod1.GetName(), metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, newDeviceAnnotationJSONPod1, pod.GetAnnotations()[model.PodDeviceAnnotationName])
	pod, err = mapper.kubernetesClient.CoreV1().Pods(existingTestPod2.GetNamespace()).
		Get(context.TODO(), existingTestPod2.GetName(), metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, newDeviceAnnotationJSONPod2, pod.GetAnnotations()[model.PodDeviceAnnotationName])
}

func getDeviceAnnotation(deviceAnnotation map[string]*model.DeviceAnnotation, podKey string, uuid string) (map[string]*model.DeviceAnnotation, string, error) {
	if deviceAnnotation == nil {
		deviceAnnotation = map[string]*model.DeviceAnnotation{
			podKey: &model.DeviceAnnotation{
				Devices: map[string][]string{
					"nvidia.com/gpu": {
						uuid,
					},
				},
			},
		}
	}
	deviceAnnotationJSON, err := json.Marshal(deviceAnnotation[podKey])
	if err != nil {
		return nil, "", err
	}
	return deviceAnnotation, string(deviceAnnotationJSON), nil
}
